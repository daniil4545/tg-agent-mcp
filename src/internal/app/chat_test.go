package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// stubDialog is a stand-in dialog transport on top of the existing
// stubTransport: sending and resolving come from there, reading is supplied here.
type stubDialog struct {
	stubTransport

	found   []Contact
	dialogs []DialogInfo
	// poll returns the history by poll number: waiting for a new message is
	// otherwise untestable - it is exactly "the history changed between polls".
	poll func(n int) []ChatMessage
	// ref replaces the correspondent's ref: a group ref is the only way to test
	// the "private chats only" gate, since dry-run returns an empty one.
	ref telegram.Ref
	// resolveErr is a resolve and read failure: an account restriction comes from
	// them earlier than from the send itself.
	resolveErr error

	polls        int
	findLimit    int
	historyDepth int
}

func (d *stubDialog) Resolve(ctx context.Context, username string) (Peer, error) {
	if d.resolveErr != nil {
		return Peer{}, d.resolveErr
	}
	if d.ref != "" {
		return Peer{Username: username, Ref: d.ref}, nil
	}
	return d.stubTransport.Resolve(ctx, username)
}

func (d *stubDialog) Lookup(_ context.Context, username string) (Peer, error) {
	return d.Resolve(context.Background(), username)
}

func (d *stubDialog) Find(_ context.Context, _ string, limit int) ([]Contact, error) {
	d.findLimit = limit
	return d.found, nil
}

func (d *stubDialog) History(_ context.Context, _ Peer, limit int) ([]ChatMessage, error) {
	d.historyDepth = limit
	d.polls++
	if d.poll == nil {
		return nil, nil
	}
	return d.poll(d.polls), nil
}

func (d *stubDialog) Dialogs(_ context.Context, _ int) ([]DialogInfo, error) {
	return d.dialogs, nil
}

// chatOn builds a dialog on the test service: the FLOOD_WAIT threshold is one
// minute, as for the sender.
func chatOn(service *Service, transport Dialog) *Chat {
	service.cfg.FloodWaitThreshold = time.Minute
	return NewChat(service, transport)
}

// offlineChat is a dialog without a database: reading, search and the dialog list
// do not touch it, so the fast tier tests them without Postgres.
func offlineChat(transport Dialog) *Chat {
	return NewChat(&Service{cfg: Config{AccountLabel: "test"}, Now: time.Now}, transport)
}

// sendOK is a manual send that must succeed.
func sendOK(t *testing.T, chat *Chat, username, text string) DirectSend {
	t.Helper()
	send, err := chat.Send(context.Background(), username, text)
	if err != nil {
		t.Fatalf("send @%s: %v", username, err)
	}
	return send
}

// directLog is the latest manual-touch journal row for a username.
type directLog struct {
	state     string
	cold      bool
	messageID *int64
}

func directRow(t *testing.T, service *Service, username string) directLog {
	t.Helper()
	var row directLog
	err := service.pool.QueryRow(context.Background(),
		`SELECT state, cold, message_id FROM direct_messages WHERE username = $1 ORDER BY id DESC LIMIT 1`,
		username).Scan(&row.state, &row.cold, &row.messageID)
	if err != nil {
		t.Fatalf("read journal %q: %v", username, err)
	}
	return row
}

func directCount(t *testing.T, service *Service) int {
	t.Helper()
	var count int
	if err := service.pool.QueryRow(context.Background(), `SELECT count(*) FROM direct_messages`).Scan(&count); err != nil {
		t.Fatalf("count journal: %v", err)
	}
	return count
}

// runSQL sets a gate directly with SQL: the stop list and the account cap are set
// bypassing the domain, otherwise a gate test depends on the tools.
func runSQL(t *testing.T, service *Service, sql string, args ...any) {
	t.Helper()
	if _, err := service.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("setup (%s): %v", sql, err)
	}
}

// claimOne hands a recipient row to the sender: from this moment the campaign
// carries it to Telegram.
func claimOne(t *testing.T, service *Service) *Recipient {
	t.Helper()
	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(context.Background(), "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	return claimed
}

// stuckClaim leaves a row claimed forever: the sender died before the end, the
// campaign was paused, and nobody is left to clean the row up.
func stuckClaim(t *testing.T, service *Service) {
	t.Helper()
	claimOne(t, service)
	if _, err := service.StopCampaign(context.Background(), false); err != nil {
		t.Fatalf("pause campaign: %v", err)
	}
	service.Now = func() time.Time { return testNow.Add(time.Hour) }
}

// Requested values are clamped to bounds: unbounded depth blows up the tool
// answer, unbounded waiting hangs the call. The service clock jumps by half the
// cap per poll, so the requested ten minutes end in two steps, not sixty.
func TestReadCapsDepthAndWait(t *testing.T) {
	transport := &stubDialog{}
	chat := offlineChat(transport)
	ctx := context.Background()

	if _, err := chat.Read(ctx, "ivan_petrov", 500, 0); err != nil {
		t.Fatalf("read with a large depth: %v", err)
	}
	if transport.historyDepth != maxHistory {
		t.Fatalf("depth %d, expected cap %d", transport.historyDepth, maxHistory)
	}
	if _, err := chat.Read(ctx, "ivan_petrov", 0, 0); err != nil {
		t.Fatalf("read without depth: %v", err)
	}
	if transport.historyDepth != defaultHistory {
		t.Fatalf("default depth %d, expected %d", transport.historyDepth, defaultHistory)
	}
	if _, err := chat.Read(ctx, "Иван Петров", 0, 0); !errors.Is(err, ErrNoUsername) {
		t.Fatalf("read by name: err = %v, expected ErrNoUsername", err)
	}

	start := time.Now()
	step := maxWait / 2
	chat.service.Now = func() time.Time { return start.Add(time.Duration(transport.polls) * step) }
	transport.polls = 0
	if _, err := chat.Read(ctx, "ivan_petrov", 0, 10*time.Minute); err != nil {
		t.Fatalf("read with waiting: %v", err)
	}
	if transport.polls != 3 {
		t.Fatalf("polls %d, expected 3: ten minutes did not fit the cap %s", transport.polls, maxWait)
	}

	// A negative wait means "do not wait at all": the answer goes out on the first poll.
	transport.polls = 0
	talk, err := chat.Read(ctx, "ivan_petrov", 0, -time.Second)
	if err != nil {
		t.Fatalf("read without waiting: %v", err)
	}
	if transport.polls != 1 || talk.Waited != 0 {
		t.Fatalf("negative wait gave %d polls and wait %s", transport.polls, talk.Waited)
	}
}

// Group and channel dialogs reach the output on a par with private ones, in
// transport order: the list no longer filters or truncates anything itself.
func TestDialogsIncludeGroupsAndChannels(t *testing.T) {
	chat := offlineChat(&stubDialog{dialogs: []DialogInfo{
		{Username: "ivan_petrov", Ref: telegram.NewRef(telegram.KindUser, 10)},
		{Name: "Чат клуба", Ref: telegram.NewRef(telegram.KindChat, 11)},
		{Name: "Анонсы", Ref: telegram.NewRef(telegram.KindChannel, 12), Broadcast: true},
	}})

	list, err := chat.Dialogs(context.Background(), 0)
	if err != nil {
		t.Fatalf("dialog list: %v", err)
	}
	if len(list) != 3 || list[1].Name != "Чат клуба" || list[2].Name != "Анонсы" || !list[2].Broadcast {
		t.Fatalf("group or channel did not reach the output: %+v", list)
	}
}

// Search returns no more than its cap and asks the transport for the same: a
// long list of names in a tool answer is not readable at all.
func TestFindNamesSearchLimits(t *testing.T) {
	transport := &stubDialog{}
	for i := range maxFound + 10 {
		transport.found = append(transport.found, Contact{Name: string(rune('а' + i%32))})
	}
	chat := offlineChat(transport)

	found, err := chat.Find(context.Background(), "Иван")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != maxFound || transport.findLimit != maxFound {
		t.Fatalf("found %d, asked the transport for %d", len(found), transport.findLimit)
	}
	if _, err := chat.Find(context.Background(), "  "); err == nil {
		t.Fatal("empty query went to Telegram")
	}
}

// Waiting has two exits, and both are tested. The other side's reply is returned
// separately from the history because the tool answer names it differently; the
// correspondent's silence is a normal answer, not an error. An attachment arrives
// as a kind marker: the content is not downloaded, but the tool must be able to
// say "there was a photo here".
func TestReadWaitEndsWithReplyOrTimeout(t *testing.T) {
	ctx := context.Background()
	history := []ChatMessage{{ID: 1, Text: "смотрите", MediaKind: "photo", Outgoing: true}}

	talk, err := offlineChat(&stubDialog{poll: func(n int) []ChatMessage {
		if n >= 2 {
			return append(history, ChatMessage{ID: 2, Text: "ага, понял"})
		}
		return history
	}}).Read(ctx, "ivan_petrov", 0, maxWait)
	if err != nil {
		t.Fatalf("read with waiting: %v", err)
	}
	if len(talk.Fresh) != 1 || talk.Fresh[0].Text != "ага, понял" {
		t.Fatalf("new message was not awaited: %+v", talk)
	}
	if len(talk.Messages) != 1 || talk.Messages[0].MediaKind != "photo" || talk.Waited <= 0 {
		t.Fatalf("history mixed with the new message, lost the attachment, or the wait was not measured: %+v", talk)
	}

	quiet, err := offlineChat(&stubDialog{poll: func(int) []ChatMessage { return history }}).
		Read(ctx, "ivan_petrov", 0, time.Second)
	if err != nil {
		t.Fatalf("wait ended with an error: %v", err)
	}
	if len(quiet.Fresh) != 0 || quiet.Waited != time.Second {
		t.Fatalf("timeout answer: %+v", quiet)
	}
}

// The journal row is written before the network call: "Telegram confirmed - the
// process died - no record" would leave the person open to the next campaign.
func TestDirectSendRecordedBeforeDelivery(t *testing.T) {
	service := newService(t)

	var stateAtSend string
	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		stateAtSend = directRow(t, service, "ivan_petrov").state
		return nil
	}}}

	send := sendOK(t, chatOn(service, transport), "ivan_petrov", "привет")
	row := directRow(t, service, "ivan_petrov")
	if stateAtSend != "pending" || row.state != "sent" || !row.cold || !send.Cold {
		t.Fatalf("during sending the row was %q, after - %q, cold %v, answer %+v",
			stateAtSend, row.state, row.cold, send)
	}
}

// A refusal in which the message certainly did not go out releases the touch: the
// person is available to campaigns again, otherwise they get nothing at all.
// FLOOD_WAIT is here too: the server refused to execute the request, no message
// was created, and pending would close the person to all campaigns forever.
func TestPermanentFailureDoesNotCloseCampaigns(t *testing.T) {
	cases := map[string]error{
		"outside the allow-list":      ErrNotAllowed,
		"private messages closed":     &telegram.Failure{Kind: telegram.KindPeerForbidden, Permanent: true},
		"username taken by a channel": &telegram.Failure{Kind: telegram.KindNotPerson, Permanent: true},
		"request rejected":            &telegram.Failure{Kind: telegram.KindBadRequest, Permanent: true},
		"short FLOOD_WAIT":            floodWait(10 * time.Second),
		// A server refusal stays a server refusal whatever it is: PEER_FLOOD also
		// stops the account, and that does not prevent closing the journal row.
		"PEER_FLOOD": &telegram.Failure{
			Kind: telegram.KindPeerFlood, Type: "PEER_FLOOD", Permanent: true},
		// No session - the request was never sent: SIGTERM between the resolve and
		// the send.
		"no session": &telegram.Failure{Kind: telegram.KindNotReady},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			service := newService(t)
			ctx := context.Background()

			transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error { return failure }}}
			if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
				t.Fatal("refusal returned success")
			}
			if row := directRow(t, service, "ivan_petrov"); row.state != "failed" {
				t.Fatalf("journal row in state %q, expected failed", row.state)
			}

			draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "after refusal")
			if err != nil {
				t.Fatalf("create campaign: %v", err)
			}
			report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if report.Accepted != 1 {
				t.Fatalf("person stayed closed: %+v", report)
			}
		})
	}
}

// The recipient row was moved to skipped during a manual send that then failed:
// the row returns to the queue, otherwise the person gets nothing and the reason
// in the report would be false.
func TestPermanentFailureReturnsSkippedRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		// The queue sees the already recorded touch and removes the recipient row -
		// exactly the race the return exists for.
		if _, err := service.ClaimNext(ctx, "test"); err != nil {
			t.Fatalf("queue pass during sending: %v", err)
		}
		return &telegram.Failure{Kind: telegram.KindPeerForbidden, Permanent: true}
	}}}

	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
		t.Fatal("closed private chat returned success")
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "planned" || reason != "" {
		t.Fatalf("recipient row stayed %q with reason %q", state, reason)
	}
}

// A gate refuses the same way wherever the refusal comes from: nothing goes out
// and no journal row is added. One mechanism, many entry points - hence a table
// rather than twelve nearly identical tests.
func TestDirectSendRefusedByGuards(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, service *Service, transport *stubDialog)
		// dryRun: the allow-list lives in the dev transport, not in the database.
		dryRun   bool
		username string
		text     string
		want     error
		// advice: what the refusal must tell the agent when the way out is not obvious.
		advice string
	}{
		{name: "not a username", username: "Иван Петров", want: ErrNoUsername},
		{name: "empty text", username: "ivan_petrov", want: ErrEmptyText, text: ""},
		{name: "whitespace-only text", username: "ivan_petrov", want: ErrEmptyText, text: "   "},
		{name: "text longer than the Telegram limit", username: "ivan_petrov", want: ErrTextTooLong,
			text: strings.Repeat("а", maxTextLen+1)},
		// A group username resolves as successfully as a person's, and without a ref
		// check the message would go to the chat.
		{name: "group chat instead of a private chat", username: "club_chat", want: ErrNotPrivate,
			prepare: func(_ *testing.T, _ *Service, transport *stubDialog) {
				transport.ref = telegram.NewRef(telegram.KindChat, 11)
			}},
		// The invariant says "from this account as a whole", not "in the mailing".
		{name: "account stopped", username: "ivan_petrov", want: ErrAccountStopped,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				if err := service.StopAccount(context.Background(), "test", telegram.KindPeerFlood); err != nil {
					t.Fatalf("stop account: %v", err)
				}
			}},
		{name: "stop list by username", username: "ivan_petrov", want: ErrStopList,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				runSQL(t, service, `INSERT INTO stop_list (username) VALUES ('ivan_petrov')`)
			}},
		// A stop-list entry without a username: the username is linked to the amoCRM
		// contact through campaign rows, otherwise the gate is bypassed with a username.
		{name: "stop list by amoCRM contact", username: "ivan_petrov", want: ErrStopList,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				startWith(t, service, person(1, "ivan_petrov"))
				runSQL(t, service, `INSERT INTO stop_list (amo_contact_id) VALUES (1)`)
			}},
		// The dev allow-list is the last gate before anything goes out.
		{name: "username outside the dev allow-list", username: "stranger_one", want: ErrNotAllowed, dryRun: true,
			prepare: func(_ *testing.T, service *Service, _ *stubDialog) {
				service.cfg.DevAllowList = []string{"allowed_one"}
			}},
		// A night mailing over a list from tg_dialogs bypasses the whole campaign machinery.
		{name: "first touch of a stranger at night", username: "ivan_petrov", want: ErrColdWindow,
			prepare: func(_ *testing.T, service *Service, _ *stubDialog) {
				service.Now = func() time.Time { return time.Date(2026, 9, 2, 3, 0, 0, 0, moscowZone) }
			}},
		// The cap is shared with the mailing: a campaign send spends it the same way a
		// manual one does, otherwise the manual channel bypasses the account daily limit.
		{name: "daily cap spent by a campaign", username: "stranger_one", want: ErrColdLimit,
			prepare: func(t *testing.T, service *Service, transport *stubDialog) {
				runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
				startWith(t, service, person(1, "ivan_petrov"))
				if err := senderOn(service, transport).step(context.Background()); err != nil {
					t.Fatalf("mailing pass: %v", err)
				}
			}},
		// The sender is already carrying the row to Telegram: a manual touch would give a second one.
		{name: "campaign is writing to this person right now", username: "ivan_petrov", want: ErrCampaignSending,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) { claimOne(t, service) }},
		// The row returned to the queue after an ambiguous refusal: the claim is
		// released, but an attempt was made and its delivery is unknown - a manual
		// touch would give a second message.
		{name: "row returned to the queue after a refusal", username: "ivan_petrov", want: ErrCampaignSending,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				claimed := claimOne(t, service)
				if _, err := service.FinishClaim(context.Background(), claimed, ResultRetry, "unknown_error"); err != nil {
					t.Fatalf("return row to the queue: %v", err)
				}
			}},
		// A stale claim does not lift the gate: the delivery of such a row is unknown.
		// The row does not heal itself, so the refusal must name the way out.
		{name: "stale claim of a paused campaign", username: "ivan_petrov", want: ErrCampaignSending, advice: "cancel",
			prepare: func(t *testing.T, service *Service, _ *stubDialog) { stuckClaim(t, service) }},
		// The delivery of the first touch is unknown, and a second row would charge
		// another unit of the cap for it. An unrecognized failure is the only such
		// case: failures named by Telegram mean "request not executed". The gate is
		// held by the service, not the transport: by the time of the repeat Telegram
		// no longer refuses.
		{name: "repeat over an unclosed touch", username: "ivan_petrov", want: ErrDirectPending, advice: "5 minutes",
			prepare: func(t *testing.T, service *Service, transport *stubDialog) {
				transport.fail = func(string) error { return errors.New("network dropped") }
				if _, err := chatOn(service, transport).Send(context.Background(), "ivan_petrov", "привет"); err == nil {
					t.Fatal("unrecognized failure returned success")
				}
				transport.fail = nil
			}},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			service := newService(t)
			transport := &stubDialog{}
			if item.prepare != nil {
				item.prepare(t, service, transport)
			}
			chat := chatOn(service, transport)
			if item.dryRun {
				chat = chatOn(service, NewDryRunTransport(service.cfg))
			}
			text := item.text
			// "" by default means "the text does not matter for this gate"; for the
			// empty-text gate, "" is the very input under test.
			if text == "" && !errors.Is(item.want, ErrEmptyText) {
				text = "привет"
			}
			// The setup itself may have written both outward and to the journal: the
			// gate is checked by the increment, not the absolute value.
			sent, journaled := len(transport.Sent()), directCount(t, service)

			_, err := chat.Send(context.Background(), item.username, text)
			if !errors.Is(err, item.want) {
				t.Fatalf("gate refusal: err = %v, expected %v", err, item.want)
			}
			if len(transport.Sent()) != sent {
				t.Fatal("message went out despite the gate")
			}
			if count := directCount(t, service); count != journaled {
				t.Fatalf("gate refused, yet the journal has %d rows instead of %d", count, journaled)
			}
			if item.advice != "" && !strings.Contains(toolError(err).Error(), item.advice) {
				t.Fatalf("refusal does not name the way out %q: %s", item.advice, toolError(err))
			}
		})
	}
}

// The row returned to the queue after a temporary failure and the campaign was
// paused: claimed_at is empty, the stuck-claim counter does not see such a row,
// and it will never resolve itself. The refusal must name the campaign and an
// executable action, otherwise the person is closed indefinitely and the agent
// looks for a nonexistent sign.
func TestPausedCampaignHoldNamesCampaign(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	claimed := claimOne(t, service)
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "unknown_error"); err != nil {
		t.Fatalf("return row to the queue: %v", err)
	}
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("pause campaign: %v", err)
	}

	_, err := chatOn(service, &stubDialog{}).Send(ctx, "ivan_petrov", "привет")
	if !errors.Is(err, ErrCampaignSending) {
		t.Fatalf("guard refusal: err = %v, expected ErrCampaignSending", err)
	}
	answer := toolError(err).Error()
	for _, want := range []string{fmt.Sprintf("campaign %d", claimed.CampaignID), `"test"`, "start", "cancel"} {
		if !strings.Contains(answer, want) {
			t.Fatalf("refusal does not name %q: %s", want, answer)
		}
	}
}

// A row of a cancelled campaign does not hold the person: nobody is left to send
// it, a human has nothing to fix it with - stop with cancel on a cancelled
// campaign refuses - and a permanent block of manual sending would cost more than
// the risk.
func TestCancelledCampaignDoesNotBlockDirectSend(t *testing.T) {
	service := newService(t)

	claimOne(t, service)
	if _, err := service.StopCampaign(context.Background(), true); err != nil {
		t.Fatalf("cancel campaign: %v", err)
	}
	// The sender died, the claim went stale: the row stayed planned forever.
	service.Now = func() time.Time { return testNow.Add(time.Hour) }

	sendOK(t, chatOn(service, &stubDialog{}), "ivan_petrov", "привет")
}

// The journal closes the person to campaigns but not to a second reply:
// otherwise the dialog would break off after the first message. The second reply
// is already warm and goes out with its own random_id - Telegram would swallow a
// stable id as a duplicate.
func TestDirectSendTwiceToSamePersonAllowed(t *testing.T) {
	service := newService(t)

	transport := &stubDialog{stubTransport: stubTransport{messageID: 4242}}
	chat := chatOn(service, transport)
	sendOK(t, chat, "ivan_petrov", "ок")
	if send := sendOK(t, chat, "ivan_petrov", "ок"); send.Cold {
		t.Fatal("second reply was counted as a cold touch")
	}
	if sent := transport.Sent(); len(sent) != 2 || sent[0].RandomID == sent[1].RandomID {
		t.Fatalf("identical replies went out with one random_id: %+v", sent)
	}
	row := directRow(t, service, "ivan_petrov")
	if count := directCount(t, service); row.state != "sent" || row.cold || count != 2 {
		t.Fatalf("second journal row %q, cold %v, total rows %d", row.state, row.cold, count)
	}
	// Without message_id a journal row is not linked to the message in the dialog,
	// and analysis of a manual touch goes by text.
	if row.messageID == nil || *row.messageID != 4242 {
		t.Fatalf("journal has message_id %v, expected 4242", row.messageID)
	}
}

// A reply in an ongoing dialog is not subject to the window and the cap: a ban on
// answering at 21:30 makes the account look less like a live one, not more.
func TestWarmReplyIgnoresWindowAndCeiling(t *testing.T) {
	service := newService(t)

	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
	service.Now = func() time.Time { return time.Date(2026, 9, 2, 21, 30, 0, 0, moscowZone) }
	// An incoming message makes it warm: the correspondent wrote first.
	transport := &stubDialog{poll: func(int) []ChatMessage {
		return []ChatMessage{{ID: 1, Text: "здравствуйте"}}
	}}

	send := sendOK(t, chatOn(service, transport), "ivan_petrov", "добрый вечер")
	row := directRow(t, service, "ivan_petrov")
	if send.Cold || row.state != "sent" || row.cold {
		t.Fatalf("reply was counted as cold: answer %+v, row %q, cold %v", send, row.state, row.cold)
	}
}

// A service recipient of the owner can be written to outside the window and with
// the cap exhausted: this is a check of one's own account, not a touch of a
// stranger - and such a send does not increase the account usage. A stranger is
// still refused under the same conditions.
func TestOwnerDirectSendIgnoresWindowAndLimit(t *testing.T) {
	service := newService(t, "owner_one")
	ctx := context.Background()

	// A cap of one and one cold touch today: usage is exhausted.
	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
	runSQL(t, service, `INSERT INTO direct_messages
		 (username, account_id, text, random_id, cold, state, created_at)
		 VALUES ('stranger_zero', (SELECT id FROM accounts WHERE label = 'test'), 'привет', 777, true, 'sent', $1)`,
		testNow)
	service.Now = func() time.Time { return time.Date(2026, 9, 2, 21, 30, 0, 0, moscowZone) }
	chat := chatOn(service, &stubDialog{})

	if _, err := chat.Send(ctx, "stranger_one", "привет"); !errors.Is(err, ErrColdWindow) {
		t.Fatalf("stranger outside the window: err = %v, expected ErrColdWindow", err)
	}
	// A non-canonical username: the service recipient is recognized by the same
	// normalization as the other gates.
	sendOK(t, chat, "@Owner_One", "привет")
	if row := directRow(t, service, "owner_one"); row.state != "sent" || row.cold {
		t.Fatalf("service recipient journal row %q, cold %v", row.state, row.cold)
	}

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if used := status.Accounts[0].UsedToday; used != 1 {
		t.Fatalf("usage after a service send is %d, expected the previous 1", used)
	}
}

// The hold while a campaign is running is lifted for a service recipient: their
// campaign row is already claimed by the sender, and the manual touch goes out
// anyway. For a stranger the same case stays ErrCampaignSending (table above),
// and the cost of lifting it is named in the spec: the owner may get two messages
// in a row.
func TestOwnerDirectSendIgnoresCampaignHold(t *testing.T) {
	service := newService(t, "owner_one")
	ctx := context.Background()

	startWith(t, service, person(1, "owner_one"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim service recipient row: %v, %+v", err, claimed)
	}

	sendOK(t, chatOn(service, &stubDialog{}), "owner_one", "привет")
	if row := directRow(t, service, "owner_one"); row.state != "sent" {
		t.Fatalf("service recipient journal row %q, expected sent", row.state)
	}
}

// A Telegram restriction on a manual send stops the whole account, and the
// journal row goes to failed: the request was not executed, no message was created.
func TestFloodWaitOnDirectSendStopsAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		return floodWait(2 * time.Minute)
	}}}
	logged := captureLogs(t)
	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
		t.Fatal("long FLOOD_WAIT returned success")
	}
	stopped, reason, err := service.AccountStopped(ctx, "test")
	if err != nil || !stopped || reason != telegram.KindFloodWait {
		t.Fatalf("account not stopped: %v, %q, %v", stopped, reason, err)
	}
	if row := directRow(t, service, "ivan_petrov"); row.state != "failed" {
		t.Fatalf("journal row in state %q, expected failed", row.state)
	}
	if !strings.Contains(logged.String(), "account stopped by telegram limit") {
		t.Fatalf("account stop not visible in the log:\n%s", logged.String())
	}
}

// A 5xx failure with a Telegram code does not cancel delivery: the server may
// have created the message and crashed after that. The journal row stays pending
// and keeps the person closed - otherwise the next campaign would write to them a
// second time with its own random_id, and Telegram deduplication by random_id
// would not help.
func TestUnknownServerFailureKeepsTouchPending(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		return &telegram.Failure{Kind: telegram.KindTemporary, Type: "RPC_CALL_FAIL"}
	}}}
	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
		t.Fatal("500 failure returned success")
	}
	if row := directRow(t, service, "ivan_petrov"); row.state != "pending" {
		t.Fatalf("journal row in state %q, expected pending", row.state)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "after 500")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonDirectPending] != 1 {
		t.Fatalf("person is open to a campaign after 500: %+v", report)
	}
}

// The restriction belongs to the account, not to the call:
// contacts.resolveUsername answers FLOOD_WAIT before sending, and without a stop
// the agent would repeat tg_send, spending one more resolve on the restricted
// account per repeat.
func TestLimitFromResolveStopsAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubDialog{resolveErr: floodWait(2 * time.Minute)}
	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); !errors.Is(err, ErrAccountStopped) {
		t.Fatalf("resolve refusal: err = %v, expected an account stop", err)
	}
	stopped, reason, err := service.AccountStopped(ctx, "test")
	if err != nil || !stopped || reason != telegram.KindFloodWait {
		t.Fatalf("account not stopped by resolve: %v, %q, %v", stopped, reason, err)
	}
	if count := directCount(t, service); count != 0 {
		t.Fatalf("journal rows %d: a refusal before the journal write creates none", count)
	}

	// Reading tools use the same session and the same limits, so they stop the
	// account on a par with sending: a false stop costs one resume_account, a
	// missed one costs the account.
	if _, _, err := service.ResumeAccount(ctx, "test"); err != nil {
		t.Fatalf("lift stop: %v", err)
	}
	if _, err := chatOn(service, transport).Read(ctx, "ivan_petrov", 0, 0); !errors.Is(err, ErrAccountStopped) {
		t.Fatalf("read under restriction: err = %v, expected an account stop", err)
	}
	if stopped, _, _ := service.AccountStopped(ctx, "test"); !stopped {
		t.Fatal("a restriction that came from reading did not stop the account")
	}
}

// Manual touches are visible in the account line, and stuck rows are counted
// separately: both a pending journal row and a stale campaign claim silently
// close the person forever, are fixed by hand and therefore must be named from
// outside.
func TestStatusShowsManualTouchesAndStuckRows(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	stuckClaim(t, service)
	insertDirect(t, service, "ivan_petrov", "sent", testNow)
	insertDirect(t, service, "lost_one", "pending", testNow.Add(-time.Hour))

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].WarmToday != 2 || status.Accounts[0].ColdToday != 0 {
		t.Fatalf("manual touches in the report: %+v", status.Accounts)
	}
	if status.StuckDirect != 1 || status.StuckClaims != 1 {
		t.Fatalf("stuck touches %d and claims %d, expected one each", status.StuckDirect, status.StuckClaims)
	}
	var text strings.Builder
	writeAccounts(&text, status)
	if !strings.Contains(text.String(), "Rows with a stuck claim: 1") {
		t.Fatalf("stuck claim not named in the report:\n%s", text.String())
	}

	logged := captureLogs(t)
	service.LogStuckDirect(ctx)
	if !strings.Contains(logged.String(), "direct message stuck pending") {
		t.Fatalf("stuck touch not named at startup:\n%s", logged.String())
	}
}

// The account line names the cap the way the gate counts it: cold manual
// touches count toward the daily limit, warm ones do not. The text is checked,
// not the counters: a gate nobody learned about is equal to its absence.
func TestStatusTellsTruthAboutDailyLimit(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	runSQL(t, service, `UPDATE accounts SET daily_limit = 2 WHERE label = 'test'`)
	chat := chatOn(service, &stubDialog{})
	for _, name := range []string{"stranger_one", "stranger_one", "stranger_two"} {
		sendOK(t, chat, name, "привет")
	}
	// Two cold touches spent the whole cap.
	if _, err := chat.Send(ctx, "stranger_three", "привет"); !errors.Is(err, ErrColdLimit) {
		t.Fatalf("third cold touch: err = %v, expected ErrColdLimit", err)
	}

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var text strings.Builder
	writeAccounts(&text, status)
	report := text.String()
	for _, want := range []string{"today 2 of 2", "of them first touches by hand: 2", "warm replies today: 1"} {
		if !strings.Contains(report, want) {
			t.Fatalf("account line lacks %q:\n%s", want, report)
		}
	}
}

// The username is normalized on input: without it @Ivan_Petrov misses the stop
// list, the race guard and its own journal.
func TestUsernameNormalizedOnDirectSend(t *testing.T) {
	service := newService(t)

	chat := chatOn(service, &stubDialog{})
	sendOK(t, chat, "@Ivan_Petrov", "привет")
	if row := directRow(t, service, "ivan_petrov"); row.state != "sent" {
		t.Fatalf("journal row for the normalized username: %q", row.state)
	}

	runSQL(t, service, `INSERT INTO stop_list (username) VALUES ('petr_sidorov')`)
	if _, err := chat.Send(context.Background(), "https://t.me/Petr_Sidorov", "привет"); !errors.Is(err, ErrStopList) {
		t.Fatalf("username in a t.me wrapper missed the stop list: err = %v", err)
	}
}
