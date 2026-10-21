package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// StatusReport is a snapshot of the service state: the only read tool returns
// it whole. There are no separate preview or per-row report tools, so
// everything the owner uses to check campaign progress is collected here.
type StatusReport struct {
	// Campaign is nil when there are no campaigns at all.
	Campaign *CampaignSummary
	Accounts []AccountSummary
	// Owners are the service recipients exempt from the send window, the daily
	// cap and the "one message per person" rule. The report always names them,
	// not only when they were written to today: a lifted guard nobody knows
	// about is the same as no guard, and a mistake in DEV_ALLOW_LIST would
	// otherwise show only as a message that already went out.
	Owners []string
	// Preview holds up to three messages rendered on real campaign rows: the
	// owner sees not the template but what a person will receive. A campaign
	// with a single recipient does not fail, the preview is just shorter.
	Preview []MessagePreview
	// Rows is filled only when full is set: the per-row campaign result.
	Rows []RecipientRow
	// Idle is why the queue stands still at the moment of the call. The campaign
	// stays active and the rows stay queued, and without this reason an idle
	// caused by the window, the cap or a broken template is visible only
	// indirectly, through the counters.
	Idle Idle
	// Draft is the prepared draft, if one exists next to the shown campaign.
	// Otherwise the owner would check the list against the wrong campaign.
	Draft *CampaignNote
	// Paused is a campaign on pause next to the shown one: start resumes it.
	Paused *CampaignNote
	// Held holds rows pinned to other accounts: nobody but the named sender
	// takes them. Without this counter such rows hang in planned with no
	// visible reason and the campaign silently fails to close.
	Held []HeldRows
	// StuckDirect counts manual touches stuck in pending longer than the claim
	// expiry. This is the worst outcome of the dialog tools: such a row closes
	// the person for good while delivery is unknown, and only a human can fix it.
	StuckDirect int
	// StuckClaims counts queue rows with an expired claim: a sender took them
	// and never closed them. While the campaign is active the queue clears such
	// a row; on a paused campaign that does not happen, and the person is
	// silently closed to manual sending.
	StuckClaims int
}

// CampaignNote is a neighbouring campaign in the report: how many rows it has.
type CampaignNote struct {
	ID   int64
	Rows int
}

// HeldRows is how many rows wait for the sender with this account label.
type HeldRows struct {
	Label string
	Rows  int
}

// CampaignSummary is a campaign and the counters of its rows by state.
type CampaignSummary struct {
	ID     int64
	Title  string
	Status string
	Total  int
	States map[string]int
}

// AccountSummary is an account with its daily usage and active stop. The stop
// is visible in every status reply: a guard nobody knows about is the same as
// no guard.
type AccountSummary struct {
	Label string
	// UsedToday is the account's whole daily usage, counted the way the guard
	// counts it: usedTodaySQL, one text for the report and for ClaimNext.
	// Campaign sends, rows in progress and manual cold touches are already
	// summed here.
	UsedToday int
	// DailyLimit is the cap from the database. ACCOUNT_DAILY_LIMIT sets it only
	// when the account row is created, so the report shows the effective number,
	// not the environment variable.
	DailyLimit int
	Stopped    bool
	StopReason string
	// ColdToday is a breakdown of UsedToday: how much of it is manual first
	// touches to strangers. A person needs to see what the cap was spent on, not
	// only how much is left.
	ColdToday int
	// WarmToday counts replies in ongoing dialogs today: they are outside the cap.
	WarmToday int
	// OwnerToday is how many messages went today to the owner's service
	// recipients, by campaign and by hand together. They are outside the cap, and
	// without this line the usage on mailing checks is not visible at all. It is
	// counted the way the cap itself is counted: a touch with unknown delivery
	// counts here just like a confirmed one, otherwise the same message would be
	// counted differently in neighbouring report lines.
	OwnerToday int
	// Idle is why this account is silent. It is computed for every account, not
	// only for the label of the MCP process: several senders run a campaign, and
	// the silence of one of them is the most likely case, invisible from outside
	// otherwise.
	Idle Idle
}

// MessagePreview is the ready message text for a specific recipient.
type MessagePreview struct {
	Username string
	Text     string
}

// RecipientRow is one line of the per-row result.
type RecipientRow struct {
	Username string
	Name     string
	State    string
	Reason   string
}

// Status builds a snapshot of the current campaign: the active one if there is
// one, otherwise the draft. full adds the per-row result; everything else is
// always returned: counters, accounts with stops and the preview.
func (s *Service) Status(ctx context.Context, full bool) (StatusReport, error) {
	var id int64
	err := s.pool.QueryRow(ctx, currentCampaignSQL).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		accounts, err := s.accountSummaries(ctx)
		if err != nil {
			return StatusReport{}, err
		}
		stuck, err := s.stuckDirect(ctx)
		if err != nil {
			return StatusReport{}, err
		}
		claims, err := s.stuckClaims(ctx)
		if err != nil {
			return StatusReport{}, err
		}
		return StatusReport{Accounts: accounts, Owners: s.ownerNames(),
			StuckDirect: stuck, StuckClaims: claims, Idle: commonIdle(accounts)}, nil
	}
	if err != nil {
		return StatusReport{}, fmt.Errorf("status: find campaign: %w", err)
	}
	return s.StatusOf(ctx, id, full)
}

// StatusOf builds a snapshot of the named campaign. Tools that have just
// changed a specific campaign report on exactly that one: with a live active
// campaign, a report on the "current" one would show foreign counters and a
// foreign preview.
func (s *Service) StatusOf(ctx context.Context, campaignID int64, full bool) (StatusReport, error) {
	var report StatusReport

	accounts, err := s.accountSummaries(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	report.Accounts = accounts
	report.Owners = s.ownerNames()

	report.Idle = commonIdle(accounts)

	if report.StuckDirect, err = s.stuckDirect(ctx); err != nil {
		return StatusReport{}, err
	}
	if report.StuckClaims, err = s.stuckClaims(ctx); err != nil {
		return StatusReport{}, err
	}

	summary := CampaignSummary{ID: campaignID}
	var template string
	err = s.pool.QueryRow(ctx, `SELECT title, status, template_text FROM campaigns WHERE id = $1`, campaignID).
		Scan(&summary.Title, &summary.Status, &template)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, nil
	}
	if err != nil {
		return StatusReport{}, fmt.Errorf("status: read campaign: %w", err)
	}

	summary.States = map[string]int{}
	rows, err := s.pool.Query(ctx,
		`SELECT state, count(*) FROM recipients WHERE campaign_id = $1 GROUP BY state`, summary.ID)
	if err != nil {
		return StatusReport{}, fmt.Errorf("status: count states: %w", err)
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return StatusReport{}, fmt.Errorf("status: scan state: %w", err)
		}
		summary.States[state] = count
		summary.Total += count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StatusReport{}, fmt.Errorf("status: read states: %w", err)
	}
	report.Campaign = &summary
	// The braces of the shown campaign, not the active one: the report line is
	// about "this campaign", and the braces of another template would point the
	// person at the wrong text.
	report.Idle.Braces, _ = badBraces(template)

	preview, err := s.previewMessages(ctx, summary.ID, template)
	if err != nil {
		return StatusReport{}, err
	}
	report.Preview = preview

	if full {
		if report.Rows, err = s.recipientRows(ctx, summary.ID); err != nil {
			return StatusReport{}, err
		}
	}

	if report.Held, err = s.heldRows(ctx, summary.ID); err != nil {
		return StatusReport{}, err
	}

	if summary.Status != "draft" {
		if report.Draft, err = s.campaignNote(ctx, "draft"); err != nil {
			return StatusReport{}, err
		}
	}
	if summary.Status != "stopped" {
		if report.Paused, err = s.campaignNote(ctx, "stopped"); err != nil {
			return StatusReport{}, err
		}
	}
	return report, nil
}

// sharedIdle reports reasons that are the same for all accounts: the send
// window, the presence of a campaign and its template are shared, and repeating
// them in every account line would bury the one reason that differs.
func sharedIdle(reason string) bool {
	switch reason {
	case IdleWindowClosed, IdleNoCampaign, IdleBrokenTemplate, IdleQueueEmpty:
		return true
	}
	return false
}

// commonIdle is the idle reason the report names in one line for the whole
// queue. With one account it is that account's reason, as before several
// senders existed. With several accounts it is only a shared reason, and only
// when it matches for all: cap usage and stop numbers are per account, and one
// line on top would promise someone else's headroom.
func commonIdle(accounts []AccountSummary) Idle {
	if len(accounts) == 0 {
		return Idle{}
	}
	first := accounts[0].Idle
	if len(accounts) == 1 {
		return first
	}
	if !sharedIdle(first.Reason) {
		return Idle{}
	}
	for _, item := range accounts[1:] {
		if item.Idle.Reason != first.Reason {
			return Idle{}
		}
	}
	return first
}

// campaignNote returns the neighbouring campaign in the named status, if there
// is one: a report on a running campaign must name the prepared draft and the
// left-over pause on a separate line, otherwise they get forgotten.
func (s *Service) campaignNote(ctx context.Context, status string) (*CampaignNote, error) {
	var note CampaignNote
	err := s.pool.QueryRow(ctx,
		`SELECT c.id, (SELECT count(*) FROM recipients r WHERE r.campaign_id = c.id)
		 FROM campaigns c WHERE c.status = $1 ORDER BY c.id LIMIT 1`, status).
		Scan(&note.ID, &note.Rows)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("status: read %s campaign: %w", status, err)
	}
	return &note, nil
}

// heldRows counts queue rows pinned to other accounts. A row belongs for good
// to the account that took it, so a dead or foreign sender keeps it planned and
// the campaign does not close. The operator must see which sender label the
// queue is waiting for.
func (s *Service) heldRows(ctx context.Context, campaignID int64) ([]HeldRows, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.label, count(*) FROM recipients r
		 JOIN accounts a ON a.id = r.account_id
		 WHERE r.campaign_id = $1 AND r.state = 'planned' AND a.label <> $2
		 GROUP BY a.label ORDER BY a.label`,
		campaignID, s.cfg.AccountLabel)
	if err != nil {
		return nil, fmt.Errorf("status: count held rows: %w", err)
	}
	defer rows.Close()

	var list []HeldRows
	for rows.Next() {
		var item HeldRows
		if err := rows.Scan(&item.Label, &item.Rows); err != nil {
			return nil, fmt.Errorf("status: scan held rows: %w", err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read held rows: %w", err)
	}
	return list, nil
}

// currentCampaignSQL selects the campaign being asked about: active beats
// draft, draft beats paused, and only then come the closed ones by recency.
// Among the closed ones recency is finished_at, not id: done and cancelled
// close in any order relative to creation, and the id of the younger campaign
// says nothing about which one closed later. NULLS LAST and id DESC remain as
// a safety net for an empty finished_at. Tools do not pass an id, so the order
// is fixed here instead of being guessed by the caller.
const currentCampaignSQL = `SELECT id FROM campaigns
	ORDER BY CASE status
		WHEN 'active' THEN 0
		WHEN 'draft' THEN 1
		WHEN 'stopped' THEN 2
		ELSE 3
	END, finished_at DESC NULLS LAST, id DESC
	LIMIT 1`

// accountsReportSQL builds the account row of the report: usage counted the way
// the cap guard counts it (otherwise status would show headroom where ClaimNext
// already refuses) and its breakdown. Service recipients are a separate number
// and fall into neither usage nor warm replies: one message must not be
// counted twice. $1 is the start of the day, $2 is the claim expiry boundary.
func accountsReportSQL(owners string) string {
	return `SELECT a.label, a.daily_limit, a.stopped, a.stop_reason,
		` + usedTodaySQL(`a.id`, owners) + `,
		` + coldDirectSQL(`a.id`, owners) + `,
		(SELECT count(*) FROM direct_messages d
		 WHERE d.account_id = a.id AND d.username <> ALL(` + owners + `)
		   AND NOT d.cold AND d.state <> 'failed' AND d.created_at >= $1),
		(SELECT count(*) FROM recipients r
		 WHERE r.account_id = a.id AND r.username = ANY(` + owners + `) AND r.sent_at >= $1)
		+ (SELECT count(*) FROM direct_messages d
		   WHERE d.account_id = a.id AND d.username = ANY(` + owners + `)
		     AND d.state <> 'failed' AND d.created_at >= $1)
	 FROM accounts a ORDER BY a.label`
}

func (s *Service) accountSummaries(ctx context.Context) ([]AccountSummary, error) {
	now := s.Now()
	rows, err := s.pool.Query(ctx, s.accountsReportSQL, startOfDay(now), s.staleBefore(now))
	if err != nil {
		return nil, fmt.Errorf("status: read accounts: %w", err)
	}
	defer rows.Close()

	var list []AccountSummary
	for rows.Next() {
		var item AccountSummary
		if err := rows.Scan(&item.Label, &item.DailyLimit, &item.Stopped, &item.StopReason,
			&item.UsedToday, &item.ColdToday, &item.WarmToday, &item.OwnerToday); err != nil {
			return nil, fmt.Errorf("status: scan account: %w", err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read accounts: %w", err)
	}
	// The idle reason is computed per account, not by the label of the MCP
	// process: there are several accounts, and the question "why is the second
	// one silent" otherwise has no answer from outside. It uses the same
	// IdleReason that writes the reason to the sender's own log: a second
	// calculation next to it would drift from the first. A failure for one
	// account does not sink the report: that line stays without a reason.
	for i := range list {
		idle, err := s.IdleReason(ctx, list[i].Label)
		if err != nil {
			slog.Warn("idle reason unavailable", "account", list[i].Label, "error", err)
			continue
		}
		list[i].Idle = idle
	}
	return list, nil
}

// stuckDirect counts manual touches stuck in pending longer than the claim
// expiry. For a queue row an expired claim gives a second chance; the journal
// has no such path: pending closes the person for good, so the counter is in
// every status reply.
func (s *Service) stuckDirect(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM direct_messages WHERE state = 'pending' AND created_at < $1`,
		s.staleBefore(s.Now())).Scan(&count); err != nil {
		return 0, fmt.Errorf("status: count stuck direct messages: %w", err)
	}
	return count, nil
}

// stuckClaims counts queue rows that a sender took and never closed. An active
// campaign sorts them out itself on the next loop pass, a paused one does not:
// such a row keeps the person closed to manual sending too, so the counter is
// in every status reply.
func (s *Service) stuckClaims(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM recipients
		 WHERE state = 'planned' AND claimed_at IS NOT NULL AND claimed_at < $1`,
		s.staleBefore(s.Now())).Scan(&count); err != nil {
		return 0, fmt.Errorf("status: count stuck claims: %w", err)
	}
	return count, nil
}

// LogStuckDirect names the stuck touches at process start. A read error does
// not stop the process by itself: this is observability, not a guard, but it
// must not stay silent, otherwise an unread journal looks like an empty one.
func (s *Service) LogStuckDirect(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, username, created_at FROM direct_messages
		 WHERE state = 'pending' AND created_at < $1 ORDER BY id`,
		s.staleBefore(s.Now()))
	if err != nil {
		slog.Error("stuck direct messages unavailable", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var username string
		var created time.Time
		if err := rows.Scan(&id, &username, &created); err != nil {
			slog.Error("stuck direct messages unavailable", "error", err)
			return
		}
		slog.Error("direct message stuck pending",
			"direct_id", id, "username", username, "created_at", created)
	}
	if err := rows.Err(); err != nil {
		slog.Error("stuck direct messages unavailable", "error", err)
	}
}

func (s *Service) previewMessages(ctx context.Context, campaignID int64, template string) ([]MessagePreview, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT username, name FROM recipients WHERE campaign_id = $1 ORDER BY id LIMIT 3`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("status: read preview: %w", err)
	}
	defer rows.Close()

	var list []MessagePreview
	for rows.Next() {
		var username, name string
		if err := rows.Scan(&username, &name); err != nil {
			return nil, fmt.Errorf("status: scan preview: %w", err)
		}
		list = append(list, MessagePreview{Username: username, Text: RenderMessage(template, name)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read preview: %w", err)
	}
	return list, nil
}

func (s *Service) recipientRows(ctx context.Context, campaignID int64) ([]RecipientRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT username, name, state, reason FROM recipients WHERE campaign_id = $1 ORDER BY id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("status: read recipients: %w", err)
	}
	defer rows.Close()

	var list []RecipientRow
	for rows.Next() {
		var item RecipientRow
		if err := rows.Scan(&item.Username, &item.Name, &item.State, &item.Reason); err != nil {
			return nil, fmt.Errorf("status: scan recipient: %w", err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read recipients: %w", err)
	}
	return list, nil
}
