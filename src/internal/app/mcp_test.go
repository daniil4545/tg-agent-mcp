package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// toolText runs a tool handler and returns the answer text. The full HTTP
// transport is not needed here: the tool's decision is under test, not the
// streamable plumbing of the server.
func toolText(t *testing.T, result *mcp.CallToolResult, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("tool refused: %v", err)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if item, ok := content.(*mcp.TextContent); ok {
			text.WriteString(item.Text)
		}
	}
	return text.String()
}

// The campaign tools take no id: add_recipients works with the single draft,
// and without a draft it must say plainly what is missing.
func TestAddRecipientsUsesSingleDraft(t *testing.T) {
	handlers := &tools{service: newService(t)}
	ctx := context.Background()

	if _, _, err := handlers.addRecipients(ctx, nil, addArgs{People: []personArgs{{ContactID: 1, Username: "ivan_petrov"}}}); err == nil {
		t.Fatal("batch accepted without a draft")
	} else if !strings.Contains(err.Error(), "load_campaign") {
		t.Fatalf("refusal without a draft does not name the next step: %v", err)
	}

	if _, _, err := handlers.loadCampaign(ctx, nil, loadArgs{Template: "Привет, {имя}!", Title: "тест"}); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	result, _, err := handlers.addRecipients(ctx, nil, addArgs{People: []personArgs{
		{ContactID: 1, Name: "Иван", Username: "ivan_petrov"},
		{ContactID: 2, Name: "Пётр", Username: "пётр"},
		{ContactID: 1, Name: "Иван", Username: "ivan_petrov"},
	}})
	text := toolText(t, result, err)

	if !strings.Contains(text, "Accepted: 1 of 3") {
		t.Fatalf("report does not name the accepted count:\n%s", text)
	}
	if !strings.Contains(text, reasonLabels[ReasonNoUsername]) || !strings.Contains(text, reasonLabels[ReasonDuplicate]) {
		t.Fatalf("report does not name the screening reasons:\n%s", text)
	}
	if !strings.Contains(text, "Привет, Иван!") {
		t.Fatalf("report has no message preview:\n%s", text)
	}
}

// A draft next to a running campaign: add_recipients reports on the draft it
// inserted into, while status reports on the active campaign and names the
// draft on a separate line. Otherwise checking the list before start would go
// by another campaign.
func TestReportsFollowTheEditedCampaign(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	startWith(t, service, person(1, "active_one"))
	if _, _, err := handlers.loadCampaign(ctx, nil, loadArgs{Template: "Черновик, {имя}!", Title: "второй"}); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	result, _, err := handlers.addRecipients(ctx, nil, addArgs{
		People: []personArgs{{ContactID: 2, Name: "Пётр", Username: "petr_ivanov"}},
	})
	added := toolText(t, result, err)
	if !strings.Contains(added, "второй") || !strings.Contains(added, "Черновик, Пётр!") {
		t.Fatalf("insert report is not about the draft:\n%s", added)
	}
	if strings.Contains(added, "active_one") {
		t.Fatalf("insert report has another campaign's preview:\n%s", added)
	}

	statusResult, _, err := handlers.status(ctx, nil, statusArgs{})
	shown := toolText(t, statusResult, err)
	if !strings.Contains(shown, "Привет, Иван!") {
		t.Fatalf("status does not show the active campaign:\n%s", shown)
	}
	if !strings.Contains(shown, "There is a draft") {
		t.Fatalf("status did not name the draft:\n%s", shown)
	}
}

// A repeated stop on an already paused campaign: the campaign is alive, and the
// refusal must name the pause and the resume through start, not advise creating
// a draft.
func TestStopAgainNamesThePause(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"))
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("pause: %v", err)
	}

	handlers := &tools{service: service}
	_, _, err := handlers.stop(ctx, nil, stopArgs{})
	if err == nil {
		t.Fatal("repeated pause accepted as a stop")
	}
	if !strings.Contains(err.Error(), "already paused") || !strings.Contains(err.Error(), "start") {
		t.Fatalf("refusal did not name the live pause: %v", err)
	}
}

// The same refusal for a "pause plus draft" pair: start will take the draft and
// cancel will remove the draft, so the advice "resume the pause with start"
// would send the agent to start another campaign, and "remove with cancel" to
// wipe the collected list.
func TestStopRefusalCountsTheDraft(t *testing.T) {
	service := newService(t)
	paused, draft := pausedAndDraft(t, service)

	handlers := &tools{service: service}
	_, _, err := handlers.stop(context.Background(), nil, stopArgs{})
	if err == nil {
		t.Fatal("repeated pause accepted as a stop")
	}
	text := err.Error()
	if !strings.Contains(text, fmt.Sprintf("draft %d", draft)) {
		t.Fatalf("refusal did not name the draft next to pause %d: %v", paused, err)
	}
	if !strings.Contains(text, "start will launch the draft") || !strings.Contains(text, "stop with cancel will cancel the draft") {
		t.Fatalf("refusal promises actions on the pause, but they will act on the draft: %v", err)
	}
}

// A queue idle is named in status: the campaign stays active, the rows stand in
// the queue, and without this line the reason is visible only indirectly. The
// two reasons that a live test did not cover are checked - the cap and the
// closed window.
func TestStatusNamesIdleReason(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim a row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)

	result, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "the account's daily cap is used up (1 of 1)") {
		t.Fatalf("status did not name the idle by the cap:\n%s", text)
	}

	// Saturday of the same week: the window is closed and overrides the cap, and
	// the opening is named by the hour, otherwise "wait for the window" does not
	// say until when.
	service.Now = func() time.Time { return testNow.AddDate(0, 0, 3) }
	result, _, err = handlers.status(ctx, nil, statusArgs{})
	text = toolText(t, result, err)
	if !strings.Contains(text, "the send window is closed, next opening 07.09 10:00 MSK") {
		t.Fatalf("status did not name the idle by the window:\n%s", text)
	}
}

// Spending on the owner's service recipients is named in status on a separate
// line, the same way as warm replies: it does not count toward the cap, and
// without this line it is not visible at all.
func TestStatusNamesOwnerSpending(t *testing.T) {
	service := newService(t, "smirnov")
	handlers := &tools{service: service}
	ctx := context.Background()

	startWith(t, service, person(1, "smirnov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim the service recipient's row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	result, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "service recipients today: 1 (not counted toward the cap)") {
		t.Fatalf("status did not name the spending on service recipients:\n%s", text)
	}
	// The membership of the list is named by username: a lifted guard that nobody
	// heard about equals its absence, and an extra username in DEV_ALLOW_LIST is
	// visible only here.
	if !strings.Contains(text, "Service recipients (the window, the cap and the one-message rule do not apply to them): @smirnov") {
		t.Fatalf("status did not name the membership of the service recipients:\n%s", text)
	}
}

// resume_account answers by the facts: a cleared stop is named with its reason,
// a live account plainly, an unknown label with a refusal. The answer "stop
// cleared" where there was no stop sends a person to fix a breakage that does
// not exist.
func TestResumeAccountAnswersByState(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	quiet, _, err := handlers.resumeAccount(ctx, nil, resumeArgs{Label: "test"})
	if text := toolText(t, quiet, err); !strings.Contains(text, `Account "test" has no stop: nothing to clear`) {
		t.Fatalf("a live account answers as if a stop was cleared:\n%s", text)
	}

	if err := service.StopAccount(ctx, "test", telegram.KindPeerFlood); err != nil {
		t.Fatalf("stop the account: %v", err)
	}
	result, _, err := handlers.resumeAccount(ctx, nil, resumeArgs{Label: "test"})
	text := toolText(t, result, err)
	if !strings.Contains(text, `Stop cleared on account "test"`) || !strings.Contains(text, telegram.KindPeerFlood) {
		t.Fatalf("clearing the stop is not named with its reason:\n%s", text)
	}
	if stopped, _, err := service.AccountStopped(ctx, "test"); err != nil || stopped {
		t.Fatalf("stop not cleared: %v, %v", stopped, err)
	}

	if _, _, err := handlers.resumeAccount(ctx, nil, resumeArgs{Label: "unknown"}); err == nil {
		t.Fatal("a label with no account accepted as a cleared stop")
	}
}

// The preview shows no more than three rows: the header must name how many the
// campaign has, otherwise three rows read as the whole list.
func TestPreviewNamesItsLimit(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}

	startWith(t, service,
		person(1, "ivan_petrov"), person(2, "petr_ivanov"),
		person(3, "anna_smirnova"), person(4, "olga_orlova"))

	result, _, err := handlers.status(context.Background(), nil, statusArgs{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "Message preview (first 3 of 4):") {
		t.Fatalf("preview does not name its display cap:\n%s", text)
	}
}

// failDialog is a transport whose read fails: the live Telegram refusal comes
// at resolve, while stubDialog can only fail a send.
type failDialog struct {
	stubDialog
	err error
}

func (d *failDialog) Lookup(context.Context, string) (Peer, error) { return Peer{}, d.err }

// Registration is the only thing that separates a written handler from one
// available to the agent: a tool outside the server's list does not exist.
func TestDialogToolsRegistered(t *testing.T) {
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	server := NewMCPServer(&Service{cfg: Config{AccountLabel: "test"}, Now: time.Now}, nil)
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("start the server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range list.Tools {
		registered[tool.Name] = true
	}
	for _, name := range []string{"tg_find", "tg_send", "tg_read", "tg_dialogs"} {
		if !registered[name] {
			t.Fatalf("tool %s is not registered: %v", name, registered)
		}
	}
}

// The arguments of status and stop are optional, and the schema must say so: a
// field without omitempty is declared required, and a call with no arguments is
// rejected by schema validation before it reaches the handler. The live status
// answered that way to an empty object. The check covers campaign_id too: an
// int64 has the same problem as a flag.
func TestOptionalFlagsAreNotRequired(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	serverSession, err := NewMCPServer(service, nil).Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("start the server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if err != nil {
		t.Fatalf("status without arguments failed: %v", err)
	}
	if text := toolText(t, result, nil); result.IsError {
		t.Fatalf("status without arguments answers with a refusal:\n%s", text)
	}

	// stop without a campaign refuses on the merits; the check is that the
	// refusal is a domain one, not a schema one: the cancel flag has the same
	// problem and the same cure.
	stopped, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "stop"})
	if err != nil {
		t.Fatalf("stop without arguments failed: %v", err)
	}
	if text := toolText(t, stopped, nil); strings.Contains(text, "validating") {
		t.Fatalf("stop without arguments rejected by the schema:\n%s", text)
	}
}

// The default cut is the most common call, and its silence is more dangerous
// than an explicit one: the agent reads the first twenty rows as the whole list
// and concludes "the person is not there".
func TestToolsNameTheirCut(t *testing.T) {
	ctx := context.Background()

	// Exactly defaultHistory items: the cut to limit is now held by the transport
	// (checked in internal/telegram), and Chat.Dialogs returns its selection as
	// is, without a second trim. The stand-in transport does not use its limit,
	// so the list is built already of the right size - the way a real one would
	// return it.
	crowd := make([]DialogInfo, 0, defaultHistory)
	for i := range defaultHistory {
		crowd = append(crowd, DialogInfo{
			Username: fmt.Sprintf("user_%02d", i),
			Ref:      telegram.NewRef(telegram.KindUser, int64(i+10)),
			Last:     ChatMessage{Text: "привет"},
		})
	}
	full := make([]ChatMessage, 0, defaultHistory)
	for i := range defaultHistory {
		full = append(full, ChatMessage{ID: i + 1, Text: "привет"})
	}
	handlers := liveTools(&stubDialog{dialogs: crowd, poll: func(int) []ChatMessage { return full }})

	result, _, err := handlers.dialogs(ctx, nil, dialogsArgs{})
	listed := toolText(t, result, err)
	if !strings.Contains(listed, "Showing the 20 most recent dialogs") || !strings.Contains(listed, "limit up to 100") {
		t.Fatalf("the dialog list did not name the default cut:\n%s", listed)
	}
	if !strings.Contains(listed, "Dialogs (20)") {
		t.Fatalf("default cut not applied:\n%s", listed)
	}

	deep, _, err := handlers.read(ctx, nil, readArgs{Username: "ivan_petrov"})
	if shown := toolText(t, deep, err); !strings.Contains(shown, "Not everything is shown") {
		t.Fatalf("the dialog read did not name the exhausted depth:\n%s", shown)
	}

	// A list shorter than the limit is shown in full: both the caveat about the
	// cut and the advice to call tg_dialogs with a larger limit would send the
	// agent after a remainder that does not exist.
	few := liveTools(&stubDialog{dialogs: crowd[:3]})
	shortList, _, err := few.dialogs(ctx, nil, dialogsArgs{})
	if shown := toolText(t, shortList, err); strings.Contains(shown, "most recent dialogs") || strings.Contains(shown, "limit up to 100") {
		t.Fatalf("a full dialog list is named a cut:\n%s", shown)
	}

	// A dialog shorter than the depth is shown in full: the caveat here would
	// send the agent after a remainder that does not exist.
	brief := liveTools(&stubDialog{poll: func(int) []ChatMessage { return full[:3] }})
	short, _, err := brief.read(ctx, nil, readArgs{Username: "ivan_petrov"})
	if shown := toolText(t, short, err); strings.Contains(shown, "Not everything is shown") {
		t.Fatalf("a full dialog is named a cut:\n%s", shown)
	}
}

// A guard refusal reaches the agent as human text with the next step named: a
// domain error in English says nothing to the caller.
func TestBoundaryRefusalReadsAsText(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('ivan_petrov')`); err != nil {
		t.Fatalf("stop list: %v", err)
	}
	handlers := &tools{service: service, chat: chatOn(service, &stubDialog{})}

	_, _, err := handlers.send(ctx, nil, sendArgs{Username: "ivan_petrov", Text: "привет"})
	if err == nil {
		t.Fatal("message to a person on the stop list accepted")
	}
	if !strings.Contains(err.Error(), "stop list") {
		t.Fatalf("guard refusal is not translated into human language: %v", err)
	}
}

// A session that is not up is the most common refusal of the live environment:
// the agent must read what to do, not a "temporary Telegram error".
func TestNotReadySessionExplainsItself(t *testing.T) {
	handlers := &tools{
		service: &Service{cfg: Config{AccountLabel: "test"}, Now: time.Now},
		chat: offlineChat(&failDialog{err: &telegram.Failure{
			Kind: telegram.KindNotReady, Message: "client not started",
		}}),
	}

	_, _, err := handlers.read(context.Background(), nil, readArgs{Username: "ivan_petrov"})
	if err == nil {
		t.Fatal("read without a session went through")
	}
	if !strings.Contains(err.Error(), "session") || !strings.Contains(err.Error(), "live-logs") {
		t.Fatalf("refusal does not name the session that is not up: %v", err)
	}
}

// tg_send reports the price, not the fact of sending: after a manual touch the
// person is closed to all campaigns, and the caller must read that.
func TestDirectSendAnswerNamesThePrice(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service, chat: chatOn(service, &stubDialog{})}

	result, _, err := handlers.send(context.Background(), nil, sendArgs{Username: "@Ivan_Petrov", Text: "привет"})
	text := toolText(t, result, err)

	if !strings.Contains(text, "Touch recorded: this person will not be included in any campaign from now on.") {
		t.Fatalf("tg_send answer does not name the price of the touch:\n%s", text)
	}
	if !strings.Contains(text, "@ivan_petrov") {
		t.Fatalf("tg_send answer does not name the normalized username:\n%s", text)
	}
}

// liveTools are tools on the live environment without a database: lists and
// reads do not go to it, and the dry-run mark would break the check of the
// answer text.
func liveTools(transport Dialog) *tools {
	return &tools{
		service: &Service{cfg: Config{AccountLabel: "test", Transport: "live"}, Now: time.Now},
		chat:    offlineChat(transport),
	}
}

// A person without a public username: the agent takes an empty @ for a username
// and substitutes it into tg_send. The answer must name the person and say that
// writing is not possible.
func TestPeopleWithoutUsernameAreNamed(t *testing.T) {
	ctx := context.Background()
	handlers := liveTools(&stubDialog{
		found: []Contact{{Name: "Дмитрий стоматолог", IsContact: true}},
		dialogs: []DialogInfo{{
			Name: "Дмитрий стоматолог",
			Ref:  telegram.NewRef(telegram.KindUser, 10),
			Last: ChatMessage{Text: "привет"},
		}},
	})

	for _, run := range []struct {
		name string
		call func() (*mcp.CallToolResult, any, error)
	}{
		{"tg_find", func() (*mcp.CallToolResult, any, error) {
			return handlers.find(ctx, nil, findArgs{Query: "Дмитрий"})
		}},
		{"tg_dialogs", func() (*mcp.CallToolResult, any, error) {
			return handlers.dialogs(ctx, nil, dialogsArgs{})
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			result, _, err := run.call()
			text := toolText(t, result, err)
			if strings.Contains(text, "@") {
				t.Fatalf("a person without a username printed as an empty @:\n%s", text)
			}
			if !strings.Contains(text, "Дмитрий стоматолог") || !strings.Contains(text, "no public username") {
				t.Fatalf("answer does not name the person without a username:\n%s", text)
			}
		})
	}
}

// The dialog list is an overview: a long reply is collapsed into one line and
// cut by runes, while a dialog read returns the same text in full.
func TestDialogListShortensLastMessage(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("я", 150) + "\nвторая строка"
	handlers := liveTools(&stubDialog{
		dialogs: []DialogInfo{{
			Username: "ivan_petrov",
			Ref:      telegram.NewRef(telegram.KindUser, 10),
			Last:     ChatMessage{Text: long},
		}},
		poll: func(int) []ChatMessage { return []ChatMessage{{ID: 1, Text: long}} },
	})

	result, _, err := handlers.dialogs(ctx, nil, dialogsArgs{})
	listed := toolText(t, result, err)
	if !strings.Contains(listed, strings.Repeat("я", snippetLen)+"...") {
		t.Fatalf("reply in the list is not cut by runes:\n%s", listed)
	}
	if strings.Contains(listed, "вторая строка") {
		t.Fatalf("reply in the list took more than one line:\n%s", listed)
	}

	readResult, _, err := handlers.read(ctx, nil, readArgs{Username: "ivan_petrov"})
	shown := toolText(t, readResult, err)
	if !strings.Contains(shown, long) {
		t.Fatalf("dialog read cut the text:\n%s", shown)
	}
}

// The list is mixed by kind: the agent decides by the word in the line where it
// can write and where it can only read. A nameless entity is a group or channel
// the account was removed from; a service top message arrives empty and prints
// no line.
func TestDialogListNamesEveryKind(t *testing.T) {
	handlers := liveTools(&stubDialog{dialogs: []DialogInfo{
		{Username: "ivan_petrov", Name: "Иван", Ref: telegram.NewRef(telegram.KindUser, 10), Unread: 2,
			Last: ChatMessage{Text: "привет"}},
		{Name: "Дмитрий стоматолог", Ref: telegram.NewRef(telegram.KindUser, 11)},
		{Name: "Чат клуба", Ref: telegram.NewRef(telegram.KindChat, 12), Unread: 7,
			Last: ChatMessage{Text: "обсуждаем"}},
		{Username: "club_core", Name: "Ядро", Ref: telegram.NewRef(telegram.KindChannel, 13)},
		{Username: "club_news", Name: "Анонсы", Ref: telegram.NewRef(telegram.KindChannel, 14), Broadcast: true, Unread: 3},
		{Ref: telegram.NewRef(telegram.KindChannel, 15)},
	}})

	result, _, err := handlers.dialogs(context.Background(), nil, dialogsArgs{})
	listed := toolText(t, result, err)
	for _, line := range []string{
		"person: @ivan_petrov Иван, 2 unread",
		"person: Дмитрий стоматолог; no public username",
		"group: Чат клуба, 7 unread",
		"group: @club_core Ядро",
		"channel: @club_news Анонсы, 3 unread",
		"group: title unknown",
	} {
		if !strings.Contains(listed, line) {
			t.Fatalf("the list has no line %q:\n%s", line, listed)
		}
	}
	if strings.Count(listed, noUsernameNote) != 1 {
		t.Fatalf("the advice about tg_send stands not only at the person:\n%s", listed)
	}
	if strings.Contains(listed, "[no text]") {
		t.Fatalf("a dialog with no last message printed as an empty reply:\n%s", listed)
	}
}

// An exact username match is the one being looked for: in a prefix result it
// stands among other people's similar usernames and gets lost, so it goes first
// and with a mark.
func TestExactUsernameGoesFirst(t *testing.T) {
	handlers := liveTools(&stubDialog{found: []Contact{
		{Username: "smirnovenko", Name: "Пётр"},
		{Username: "smirnov", Name: "Алексей", IsContact: true, Exact: true},
	}})

	result, _, err := handlers.find(context.Background(), nil, findArgs{Query: "smirnov"})
	text := toolText(t, result, err)
	if !strings.Contains(text, "@smirnov Алексей - exact username - in contacts") {
		t.Fatalf("exact username printed without a mark:\n%s", text)
	}
	if strings.Index(text, "@smirnov ") > strings.Index(text, "@smirnovenko") {
		t.Fatalf("exact username is not first in the list:\n%s", text)
	}
}

// Similar usernames are other people: without a warning the agent writes to the
// first one in the list. The query is normalized as in the search, otherwise
// "@SmirnovE" is not recognized as a username and prints with a double at-sign.
// There is no warning for a name: an exact match is not expected there.
func TestSimilarUsernamesAreWarnedAbout(t *testing.T) {
	ctx := context.Background()
	handlers := liveTools(&stubDialog{found: []Contact{{Username: "smirnovenko", Name: "Пётр"}}})

	// "@ SmirnovE" is the same username: a space after the at-sign does not make
	// the query a name.
	for _, query := range []string{"@SmirnovE", "@ SmirnovE"} {
		result, _, err := handlers.find(ctx, nil, findArgs{Query: query})
		text := toolText(t, result, err)
		if !strings.Contains(text, "None of the results is the exact username @smirnove") {
			t.Fatalf("similar usernames for the query %q returned without a warning:\n%s", query, text)
		}
	}

	byName, _, err := handlers.find(ctx, nil, findArgs{Query: "Пётр"})
	if shown := toolText(t, byName, err); strings.Contains(shown, "None of the results") {
		t.Fatalf("a search by name warns about an exact username:\n%s", shown)
	}
}

// status by id answers about the named campaign: otherwise the result of a run
// is unavailable - the next campaign covers it. A foreign id is a refusal, not
// a report without a campaign: accounts in place of the totals read as "the
// campaign is empty".
func TestStatusAnswersByCampaignID(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	closed := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("cancel the campaign: %v", err)
	}
	live, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("create the second campaign: %v", err)
	}
	if _, err := service.AddRecipients(ctx, live, []Person{person(2, "petr_ivanov")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("start the second campaign: %v", err)
	}

	current, _, err := handlers.status(ctx, nil, statusArgs{})
	if text := toolText(t, current, err); !strings.Contains(text, fmt.Sprintf("id %d", live)) {
		t.Fatalf("status without a parameter showed not the current campaign:\n%s", text)
	}

	named, _, err := handlers.status(ctx, nil, statusArgs{CampaignID: closed})
	text := toolText(t, named, err)
	if !strings.Contains(text, fmt.Sprintf("id %d", closed)) || !strings.Contains(text, "cancelled") {
		t.Fatalf("status by id showed not the named campaign:\n%s", text)
	}

	// A negative id is the same typo as a foreign one: a report on the current
	// campaign in answer to it reads as a report on the requested one.
	for _, id := range []int64{9999, -1} {
		_, _, err := handlers.status(ctx, nil, statusArgs{CampaignID: id})
		if err == nil {
			t.Fatalf("id %d accepted as a campaign", id)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", id)) {
			t.Fatalf("refusal does not name the requested id %d: %v", id, err)
		}
	}
}

// The ReadOnlyHint annotation is a promise to the client that the call changes
// nothing: with it the tool passes without confirmation. Promising it for
// tg_send or start would let sending out go silently.
func TestReadToolsAreReadOnly(t *testing.T) {
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	server := NewMCPServer(&Service{cfg: Config{AccountLabel: "test"}, Now: time.Now}, nil)
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("start the server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	readOnly := map[string]bool{}
	for _, tool := range list.Tools {
		readOnly[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
	}
	for _, name := range []string{"tg_find", "tg_read", "tg_dialogs", "status"} {
		if !readOnly[name] {
			t.Fatalf("%s is not marked as a reading tool", name)
		}
	}
	for _, name := range []string{"tg_send", "start"} {
		if readOnly[name] {
			t.Fatalf("%s is marked as reading, but it writes outward", name)
		}
	}
}

// start outside the window must name when sending will begin: the rule "window
// 10:00-19:00" does not answer "and what about now", and a start on Friday
// evening looks like sending in progress.
func TestStartOutsideWindowNamesOpening(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	// Saturday of the same week: the window is closed until Monday morning.
	service.Now = func() time.Time { return testNow.AddDate(0, 0, 3) }

	result, _, err := handlers.start(ctx, nil, struct{}{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "the send window is closed, next opening 07.09 10:00 MSK") {
		t.Fatalf("start outside the window did not name the opening:\n%s", text)
	}
}

// The advice during an idle follows the situation: start launches a draft, while
// a closed campaign does not run again - start refuses there, and a new one has
// to be created.
func TestIdleAdviceFollowsWhatCanStart(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}

	draft, _, err := handlers.status(ctx, nil, statusArgs{})
	if text := toolText(t, draft, err); !strings.Contains(text, "launch one with start") {
		t.Fatalf("the lone draft is left without the start advice:\n%s", text)
	}

	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("cancel the draft: %v", err)
	}
	closed, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, closed, err)
	if strings.Contains(text, "start") {
		t.Fatalf("start is advised for a closed campaign:\n%s", text)
	}
	if !strings.Contains(text, "load_campaign") {
		t.Fatalf("a closed campaign does not name what creates a new one:\n%s", text)
	}
}

// A broken template is named on any campaign, but removing it through stop is
// advised only for a live one: on a closed one stop refuses - nothing to remove.
func TestBrokenTemplateAdviceFitsTheCampaign(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	runSQL(t, service, `UPDATE campaigns SET template_text = 'Привет, {{имя}}!' WHERE id = $1`, id)

	live, _, err := handlers.status(ctx, nil, statusArgs{})
	if text := toolText(t, live, err); !strings.Contains(text, "remove the broken one with stop and cancel") {
		t.Fatalf("a running campaign has no advice to remove it:\n%s", text)
	}

	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("cancel the campaign: %v", err)
	}
	closed, _, err := handlers.status(ctx, nil, statusArgs{CampaignID: id})
	text := toolText(t, closed, err)
	if !strings.Contains(text, "This campaign's template could not be parsed") {
		t.Fatalf("the broken template is not named on a closed campaign:\n%s", text)
	}
	if strings.Contains(text, "stop and cancel") {
		t.Fatalf("stop is advised for a closed campaign:\n%s", text)
	}
}

// A message with no text and no attachment: an empty line "them: " tells the
// reader nothing. A zero time on such a row must not break it either.
func TestEmptyMessageIsNamed(t *testing.T) {
	handlers := liveTools(&stubDialog{
		poll: func(int) []ChatMessage { return []ChatMessage{{ID: 1}} },
	})

	result, _, err := handlers.read(context.Background(), nil, readArgs{Username: "ivan_petrov"})
	text := toolText(t, result, err)
	if !strings.Contains(text, "  them: [no text]\n") {
		t.Fatalf("a message with no text printed empty:\n%s", text)
	}
}

// agentService is a service with both tokens and the agent's list; no database needed.
func agentService() *Service {
	return &Service{cfg: Config{
		AccountLabel:   "test",
		MCPToken:       "owner-token",
		MCPAgentToken:  "agent-token",
		AgentAllowList: []string{"dev_account"},
	}, Now: time.Now}
}

// bearer puts the token into every request of the MCP client.
type bearer struct{ token string }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// toolsBehind connects to the endpoint with a token and returns the tool names.
func toolsBehind(t *testing.T, endpoint, token string) (map[string]bool, error) {
	t.Helper()
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearer{token}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	return names, nil
}

// The token determines the set of tools: the agent sees only the two dialog
// tools, and campaigns, the account stop and other people's conversations are
// not available to it at all. The exact set, not the absence of a single start:
// a leaked tg_dialogs would otherwise pass the test.
func TestTokenPicksTools(t *testing.T) {
	service := agentService()
	server := httptest.NewServer(MCPHandler(service, offlineChat(&stubDialog{})))
	t.Cleanup(server.Close)

	owner, err := toolsBehind(t, server.URL, "owner-token")
	if err != nil {
		t.Fatalf("owner did not connect: %v", err)
	}
	if len(owner) != 10 {
		t.Fatalf("owner sees %d tools, expected 10: %v", len(owner), owner)
	}

	agent, err := toolsBehind(t, server.URL, "agent-token")
	if err != nil {
		t.Fatalf("agent did not connect: %v", err)
	}
	if len(agent) != 2 || !agent["tg_send"] || !agent["tg_read"] {
		t.Fatalf("agent sees %v, expected exactly tg_send and tg_read", agent)
	}

	if _, err := toolsBehind(t, server.URL, "wrong-token"); err == nil {
		t.Fatal("a foreign token was let in")
	}
}

// A session belongs to the token that opened it: the agent token does not
// continue the owner's session, and vice versa. Otherwise the agent would get
// the campaigns by learning a session id.
func TestSessionBelongsToItsToken(t *testing.T) {
	server := httptest.NewServer(MCPHandler(agentService(), offlineChat(&stubDialog{})))
	t.Cleanup(server.Close)

	post := func(token, session, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

	for _, pair := range [][2]string{{"owner-token", "agent-token"}, {"agent-token", "owner-token"}} {
		opened := post(pair[0], "", initialize)
		session := opened.Header.Get("Mcp-Session-Id")
		if session == "" {
			t.Fatalf("%s: session did not open, code %d", pair[0], opened.StatusCode)
		}
		if resp := post(pair[1], session, list); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("session of %s continued with token %s: code %d", pair[0], pair[1], resp.StatusCode)
		}
	}
}

// The agent writes to and reads from test accounts only, and the service itself
// refuses before going to Telegram. The calls go through the assembled
// NewAgentServer: swapping the handler or the list in the assembly breaks the
// test. The username is normalized the same way as in Chat: a form with @, in
// another case or as a link neither bypasses the list nor breaks a legitimate
// username.
func TestAgentReachesOnlyTestAccounts(t *testing.T) {
	ctx := context.Background()
	notReady := &telegram.Failure{Kind: telegram.KindNotReady, Message: "client not started"}
	service := agentService()
	service.cfg.OwnerAccounts = []string{"smirnov"}
	server := NewAgentServer(service, offlineChat(&failDialog{err: notReady}))

	serverSide, clientSide := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("start the server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	cases := []struct {
		username string
		allowed  bool
	}{
		{"dev_account", true},
		{"@Dev_Account", true},
		{"t.me/dev_account", true},
		{"smirnov", false},
		{"", false},
		{"dev_account/x", false},
	}
	for _, tc := range cases {
		// An empty text is rejected by Chat.Send before the database and Telegram:
		// a refusal about the text means the call passed the agent's list.
		calls := map[string]map[string]any{
			"tg_send": {"username": tc.username, "text": ""},
			"tg_read": {"username": tc.username},
		}
		for tool, arguments := range calls {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: arguments})
			if err != nil {
				t.Fatalf("%s %q: call: %v", tool, tc.username, err)
			}
			var text strings.Builder
			for _, content := range result.Content {
				if item, ok := content.(*mcp.TextContent); ok {
					text.WriteString(item.Text)
				}
			}
			refused := strings.Contains(text.String(), "AGENT_ALLOW_LIST")
			if refused == tc.allowed {
				t.Fatalf("%s %q: expected pass %v, answer %q", tool, tc.username, tc.allowed, text.String())
			}
		}
	}
}
