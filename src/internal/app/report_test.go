package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An empty database is the normal state before the first campaign: status must
// answer instead of failing on a missing campaign.
func TestStatusWithoutCampaign(t *testing.T) {
	service := newService(t)

	report, err := service.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if report.Campaign != nil {
		t.Fatalf("campaign came out of an empty database: %+v", report.Campaign)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].Label != "test" {
		t.Fatalf("accounts in the report: %+v", report.Accounts)
	}
}

// A populated campaign: counters by state, the account's daily usage, the
// preview on real rows and the per-row result with full.
func TestStatusCountsStatesAndPreview(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	sender := senderOn(service, &stubTransport{})
	if err := sender.step(ctx); err != nil {
		t.Fatalf("sender step: %v", err)
	}

	report, err := service.Status(ctx, true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if report.Campaign == nil || report.Campaign.Total != 2 {
		t.Fatalf("campaign in the report: %+v", report.Campaign)
	}
	if report.Campaign.States["sent"] != 1 || report.Campaign.States["planned"] != 1 {
		t.Fatalf("state counters: %+v", report.Campaign.States)
	}
	if report.Accounts[0].UsedToday != 1 {
		t.Fatalf("account daily usage: %+v", report.Accounts[0])
	}
	if len(report.Preview) != 2 || !strings.Contains(report.Preview[0].Text, "Иван") {
		t.Fatalf("preview: %+v", report.Preview)
	}
	if len(report.Rows) != 2 || report.Rows[0].State != "sent" {
		t.Fatalf("per-row result: %+v", report.Rows)
	}

	// The remaining row is pinned to another account: nobody but that account
	// takes it, and the report must name whose sender the queue is waiting for.
	if _, err := service.pool.Exec(ctx,
		`WITH other AS (INSERT INTO accounts (label, daily_limit) VALUES ('second', 10) RETURNING id)
		 UPDATE recipients SET account_id = (SELECT id FROM other) WHERE state = 'planned'`); err != nil {
		t.Fatalf("pin the row to another account: %v", err)
	}
	report, err = service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(report.Held) != 1 || report.Held[0].Label != "second" || report.Held[0].Rows != 1 {
		t.Fatalf("rows held by another account: %+v", report.Held)
	}
}

// Among closed campaigns recency is decided by finished_at, not id: the
// campaign that closed later must win in a status call without parameters,
// even if its id is smaller than that of the campaign closed earlier.
func TestStatusPrefersLastClosedOverBiggerID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	doneID := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("claim a row: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// Closes later than the second campaign although its id is smaller.
	service.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	if done, err := service.FinishIfEmpty(ctx); err != nil || !done {
		t.Fatalf("close the first campaign: %v, done=%v", err, done)
	}

	cancelledID, err := service.LoadCampaign(ctx, "Привет, {имя}!", "second")
	if err != nil {
		t.Fatalf("create the second campaign: %v", err)
	}
	if _, err := service.AddRecipients(ctx, cancelledID, []Person{person(2, "petr_ivanov")}); err != nil {
		t.Fatalf("add recipients to the second: %v", err)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("start the second: %v", err)
	}
	// Closes earlier than the first by time, but its id is bigger.
	service.Now = func() time.Time { return testNow.Add(time.Hour) }
	if id, err := service.StopCampaign(ctx, true); err != nil || id != cancelledID {
		t.Fatalf("cancel the second: id %d, err %v", id, err)
	}

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Campaign == nil || status.Campaign.ID != doneID {
		t.Fatalf("status without parameters showed not the last closed campaign: %+v, expected id %d", status.Campaign, doneID)
	}
}

// secondAccount creates a second sender on the same pool: its own label,
// everything else as for the first. This is exactly how a second account
// appears in real life: the process starts with its own label and creates the
// row itself.
func secondAccount(t *testing.T, service *Service, label string) *Service {
	t.Helper()
	cfg := service.cfg
	cfg.AccountLabel = label
	other := NewService(service.pool, cfg)
	other.Now = service.Now
	if _, _, err := other.EnsureAccount(context.Background()); err != nil {
		t.Fatalf("create account %q: %v", label, err)
	}
	return other
}

// Two senders on one campaign: each gets a row, but its own. This is the "one
// person, one message" guard on a path that did not exist before: before
// several accounts, two consecutive claims came from one process.
func TestTwoAccountsSplitQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	secondAccount(t, service, "second")

	first, err := service.ClaimNext(ctx, "test")
	if err != nil || first == nil {
		t.Fatalf("claim by the first account: %v, %+v", err, first)
	}
	other, err := service.ClaimNext(ctx, "second")
	if err != nil || other == nil {
		t.Fatalf("claim by the second account: %v, %+v", err, other)
	}
	if first.Username == other.Username {
		t.Fatalf("both accounts took the same person: %q", first.Username)
	}

	// There is no third row: the queue is empty, and a third claim must not hand
	// out anyone already taken.
	extra, err := service.ClaimNext(ctx, "second")
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if extra != nil {
		t.Fatalf("the queue handed out an extra row: %+v", extra)
	}
}

// A stop of one account does not touch its neighbour and is visible in its
// report line: the idle reason is computed per account, otherwise "why is the
// second one silent" has no answer from outside.
func TestStatusIdlePerAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	secondAccount(t, service, "second")
	if err := service.StopAccount(ctx, "test", "PEER_FLOOD"); err != nil {
		t.Fatalf("account stop: %v", err)
	}

	report, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(report.Accounts) != 2 {
		t.Fatalf("accounts in the report: %+v", report.Accounts)
	}
	byLabel := map[string]AccountSummary{}
	for _, item := range report.Accounts {
		byLabel[item.Label] = item
	}
	if got := byLabel["test"].Idle.Reason; got != IdleAccountStop {
		t.Fatalf("idle reason of the stopped account: %q", got)
	}
	if got := byLabel["second"].Idle.Reason; got != IdleClaimRaceLost {
		t.Fatalf("idle reason of the live account: %q", got)
	}
	// There is no shared line: the reasons differ, and one for all would hide the stop.
	if report.Idle.Reason != "" {
		t.Fatalf("shared idle reason despite different reasons: %+v", report.Idle)
	}

	// The live account keeps working: its neighbour's stop does not stop it.
	picked, err := service.ClaimNext(ctx, "second")
	if err != nil || picked == nil {
		t.Fatalf("claim by the live account after its neighbour's stop: %v, %+v", err, picked)
	}
}

// A shared reason stays one line: the send window, the campaign and its
// template are the same for all accounts, and repeating them for each would
// bury the one reason that differs.
func TestCommonIdleCollapses(t *testing.T) {
	window := Idle{Reason: IdleWindowClosed}
	both := []AccountSummary{{Label: "a", Idle: window}, {Label: "b", Idle: window}}
	if got := commonIdle(both).Reason; got != IdleWindowClosed {
		t.Fatalf("matching shared reason did not collapse: %q", got)
	}

	limits := []AccountSummary{
		{Label: "a", Idle: Idle{Reason: IdleDailyLimit, Used: 3, Limit: 3}},
		{Label: "b", Idle: Idle{Reason: IdleDailyLimit, Used: 7, Limit: 20}},
	}
	if got := commonIdle(limits).Reason; got != "" {
		t.Fatalf("per-account reason leaked into the shared line with foreign numbers: %q", got)
	}

	// With one account the line is shared, as before several senders existed.
	single := []AccountSummary{{Label: "a", Idle: Idle{Reason: IdleDailyLimit, Used: 3, Limit: 3}}}
	if got := commonIdle(single).Reason; got != IdleDailyLimit {
		t.Fatalf("the only account lost the shared line: %q", got)
	}
}
