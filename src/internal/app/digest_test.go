package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// digestEvening is a weekday evening after the window closes: the time at
// which the digest goes out.
var digestEvening = time.Date(2026, 9, 2, 19, 30, 0, 0, moscowZone)

// digestSenderOn builds a sender with the digest on a stub dialog and sets the
// time to the evening: the digest depends on time, not on process state.
func digestSenderOn(t *testing.T, service *Service, dialog *stubDialog) *Sender {
	t.Helper()
	service.cfg.Transport = "live"
	service.Now = func() time.Time { return digestEvening }
	sender := senderOn(service, dialog)
	sender.UseChat(chatOn(service, dialog))
	return sender
}

// sentToday marks some campaign rows as sent today: the digest counts the day
// by sent_at, not by queue state.
func sentToday(t *testing.T, service *Service, campaignID int64, sent, replied int) {
	t.Helper()
	if _, err := service.pool.Exec(context.Background(),
		`UPDATE recipients SET state = 'sent', sent_at = $2,
		        account_id = (SELECT id FROM accounts WHERE label = 'test')
		 WHERE id IN (SELECT id FROM recipients WHERE campaign_id = $1 ORDER BY id LIMIT $3)`,
		campaignID, digestEvening, sent+replied); err != nil {
		t.Fatalf("mark sent rows: %v", err)
	}
	if replied == 0 {
		return
	}
	if _, err := service.pool.Exec(context.Background(),
		`UPDATE recipients SET state = 'replied', replied_at = $2
		 WHERE id IN (SELECT id FROM recipients WHERE campaign_id = $1 AND state = 'sent' ORDER BY id LIMIT $3)`,
		campaignID, digestEvening, replied); err != nil {
		t.Fatalf("mark replied rows: %v", err)
	}
}

// Digest time: a weekday evening after 19:00. There is no digest before 19:00
// or on a weekend, otherwise the owner would get a report on an incomplete day.
func TestDigestDue(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"before the window", time.Date(2026, 9, 2, 18, 59, 0, 0, moscowZone), false},
		{"exactly at 19", time.Date(2026, 9, 2, 19, 0, 0, 0, moscowZone), true},
		{"late evening", time.Date(2026, 9, 2, 22, 40, 0, 0, moscowZone), true},
		{"saturday", time.Date(2026, 9, 5, 19, 30, 0, 0, moscowZone), false},
	}
	for _, c := range cases {
		if got := digestDue(c.when); got != c.want {
			t.Errorf("%s: digestDue = %v, want %v", c.name, got, c.want)
		}
	}
}

// The digest goes to each service recipient once a day: there are several
// senders, and without a lock in the database each would send its own.
func TestDigestSentOncePerDay(t *testing.T) {
	service := newService(t, "owner_one", "owner_two")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	sentToday(t, service, campaignID, 1, 1)

	for range 3 {
		if err := sender.maybeSendDigest(ctx); err != nil {
			t.Fatalf("digest: %v", err)
		}
	}

	if got := len(dialog.Sent()); got != 2 {
		t.Fatalf("%d digests sent, want one per recipient", got)
	}
	var rows int
	if err := service.pool.QueryRow(ctx,
		`SELECT count(*) FROM daily_digest WHERE sent_at IS NOT NULL`).Scan(&rows); err != nil {
		t.Fatalf("digest journal: %v", err)
	}
	if rows != 2 {
		t.Fatalf("journal has %d rows, want 2", rows)
	}
}

// Only the named account sends the digest: with two senders it would otherwise
// arrive now from one chat, now from another, and read as a service failure.
func TestDigestSentOnlyByDigestAccount(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.DigestAccount = "other"
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest of another account: %v", err)
	}
	if got := len(dialog.Sent()); got != 0 {
		t.Fatalf("%d digests sent, want none: the sender is another account", got)
	}

	service.cfg.DigestAccount = "test"
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest of own account: %v", err)
	}
	if got := len(dialog.Sent()); got != 1 {
		t.Fatalf("%d digests sent, want one", got)
	}
}

// Digest recipients are their own list, not all service recipients:
// DEV_ALLOW_LIST lifts the guards for mailing checks and is wider than the
// circle of report readers.
func TestDigestGoesToDigestTo(t *testing.T) {
	service := newService(t, "owner_one", "owner_two", "owner_three")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.DigestTo = []string{"owner_one", "owner_two"}
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest: %v", err)
	}
	got := make(map[string]bool)
	for _, item := range dialog.Sent() {
		got[item.Username] = true
	}
	if len(got) != 2 || !got["owner_one"] || !got["owner_two"] {
		t.Fatalf("digest went to %v, want owner_one and owner_two", got)
	}
}

// A sender label without a live process would stop the digest for good and
// without a word. The process that does not send it notices this and says so in
// the log - the only trace of such a breakage. Within the one-hour slack it
// stays silent: that is when the sender is sending the digest.
func TestDigestMissingWarnedByOtherSender(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.DigestAccount = "other"
	log := captureLogs(t)
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("evening before the slack: %v", err)
	}
	if strings.Contains(log.String(), "digest not sent today") {
		t.Fatalf("warning earlier than %d:00 Moscow time:\n%s", digestWarnHour, log)
	}

	service.Now = func() time.Time { return digestEvening.Add(2 * time.Hour) }
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("late evening: %v", err)
	}
	if !strings.Contains(log.String(), "digest not sent today") {
		t.Fatalf("an evening without a digest passed silently:\n%s", log)
	}
	if len(dialog.Sent()) != 0 {
		t.Fatal("a process with another label sent the digest")
	}

	// The digest went out - the warning is gone: the signal must clear itself,
	// otherwise people stop reading it.
	service.cfg.DigestAccount = "test"
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest of own account: %v", err)
	}
	service.cfg.DigestAccount = "other"
	log.Reset()
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("evening after the digest: %v", err)
	}
	if strings.Contains(log.String(), "digest not sent today") {
		t.Fatalf("warning about a digest that was sent:\n%s", log)
	}
}

// The digest text carries the day, the per-account breakdown and the campaign
// total: the owner needs numbers, not the fact of sending. The text is Russian
// product output, so the expected fragments are Russian.
func TestDigestTextCounts(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()

	campaignID := startWith(t, service,
		person(1, "ivan_petrov"), person(2, "petr_ivanov"), person(3, "anna_sidorova"))
	sentToday(t, service, campaignID, 1, 1)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest: %v", err)
	}
	sent := dialog.Sent()
	if len(sent) != 1 {
		t.Fatalf("%d digests sent, want one", len(sent))
	}
	text := sent[0].Text
	for _, want := range []string{"2 сентября", "Сегодня ушло 2, ответили 1", "test - ушло 2, ответили 1", "Всего ушло 2 из 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
}

// A failure for one recipient does not deprive the others of the digest and
// does not cause a second send to the one who already received it.
func TestDigestRetriesOnlyFailed(t *testing.T) {
	service := newService(t, "owner_one", "owner_two")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	dialog.fail = func(username string) error {
		if username == "owner_two" {
			return errors.New("telegram unavailable")
		}
		return nil
	}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err == nil {
		t.Fatal("send failure not reported as an error")
	}
	// The retry comes on the next pass, not at once: after a failure Chat
	// quarantines the recipient - a message in an unknown state must not be
	// duplicated. The loop pause of 10-30 minutes outlasts this quarantine.
	dialog.fail = nil
	later := digestEvening.Add(10 * time.Minute)
	service.Now = func() time.Time { return later }
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest retry: %v", err)
	}

	got := map[string]int{}
	for _, d := range dialog.Sent() {
		got[d.Username]++
	}
	if got["owner_one"] != 1 || got["owner_two"] != 1 {
		t.Fatalf("digests went %v, want one each", got)
	}
}

// In dry-run the digest does not go out at all: the message would not be sent,
// but the lock would eat the day, and in the live setup the digest would
// already count as sent.
func TestDigestSilentInDryRun(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.Transport = "dry-run"
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got := len(dialog.Sent()); got != 0 {
		t.Fatalf("%d digests sent in dry-run", got)
	}
}

// setRecipient sets the state, reason and account of a campaign row: the funnel
// is counted from them, and the test need not bring every row to its state the
// honest way. An empty label leaves the row without an account.
func setRecipient(t *testing.T, service *Service, campaignID, contactID int64, state, reason, label string) {
	t.Helper()
	tag, err := service.pool.Exec(context.Background(),
		`UPDATE recipients SET state = $3, reason = $4, sent_at = CASE WHEN $3 IN ('sent', 'replied') THEN $6::timestamptz END,
		        account_id = (SELECT id FROM accounts WHERE label = $5)
		 WHERE campaign_id = $1 AND amo_contact_id = $2`,
		campaignID, contactID, state, reason, label, digestEvening)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("row %d of campaign %d: %v, rows %d", contactID, campaignID, err, tag.RowsAffected())
	}
}

// The funnel in three breakdowns matches the row states: a bad username counts
// as sent, a service failure does not; rows removed by a guard, cancelled rows,
// the service recipient and the draft are not counted, a campaign cancelled
// before the start is not listed as a mailing; an account without rows shows
// zeros without shares.
func TestDigestFunnel(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()
	for _, label := range []string{"second", "idle"} {
		if _, err := service.pool.Exec(ctx,
			`INSERT INTO accounts (label, daily_limit) VALUES ($1, 30)`, label); err != nil {
			t.Fatalf("account %s: %v", label, err)
		}
	}

	past := startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"), person(3, "owner_one"))
	setRecipient(t, service, past, 1, "sent", "", "test")
	setRecipient(t, service, past, 2, "replied", "", "test")
	setRecipient(t, service, past, 3, "sent", "", "test")
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET status = 'done', finished_at = $2 WHERE id = $1`, past, digestEvening); err != nil {
		t.Fatalf("close the past campaign: %v", err)
	}

	current := startWith(t, service,
		person(11, "anna_sidorova"), person(12, "oleg_smirnov"), person(13, "gone_user"),
		person(14, "broken_send"), person(15, "back_in_queue"), person(16, "seen_before"),
		person(17, "was_cancelled"), person(18, "not_claimed"), person(19, "maria_orlova"),
		person(20, "owner_one"), person(21, "group_nick"))
	setRecipient(t, service, current, 11, "sent", "", "test")
	setRecipient(t, service, current, 12, "replied", "", "test")
	setRecipient(t, service, current, 13, "undelivered", "telegram_peer_forbidden", "test")
	setRecipient(t, service, current, 14, "undelivered", ReasonRetryExhausted, "second")
	setRecipient(t, service, current, 15, "planned", "", "second")
	setRecipient(t, service, current, 16, "skipped", ReasonAlreadySent, "second")
	setRecipient(t, service, current, 17, "cancelled", ReasonCampaignGone, "second")
	setRecipient(t, service, current, 19, "sent", "", "second")
	setRecipient(t, service, current, 20, "replied", "", "test")
	setRecipient(t, service, current, 21, "undelivered", "telegram_not_person", "test")

	var cancelled int64
	if err := service.pool.QueryRow(ctx,
		`INSERT INTO campaigns (title, template_text, status, finished_at)
		 VALUES ('cancelled-campaign', 'Привет!', 'cancelled', $1) RETURNING id`, digestEvening).Scan(&cancelled); err != nil {
		t.Fatalf("cancelled campaign: %v", err)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, username, state, random_id)
		 VALUES ($1, 40, 'never_started', 'cancelled', 40)`, cancelled); err != nil {
		t.Fatalf("row of the cancelled campaign: %v", err)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "draft")
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(30, "future_lead")}); err != nil {
		t.Fatalf("draft recipients: %v", err)
	}

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("digest: %v", err)
	}
	sent := dialog.Sent()
	if len(sent) != 1 {
		t.Fatalf("%d digests sent, want one", len(sent))
	}
	text := sent[0].Text
	for _, want := range []string{
		"Аккаунты в порядке, стопов нет.\n\nИтог за кампанию",
		"```\n           взято отпр. дост.  отв.\n",
		"idle           0     0     0     0\nsecond",
		"second         3     1     1     0\n                   33%  100%    0%\n",
		"test           4     4     2     1\n                  100%   50%   50%\n",
		"Кампания       8     5     3     1\n                   63%   60%   33%\n",
		"Все (2)       10     7     5     2\n                   70%   71%   40%\n```",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
}

// A draft does not trigger a digest: a lone draft means no digest, a closed
// campaign with a draft loaded after it means a digest on the closed one's
// result.
func TestDigestCampaignSkipsDraft(t *testing.T) {
	service := newService(t, "owner_one")
	ctx := context.Background()

	if _, err := service.LoadCampaign(ctx, "Привет, {имя}!", "first"); err != nil {
		t.Fatalf("first draft: %v", err)
	}
	if id, err := service.digestCampaign(ctx); err != nil || id != 0 {
		t.Fatalf("only a draft exists: id %d, error %v", id, err)
	}
	done := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET status = 'done', finished_at = $2 WHERE id = $1`, done, digestEvening); err != nil {
		t.Fatalf("close the campaign: %v", err)
	}
	if _, err := service.LoadCampaign(ctx, "Привет, {имя}!", "next"); err != nil {
		t.Fatalf("draft: %v", err)
	}

	// Here the draft also loses on the finished_at order: this check holds the
	// ticket scenario, while the draft condition is caught by the check above.
	id, err := service.digestCampaign(ctx)
	if err != nil {
		t.Fatalf("campaign choice: %v", err)
	}
	if id != done {
		t.Fatalf("digest about campaign %d, want the closed one %d", id, done)
	}
}
