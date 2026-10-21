package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// insertDirect inserts a manual-touch journal row directly with SQL: the dialog
// writes the journal, and screening must be tested separately from the write.
func insertDirect(t *testing.T, service *Service, username, state string, created time.Time) {
	t.Helper()
	if _, err := service.pool.Exec(context.Background(),
		`INSERT INTO direct_messages (username, account_id, text, random_id, state, created_at)
		 VALUES ($1, (SELECT id FROM accounts WHERE label = 'test'), 'привет', $2, $3, $4)`,
		username, newRandomID(), state, created); err != nil {
		t.Fatalf("insert touch %q: %v", state, err)
	}
}

// A manual send closes the person to campaigns: a journal row is a message just
// like a queue row. Both entry points are checked: the import of the next
// campaign and the queue of one already running.
func TestDirectSendClosesPersonForCampaigns(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	insertDirect(t, service, "ivan_petrov", "sent", testNow)

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("person with a manual touch was claimed for sending: %+v", claimed)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonDirectSent {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("second campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("import into the second campaign: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonDirectSent] != 1 {
		t.Fatalf("screening report %+v", report)
	}
}

// An unclosed touch closes the person but is reported under its own reason:
// pending means "the message may have gone out", and the report must not pass
// that off as a completed send. The age of the touch does not change the reason:
// a fresh pending is as unknown as an old one, and both entry points - the queue
// and the import - must say the same thing about it.
func TestPendingTouchReportedSeparately(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	insertDirect(t, service, "ivan_petrov", "pending", testNow)

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("person with a stuck touch was claimed for sending: %+v", claimed)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonDirectPending {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("second campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("import into the second campaign: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonDirectPending] != 1 {
		t.Fatalf("screening report %+v", report)
	}
}

// A refusal in which the message certainly did not go out returns the person to
// campaigns: otherwise a failed manual attempt would silently strike them off
// forever.
func TestFailedTouchDoesNotClosePerson(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	insertDirect(t, service, "ivan_petrov", "failed", testNow)

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "test")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("add recipients: %v", err)
	}
	if report.Accepted != 1 {
		t.Fatalf("failed touch screened the person out: %+v", report)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("start campaign: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("row not claimed: %v, %+v", err, claimed)
	}
}

// A campaign that is not active sends nothing: the claim requires an active campaign.
func TestNoSendWhenCampaignNotActive(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "test")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("row claimed from a draft: %+v", claimed)
	}
}

// Restart after sending: the row is already sent, and claim staleness does not
// return it to the queue.
func TestNoResendAfterRestart(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	service.Now = func() time.Time { return testNow.Add(time.Hour) }
	again, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again != nil {
		t.Fatalf("sent row claimed again: %+v", again)
	}
}

// A person's reply closes the row: there are no more touches after replied.
func TestNoTouchAfterReply(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	rows, err := service.MarkReplied(ctx, 0, "@Ivan_Petrov")
	if err != nil || rows != 1 {
		t.Fatalf("reply not recorded: %v, rows %d", err, rows)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "replied" {
		t.Fatalf("row in state %q", state)
	}
	service.Now = func() time.Time { return testNow.Add(time.Hour) }
	again, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if again != nil {
		t.Fatalf("person who replied was claimed for sending again: %+v", again)
	}
}

// The person wrote first: the invitation is cancelled, but this does not count
// as a reply. The row goes to skipped with the reason "wrote first": they never
// received the message, and belong in the screening report, not in the reply
// metric.
func TestInboundBeforeSendCancelsTouch(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	replied, err := service.MarkReplied(ctx, 0, "ivan_petrov")
	if err != nil || replied != 0 {
		t.Fatalf("unsent row was recorded as a reply: %v, rows %d", err, replied)
	}
	rows, err := service.markWroteFirst(ctx, 0, "ivan_petrov")
	if err != nil || rows != 1 {
		t.Fatalf("inbound message not recorded: %v, rows %d", err, rows)
	}

	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonWroteFirst {
		t.Fatalf("row in state %q (%s), expected skipped/%s", state, reason, ReasonWroteFirst)
	}
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("invitation was claimed for sending anyway: %+v", claimed)
	}
}

// Outside the 10:00-19:00 weekday window nothing is sent: not early in the morning, not on a weekend.
func TestNoSendOutsideWindow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	moments := map[string]time.Time{
		"before the window": time.Date(2026, 9, 2, 9, 30, 0, 0, moscowZone),
		"after the window":  time.Date(2026, 9, 2, 19, 0, 0, 0, moscowZone),
		"saturday":          time.Date(2026, 9, 5, 12, 0, 0, 0, moscowZone),
		"inside the window in UTC, outside it in MSK": time.Date(2026, 9, 2, 17, 30, 0, 0, time.UTC),
	}
	for name, moment := range moments {
		service.Now = func() time.Time { return moment }
		claimed, err := service.ClaimNext(ctx, "test")
		if err != nil {
			t.Fatalf("%s: claim: %v", name, err)
		}
		if claimed != nil {
			t.Fatalf("%s: row claimed outside the window", name)
		}
	}
}

// The daily cap holds under concurrency: two processes on one account do not
// exceed it together - the count and the row claim run under the account lock.
func TestNoSendOverDailyLimit(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	if _, err := service.pool.Exec(ctx, `UPDATE accounts SET daily_limit = 1`); err != nil {
		t.Fatalf("set cap: %v", err)
	}

	var wait sync.WaitGroup
	results := make([]*Recipient, 2)
	errs := make([]error, 2)
	for i := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[i], errs[i] = service.ClaimNext(ctx, "test")
		}()
	}
	wait.Wait()

	claimed := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		if results[i] != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed %d rows with a cap of 1", claimed)
	}
}

// The daily cap is shared by both channels: a cold manual touch spends it the
// same way the mailing does. Otherwise the agent and a campaign would produce
// two caps of cold touches from one number in the same day.
func TestColdDirectSpendsCampaignLimit(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
	sendOK(t, chatOn(service, &stubDialog{}), "stranger_one", "привет")

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("campaign claimed a row over the spent cap: %+v", claimed)
	}
	idle, err := service.IdleReason(ctx, "test")
	if err != nil || idle.Reason != IdleDailyLimit {
		t.Fatalf("idle reason %q (%v), expected %q", idle.Reason, err, IdleDailyLimit)
	}
}

// Only a human lifts an account stop: while the flag is set, nothing is sent.
func TestNoSendWhenAccountStopped(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE accounts SET stopped = true, stop_reason = 'PEER_FLOOD', stopped_at = now()`); err != nil {
		t.Fatalf("stop account: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("stopped account claimed a row: %+v", claimed)
	}
}

// A person without a name gets a message without a greeting instead of dropping
// out of the campaign (a deliberate choice). Go collapses a name made of
// a single newline into an empty one, and the greeting is removed entirely -
// otherwise the message would start with a comma.
func TestNamelessRowGoesWithoutGreeting(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, amo_lead_id, name, username, state, random_id)
		 SELECT id, 2, 20, E'\n', 'petr_ivanov', 'planned', 777 FROM campaigns WHERE status = 'active'`); err != nil {
		t.Fatalf("insert row without a name: %v", err)
	}

	first, err := service.ClaimNext(ctx, "test")
	if err != nil || first == nil || first.Username != "ivan_petrov" {
		t.Fatalf("first row: %v, %+v", err, first)
	}
	if _, err := service.FinishClaim(ctx, first, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	next, err := service.ClaimNext(ctx, "test")
	if err != nil || next == nil {
		t.Fatalf("row without a name was not sent: %v, %+v", err, next)
	}
	if next.Username != "petr_ivanov" {
		t.Fatalf("claimed row %q, expected petr_ivanov", next.Username)
	}
	// The campaign template is "Привет, {имя}!": without a name the greeting is
	// removed together with the comma, and the rest of the text stays verbatim.
	if next.Text != "Привет!" {
		t.Fatalf("message text %q, expected \"Привет!\"", next.Text)
	}
}

// A campaign with an unresolved brace in the template sends nothing, even if it
// passed validation in an earlier version, but it does not change its own state
// either: a broken template is a reason for the queue to idle, and a human drives
// the campaign state. The template is broken directly in the database -
// load_campaign no longer lets such a template through.
func TestBrokenTemplateStopsTheQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET template_text = 'Привет, {{имя}}!' WHERE id = $1`, id); err != nil {
		t.Fatalf("break template: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("campaign with a broken template returned a row: %+v", claimed)
	}
	if status := campaignStatus(t, service.pool, id); status != "active" {
		t.Fatalf("service changed the campaign state to %q by itself", status)
	}
	idle, err := service.IdleReason(ctx, "test")
	if err != nil || idle.Reason != IdleBrokenTemplate {
		t.Fatalf("idle reason %q (%v), expected %q", idle.Reason, err, IdleBrokenTemplate)
	}
	status, err := service.Status(ctx, false)
	if err != nil || len(status.Idle.Braces) == 0 {
		t.Fatalf("status did not name the broken template: %+v, %v", status.Idle, err)
	}

	// Pause and resume stay with the human, but start does not let an unfit
	// campaign continue: the list has to be rebuilt.
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := service.StartCampaign(ctx); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("starting a campaign with a broken template returned %v", err)
	}
}

// An unfit name is not sent even when it got into the queue bypassing the
// import: a brace in the name and a message longer than the Telegram limit are
// the same gates as on import, and the row gets the same reason.
func TestBadNamesNeverLeaveTheQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET template_text = 'Привет, {имя}! ' || repeat('а', $2) WHERE id = $1`,
		id, maxTextLen-200); err != nil {
		t.Fatalf("lengthen template: %v", err)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, amo_lead_id, name, username, state, random_id)
		 VALUES ($1, 2, 20, 'Ирина {Клуб}', 'irina_petrova', 'planned', 777),
		        ($1, 3, 30, repeat('🙂', 100), 'anna_smirnova', 'planned', 778),
		        ($1, 4, 40, repeat('🙂', 100), 'olga_orlova', 'planned', 779)`, id); err != nil {
		t.Fatalf("insert rows: %v", err)
	}

	// The fit row goes first, after that the queue must empty.
	first, err := service.ClaimNext(ctx, "test")
	if err != nil || first == nil || first.Username != "ivan_petrov" {
		t.Fatalf("first row: %v, %+v", err, first)
	}
	if _, err := service.FinishClaim(ctx, first, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// One pass, not three: unfit rows are removed one after another until a fit
	// one is found. At one per pass a queue of forty such rows takes hours to
	// clear, and the campaign looks like it is running the whole time.
	next, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if next != nil {
		t.Fatalf("row with an unfit name was sent: %+v", next)
	}

	if state, reason := recipientState(t, service.pool, "irina_petrova"); state != "skipped" || reason != ReasonNameBraces {
		t.Fatalf("name with a brace in state %q (%s)", state, reason)
	}
	for _, username := range []string{"anna_smirnova", "olga_orlova"} {
		if state, reason := recipientState(t, service.pool, username); state != "skipped" || reason != ReasonTextTooLong {
			t.Fatalf("long name @%s in state %q (%s)", username, state, reason)
		}
	}
}

// The stop list is checked at send time, not only on import: a person is added
// to the list after loading too.
func TestStopListCheckedAtSendTime(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('ivan_petrov')`); err != nil {
		t.Fatalf("stop list: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Fatalf("stop-list row was claimed for sending: %+v", claimed)
	}
	state, reason := recipientState(t, service.pool, "ivan_petrov")
	if state != "skipped" || reason != ReasonStopList {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}
}

// One person gets one message, across campaigns too: screening on import and
// the same gate in the claim, if a row got into a new campaign bypassing import.
func TestNoSecondMessageAcrossCampaigns(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	first := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	second, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("second campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, second, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("import into the second campaign: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonAlreadySent] != 1 {
		t.Fatalf("screening report %+v", report)
	}

	// The same person entered in the CRM as a second contact: import recognizes
	// them by username, but we also check the claim gate - the row is inserted
	// bypassing import.
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, name, username, random_id)
		 VALUES ($1, 999, 'Иван', 'ivan_petrov', 42)`, second); err != nil {
		t.Fatalf("insert row bypassing import: %v", err)
	}
	if _, err := service.FinishIfEmpty(ctx); err != nil {
		t.Fatalf("close the first campaign: %v", err)
	}
	if status := campaignStatus(t, service.pool, first); status != "done" {
		t.Fatalf("first campaign in state %q", status)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("start the second: %v", err)
	}

	again, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if again != nil {
		t.Fatalf("second message to the same person: %+v", again)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonAlreadySent {
		t.Fatalf("second campaign row in state %q with reason %q", state, reason)
	}
}

// A campaign closes only on an empty queue: a row in progress is still planned.
func TestCampaignDoneWhenQueueEmpty(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}

	done, err := service.FinishIfEmpty(ctx)
	if err != nil {
		t.Fatalf("emptiness check: %v", err)
	}
	if done || campaignStatus(t, service.pool, id) != "active" {
		t.Fatalf("campaign closed with a row in progress")
	}

	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	done, err = service.FinishIfEmpty(ctx)
	if err != nil {
		t.Fatalf("emptiness check: %v", err)
	}
	if !done || campaignStatus(t, service.pool, id) != "done" {
		t.Fatalf("campaign did not close on an empty queue")
	}
}

// A temporary failure does not spend an attempt: the row returns to the queue
// with its claim released and is claimed again.
func TestRetryReturnsRowToQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "flood_wait"); err != nil {
		t.Fatalf("return to queue: %v", err)
	}

	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil {
		t.Fatalf("row did not return to the queue: %v, %+v", err, again)
	}
	if again.ID != claimed.ID || again.RandomID != claimed.RandomID {
		t.Fatalf("a different row or random_id came back: %+v vs %+v", again, claimed)
	}
}

// Cancelling a campaign does not touch a row with a live claim: the sender is
// carrying it right now. Otherwise the delivery would not be recorded - the
// person would pass the next campaign's import and miss the daily count.
func TestCancelKeepsClaimedRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}

	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("cancel campaign: %v", err)
	}
	if campaignStatus(t, service.pool, id) != "cancelled" {
		t.Fatalf("campaign not cancelled")
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "planned" {
		t.Fatalf("claimed row was cancelled with the campaign: state %q", state)
	}

	closed, err := service.FinishClaim(ctx, claimed, ResultSent, "")
	if err != nil || !closed {
		t.Fatalf("finalize delivery: %v, closed %v", err, closed)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("row in state %q, expected sent", state)
	}
	var sentAt *time.Time
	if err := service.pool.QueryRow(ctx, `SELECT sent_at FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&sentAt); err != nil {
		t.Fatalf("read sent_at: %v", err)
	}
	if sentAt == nil {
		t.Fatal("sent_at not set: the send will not enter the daily count")
	}
}

// A temporary failure on a cancelled campaign moves the row to cancelled: nobody
// works through the queue of a cancelled campaign, and planned would stay forever.
func TestRetryOnCancelledCampaignCancelsRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("cancel campaign: %v", err)
	}

	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "flood_wait"); err != nil {
		t.Fatalf("return to queue: %v", err)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "cancelled" || reason != ReasonCampaignGone {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}
}

// A real Telegram id is above 2^31: finalization must store it rather than fail
// on encoding the parameter - otherwise the very first real send breaks the loop.
func TestFinishClaimStoresBigUserID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	claimed.UserID = 7123456789

	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize with a real id: %v", err)
	}
	var stored int64
	if err := service.pool.QueryRow(ctx, `SELECT tg_user_id FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&stored); err != nil {
		t.Fatalf("read tg_user_id: %v", err)
	}
	if stored != claimed.UserID {
		t.Fatalf("stored tg_user_id %d, expected %d", stored, claimed.UserID)
	}
}

// A reply from a person with a real Telegram id closes the row: the "wrote
// first" gate must not break on exactly the real ids.
func TestMarkRepliedByBigUserID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	const userID int64 = 7123456789
	// Only someone who was sent the message can reply: the row is moved to sent
	// together with the id under which they are recognized in an incoming message.
	if _, err := service.pool.Exec(ctx, `UPDATE recipients SET tg_user_id = $1, state = 'sent'`, userID); err != nil {
		t.Fatalf("set tg_user_id: %v", err)
	}

	rows, err := service.MarkReplied(ctx, userID, "")
	if err != nil || rows != 1 {
		t.Fatalf("reply not recorded: %v, rows %d", err, rows)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "replied" {
		t.Fatalf("row in state %q", state)
	}
}

// Nothing has been sent for a draft yet: an incoming message from its person
// does not move the row to replied, otherwise they would be screened out of a
// future mailing with the reason "already received".
func TestMarkRepliedSkipsDraft(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "draft")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("add recipients: %v", err)
	}

	rows, err := service.MarkReplied(ctx, 0, "ivan_petrov")
	if err != nil {
		t.Fatalf("inbound message: %v", err)
	}
	if rows != 0 {
		t.Fatalf("draft row closed by a reply: rows %d", rows)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "planned" {
		t.Fatalf("draft row in state %q", state)
	}
}

// The row left the queue while sending was in progress: the delivery is still
// recorded - otherwise the person would pass the next campaign's import and get
// a second message.
func TestSentRecordedFromSkippedRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`UPDATE recipients SET state = 'skipped' WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("move row out of the queue: %v", err)
	}

	closed, err := service.FinishClaim(ctx, claimed, ResultSent, "")
	if err != nil || !closed {
		t.Fatalf("finalize delivery: %v, closed %v", err, closed)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("row in state %q, expected sent", state)
	}
	var sentAt *time.Time
	if err := service.pool.QueryRow(ctx, `SELECT sent_at FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&sentAt); err != nil {
		t.Fatalf("read sent_at: %v", err)
	}
	if sentAt == nil {
		t.Fatal("sent_at not set: delivery lost")
	}

	next, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("second campaign: %v", err)
	}
	report, err := service.AddRecipients(ctx, next, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("import into the second campaign: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonAlreadySent] != 1 {
		t.Fatalf("person who received a message passed import: %+v", report)
	}
}

// A second process with the same label took over a stale claim: the first
// managed to record a refusal, the second delivered. The fact of delivery must
// win - otherwise a person without sent_at passes the next campaign's import and
// gets a second message.
func TestSentRecordedFromUndeliveredRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultUndelivered, ReasonNotAllowed); err != nil {
		t.Fatalf("record refusal: %v", err)
	}

	closed, err := service.FinishClaim(ctx, claimed, ResultSent, "")
	if err != nil || !closed {
		t.Fatalf("finalize delivery: %v, closed %v", err, closed)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("row in state %q, expected sent", state)
	}
	var sentAt *time.Time
	if err := service.pool.QueryRow(ctx, `SELECT sent_at FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&sentAt); err != nil {
		t.Fatalf("read sent_at: %v", err)
	}
	if sentAt == nil {
		t.Fatal("sent_at not set: delivery lost")
	}
}

// A person's reply does not open a hole in the cap: a row in progress stays in
// the daily count until finalization, otherwise a second account process would
// take an extra one.
func TestReplyKeepsRowInDailyCount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	if _, err := service.pool.Exec(ctx, `UPDATE accounts SET daily_limit = 1`); err != nil {
		t.Fatalf("set cap: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	rows, err := service.MarkReplied(ctx, 0, claimed.Username)
	if err != nil || rows != 1 {
		t.Fatalf("reply not recorded: %v, rows %d", err, rows)
	}

	next, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if next != nil {
		t.Fatalf("cap exceeded: second row claimed %+v", next)
	}
}

// Attempt budget: a row with a stable unrecognized error leaves the queue when
// the budget runs out and does not hold its head. FLOOD_WAIT does not spend an
// attempt - waiting is not spending.
func TestRetryBudgetExhausted(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "flood_wait"); err != nil {
		t.Fatalf("return to queue: %v", err)
	}
	var attempts int
	if err := service.pool.QueryRow(ctx, `SELECT attempts FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("FLOOD_WAIT spent an attempt: attempts %d", attempts)
	}

	for i := range maxAttempts {
		again, err := service.ClaimNext(ctx, "test")
		if err != nil || again == nil {
			t.Fatalf("row did not return to the queue on attempt %d: %v, %+v", i+1, err, again)
		}
		if _, err := service.FinishClaim(ctx, again, ResultRetryCounted, "unknown_error"); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}

	state, reason := recipientState(t, service.pool, "ivan_petrov")
	if state != "undelivered" || reason != ReasonRetryExhausted {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}
	if next, err := service.ClaimNext(ctx, "test"); err != nil || next != nil {
		t.Fatalf("exhausted row stayed in the queue: %+v, %v", next, err)
	}
}

// The account keeps a claimed row even after it returns to the queue: a
// temporary failure may have hit a message that had already gone out, and
// Telegram deduplication by random_id works only within one sender.
func TestRetryKeepsRowWithAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "unknown_error"); err != nil {
		t.Fatalf("return to queue: %v", err)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ('second', 10)`); err != nil {
		t.Fatalf("create second account: %v", err)
	}

	if other, err := service.ClaimNext(ctx, "second"); err != nil || other != nil {
		t.Fatalf("another account claimed the returned row: %+v, %v", other, err)
	}
	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil || again.ID != claimed.ID {
		t.Fatalf("own account did not get the row back: %v, %+v", err, again)
	}
}

// Only the same account takes over a stale claim: Telegram deduplicates
// random_id within an account, and a retry from a neighboring one would give the
// person a second message.
func TestStaleClaimStaysWithAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ('second', 10)`); err != nil {
		t.Fatalf("create second account: %v", err)
	}

	// The first account process died, the claim went stale.
	service.Now = func() time.Time { return testNow.Add(10 * time.Minute) }
	if other, err := service.ClaimNext(ctx, "second"); err != nil || other != nil {
		t.Fatalf("another account took over the row: %+v, %v", other, err)
	}
	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil {
		t.Fatalf("own account did not get the row back: %v, %+v", err, again)
	}
	if again.ID != claimed.ID {
		t.Fatalf("a different row came back: %d vs %d", again.ID, claimed.ID)
	}
}

// A queue idle is explained by a reason: the analysis of "why did the message
// not go out on time" runs from the log, and the reason must follow the service
// state. The scenario runs as one story because the reasons override each other
// in gate order: window, stop, cap, campaign, queue.
func TestIdleReasonFollowsState(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	assertIdle := func(want string) {
		t.Helper()
		got, err := service.IdleReason(ctx, "test")
		if err != nil {
			t.Fatalf("idle reason: %v", err)
		}
		if got.Reason != want {
			t.Fatalf("reason %q, expected %q", got.Reason, want)
		}
	}

	assertIdle(IdleNoCampaign)

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	assertIdle(IdleQueueEmpty)

	if _, err := service.pool.Exec(ctx,
		`UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`); err != nil {
		t.Fatalf("lower cap: %v", err)
	}
	assertIdle(IdleDailyLimit)

	if err := service.StopAccount(ctx, "test", "PEER_FLOOD"); err != nil {
		t.Fatalf("stop account: %v", err)
	}
	assertIdle(IdleAccountStop)

	// Saturday of the same week: the window is closed and overrides all the reasons below.
	service.Now = func() time.Time { return testNow.AddDate(0, 0, 3) }
	assertIdle(IdleWindowClosed)
}

// Rows that a gate will remove are not counted as available: a report that
// counted them says "the queue is moving, the sender takes rows one by one" for a
// campaign that will not send a single message. The headline promise of the
// release - the report itself names why the queue is stalled - is false with such
// a count.
func TestIdleReasonCountsOnlyRowsThatCanGo(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	// The name is broken in the database: that is how such a row gets into the
	// queue - loaded before the gate existed rather than through import. A brace
	// in the name would go into the message, which is the same symptom the
	// template check guards against.
	startWith(t, service, person(1, "ivan_petrov"))
	runSQL(t, service, `UPDATE recipients SET name = '{Пётр}' WHERE username = 'ivan_petrov'`)

	idle, err := service.IdleReason(ctx, "test")
	if err != nil {
		t.Fatalf("idle reason: %v", err)
	}
	if idle.Reason != IdleQueueDisqualified || idle.Left != 1 {
		t.Fatalf("reason %q (left %d), expected %q", idle.Reason, idle.Left, IdleQueueDisqualified)
	}

	handlers := &tools{service: service}
	result, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, result, err)
	if strings.Contains(text, "Queue is moving") {
		t.Fatalf("report promises movement of a stalled queue:\n%s", text)
	}
	if !strings.Contains(text, "removes all the remaining rows") {
		t.Fatalf("report did not name the idle reason:\n%s", text)
	}

	// A selection pass clears such a queue, and the reason changes to empty:
	// without this, the "gate removes" wording would stay hanging on a finished campaign.
	if next, err := service.ClaimNext(ctx, "test"); err != nil || next != nil {
		t.Fatalf("row without a name was sent: %v, %+v", err, next)
	}
	if idle, err := service.IdleReason(ctx, "test"); err != nil || idle.Reason != IdleQueueEmpty {
		t.Fatalf("after the pass the reason is %q (%v), expected %q", idle.Reason, err, IdleQueueEmpty)
	}
}

// A busy queue differs from an empty one: a row pinned to a neighboring account
// waits for its process, and the campaign is not finished. Confusing them means
// showing "no work" where work is stalled.
func TestIdleReasonSeesBlockedQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed == nil {
		t.Fatalf("claim row: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ('second', 10)`); err != nil {
		t.Fatalf("create second account: %v", err)
	}

	idle, err := service.IdleReason(ctx, "second")
	if err != nil {
		t.Fatalf("idle reason: %v", err)
	}
	if idle.Reason != IdleQueueBlocked {
		t.Fatalf("reason %q, expected %q", idle.Reason, IdleQueueBlocked)
	}
}

// The window and the daily cap restrict the mailing to strangers and do not
// restrict the owner's service recipients: a mailing check runs on Sunday with
// the cap exhausted and does not spend a single unit of it.
func TestOwnerLeavesQueueOutsideWindowAndOverLimit(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	// Sunday, deep night: the window is closed on both grounds.
	night := time.Date(2026, 9, 6, 3, 0, 0, 0, moscowZone)
	startWith(t, service, person(1, "ivan_petrov"), person(2, "smirnov"), person(3, "petr_sidorov"))
	// A cap of one and one send to a stranger today: usage is exhausted.
	if _, err := service.pool.Exec(ctx,
		`UPDATE recipients SET state = 'sent', sent_at = $1,
		        account_id = (SELECT id FROM accounts WHERE label = 'test')
		 WHERE username = 'petr_sidorov'`, night); err != nil {
		t.Fatalf("spend cap: %v", err)
	}
	if _, err := service.pool.Exec(ctx, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`); err != nil {
		t.Fatalf("lower cap: %v", err)
	}
	service.Now = func() time.Time { return night }

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.Username != "smirnov" {
		t.Fatalf("claim returned %+v, expected a service recipient row", claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("close row: %v", err)
	}

	if next, err := service.ClaimNext(ctx, "test"); err != nil || next != nil {
		t.Fatalf("stranger row claimed outside the window: %+v, %v", next, err)
	}

	report, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	account := report.Accounts[0]
	if account.UsedToday != 1 || account.OwnerToday != 1 {
		t.Fatalf("usage %d with cap %d, service %d: sending to the owner spent the cap",
			account.UsedToday, account.DailyLimit, account.OwnerToday)
	}
}

// The stop list removes a service recipient from the queue too: sending does not
// take a person added to the stop list after import, and the row must leave the
// queue with the reason stop_list - otherwise the campaign never closes and the
// idle would be called a screening.
func TestStopListLeavesQueueForOwner(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	startWith(t, service, person(1, "smirnov"))
	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('smirnov')`); err != nil {
		t.Fatalf("stop list: %v", err)
	}

	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed != nil {
		t.Fatalf("claim took a person from the stop list: %+v, %v", claimed, err)
	}
	if state, reason := recipientState(t, service.pool, "smirnov"); state != "skipped" || reason != ReasonStopList {
		t.Fatalf("row in state %q with reason %q", state, reason)
	}
	if done, err := service.FinishIfEmpty(ctx); err != nil || !done {
		t.Fatalf("campaign did not close on the emptied queue: %v, %v", done, err)
	}
}
