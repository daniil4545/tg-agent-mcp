package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Service holds the domain operations of the campaign and the queue on top of
// the pool. There is no in-memory state at all: counters, pauses and stops
// survive a restart only in the database.
type Service struct {
	pool *pgxpool.Pool
	cfg  Config

	// owners is the set of service recipients: the single source of the isOwner
	// answer.
	owners map[string]bool

	// Queries with guards are built once here, because the list of service
	// recipients enters them as an SQL literal, not as a parameter. An array
	// parameter does not work: pgx encodes a nil slice as NULL, and
	// `x <> ALL(NULL)` would give NULL - the cap would count as zero and the
	// filtering would leave rows hanging in planned. The literal is safe:
	// LoadConfig has checked the username shape.
	accountUsedSQL      string
	claimSQL            string
	idleQueueSQL        string
	skipDisqualifiedSQL string
	accountsReportSQL   string

	// Now is the time source. A field rather than time.Now in place: otherwise
	// the send window and claim expiry cannot be tested.
	Now func() time.Time
}

// NewService builds a service on an open pool.
func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	owners := ownersLiteral(cfg.OwnerAccounts)
	s := &Service{
		pool:                pool,
		cfg:                 cfg,
		owners:              make(map[string]bool, len(cfg.OwnerAccounts)),
		accountUsedSQL:      `SELECT ` + usedTodaySQL(`$3`, owners),
		claimSQL:            claimSQL(owners),
		idleQueueSQL:        idleQueueSQL(owners),
		skipDisqualifiedSQL: skipDisqualifiedSQL(owners),
		accountsReportSQL:   accountsReportSQL(owners),
		Now:                 time.Now,
	}
	// The same selection as in ownersLiteral: if they drifted apart, Go and SQL
	// would lift the guards for different people.
	for _, name := range cfg.OwnerAccounts {
		if telegram.IsUsername(name) {
			s.owners[name] = true
		}
	}
	return s
}

// isOwner reports whether this is a service recipient: the single check point
// in Go. The username is normalized the same way as on list import, otherwise
// "@Smirnov" and "smirnov" would diverge at the guards.
func (s *Service) isOwner(username string) bool {
	return s.owners[normalizeName(username)]
}

// ownerNames returns the service recipients for the report, in a stable order.
// The report names those the guards really do not hold, so it takes them from
// the same set as isOwner, not from the settings string.
func (s *Service) ownerNames() []string {
	return slices.Sorted(maps.Keys(s.owners))
}

// DigestRecipients returns who gets the evening digest: DIGEST_TO if set,
// otherwise all service recipients, as before the lists were split. The lists
// differ because DEV_ALLOW_LIST lifts the guards for mailing checks and cannot
// be narrowed down to the two report readers.
func (s *Service) DigestRecipients() []string {
	if len(s.cfg.DigestTo) > 0 {
		return s.cfg.DigestTo
	}
	return s.ownerNames()
}

// ownersLiteral renders the list of service recipients as an SQL literal for
// building queries; an empty list gives ARRAY[]::text[], on which `<> ALL` is
// true and `= ANY` is false, so the service behaves exactly as before.
//
// The username shape is checked again here after LoadConfig: the lock sits
// where the string is glued into the query, not two hundred lines away.
// NewService accepts a Config built by anyone, and a quote in a username would
// rewrite the WHERE clause.
func ownersLiteral(list []string) string {
	quoted := make([]string, 0, len(list))
	for _, name := range list {
		if !telegram.IsUsername(name) {
			continue
		}
		quoted = append(quoted, `'`+name+`'`)
	}
	if len(quoted) == 0 {
		return `ARRAY[]::text[]`
	}
	return `ARRAY[` + strings.Join(quoted, ",") + `]::text[]`
}

// maxBatch is the cap on an add_recipients batch. The reason is not the
// database but the client: a large tool argument is silently truncated on the
// model side.
const maxBatch = 50

// Reasons for filtering out or removing a row. The list is closed: a new entry
// here is a new service decision, not new text in the report.
const (
	ReasonBadContact   = "bad_contact"    // no contact id
	ReasonNoUsername   = "no_username"    // username is empty or does not look like one
	ReasonNoName       = "no_name"        // no longer set, found only in older campaigns
	ReasonTextTooLong  = "text_too_long"  // the message with his name exceeds the Telegram limit
	ReasonNameBraces   = "name_braces"    // the name has a curly brace: it would go into the message
	ReasonDuplicate    = "duplicate"      // contact or username is already in this campaign
	ReasonStopList     = "stop_list"      // the person is on the stop list
	ReasonAlreadySent  = "already_sent"   // received a message in any campaign
	ReasonCampaignGone = "campaign_ended" // the campaign was cancelled together with its queue

	// A manual touch through tg_send closes the person to campaigns just like a
	// sent row. There are two reasons because the journal does not always know
	// about delivery: pending means "the message may have gone out", and the
	// filtering report must not present that as a completed send.
	ReasonDirectSent    = "direct_sent"    // written by hand, delivery confirmed
	ReasonDirectPending = "direct_pending" // written by hand, delivery unknown

	// The person wrote first, before the queue reached them: the template is not
	// sent over a conversation they started, but this does not count as a reply
	// to our message.
	ReasonWroteFirst = "wrote_first"
)

// Refusals by campaign state. There are several, not one: "no suitable
// campaign" sounded the same where there is nothing to create and where a
// campaign is alive, and the advice "create a draft" led away from what is
// actually going on.
var (
	ErrBatchTooBig     = errors.New("batch is limited to 50 people")
	ErrNoDraft         = errors.New("no draft campaign to add recipients to")
	ErrNothingToStart  = errors.New("no draft and no paused campaign to start")
	ErrNothingToStop   = errors.New("no active campaign to pause")
	ErrNothingToCancel = errors.New("no campaign to cancel")
	ErrActiveCampaign  = errors.New("another campaign is already running")
	ErrManyCampaigns   = errors.New("more than one campaign to start, cancel the extra ones")
	ErrEmptyDraft      = errors.New("draft has no eligible recipients")
	ErrNotDraft        = errors.New("recipients can be added to a draft campaign only")

	ErrUnknownPlaceholder = errors.New("template has placeholders the service does not know")
	ErrTemplateTooLong    = errors.New("rendered message does not fit the telegram limit")
)

// badTemplate is a template with unparsed braces. Pieces of text around them
// travel inside the refusal: without them a person hunts for the typo across
// the whole template. total is how many spots were found in all: not all are
// shown. stored means the template is already in the database: the advice then
// differs, because no tool edits the template of an existing campaign.
type badTemplate struct {
	spots  []string
	total  int
	stored bool
}

func (e *badTemplate) Error() string {
	return fmt.Sprintf("%s: %d spots, %s", ErrUnknownPlaceholder, e.total, strings.Join(e.spots, " | "))
}

func (e *badTemplate) Unwrap() error { return ErrUnknownPlaceholder }

// longTemplate is a template that does not fit into a message together with
// the name. The numbers travel inside: without them the advice "shorten it"
// does not say by how much.
type longTemplate struct{ used, limit, budget int }

func (e *longTemplate) Error() string {
	return fmt.Sprintf("%s: %d of %d", ErrTemplateTooLong, e.used, e.limit)
}

func (e *longTemplate) Unwrap() error { return ErrTemplateTooLong }

// nearCampaign is a live campaign next to a refusal. It travels inside the
// error because the next step depends on it: "no draft" on an empty system and
// "no draft" with a running campaign are resolved differently.
type nearCampaign struct {
	err    error
	id     int64
	title  string
	status string
	// draft is the draft next to the found campaign; 0 if there is none. The
	// advice needs it: start takes the draft before a pause, and cancel hits it
	// before a pause, so "continue with start" with a live draft sends the agent
	// the wrong way.
	draft int64
	// pauses is how many campaigns are on pause. The advice needs it too: with
	// two pauses and no draft, start refuses with ErrManyCampaigns, and the
	// promise "start will continue it" would be a lie.
	pauses int
}

func (e *nearCampaign) Error() string {
	return fmt.Sprintf("%s: campaign %d %q (%s)", e.err, e.id, e.title, e.status)
}

func (e *nearCampaign) Unwrap() error { return e.err }

// rowQuerier is the common part of the pool and a transaction: the neighbouring
// campaign is looked up both outside a transaction (add_recipients) and inside
// one (stop).
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withNear attaches to a refusal the first campaign in one of the named
// statuses; the order of statuses sets the answer priority. If there is no such
// campaign, the refusal goes out as is.
func withNear(ctx context.Context, q rowQuerier, err error, statuses ...string) error {
	near := nearCampaign{err: err}
	scanErr := q.QueryRow(ctx,
		`SELECT c.id, c.title, c.status,
		        COALESCE((SELECT d.id FROM campaigns d
		                  WHERE d.status = 'draft' AND d.id <> c.id ORDER BY d.id LIMIT 1), 0),
		        (SELECT count(*) FROM campaigns p WHERE p.status = 'stopped')
		 FROM campaigns c WHERE c.status = ANY($1)
		 ORDER BY array_position($1, c.status), c.id LIMIT 1`,
		statuses).Scan(&near.id, &near.title, &near.status, &near.draft, &near.pauses)
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows):
		return err
	case scanErr != nil:
		return fmt.Errorf("find campaign near %w: %v", err, scanErr)
	default:
		return &near
	}
}

// Person is a row of the input list. The agent builds the list through its own
// access to a CRM: the service itself does not call the CRM.
type Person struct {
	ContactID int64
	LeadID    int64
	Name      string
	Username  string
}

// BatchReport is the result of a batch: how many were accepted and how many
// filtered out for each reason. Filtering is not an error: the owner checks
// the report before the start, and a retry of a batch after a timeout must go
// through without failing.
type BatchReport struct {
	Accepted int
	Skipped  map[string]int
}

// LoadCampaign creates a new draft, cancelling the previous one in the same
// transaction. A race of two calls is resolved by a partial unique index on
// draft: the second call fails on the index instead of leaving two drafts.
func (s *Service) LoadCampaign(ctx context.Context, template, title string) (int64, error) {
	template = strings.TrimSpace(template)
	title = strings.TrimSpace(title)
	if template == "" || title == "" {
		return 0, errors.New("campaign title and template must not be empty")
	}
	// The template is checked on input, not at send time: a campaign is built
	// once and goes out to hundreds, and a typo in a placeholder or an extra
	// thousand characters must be rejected before the list is assembled.
	if spots, total := badBraces(template); total > 0 {
		return 0, &badTemplate{spots: spots, total: total}
	}
	if used, budget := renderedLen(template); used > maxTextLen {
		return 0, &longTemplate{used: used, limit: maxTextLen, budget: budget}
	}

	now := s.Now()
	var id int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		previous, err := findCampaign(ctx, tx, "draft")
		if err != nil {
			return err
		}
		if previous != 0 {
			if err := cancelCampaign(ctx, tx, previous, now, s.staleBefore(now)); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx,
			`INSERT INTO campaigns (title, template_text, status, created_at)
			 VALUES ($1, $2, 'draft', $3) RETURNING id`,
			title, template, now).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("load campaign: %w", err)
	}
	return id, nil
}

// AddRecipients adds a batch to the draft and returns the filtering report.
// Filtered out are: a bad contact, an unusable username, a duplicate within the
// batch and against those already inserted, the stop list, "received in any
// campaign". A retry of the same batch after a timeout is idempotent - all
// entries land in duplicates.
func (s *Service) AddRecipients(ctx context.Context, campaignID int64, people []Person) (BatchReport, error) {
	report := BatchReport{Skipped: map[string]int{}}
	if len(people) > maxBatch {
		return report, fmt.Errorf("%w: got %d", ErrBatchTooBig, len(people))
	}

	// Filtering by shape comes before the transaction: the database takes no part.
	type candidate struct {
		person   Person
		username string
		name     string
	}
	candidates := make([]candidate, 0, len(people))
	seenContacts := make(map[int64]bool, len(people))
	seenNames := make(map[string]bool, len(people))
	for _, person := range people {
		if person.ContactID <= 0 {
			report.Skipped[ReasonBadContact]++
			continue
		}
		username := normalizeUsername(person.Username)
		if username == "" {
			report.Skipped[ReasonNoUsername]++
			continue
		}
		if seenContacts[person.ContactID] || seenNames[username] {
			report.Skipped[ReasonDuplicate]++
			continue
		}
		seenContacts[person.ContactID] = true
		seenNames[username] = true
		candidates = append(candidates, candidate{person: person, username: username, name: cleanName(person.Name)})
	}

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status, template string
		err := tx.QueryRow(ctx,
			`SELECT status, template_text FROM campaigns WHERE id = $1 FOR UPDATE`,
			campaignID).Scan(&status, &template)
		if err != nil {
			return fmt.Errorf("lock campaign: %w", err)
		}
		if status != "draft" {
			return fmt.Errorf("%w: campaign is %s", ErrNotDraft, status)
		}
		// A broken template is rejected here too: otherwise five hundred people
		// could be stuffed into an unusable draft and the problem found only at
		// start.
		if spots, total := badBraces(template); total > 0 {
			return &badTemplate{spots: spots, total: total, stored: true}
		}

		for _, item := range candidates {
			var inStopList, alreadySent, present, directSent, directPending bool
			err := tx.QueryRow(ctx, checkPersonSQL, item.person.ContactID, item.username, campaignID).
				Scan(&inStopList, &alreadySent, &present, &directSent, &directPending)
			if err != nil {
				return fmt.Errorf("check person %d: %w", item.person.ContactID, err)
			}
			// The owner's service recipient is written to repeatedly on purpose: the
			// "one person, one message" rule exists for strangers. The stop list
			// and a duplicate within the campaign still apply to them.
			if s.isOwner(item.username) {
				alreadySent, directSent, directPending = false, false, false
			}
			switch {
			case present:
				report.Skipped[ReasonDuplicate]++
				continue
			case inStopList:
				report.Skipped[ReasonStopList]++
				continue
			case alreadySent:
				report.Skipped[ReasonAlreadySent]++
				continue
			case directSent:
				report.Skipped[ReasonDirectSent]++
				continue
			case directPending:
				report.Skipped[ReasonDirectPending]++
				continue
			}

			// Name guards come after the general ones: a person on the stop list must
			// be reported as stop list (invariant 4), not as a missing name -
			// otherwise the strongest guard is not visible in the import report at
			// all.
			if fault := nameFault(template, item.name); fault != "" {
				report.Skipped[fault]++
				continue
			}

			// ON CONFLICT covers a batch retry that outran the check above: the
			// insert must not fail the whole load because of a single duplicate.
			tag, err := tx.Exec(ctx,
				`INSERT INTO recipients (campaign_id, amo_contact_id, amo_lead_id, name, username, state, random_id)
				 VALUES ($1, $2, $3, $4, $5, 'planned', $6)
				 ON CONFLICT (campaign_id, amo_contact_id) DO NOTHING`,
				campaignID, item.person.ContactID, item.person.LeadID,
				item.name, item.username, newRandomID())
			if err != nil {
				return fmt.Errorf("insert recipient %d: %w", item.person.ContactID, err)
			}
			if tag.RowsAffected() == 0 {
				report.Skipped[ReasonDuplicate]++
				continue
			}
			report.Accepted++
		}
		return nil
	})
	if err != nil {
		return BatchReport{Skipped: map[string]int{}}, fmt.Errorf("add recipients: %w", err)
	}
	return report, nil
}

// checkPersonSQL answers the import guards in one pass: the stop list,
// "received in any campaign", "already in this campaign" and a manual touch
// through tg_send. A person is recognised both by the contact id and by
// username: one person gets created twice in the CRM. The journal is keyed by
// username only - a manual touch has no contact_id to take.
//
// The touch is split into two flags rather than one: pending and sent close the
// person the same way but are named differently in the report, and one cannot
// be derived from the other. No expiry threshold is needed here - any pending
// means "delivery unknown", and the import must say exactly that.
const checkPersonSQL = `SELECT
	EXISTS (SELECT 1 FROM stop_list WHERE amo_contact_id = $1 OR username = $2),
	EXISTS (SELECT 1 FROM recipients
	        WHERE (amo_contact_id = $1 OR username = $2) AND state IN ('sent', 'replied')),
	EXISTS (SELECT 1 FROM recipients
	        WHERE campaign_id = $3 AND (amo_contact_id = $1 OR username = $2)),
	EXISTS (SELECT 1 FROM direct_messages WHERE username = $2 AND state = 'sent'),
	EXISTS (SELECT 1 FROM direct_messages WHERE username = $2 AND state = 'pending')`

// StartCampaign moves a draft or a paused campaign to active. There is no
// separate resume tool: start works from both draft and stopped. When both a
// draft and a pause exist, the draft starts: a new list is fresher than a
// resume, and the pause stays stopped and is continued by the next start. An
// empty draft does not start - there is nobody to send to.
func (s *Service) StartCampaign(ctx context.Context) (int64, error) {
	now := s.Now()
	var id int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// There is one active campaign in the system, enforced by a partial index.
		// The check comes before the start so the operator gets a clear refusal
		// rather than the Postgres unique index violation text.
		active, err := findCampaign(ctx, tx, "active")
		if err != nil {
			return err
		}
		if active != 0 {
			return ErrActiveCampaign
		}

		type row struct {
			id       int64
			status   string
			template string
		}
		rows, err := tx.Query(ctx,
			`SELECT id, status, template_text FROM campaigns WHERE status IN ('draft', 'stopped') ORDER BY id FOR UPDATE`)
		if err != nil {
			return fmt.Errorf("select campaigns: %w", err)
		}
		var found []row
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.id, &item.status, &item.template); err != nil {
				rows.Close()
				return fmt.Errorf("scan campaign: %w", err)
			}
			found = append(found, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read campaigns: %w", err)
		}

		// There is one draft in the system (partial index), there may be several
		// pauses: the draft wins, and indistinguishable pauses are sorted out by
		// a human.
		target := row{}
		for _, item := range found {
			if item.status == "draft" {
				target = item
				break
			}
		}
		switch {
		case len(found) == 0:
			return ErrNothingToStart
		case target.id != 0:
		case len(found) == 1:
			target = found[0]
		default:
			return ErrManyCampaigns
		}

		// The template is rechecked here too: the campaign could have been built
		// by a previous version where "{{имя}}" passed input. The refusal at start
		// is for the human; the guard itself sits in ClaimNext: a campaign that
		// became active before an update arrives there too, while it no longer
		// passes through start.
		if spots, total := badBraces(target.template); total > 0 {
			return &badTemplate{spots: spots, total: total, stored: true}
		}

		if target.status == "draft" {
			var planned int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM recipients WHERE campaign_id = $1 AND state = 'planned'`,
				target.id).Scan(&planned); err != nil {
				return fmt.Errorf("count planned: %w", err)
			}
			if planned == 0 {
				return ErrEmptyDraft
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE campaigns SET status = 'active', started_at = COALESCE(started_at, $2) WHERE id = $1`,
			target.id, now); err != nil {
			return fmt.Errorf("activate campaign: %w", err)
		}
		id = target.id
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("start campaign: %w", err)
	}
	return id, nil
}

// StopCampaign pauses a campaign or cancels it and returns the id of the
// affected campaign. Pause is possible only from active; cancel hits one
// campaign in the order active, draft, stopped. The draft goes before the pause
// so that the pair "pause plus draft" is resolvable with the tools: the first
// cancel removes the draft, the second the pause, and neither takes both.
func (s *Service) StopCampaign(ctx context.Context, cancel bool) (int64, error) {
	now := s.Now()
	var id int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if cancel {
			var target int64
			for _, status := range []string{"active", "draft", "stopped"} {
				found, err := findCampaign(ctx, tx, status)
				if err != nil {
					return err
				}
				if found != 0 {
					target = found
					break
				}
			}
			if target == 0 {
				return ErrNothingToCancel
			}
			if err := cancelCampaign(ctx, tx, target, now, s.staleBefore(now)); err != nil {
				return err
			}
			id = target
			return nil
		}
		err := tx.QueryRow(ctx,
			`UPDATE campaigns SET status = 'stopped' WHERE status = 'active' RETURNING id`).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// The refusal names the pause and the draft: a repeated stop on an
			// already paused campaign would otherwise read as "the campaign is
			// gone", although start continues it.
			return withNear(ctx, tx, ErrNothingToStop, "stopped", "draft")
		}
		if err != nil {
			return fmt.Errorf("pause campaign: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("stop campaign: %w", err)
	}
	return id, nil
}

// FinishIfEmpty closes the active campaign when no planned rows are left. A row
// taken by a sender is also planned: the campaign does not close while at least
// one row is in progress.
func (s *Service) FinishIfEmpty(ctx context.Context) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE campaigns SET status = 'done', finished_at = $1
		 WHERE status = 'active'
		   AND NOT EXISTS (SELECT 1 FROM recipients r WHERE r.campaign_id = campaigns.id AND r.state = 'planned')`,
		s.Now())
	if err != nil {
		return false, fmt.Errorf("finish campaign: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// findCampaign returns the id of the first campaign in the named status under a
// row lock; 0 means there is no such campaign. Cancel and start need the lock:
// the decision is made on the same state that is changed afterwards.
func findCampaign(ctx context.Context, tx pgx.Tx, status string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM campaigns WHERE status = $1 ORDER BY id LIMIT 1 FOR UPDATE`,
		status).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find %s campaign: %w", status, err)
	}
	return id, nil
}

// cancelCampaign cancels one campaign together with its queue. The rows are
// removed first: after the campaign status changes they can no longer be found
// by this condition.
//
// A row with a live claim is left alone: a sender is carrying it right now, and
// cancelling its state would lose the fact of sending - finalization would no
// longer find such a row. Its fate is decided by FinishClaim: a delivered one
// goes to sent, and a temporary failure on a cancelled campaign goes to
// cancelled.
func cancelCampaign(ctx context.Context, tx pgx.Tx, campaignID int64, now, stale time.Time) error {
	if _, err := tx.Exec(ctx,
		`UPDATE recipients SET state = 'cancelled', reason = $3, claimed_at = NULL
		 WHERE campaign_id = $1 AND state = 'planned'
		   AND (claimed_at IS NULL OR claimed_at < $2)`,
		campaignID, stale, ReasonCampaignGone); err != nil {
		return fmt.Errorf("cancel recipients: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE campaigns SET status = 'cancelled', finished_at = $2 WHERE id = $1`,
		campaignID, now); err != nil {
		return fmt.Errorf("cancel campaign %d: %w", campaignID, err)
	}
	return nil
}

// staleBefore is the claim expiry boundary: before it nobody is carrying the row any more.
func (s *Service) staleBefore(now time.Time) time.Time {
	return now.Add(-s.cfg.ClaimStaleAfter)
}

// namePlaceholder is the only template substitution. A campaign without it is
// legal, but a recipient without a name does not enter a campaign that uses it.
// It is Russian because the message templates are Russian text.
const namePlaceholder = "{имя}"

// nameBudget is the character allowance for a name when checking template
// length: the name is unknown in advance, and learning about a message that
// does not fit at mailing start is too late.
const nameBudget = 64

// namePlaceholderPattern matches the only substitution the service understands:
// case does not matter, spaces inside the braces are allowed. "{Имя}" at the
// start of a sentence is a natural typo, and it must not go into the message
// literally.
var namePlaceholderPattern = regexp.MustCompile(`(?i)\{\s*имя\s*\}`)

// RenderMessage substitutes the name into the template. The replacement is
// literal: the name comes from the CRM, and "$1" in it must not expand into a
// piece of the match.
//
// An empty name does not filter the person out but removes the address
// entirely: "{имя}, текст" becomes "текст", "Привет, {имя}!" becomes
// "Привет!". Otherwise the message would start with a comma. The case of the
// first letter is deliberately not fixed: the service does not rewrite the
// caller's text, and the readability of both forms is checked at approval.
func RenderMessage(template, name string) string {
	clean := cleanName(name)
	if clean == "" {
		return dropName(template)
	}
	return namePlaceholderPattern.ReplaceAllLiteralString(template, clean)
}

// namePlaceholderAfterComma and namePlaceholderBeforeComma match the
// substitution together with the comma stuck to it. They are split into two
// replacements because the comma sits on the side where the template author
// put it, and exactly that one must be removed.
var (
	namePlaceholderAfterComma  = regexp.MustCompile(`(?i),[ \t]*\{\s*имя\s*\}`)
	namePlaceholderBeforeComma = regexp.MustCompile(`(?i)\{\s*имя\s*\}[ \t]*,[ \t]*`)
)

// dropName removes the address from the template when there is no name.
func dropName(template string) string {
	text := namePlaceholderAfterComma.ReplaceAllLiteralString(template, "")
	text = namePlaceholderBeforeComma.ReplaceAllLiteralString(text, "")
	text = namePlaceholderPattern.ReplaceAllLiteralString(text, "")
	return strings.TrimSpace(text)
}

// hasName reports whether the template uses the recipient name.
func hasName(template string) bool {
	return namePlaceholderPattern.MatchString(template)
}

// badBraces returns the spots where the service did not parse a curly brace.
// Every brace is checked, not only properly paired ones: "{{имя}}" is a common
// Jinja typo, and substitution inside the outer pair would give the person
// "Привет, {Пётр}!", while an unclosed "{имя!" would go into the message
// literally. After the known substitutions are parsed, no braces may remain in
// the template.
// It returns the first maxSpots spots and their total count: a refusal that
// showed three spots out of five reads as a full list, and the person fixes
// three, gets the refusal again and takes it for a new defect.
func badBraces(template string) (spots []string, total int) {
	known := namePlaceholderPattern.FindAllStringIndex(template, -1)
	runes := []rune(template)
	// shown is how far the template has already been shown: adjacent braces of
	// one typo ("{{") give one spot in the list, not two overlapping ones.
	shown, pos := 0, -1
	for at, symbol := range template {
		pos++
		if symbol != '{' && symbol != '}' || covered(known, at) || pos < shown {
			continue
		}
		total++
		shown = min(pos+16, len(runes))
		if len(spots) < maxSpots {
			spots = append(spots, string(runes[max(pos-8, 0):shown]))
		}
	}
	return spots, total
}

// maxSpots is how many spots a refusal shows: a person fixes a longer list one
// by one anyway, and it would crowd out the tool reply.
const maxSpots = 3

// covered reports whether the brace falls inside a recognised substitution.
func covered(known [][]int, at int) bool {
	for _, span := range known {
		if at >= span[0] && at < span[1] {
			return true
		}
	}
	return false
}

// nameFault returns why the name is unfit for this template: a brace in it, or
// a message longer than the limit. An empty string means the name is fine. One
// answer for import and for the queue: the guard must speak the same way in both
// places, and the order of reasons determines what the person sees in the
// report.
//
// A brace in the name is checked only where the template substitutes the name:
// without {имя} it does not enter the message by a single character, and there
// is nothing to filter the person for. An empty name is not a fault: a person
// without a usable name stays in the list
// and the message goes out without the address. RenderMessage removes the
// substitution.
func nameFault(template, name string) string {
	if hasName(template) {
		switch {
		// The name comes from the CRM and goes into the text as is: the service
		// deliberately does not strip markup, but a curly brace in the name is the
		// same symptom the template check stands against, up to a name "{имя}".
		case strings.ContainsAny(name, "{}"):
			return ReasonNameBraces
		}
	}
	// The name allowance in load_campaign is a rough estimate at input, while the
	// real name is known here: a long name makes the message exceed the limit, and
	// Telegram would reject it at send time, when the list is already assembled.
	if telegramLen(RenderMessage(template, name)) > maxTextLen {
		return ReasonTextTooLong
	}
	return ""
}

// telegramLen returns the message length the way Telegram measures it: in UTF-16
// units, where a symbol outside the BMP (emoji, rare ideographs) counts as two.
// Counting runes understates the length, and a message with a hundred emoji
// passed the check and then got MESSAGE_TOO_LONG at send time, when the list was
// already assembled.
func telegramLen(text string) int {
	length := 0
	for _, symbol := range text {
		length++
		if symbol > 0xFFFF {
			length++
		}
	}
	return length
}

// renderedLen returns the length of what will actually go to the person and the
// allowance for names built into it: nameBudget per substitution, zero if there
// are no substitutions. The real name is known only in the queue, while the
// refusal must happen at input, so the allowance is returned outward - the
// refusal text must not name an allowance that was not counted.
func renderedLen(template string) (used, budget int) {
	used = telegramLen(RenderMessage(template, strings.Repeat("и", nameBudget)))
	return used, nameBudget * len(namePlaceholderPattern.FindAllStringIndex(template, -1))
}

// cleanName brings a CRM name to a form fit for a message: line breaks and
// repeated spaces collapse, "Иван\n\nПетров" in a message body looks like a
// glitch. Markup is deliberately neither stripped nor escaped: sending is plain
// text, and <b> in a name is visual noise, not an injection. An empty string
// means filtering out.
func cleanName(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

// normalizeUsername brings a username to the form stored in the database:
// without spaces, "@" and the t.me wrapper, lowercase. The username shape is
// checked by telegram.IsUsername: Cyrillic, a phone number and a stub like
// "ivan" do not pass - a lead without a usable username does not enter the
// mailing, guessing from the name is forbidden. An empty string means
// filtering out.
func normalizeUsername(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if index := strings.LastIndex(name, "t.me/"); index >= 0 {
		name = name[index+len("t.me/"):]
	}
	name = strings.TrimPrefix(name, "@")
	name = strings.TrimSuffix(strings.TrimSpace(name), "/")
	if !telegram.IsUsername(name) {
		return ""
	}
	return name
}

// newRandomID returns the random_id of a row: Telegram deduplicates by it, so it
// is generated once at insert and survives any delivery retry. Zero is excluded
// - Telegram treats it as "no deduplication".
func newRandomID() int64 {
	return rand.Int64N(math.MaxInt64-1) + 1
}
