package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
)

// moscowZone is MSK as a fixed offset rather than Europe/Moscow: Moscow has had
// no clock changes since 2014, and the container may lack tzdata, in which case
// the send window would silently shift to UTC.
var moscowZone = time.FixedZone("MSK", 3*60*60)

// Recipient is a queue row taken by the sender. The text is already rendered:
// the sender does not go back to the database for the template.
type Recipient struct {
	ID         int64
	CampaignID int64
	ContactID  int64
	LeadID     int64
	Name       string
	Username   string
	RandomID   int64
	Text       string

	// UserID is the person's Telegram id, known after the resolve. The sender
	// fills it in before finalizing: it is how a reply is matched to a person
	// whose update arrived without a username.
	UserID int64
}

// SendResult is the delivery outcome of a claimed row.
type SendResult string

const (
	ResultSent         SendResult = "sent"          // delivered, the person is used up
	ResultUndelivered  SendResult = "undelivered"   // refused for this person, a retry will not help
	ResultRetry        SendResult = "retry"         // temporary failure: the row goes back to the queue
	ResultRetryCounted SendResult = "retry_counted" // temporary failure that spends an attempt
)

// ReasonRetryExhausted is the reason a row leaves the queue after its attempt
// budget runs out. It lives here, not among the import screening reasons: only
// finalization sets it.
const ReasonRetryExhausted = "retry_exhausted"

// maxAttempts is the budget of spendable attempts per row. The selection takes
// the first row by id, so a row with a stable unrecognized error and no budget
// would hold the head of the queue forever and the campaign would not move.
const maxAttempts = 5

// maxSkips is how many unfit rows one selection pass removes before handing the
// queue back. The cap exists so the transaction stays short: it holds the
// account row lock, which manual sending also waits on.
const maxSkips = 50

// EnsureAccount creates the account row for the process label on first start
// and returns the effective cap. The daily_limit of an existing row is not
// overwritten: the cap lives in the database, and a restart with a different
// environment variable must not quietly raise it - process startup reports the
// mismatch.
func (s *Service) EnsureAccount(ctx context.Context) (int64, int, error) {
	var id int64
	var limit int
	// DO UPDATE instead of DO NOTHING only for RETURNING: DO NOTHING returns
	// no row at all on a conflict.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ($1, $2)
		 ON CONFLICT (label) DO UPDATE SET label = EXCLUDED.label
		 RETURNING id, daily_limit`,
		s.cfg.AccountLabel, s.cfg.AccountDailyLimit).Scan(&id, &limit)
	if err != nil {
		return 0, 0, fmt.Errorf("ensure account: %w", err)
	}
	return id, limit, nil
}

// ClaimNext takes one queue row for the account. The transaction is short and
// makes no network call: under the account row lock it checks the gates (send
// window, account stop, daily cap, active campaign, stop list, "received in any
// campaign"), then marks the row with claimed_at.
//
// nil without an error means the queue is empty or a gate blocks sending; this
// is a normal answer.
func (s *Service) ClaimNext(ctx context.Context, accountLabel string) (*Recipient, error) {
	now := s.Now()
	// The early exit on a closed window stays while the list of service
	// recipients is empty: at night and on weekends we do not touch the
	// database at all. A non-empty list gives owner rows a path outside the
	// window too, so the transaction is still needed then.
	ownerOnly := !s.inSendWindow(now)
	if ownerOnly && len(s.owners) == 0 {
		return nil, nil
	}
	stale := s.staleBefore(now)

	var claimed *Recipient
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The account row lock serializes senders of one account: otherwise two
		// processes would pass the cap check at the same time.
		var accountID int64
		var dailyLimit int
		var stopped bool
		err := tx.QueryRow(ctx,
			`SELECT id, daily_limit, stopped FROM accounts WHERE label = $1 FOR UPDATE`,
			accountLabel).Scan(&accountID, &dailyLimit, &stopped)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("account %q is not registered", accountLabel)
		}
		if err != nil {
			return fmt.Errorf("lock account: %w", err)
		}
		if stopped {
			return nil
		}

		// A live claim counts the same as a sent message: a row taken by another
		// process is almost a message already.
		var used int
		if err := tx.QueryRow(ctx, s.accountUsedSQL,
			startOfDay(now), stale, accountID).Scan(&used); err != nil {
			return fmt.Errorf("count daily usage: %w", err)
		}
		if used >= dailyLimit {
			if len(s.owners) == 0 {
				return nil
			}
			ownerOnly = true
		}

		var campaignID int64
		var template string
		err = tx.QueryRow(ctx, `SELECT id, template_text FROM campaigns WHERE status = 'active'`).
			Scan(&campaignID, &template)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load active campaign: %w", err)
		}

		// A template with an unresolved brace must not be sent: a campaign built
		// by an earlier version, where "{{имя}}" passed validation, would give a
		// real person "Привет, {Пётр}!". The gate sits here, not only in
		// StartCampaign, because a campaign that became active before the
		// upgrade no longer goes through start, while every message comes
		// through here. The campaign state does not change: a human drives it,
		// and a broken template is a reason for the queue to idle, which
		// IdleReason reports.
		if _, total := badBraces(template); total > 0 {
			return nil
		}

		if _, err := tx.Exec(ctx, s.skipDisqualifiedSQL, campaignID, stale); err != nil {
			return fmt.Errorf("skip disqualified: %w", err)
		}

		// Rows with an unfit name are removed here too, not only on import: rows
		// loaded before this gate existed would otherwise reach real people in a
		// message starting with a comma or a stray brace. The order after the
		// general screening is deliberate: for a person who is both on the stop
		// list and nameless, the reason is the stop list - the gate that would
		// have stopped them anyway.
		needName := hasName(template)
		if needName {
			if _, err := tx.Exec(ctx, skipBadNamesSQL, campaignID, stale); err != nil {
				return fmt.Errorf("skip bad names: %w", err)
			}
		}

		// The last name check runs on the claimed row and gives the same answer as
		// on import: the message length depends on the template and is measured in
		// UTF-16 units, which the selection's SQL condition cannot express
		// exactly. Unfit rows are removed one after another, not one per pass: a
		// removed row leaves planned and the same query no longer sees it -
		// otherwise forty such rows take hours to clear while the campaign looks
		// like it is running. The pass cap keeps the transaction short: it locks
		// the account row, which manual sending also waits on.
		var picked *Recipient
		for range maxSkips {
			var item Recipient
			err = tx.QueryRow(ctx, s.claimSQL, campaignID, stale, accountID, needName, ownerOnly).Scan(
				&item.ID, &item.CampaignID, &item.ContactID, &item.LeadID,
				&item.Name, &item.Username, &item.RandomID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("select recipient: %w", err)
			}

			fault := nameFault(template, cleanName(item.Name))
			if fault == "" {
				picked = &item
				break
			}
			slog.Warn("recipient skipped by name rule",
				"recipient_id", item.ID, "username", item.Username, "reason", fault)
			if _, err := tx.Exec(ctx,
				`UPDATE recipients SET state = 'skipped', reason = $2, claimed_at = NULL WHERE id = $1`,
				item.ID, fault); err != nil {
				return fmt.Errorf("skip recipient %d: %w", item.ID, err)
			}
		}
		if picked == nil {
			return nil
		}

		if _, err := tx.Exec(ctx,
			`UPDATE recipients SET claimed_at = $2, account_id = $3 WHERE id = $1`,
			picked.ID, now, accountID); err != nil {
			return fmt.Errorf("claim recipient: %w", err)
		}

		picked.Text = RenderMessage(template, picked.Name)
		claimed = picked
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim next: %w", err)
	}
	return claimed, nil
}

// Queue idle reasons: why the sender is silent right now. They answer the
// question "why did the message not go out on time", which is what the service
// is watched for.
const (
	IdleWindowClosed   = "send_window_closed"  // not a weekday or outside the send window
	IdleAccountStop    = "account_stopped"     // account restriction, lifted by a human
	IdleDailyLimit     = "daily_limit_reached" // the account daily cap is used up
	IdleNoCampaign     = "no_active_campaign"  // nothing to work on
	IdleBrokenTemplate = "broken_template"     // the campaign template has unresolved braces
	IdleQueueEmpty     = "queue_empty"         // the campaign has no rows left in the queue
	IdleQueueBlocked   = "queue_blocked"       // rows exist but are pinned to another account or claimed
	// rows exist but a gate removes every one of them: nothing will be sent
	IdleQueueDisqualified = "queue_disqualified"
	IdleClaimRaceLost     = "claim_race_lost" // a row is available but a neighbor took it between passes
)

// Idle is an idle reason with the numbers that make it checkable: "cap used
// up" without "7 of 7" and "window closed" without the opening time force a
// human to look up the same figures by hand.
type Idle struct {
	Reason string
	// Used, Limit are the account daily usage and its cap from the database: set
	// for the IdleDailyLimit reason.
	Used  int
	Limit int
	// OpensAt is the next send window opening: set for the IdleWindowClosed reason.
	OpensAt time.Time
	// Left is how many rows remain in the queue: set for the IdleQueueDisqualified
	// reason. Without the number, "a gate will remove them" cannot be checked
	// against the campaign counters.
	Left int
	// Braces are the unresolved template braces: set for the IdleBrokenTemplate
	// reason. The report replaces them with the braces of the campaign it shows -
	// its line speaks about "this campaign", not the active one.
	Braces []string
}

// IdleReason explains an empty ClaimNext answer. It is called when idle and from
// the report, not on every send, and is not a gate: it is diagnostics, and a
// disagreement with ClaimNext would spoil the log but not let a message through.
func (s *Service) IdleReason(ctx context.Context, accountLabel string) (Idle, error) {
	now := s.Now()
	if !s.inSendWindow(now) {
		return Idle{Reason: IdleWindowClosed, OpensAt: s.nextWindow(now)}, nil
	}

	var accountID int64
	var dailyLimit int
	var stopped bool
	err := s.pool.QueryRow(ctx,
		`SELECT id, daily_limit, stopped FROM accounts WHERE label = $1`,
		accountLabel).Scan(&accountID, &dailyLimit, &stopped)
	if err != nil {
		return Idle{}, fmt.Errorf("read account: %w", err)
	}
	if stopped {
		return Idle{Reason: IdleAccountStop}, nil
	}

	stale := s.staleBefore(now)
	var used int
	if err := s.pool.QueryRow(ctx, s.accountUsedSQL,
		startOfDay(now), stale, accountID).Scan(&used); err != nil {
		return Idle{}, fmt.Errorf("count daily usage: %w", err)
	}
	if used >= dailyLimit {
		return Idle{Reason: IdleDailyLimit, Used: used, Limit: dailyLimit}, nil
	}

	var campaignID int64
	var template string
	err = s.pool.QueryRow(ctx, `SELECT id, template_text FROM campaigns WHERE status = 'active'`).
		Scan(&campaignID, &template)
	if errors.Is(err, pgx.ErrNoRows) {
		return Idle{Reason: IdleNoCampaign}, nil
	}
	if err != nil {
		return Idle{}, fmt.Errorf("load active campaign: %w", err)
	}
	if spots, total := badBraces(template); total > 0 {
		return Idle{Reason: IdleBrokenTemplate, Braces: spots}, nil
	}

	var planned, sendable, available int
	if err := s.pool.QueryRow(ctx, s.idleQueueSQL, campaignID, stale, accountID, hasName(template)).
		Scan(&planned, &sendable, &available); err != nil {
		return Idle{}, fmt.Errorf("count queue: %w", err)
	}
	switch {
	case planned == 0:
		return Idle{Reason: IdleQueueEmpty}, nil
	case available > 0:
		return Idle{Reason: IdleClaimRaceLost}, nil
	case sendable > 0:
		return Idle{Reason: IdleQueueBlocked}, nil
	default:
		// Rows exist but none can go out: the next selection pass will remove
		// them. This case used to be reported as another account - advice to wait
		// for a sender that does not exist.
		return Idle{Reason: IdleQueueDisqualified, Left: planned}, nil
	}
}

// FinishClaim closes a row by its delivery outcome - in a separate short
// transaction after the network call. The stop list and "received in any
// campaign" are not rechecked here: they are ClaimNext gates, which stand
// before sending, not after.
//
// false without an error means the row left the queue while sending was in
// progress (the person replied first), and the outcome is discarded.
func (s *Service) FinishClaim(ctx context.Context, item *Recipient, result SendResult, reason string) (bool, error) {
	now := s.Now()
	if result == ResultRetryCounted {
		var state string
		var attempts int
		err := s.pool.QueryRow(ctx, countedRetrySQL,
			item.ID, reason, ReasonCampaignGone, maxAttempts, ReasonRetryExhausted).Scan(&state, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("finish claim: %w", err)
		}
		if state == "undelivered" {
			slog.Warn("retry budget exhausted",
				"recipient_id", item.ID, "username", item.Username, "attempts", attempts, "reason", reason)
		}
		return true, nil
	}

	var query string
	var args []any
	switch result {
	case ResultSent:
		// Delivery happened - the fact must land in the database from whatever
		// state the row was moved to while sending was in progress: without
		// sent_at the person would pass the next campaign's import and get a
		// second message. A reply may have outrun the write: replied stays
		// replied, but gets sent_at. undelivered is overridden by delivery: the
		// first process with the same label may have recorded a refusal while a
		// second one, which took over a stale claim, delivered.
		// tg_user_id is written by the same query: a reply without a username is
		// looked up by it.
		// $4::bigint is an explicit type: without the cast NULLIF infers int4, and
		// a real Telegram id above 2^31 cannot be encoded at all.
		query = `UPDATE recipients
			SET state = CASE WHEN state = 'replied' THEN 'replied' ELSE 'sent' END,
			    reason = $3, sent_at = COALESCE(sent_at, $2), claimed_at = NULL,
			    tg_user_id = COALESCE(NULLIF($4::bigint, 0), tg_user_id)
			WHERE id = $1 AND state IN ('planned', 'replied', 'skipped', 'cancelled', 'undelivered')`
		args = []any{item.ID, now, reason, item.UserID}
	case ResultUndelivered:
		query = `UPDATE recipients SET state = 'undelivered', reason = $2, claimed_at = NULL
			WHERE id = $1 AND state = 'planned'`
		args = []any{item.ID, reason}
	case ResultRetry:
		// The attempt is not spent: we release the claim and keep the reason for
		// the log. The exception is a cancelled campaign: its queue is already
		// cleared, and a row that cancellation left alone because of a live claim
		// goes to cancelled here. Otherwise it would stay planned forever - the
		// selection takes only rows of the active campaign.
		query = `UPDATE recipients AS r
			SET state = CASE WHEN c.status = 'cancelled' THEN 'cancelled' ELSE r.state END,
			    reason = CASE WHEN c.status = 'cancelled' THEN $3 ELSE $2 END,
			    claimed_at = NULL
			FROM campaigns c
			WHERE r.id = $1 AND r.state = 'planned' AND c.id = r.campaign_id`
		args = []any{item.ID, reason, ReasonCampaignGone}
	default:
		return false, fmt.Errorf("unknown send result %q", result)
	}

	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("finish claim: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkReplied closes the rows of a person who started the conversation
// themselves. A sent row becomes replied, and so does a not-yet-sent one, which
// cancels the invitation. Draft rows are not touched: nothing was sent to them
// yet, and replied would filter the person out of a future mailing with the
// reason "already received".
//
// The person is identified by Telegram id and by username: a short update
// carries no username, and the id is known only for rows that were already
// sent. Both conditions in one query mean the same person, and both of their
// rows are closed together.
// $1::bigint is an explicit type: comparison with a literal would infer int4
// for the parameter, and a real Telegram id above 2^31 would not encode.
//
// claimed_at is not cleared: a row in progress must stay in the daily count
// until finalization, otherwise a second account process would take an extra
// one. The row leaves the selection anyway - the selection takes only planned.
func (s *Service) MarkReplied(ctx context.Context, userID int64, username string) (int, error) {
	name := normalizeUsername(username)
	if name == "" && userID <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE recipients AS r SET state = 'replied', replied_at = $3
		 FROM campaigns c
		 WHERE c.id = r.campaign_id AND c.status IN ('active', 'stopped', 'done')
		   AND r.state = 'sent'
		   AND (($1::bigint > 0 AND r.tg_user_id = $1) OR ($2 <> '' AND r.username = $2))`,
		userID, name, s.Now())
	if err != nil {
		return 0, fmt.Errorf("mark replied: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// markWroteFirst removes from the queue a person who wrote first, before their
// turn came. Their row does not go to replied: they were not answering our
// message, and they do not belong in the campaign reply metric - that metric is
// exactly what the offer test measures. The template must not be sent to them
// either: they started the conversation themselves.
//
// A row with a live claim is not touched: the sender is carrying it right now,
// and the message may already be out. Releasing its claim is even less
// acceptable - a row in progress must stay in the daily count until
// finalization, otherwise a second account process would take an extra one.
func (s *Service) markWroteFirst(ctx context.Context, userID int64, username string) (int, error) {
	name := normalizeUsername(username)
	if name == "" && userID <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE recipients AS r SET state = 'skipped', reason = '`+ReasonWroteFirst+`'
		 FROM campaigns c
		 WHERE c.id = r.campaign_id AND c.status IN ('active', 'stopped')
		   AND r.state = 'planned'
		   AND (r.claimed_at IS NULL OR r.claimed_at < $3)
		   AND (($1::bigint > 0 AND r.tg_user_id = $1) OR ($2 <> '' AND r.username = $2))`,
		userID, name, s.staleBefore(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("mark wrote first: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// inSendWindow is the window gate: weekdays only, and only between the config
// bounds in MSK. A malformed window stays silent instead of sending: the config
// is validated at startup, so a parse error should not reach this point.
func (s *Service) inSendWindow(now time.Time) bool {
	local := now.In(moscowZone)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return false
	}
	from, err := parseClock(s.cfg.SendWindowStart)
	if err != nil {
		return false
	}
	to, err := parseClock(s.cfg.SendWindowEnd)
	if err != nil {
		return false
	}
	minutes := local.Hour()*60 + local.Minute()
	return minutes >= from && minutes < to
}

func parseClock(value string) (int, error) {
	point, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("parse clock %q: %w", value, err)
	}
	return point.Hour()*60 + point.Minute(), nil
}

// startOfDay is the start of the day in MSK: the daily cap counts a calendar day
// in the same time zone as the send window.
func startOfDay(now time.Time) time.Time {
	local := now.In(moscowZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, moscowZone)
}

// Recipient screening before sending. Both conditions are checked in the claim
// transaction, not only on import: a person is added to the stop list after the
// list is loaded, and may have received a message in a neighboring campaign
// between the import and their turn.
const inStopListSQL = `EXISTS (SELECT 1 FROM stop_list s
	WHERE s.amo_contact_id = r.amo_contact_id OR s.username = r.username)`

const alreadySentSQL = `EXISTS (SELECT 1 FROM recipients o
	WHERE (o.amo_contact_id = r.amo_contact_id OR o.username = r.username)
	  AND o.id <> r.id AND o.state IN ('sent', 'replied'))`

// A manual touch closes the person to campaigns the same way a sent row does: a
// message sent through tg_send does not land in recipients, and without this
// condition the next campaign would write to them a second time. The direction
// is one-way - the log blocks a campaign but does not block a repeated tg_send,
// otherwise a conversation would break off after the first reply. failed does
// not count: with such a refusal the message certainly did not go out.
//
// The only key is the username: a manual touch has no contact_id to use.
const touchedDirectSQL = `EXISTS (SELECT 1 FROM direct_messages d
	WHERE d.username = r.username AND d.state <> 'failed')`

// The screening reason must tell a delivered touch from an unknown one: pending
// means "the message may have gone out", and the report must not call that
// "already written to". A threshold is not needed here and would be a lie: a
// fresh pending is as unknown as an old one. The same question asked through
// checkPersonSQL is answered with this same split - two entry points must say
// the same thing about one person.
const sentDirectSQL = `EXISTS (SELECT 1 FROM direct_messages d
	WHERE d.username = r.username AND d.state = 'sent')`

// Rows removed by a gate go to skipped rather than staying planned: otherwise
// the queue would never empty and the campaign would never close. A row with a
// live claim is not touched - another process is carrying it right now, and
// finalization decides its fate.
//
// An owner service recipient is not removed for "already received" or for a
// manual touch - that rule does not apply to them - but the stop list removes
// them too. Otherwise such a row would hang as planned forever: sendableSQL
// does not let it go out, the campaign does not close, and status would name
// the idle reason queue_disqualified, which is the wrong reason.
func skipDisqualifiedSQL(owners string) string {
	return `UPDATE recipients AS r
	SET state = 'skipped',
	    reason = CASE
	        WHEN ` + inStopListSQL + ` THEN '` + ReasonStopList + `'
	        WHEN ` + alreadySentSQL + ` THEN '` + ReasonAlreadySent + `'
	        WHEN ` + sentDirectSQL + ` THEN '` + ReasonDirectSent + `'
	        ELSE '` + ReasonDirectPending + `' END,
	    claimed_at = NULL
	WHERE r.campaign_id = $1 AND r.state = 'planned'
	  AND (r.claimed_at IS NULL OR r.claimed_at < $2)
	  AND (` + inStopListSQL + `
	       OR (r.username <> ALL(` + owners + `)
	           AND (` + alreadySentSQL + ` OR ` + touchedDirectSQL + `)))`
}

// The empty-name gate no longer exists in SQL: a person without a name gets a
// message without a greeting, and there is no reason to remove their row.

// bracedNameSQL matches a curly brace in a name: it would go into the message
// as is, which is the same symptom the template check guards against.
const bracedNameSQL = `r.name ~ '[{}]'`

// skipBadNamesSQL removes rows with a brace in the name. Only a template with
// {имя} calls for this reason, so the query runs under the same condition as
// claim. The message length does not belong here - it depends on the template
// and is measured in UTF-16 units, which SQL cannot express exactly; nameFault
// checks it on the claimed row. A row with a live claim is not touched - another
// process is carrying it.
//
// An empty name is no longer a gate: the message goes out without a greeting
// (a deliberate choice).
const skipBadNamesSQL = `UPDATE recipients AS r SET state = 'skipped', claimed_at = NULL,
	    reason = '` + ReasonNameBraces + `'
	WHERE r.campaign_id = $1 AND r.state = 'planned'
	  AND ` + bracedNameSQL + `
	  AND (r.claimed_at IS NULL OR r.claimed_at < $2)`

// The screening condition is repeated in the selection on purpose: the gate
// must not depend on whether the screening above ran. The name follows the same
// order: $4 says whether the active campaign template uses the name (the
// template itself is not visible in the query), and a row without a name under
// such a template is not selected even if the screening somehow did not remove
// it.
//
// A row once claimed by an account ($3) stays with it forever - both under a
// stale claim and after returning to the queue: redelivery relies on random_id,
// and Telegram deduplicates it within one account. Otherwise a temporary
// failure in which the message may have gone out would hand the row to a
// neighboring number and the person would get a second message. A row of a dead
// foreign account waits for a human, and status shows how many such rows exist.
// sendableSQL is a row that the gate lets through: no stop list, no second
// message, no manual touch, no unfit name under a template with {имя} ($4).
// One text for the selection and for the queue count in the report: if they
// diverged, they would say "the queue is moving" where the gate removes every
// last row.
//
// An owner service recipient passes "already received" and "written to by
// hand", but not the stop list or an unfit name: the "one message" rule is
// lifted, not the gates that hold a person back in general.
func sendableSQL(owners string) string {
	return `(NOT ` + inStopListSQL + `
	           AND (r.username = ANY(` + owners + `)
	                OR NOT (` + alreadySentSQL + ` OR ` + touchedDirectSQL + `))
	           AND NOT ($4 AND ` + bracedNameSQL + `))`
}

// claimSQL selects the next row. $5 is the "service recipients only" mode:
// a closed window or an exhausted cap turns it on, and then the queue moves on
// the owner's test rows alone.
func claimSQL(owners string) string {
	return `SELECT r.id, r.campaign_id, r.amo_contact_id, r.amo_lead_id, r.name, r.username, r.random_id
	FROM recipients r
	WHERE r.campaign_id = $1 AND r.state = 'planned'
	  AND (r.claimed_at IS NULL OR r.claimed_at < $2)
	  AND (r.account_id IS NULL OR r.account_id = $3)
	  AND ($5 = false OR r.username = ANY(` + owners + `))
	  AND ` + sendableSQL(owners) + `
	ORDER BY r.id
	LIMIT 1
	FOR UPDATE OF r SKIP LOCKED`
}

// idleQueueSQL counts what remains in the active campaign queue, how much of it
// can go out at all, and how much is available to our account right now. The
// screening is repeated in full, not roughly: a row that a gate removes is not
// available, and a report that counted it would promise queue movement that
// will not happen. The message length does not belong here - it depends on the
// template and is measured in UTF-16, which SQL cannot express; nameFault
// removes such rows on a selection pass.
func idleQueueSQL(owners string) string {
	return `SELECT count(*),
	count(*) FILTER (WHERE ` + sendableSQL(owners) + `),
	count(*) FILTER (WHERE ` + sendableSQL(owners) + `
	                   AND (r.claimed_at IS NULL OR r.claimed_at < $2)
	                   AND (r.account_id IS NULL OR r.account_id = $3))
	FROM recipients r
	WHERE r.campaign_id = $1 AND r.state = 'planned'`
}

// usedTodaySQL is the full daily usage of an account: campaign sends plus cold
// manual touches today ($1 is the start of the day, $2 the claim staleness
// bound). The cap is shared by both channels: the invariant says "the daily cap
// covers the mailing and the first touch of a stranger", and a one-sided count
// would allow two caps per day from one number.
//
// account is how the account is named in the query: $3 in the gate, a.id in the
// report. The sum is the same for both, so they can no longer diverge.
//
// Messages to service recipients are excluded from usage in every subquery: the
// cap exists for strangers, and a mailing check on the owner's account must not
// spend it. How many went to them is shown by status on a separate line.
func usedTodaySQL(account, owners string) string {
	return `(SELECT count(*) FROM recipients r
		WHERE r.account_id = ` + account + ` AND r.username <> ALL(` + owners + `) AND ` + dailyUsedSQL + `)
		+ ` + coldDirectSQL(account, owners)
}

// coldDirectSQL counts the account's cold manual touches today ($1 is the start
// of the day). They count toward the cap like the mailing, so they live inside
// usedTodaySQL; the report shows them on the same line, not on a line of their own.
func coldDirectSQL(account, owners string) string {
	return `(SELECT count(*) FROM direct_messages d
		WHERE d.account_id = ` + account + ` AND d.username <> ALL(` + owners + `)
		  AND d.cold AND d.state <> 'failed' AND d.created_at >= $1)`
}

// dailyUsedSQL is the campaign part of the usage: what was sent since the start
// of the day ($1) plus live claims ($2 is the staleness bound). One text for the
// cap gate and for the report: if they diverged, status would show "29 of 30"
// where ClaimNext already refuses.
//
// The row state under a live claim is deliberately not checked: a row taken by
// the sender stays in the count whatever a parallel reply from the person makes
// of it. A state = 'planned' condition would open a hole in the cap on every
// replied row before finalization.
const dailyUsedSQL = `(r.sent_at >= $1 OR (r.claimed_at >= $2 AND r.sent_at IS NULL))`

// countedRetrySQL handles a temporary failure that spends an attempt: when the
// budget runs out the row leaves the queue, otherwise a stable unrecognized
// error holds its head. A cancelled campaign is handled here too, as in a plain
// retry.
const countedRetrySQL = `UPDATE recipients AS r
	SET attempts = r.attempts + 1,
	    state = CASE
	        WHEN c.status = 'cancelled' THEN 'cancelled'
	        WHEN r.attempts + 1 >= $4 THEN 'undelivered'
	        ELSE r.state END,
	    reason = CASE
	        WHEN c.status = 'cancelled' THEN $3
	        WHEN r.attempts + 1 >= $4 THEN $5
	        ELSE $2 END,
	    claimed_at = NULL
	FROM campaigns c
	WHERE r.id = $1 AND r.state = 'planned' AND c.id = r.campaign_id
	RETURNING r.state, r.attempts`
