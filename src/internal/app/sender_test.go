package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// senderOn builds a sender on the test service: pauses are almost zero so the
// loop runs fast, the FLOOD_WAIT threshold is one minute.
func senderOn(service *Service, transport Transport) *Sender {
	service.cfg.SendPauseMin = time.Millisecond
	service.cfg.SendPauseMax = 2 * time.Millisecond
	service.cfg.FloodWaitThreshold = time.Minute
	return NewSender(service, transport)
}

// stubTransport is a substitute transport with a set refusal: there is no live
// Telegram in the slice, refusals come from the template's error parsing.
type stubTransport struct {
	fail func(username string) error
	// peerID is the id that resolve returns as a person reference; 0 means no
	// reference, as in dry-run.
	peerID int64
	// messageID is the id that a send reports as success; 0 repeats the duplicate
	// by random_id, where Telegram gives no number.
	messageID int

	mu   sync.Mutex
	sent []Delivery
}

func (t *stubTransport) Resolve(_ context.Context, username string) (Peer, error) {
	peer := Peer{Username: username}
	if t.peerID != 0 {
		peer.Ref = telegram.NewRef(telegram.KindUser, t.peerID)
	}
	return peer, nil
}

func (t *stubTransport) Send(_ context.Context, peer Peer, text string, randomID int64) (int, error) {
	if t.fail != nil {
		if err := t.fail(peer.Username); err != nil {
			return 0, err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, Delivery{Username: peer.Username, Text: text, RandomID: randomID})
	return t.messageID, nil
}

func (t *stubTransport) Sent() []Delivery {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Delivery(nil), t.sent...)
}

// floodWait is a Telegram refusal of the "wait so many seconds" kind.
func floodWait(pause time.Duration) error {
	return &telegram.Failure{Kind: telegram.KindFloodWait, RetryAt: time.Now().Add(pause)}
}

// Death between delivery and the write: the row is sent but not finalized.
// The stale claim returns it to the queue, the retry goes with the same random_id -
// Telegram deduplicates, and the person does not get a second message.
func TestResendAfterCrashKeepsRandomID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "ivan_petrov"))

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim a row: %v, %+v", err, claimed)
	}
	peer, err := transport.Resolve(ctx, claimed.Username)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := transport.Send(ctx, peer, claimed.Text, claimed.RandomID); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The process was killed before finalization: the claim goes stale, the next
	// iteration takes the same row.
	service.Now = func() time.Time { return testNow.Add(10 * time.Minute) }
	if err := sender.step(ctx); err != nil {
		t.Fatalf("iteration after restart: %v", err)
	}

	sent := transport.Sent()
	if len(sent) != 2 || sent[0].RandomID != sent[1].RandomID {
		t.Fatalf("retry went out with a different random_id: %+v", sent)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("row in state %q, expected sent", state)
	}
}

// Transient error: FLOOD_WAIT shorter than the threshold - wait, the row stays
// planned, the attempt is not spent, the account is not stopped.
func TestFloodWaitKeepsRowPlanned(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{fail: func(string) error { return floodWait(20 * time.Millisecond) }}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "ivan_petrov"))

	if err := sender.step(ctx); err != nil {
		t.Fatalf("sender iteration: %v", err)
	}

	state, reason := recipientState(t, service.pool, "ivan_petrov")
	if state != "planned" || reason != telegram.KindFloodWait {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}
	if stopped, _, err := service.AccountStopped(ctx, "test"); err != nil || stopped {
		t.Fatalf("short FLOOD_WAIT stopped the account: %v, %v", stopped, err)
	}
	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil {
		t.Fatalf("row did not return to the queue: %v, %+v", err, again)
	}
}

// PEER_FLOOD is an account-level restriction, not a refusal about a person:
// sending from it stops entirely, and the row returns to the queue unspent.
func TestPeerFloodStopsAccount(t *testing.T) {
	outcome := classifyDelivery(&telegram.Failure{Kind: telegram.KindPeerFlood, Permanent: true}, time.Minute)
	if !outcome.stop || outcome.result != ResultRetry {
		t.Fatalf("PEER_FLOOD parsed as %+v", outcome)
	}
}

// Permanent error about a person: the row is undelivered with a reason, the
// campaign goes on and closes on an empty queue.
func TestPeerForbiddenMarksUndelivered(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{fail: func(username string) error {
		if username == "closed_one" {
			return &telegram.Failure{Kind: telegram.KindPeerForbidden, Permanent: true}
		}
		return nil
	}}
	sender := senderOn(service, transport)
	id := startWith(t, service, person(1, "closed_one"), person(2, "open_one"))

	for range 3 {
		if err := sender.step(ctx); err != nil {
			t.Fatalf("sender iteration: %v", err)
		}
	}

	state, reason := recipientState(t, service.pool, "closed_one")
	if state != "undelivered" || reason != telegram.KindPeerForbidden {
		t.Fatalf("closed private chat gave state %q with reason %q", state, reason)
	}
	if state, _ := recipientState(t, service.pool, "open_one"); state != "sent" {
		t.Fatalf("campaign did not continue: second recipient in state %q", state)
	}
	if status := campaignStatus(t, service.pool, id); status != "done" {
		t.Fatalf("campaign in state %q", status)
	}
}

// Permanent account error: FLOOD_WAIT longer than the threshold stops the
// account in the DB, the row returns to planned, no more sends from it, and the
// stop is visible from outside - the healthcheck relies on it.
func TestAccountStopOnLongFloodWait(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{fail: func(string) error { return floodWait(2 * time.Minute) }}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "ivan_petrov"))

	if err := sender.step(ctx); err != nil {
		t.Fatalf("sender iteration: %v", err)
	}

	stopped, reason, err := service.AccountStopped(ctx, "test")
	if err != nil || !stopped || reason != telegram.KindFloodWait {
		t.Fatalf("account not stopped: %v, %q, %v", stopped, reason, err)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "planned" {
		t.Fatalf("row in state %q, expected planned", state)
	}
	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed != nil {
		t.Fatalf("stopped account took a row: %+v, %v", claimed, err)
	}

	// Lifting is only by a human command.
	if _, _, err := service.ResumeAccount(ctx, "test"); err != nil {
		t.Fatalf("lift the stop: %v", err)
	}
	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed == nil {
		t.Fatalf("row was not taken after the stop was lifted: %+v, %v", claimed, err)
	}
}

// Restart in the middle of a campaign: the new process continues from planned,
// sends to nobody a second time and brings the campaign to done; the loop exits on context cancel.
func TestRestartContinuesCampaign(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	first := &stubTransport{}
	id := startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	if err := senderOn(service, first).step(ctx); err != nil {
		t.Fatalf("iteration before restart: %v", err)
	}

	second := &stubTransport{}
	sender := senderOn(service, second)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sender.Run(runCtx) }()

	deadline := time.Now().Add(10 * time.Second)
	for campaignStatus(t, service.pool, id) != "done" {
		if time.Now().After(deadline) {
			t.Fatal("campaign did not close after restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("loop returned an error: %v", err)
	}

	delivered := map[string]int{}
	for _, item := range append(first.Sent(), second.Sent()...) {
		delivered[item.Username]++
	}
	if len(delivered) != 2 || delivered["first_one"] != 1 || delivered["second_one"] != 1 {
		t.Fatalf("sends after restart: %+v", delivered)
	}
}

// An incoming private message closes the person's row, but differently: a sent
// row moves to replied, a not yet sent one to skipped with the reason
// "wrote first". The invitation goes out in neither case, and only the person
// the message actually reached counts in the reply metric.
func TestInboundEventMarksReplied(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "answered_one"), person(2, "wrote_first"))
	if err := sender.step(ctx); err != nil {
		t.Fatalf("sender iteration: %v", err)
	}

	want := map[string]struct{ state, reason string }{
		"answered_one": {"replied", ""},
		"wrote_first":  {"skipped", ReasonWroteFirst},
	}
	for _, name := range []string{"answered_one", "wrote_first"} {
		event := telegram.Message{Peer: telegram.NewRef(telegram.KindUser, 100), Username: name, Text: "hello"}
		if err := sender.OnMessage(ctx, event); err != nil {
			t.Fatalf("incoming from %s: %v", name, err)
		}
		state, reason := recipientState(t, service.pool, name)
		if state != want[name].state || reason != want[name].reason {
			t.Fatalf("row %s in state %q (%s), expected %q (%s)",
				name, state, reason, want[name].state, want[name].reason)
		}
	}

	if err := sender.step(ctx); err != nil {
		t.Fatalf("iteration after replies: %v", err)
	}
	if sent := transport.Sent(); len(sent) != 1 || sent[0].Username != "answered_one" {
		t.Fatalf("invitation went out to the one who wrote first: %+v", sent)
	}
}

// A short incoming update carries no username: the person is recognized by the
// Telegram id saved at send time.
func TestReplyMatchedByUserID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	sender := senderOn(service, &stubTransport{peerID: 777})
	startWith(t, service, person(1, "ivan_petrov"))
	if err := sender.step(ctx); err != nil {
		t.Fatalf("sender iteration: %v", err)
	}

	event := telegram.Message{Peer: telegram.NewRef(telegram.KindUser, 777), Text: "hello"}
	if err := sender.OnMessage(ctx, event); err != nil {
		t.Fatalf("incoming without username: %v", err)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "replied" {
		t.Fatalf("row in state %q, expected replied", state)
	}
}

// The row was taken out of the queue while the send was in flight: the delivery
// is still recorded and not lost in the ERROR log - without sent_at the person
// would get a second message in the next campaign.
func TestDeliveredRowRecordedAfterSkip(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	sender := senderOn(service, &stubTransport{})
	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim a row: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`UPDATE recipients SET state = 'skipped' WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("take the row out of the queue: %v", err)
	}

	logged := captureLogs(t)
	if err := sender.SendClaimed(ctx, claimed); err != nil {
		t.Fatalf("send: %v", err)
	}
	if text := logged.String(); strings.Contains(text, "delivered message was not recorded") {
		t.Fatalf("delivery was not recorded:\n%s", text)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("row in state %q, expected sent", state)
	}
}

// The row was closed by a second process with the same label that took over a
// stale claim: the delivery is already recorded in the DB, and ERROR here would
// be a false alarm at the observability guard - a real stop would get lost among such records.
func TestDeliveryRecordedByOtherProcessIsNotError(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	sender := senderOn(service, &stubTransport{})
	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim a row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("recording the delivery by the second process: %v", err)
	}

	logged := captureLogs(t)
	if err := sender.SendClaimed(ctx, claimed); err != nil {
		t.Fatalf("send: %v", err)
	}
	if text := logged.String(); strings.Contains(text, "delivered message was not recorded") {
		t.Fatalf("false alarm on a recorded delivery:\n%s", text)
	}
}

// captureLogs captures WARN and higher records for the duration of a test: a
// guard whose only trace is the log cannot be checked otherwise.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}
