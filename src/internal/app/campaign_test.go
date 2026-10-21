package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
)

// testNow is a Wednesday, the middle of the send window. The time is injected
// into Service instead of read from the clock: otherwise a run on Saturday
// turns the guard tests red.
var testNow = time.Date(2026, 9, 2, 12, 0, 0, 0, moscowZone)

// newService brings up a service on empty tables of the test database. Service
// recipients are passed here, not into the cfg of a ready service: the list
// enters the query texts built in NewService.
func newService(t *testing.T, owners ...string) *Service {
	t.Helper()

	if testing.Short() {
		t.Skip("-short: integration tier skipped")
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, url, 4)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE direct_messages, recipients, campaigns, accounts, stop_list RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}

	service := NewService(pool, Config{
		AccountLabel:      "test",
		AccountDailyLimit: 10,
		SendWindowStart:   "10:00",
		SendWindowEnd:     "19:00",
		ClaimStaleAfter:   5 * time.Minute,
		OwnerAccounts:     owners,
	})
	service.Now = func() time.Time { return testNow }
	if _, _, err := service.EnsureAccount(ctx); err != nil {
		t.Fatalf("create account: %v", err)
	}
	return service
}

func person(contactID int64, username string) Person {
	return Person{ContactID: contactID, LeadID: contactID * 10, Name: "Иван", Username: username}
}

// startWith is the usual setup: a draft with people and a campaign start.
func startWith(t *testing.T, service *Service, people ...Person) int64 {
	t.Helper()
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "test")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, people); err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("start campaign: %v", err)
	}
	return id
}

func recipientState(t *testing.T, pool *pgxpool.Pool, username string) (string, string) {
	t.Helper()
	var state, reason string
	err := pool.QueryRow(context.Background(),
		`SELECT state, reason FROM recipients WHERE username = $1 ORDER BY id DESC LIMIT 1`, username).
		Scan(&state, &reason)
	if err != nil {
		t.Fatalf("read row %q: %v", username, err)
	}
	return state, reason
}

func campaignStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM campaigns WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read campaign %d: %v", id, err)
	}
	return status
}

// The substitution recognises the name in any case and with spaces inside the
// braces: "{Имя}" at the start of a sentence is a natural typo, and it must not
// go into the message literally. Foreign braces stay as they are - they are
// rejected by load_campaign.
func TestRenderMessage(t *testing.T) {
	got := RenderMessage("{Имя}, ждём {дата} в {ИМЯ}-клубе, { имя }", " Пётр\n")
	want := "Пётр, ждём {дата} в Пётр-клубе, Пётр"
	if got != want {
		t.Fatalf("substitution gave %q, want %q", got, want)
	}
}

// The template is checked on input: a campaign is built once and goes out to
// hundreds, so a foreign placeholder and an over-long text must be rejected
// before the list is assembled, not on the first send.
func TestLoadCampaignChecksTemplate(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	// Every brace is checked, not only properly paired ones: "{{имя}}" is a Jinja
	// typo, and substitution inside the outer pair would give the person
	// "Привет, {Пётр}!", while an unclosed brace would go into the message
	// literally.
	for _, template := range []string{
		"Привет, {имя}! Ждём в {город}, {name}",
		"Привет, {{имя}}!",
		"Привет, {имя! Как дела?",
		"Привет, имя}!",
	} {
		if _, err := service.LoadCampaign(ctx, template, "test"); !errors.Is(err, ErrUnknownPlaceholder) {
			t.Fatalf("template %q accepted: %v", template, err)
		}
	}

	long := "Привет, {имя}! " + strings.Repeat("а", maxTextLen)
	if _, err := service.LoadCampaign(ctx, long, "test"); !errors.Is(err, ErrTemplateTooLong) {
		t.Fatalf("template over the Telegram limit accepted: %v", err)
	}

	// By runes this template fits, by the Telegram count it does not: an emoji counts as two.
	emoji := strings.Repeat("а", maxTextLen-150) + strings.Repeat("🙂", 100)
	if _, err := service.LoadCampaign(ctx, emoji, "test"); !errors.Is(err, ErrTemplateTooLong) {
		t.Fatalf("template with emoji accepted: %v", err)
	}

	if _, err := service.LoadCampaign(ctx, "Привет, {Имя}!", "test"); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
}

// A template with {имя} and a person without a name: the recipient is accepted
// and the address is removed together with the comma - a message starting with
// a comma goes to nobody, but the person does not vanish from the campaign
// either (owner's decision 02.09.2026). The CRM name collapses too:
// "Иван\n\nПетров" in a message body looks like a glitch.
func TestNamelessRecipientKeptWithoutGreeting(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	nameless := Person{ContactID: 1, Name: "\n\t", Username: "nameless_one"}

	id, err := service.LoadCampaign(ctx, "Привет, {имя}! Это проверка", "with name")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{
		nameless,
		{ContactID: 2, Name: "Иван\n\nПетров", Username: "ivan_petrov"},
	})
	if err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	if report.Accepted != 2 || len(report.Skipped) != 0 {
		t.Fatalf("filtering report %+v", report)
	}

	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("start campaign: %v", err)
	}
	texts := map[string]string{
		"nameless_one": "Привет! Это проверка",
		"ivan_petrov":  "Привет, Иван Петров! Это проверка",
	}
	for range texts {
		claimed, err := service.ClaimNext(ctx, "test")
		if err != nil || claimed == nil {
			t.Fatalf("claim a row: %v, %+v", err, claimed)
		}
		if want := texts[claimed.Username]; claimed.Text != want {
			t.Fatalf("text for %s: %q, want %q", claimed.Username, claimed.Text, want)
		}
		if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
			t.Fatalf("finalize: %v", err)
		}
	}

	free, err := service.LoadCampaign(ctx, "Приглашаем в клуб", "without name")
	if err != nil {
		t.Fatalf("create campaign without placeholder: %v", err)
	}
	// The person is new: the first nameless one already got a message, and in
	// the second campaign they are closed by the "one person, one message" rule,
	// not by the name.
	second, err := service.AddRecipients(ctx, free,
		[]Person{{ContactID: 3, Name: " ", Username: "nameless_two"}})
	if err != nil {
		t.Fatalf("add the nameless one: %v", err)
	}
	if second.Accepted != 1 {
		t.Fatalf("template without {имя} filtered out the nameless one: %+v", second)
	}
}

// A CRM name is the last path for a curly brace into a message: "Ирина {тест}"
// and especially "{имя}" would give the person exactly the text the template
// check stands against. Length is counted the Telegram way, not by runes: an
// emoji counts as two, and a name made of emoji overflows the message where the
// rune count saw headroom.
func TestBadNamesSkippedAtImport(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}! "+strings.Repeat("а", maxTextLen-200), "test")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{
		{ContactID: 1, Name: "Ирина {тест}", Username: "irina_petrova"},
		{ContactID: 2, Name: "{имя}", Username: "petr_ivanov"},
		{ContactID: 3, Name: strings.Repeat("🙂", 100), Username: "anna_smirnova"},
		{ContactID: 4, Name: "Иван", Username: "ivan_petrov"},
	})
	if err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	if report.Accepted != 1 || report.Skipped[ReasonNameBraces] != 2 || report.Skipped[ReasonTextTooLong] != 1 {
		t.Fatalf("filtering report %+v", report)
	}

	// A template without a substitution takes no character of the name into the
	// message, and a brace in the name cannot be a reason to filter.
	free, err := service.LoadCampaign(ctx, "Приглашаем в клуб", "without name")
	if err != nil {
		t.Fatalf("create campaign without placeholder: %v", err)
	}
	second, err := service.AddRecipients(ctx, free, []Person{
		{ContactID: 1, Name: "Ирина {Клуб}", Username: "irina_petrova"},
	})
	if err != nil {
		t.Fatalf("add the recipient: %v", err)
	}
	if second.Accepted != 1 {
		t.Fatalf("a brace in the name filtered out a person from a campaign without a placeholder: %+v", second)
	}
}

// A lead without a usable username does not enter the mailing: guessing from
// the name is forbidden, so Cyrillic, a phone number and an empty string must
// all be filtered out.
func TestNormalizeUsername(t *testing.T) {
	cases := map[string]string{
		"  @Ivan_Petrov ":       "ivan_petrov",
		"https://t.me/Ivan/":    "",
		"t.me/ivan_petrov":      "ivan_petrov",
		"Иван Петров":           "",
		"+7 999 123-45-67":      "",
		"":                      "",
		"ab":                    "",
		"1ivan":                 "",
		"ivan petrov":           "",
		"ivanivanivanivanivan1": "ivanivanivanivanivan1",
		// Telegram does not issue such usernames: shorter than five characters,
		// with a trailing or a double underscore. They used to pass, and the queue
		// spent a minute on each.
		"abcd":          "",
		"abcde":         "abcde",
		"zzz_trailing_": "",
		"zzz__double":   "",
		"a_b_c":         "a_b_c",
	}
	for raw, want := range cases {
		if got := normalizeUsername(raw); got != want {
			t.Errorf("normalizeUsername(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Reloading the list: a struck-out person must not remain in the queue, and
// exactly one draft must remain.
func TestReloadCancelsOldDraft(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	first, err := service.LoadCampaign(ctx, "Привет, {имя}!", "first")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, err := service.AddRecipients(ctx, first, []Person{person(1, "striked_out")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}

	second, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if status := campaignStatus(t, service.pool, first); status != "cancelled" {
		t.Fatalf("previous draft is in state %q, want cancelled", status)
	}
	if state, _ := recipientState(t, service.pool, "striked_out"); state != "cancelled" {
		t.Fatalf("struck-out person stayed in state %q", state)
	}
	var drafts int
	if err := service.pool.QueryRow(ctx, `SELECT count(*) FROM campaigns WHERE status = 'draft'`).Scan(&drafts); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if drafts != 1 || campaignStatus(t, service.pool, second) != "draft" {
		t.Fatalf("drafts after reload: %d", drafts)
	}
}

// A batch retry after an MCP call timeout: duplicates go to the filtering
// report with a reason, not to an error, and no second row appears.
func TestRepeatBatchSkipsDuplicates(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "test")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	batch := []Person{person(1, "ivan_petrov"), person(2, "petr_ivanov")}

	first, err := service.AddRecipients(ctx, id, batch)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	second, err := service.AddRecipients(ctx, id, batch)
	if err != nil {
		t.Fatalf("batch retry: %v", err)
	}

	if first.Accepted != 2 || second.Accepted != 0 || second.Skipped[ReasonDuplicate] != 2 {
		t.Fatalf("first batch %+v, retry %+v", first, second)
	}
	var rows int
	if err := service.pool.QueryRow(ctx, `SELECT count(*) FROM recipients WHERE campaign_id = $1`, id).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("campaign has %d rows, want 2", rows)
	}
}

// Zero usable recipients: start refuses, the filtering report names the reason.
func TestStartRefusesEmptyDraft(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "test")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "Иван"), person(2, "+79991234567")})
	if err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonNoUsername] != 2 {
		t.Fatalf("filtering report %+v", report)
	}

	if _, err := service.StartCampaign(ctx); !errors.Is(err, ErrEmptyDraft) {
		t.Fatalf("start of an empty draft returned %v", err)
	}
}

// A restart after a pause: sent rows are untouched, the campaign continues
// from planned.
func TestRestartAfterPauseKeepsSent(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim a row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("pause: %v", err)
	}

	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if status := campaignStatus(t, service.pool, id); status != "active" {
		t.Fatalf("campaign is in state %q", status)
	}
	next, err := service.ClaimNext(ctx, "test")
	if err != nil || next == nil {
		t.Fatalf("no row was claimed after start: %v, %+v", err, next)
	}
	if next.ID == claimed.ID {
		t.Fatalf("an already sent row %d was claimed again", next.ID)
	}
}

// Start with a live active campaign refuses clearly instead of failing on the
// unique index: the operator must see what to do next.
func TestStartRefusedWhileActive(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"))
	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(2, "second_one")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}

	if _, err := service.StartCampaign(ctx); !errors.Is(err, ErrActiveCampaign) {
		t.Fatalf("start with an active campaign returned %v", err)
	}
	if status := campaignStatus(t, service.pool, draft); status != "draft" {
		t.Fatalf("draft after the refusal is in state %q", status)
	}
}

// pausedAndDraft is an awkward pair: a paused campaign and a prepared draft
// side by side. The tools must resolve it unambiguously.
func pausedAndDraft(t *testing.T, service *Service) (paused, draft int64) {
	t.Helper()
	ctx := context.Background()

	paused = startWith(t, service, person(1, "first_one"))
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "draft")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(2, "second_one")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	return paused, draft
}

// The pair "pause plus draft": start starts the draft - a new list is fresher
// than a resume, and the pause stays resumable.
func TestStartPrefersDraftOverPause(t *testing.T) {
	service := newService(t)
	paused, draft := pausedAndDraft(t, service)

	started, err := service.StartCampaign(context.Background())
	if err != nil || started != draft {
		t.Fatalf("campaign %d started instead of draft %d: %v", started, draft, err)
	}
	if status := campaignStatus(t, service.pool, paused); status != "stopped" {
		t.Fatalf("pause after the draft start is in state %q", status)
	}
}

// The same pair under cancel: stop with cancel removes the draft, the pause is
// intact and continues with the next start. A second cancel removes it.
func TestCancelHitsDraftBeforePause(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	paused, draft := pausedAndDraft(t, service)

	cancelled, err := service.StopCampaign(ctx, true)
	if err != nil || cancelled != draft {
		t.Fatalf("campaign %d cancelled instead of draft %d: %v", cancelled, draft, err)
	}
	if status := campaignStatus(t, service.pool, paused); status != "stopped" {
		t.Fatalf("pause removed together with the draft: state %q", status)
	}

	second, err := service.StopCampaign(ctx, true)
	if err != nil || second != paused {
		t.Fatalf("second cancel removed campaign %d instead of pause %d: %v", second, paused, err)
	}
}

// Cancel hits one campaign: the running one if there is one, otherwise the
// draft. A prepared draft does not vanish together with a cancelled mailing.
func TestCancelHitsOneCampaign(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	active := startWith(t, service, person(1, "first_one"))
	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(2, "second_one")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}

	cancelled, err := service.StopCampaign(ctx, true)
	if err != nil || cancelled != active {
		t.Fatalf("campaign %d cancelled instead of %d: %v", cancelled, active, err)
	}
	if status := campaignStatus(t, service.pool, draft); status != "draft" {
		t.Fatalf("draft vanished together with the active campaign: state %q", status)
	}
	if state, _ := recipientState(t, service.pool, "second_one"); state != "planned" {
		t.Fatalf("draft row is in state %q", state)
	}

	// There is no running campaign any more - the second cancel removes the draft.
	second, err := service.StopCampaign(ctx, true)
	if err != nil || second != draft {
		t.Fatalf("second cancel removed campaign %d instead of %d: %v", second, draft, err)
	}
	if state, _ := recipientState(t, service.pool, "second_one"); state != "cancelled" {
		t.Fatalf("row of the removed draft is in state %q", state)
	}
}

// The "one person, one message" rule is lifted for the owner's service
// recipients and kept for everyone else: a manual touch closes a stranger and
// does not close the owner's account, which is used to check this very
// mailing.
func TestOwnerPassesImportAfterDirectTouch(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	insertDirect(t, service, "smirnov", "sent", testNow)
	insertDirect(t, service, "ivan_petrov", "sent", testNow)

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "check")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "smirnov"), person(2, "ivan_petrov")})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Accepted != 1 || report.Skipped[ReasonDirectSent] != 1 {
		t.Fatalf("filtering report %+v", report)
	}
	if state, _ := recipientState(t, service.pool, "smirnov"); state != "planned" {
		t.Fatalf("service recipient is in state %q", state)
	}
}

// The stop list holds a service recipient too: the change lifts the "one
// message" rule, not the guard that forbids writing to a person at all.
func TestOwnerStillHeldByStopList(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('smirnov')`); err != nil {
		t.Fatalf("stop list: %v", err)
	}
	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "check")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "smirnov")})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonStopList] != 1 {
		t.Fatalf("filtering report %+v", report)
	}
}
