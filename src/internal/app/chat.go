package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Chat is the dialog: manual correspondence and reading over the same session
// as the mailing. Its shape mirrors Sender - domain, outward boundary, account
// label - because they share the gates: the same account, the same daily cap,
// the same stop.
type Chat struct {
	service   *Service
	transport Dialog
	label     string
}

// NewChat builds a dialog on a service and a transport.
func NewChat(service *Service, transport Dialog) *Chat {
	return &Chat{service: service, transport: transport, label: service.cfg.AccountLabel}
}

const (
	defaultHistory = 20  // how many messages are returned without an explicit depth
	maxHistory     = 100 // depth cap: a longer tool answer is not readable
	maxFound       = 20  // cap on search results
	// maxWait is the cap on waiting for a reply. It is held not by the service
	// but by the MCP client: a tool call has its own cap of about a minute, and
	// on the live setup waits of 60 seconds or more timed out on the client side
	// before our answer arrived. 45 seconds were tested and return in full; for
	// a longer wait, repeat tg_read.
	maxWait      = 45 * time.Second
	pollInterval = 3 * time.Second // history polling step while waiting
	maxTextLen   = 4096            // limit of one Telegram message
	stopTimeout  = 5 * time.Second // to record a stop outside the request context
)

// Refusals of the manual send gates. The list is closed: a new line here is a
// new service decision, not new text in an answer to the agent.
var (
	ErrNoUsername      = errors.New("username is empty or does not look like a telegram username")
	ErrEmptyText       = errors.New("message text is empty")
	ErrTextTooLong     = errors.New("message is longer than telegram allows")
	ErrNotPrivate      = errors.New("only private chats are supported")
	ErrAccountStopped  = errors.New("account is stopped, resume it by hand after checking it")
	ErrStopList        = errors.New("person is in the stop list")
	ErrCampaignSending = errors.New("campaign is sending to this person right now")
	ErrColdWindow      = errors.New("first message to a stranger is allowed in the send window only")
	ErrColdLimit       = errors.New("daily limit of the account is spent")
	ErrDirectPending   = errors.New("previous message to this person is in unknown state")
)

// pendingWait refuses a repeat on top of an unclosed touch, together with the
// time left. The error itself carries the period: the service computes it from
// CLAIM_STALE_AFTER, and the text of the answer to the agent knows nothing about
// settings.
type pendingWait struct {
	username string
	left     time.Duration
}

func (e *pendingWait) Error() string {
	return fmt.Sprintf("%s: @%s, %s left", ErrDirectPending, e.username, e.left)
}

func (e *pendingWait) Unwrap() error { return ErrDirectPending }

// campaignHold is a campaign whose row keeps a person closed to manual sending.
// The error itself carries the id, title and status: the block can be lifted
// only by an action on that specific campaign, and which action works is decided
// by its status. queueOpensAt is when the queue starts moving again (zero if it
// is moving right now); staleAfter is how long until an abandoned claim goes
// stale. Both are needed by the refusal text: without them, the advice "try
// again in a few minutes" is false at night and on weekends.
type campaignHold struct {
	username     string
	id           int64
	title        string
	status       string
	queueOpensAt time.Time
	staleAfter   time.Duration
	// broken means the campaign template has unresolved braces and its queue is
	// stalled. Without this flag the advice "the queue will close the row by
	// itself" would be false: a running campaign with a broken template sends
	// nothing until it is rebuilt.
	broken bool
}

func (e *campaignHold) Error() string {
	return fmt.Sprintf("%s: @%s, campaign %d %q (%s)", ErrCampaignSending, e.username, e.id, e.title, e.status)
}

func (e *campaignHold) Unwrap() error { return ErrCampaignSending }

// accountStop is an account stop with its reason. The reason travels with the
// error because the person's next step depends on it: a Telegram restriction is
// lifted by resume_account after checking the account, while a revoked session
// needs a fresh login, and mixing up the two pieces of advice sends the agent
// in circles.
type accountStop struct {
	reason string
	// cause is the failure that set the stop just now; empty if the stop was
	// already in the database.
	cause error
}

func (e *accountStop) Error() string {
	if e.cause == nil {
		return fmt.Sprintf("%s: %s", ErrAccountStopped, e.reason)
	}
	return fmt.Sprintf("%s: %s: %v", ErrAccountStopped, e.reason, e.cause)
}

func (e *accountStop) Unwrap() error { return ErrAccountStopped }

// DirectSend is a manual touch that took place. Cold names the cost: a cold
// touch spent the account daily cap, a warm reply did not.
type DirectSend struct {
	ID       int64
	Username string
	UserID   int64
	Cold     bool
}

// Conversation is a dialog that was read: the history at the time of the request
// and, separately, what arrived while waiting. They are kept apart because the
// tool answer names them differently.
type Conversation struct {
	Username string
	UserID   int64
	Messages []ChatMessage
	// Fresh holds the incoming messages that arrived while waiting; empty if the wait timed out.
	Fresh  []ChatMessage
	Waited time.Duration
}

// Find looks a person up by username or name. The search writes nothing outward
// and is not limited by the gates: the allow-list covers sending, not reading.
func (c *Chat) Find(ctx context.Context, query string) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query: give a username or a name")
	}
	found, err := c.transport.Find(ctx, query, maxFound)
	if err != nil {
		return nil, c.guard(ctx, query, err)
	}
	if len(found) > maxFound {
		found = found[:maxFound]
	}
	return found, nil
}

// Send writes to a person by hand. The gates are the same as for the mailing,
// except pauses: the human sets the pace. The window and the daily cap apply
// only to the first touch of a stranger - a reply in an ongoing dialog is free
// at any time, otherwise a ban on answering at 21:30 makes the account look less
// like a live one, not more.
//
// The journal row is written before the network call: "Telegram confirmed - the
// process died - no record" would leave the person open to the next campaign.
func (c *Chat) Send(ctx context.Context, username, text string) (DirectSend, error) {
	name := normalizeUsername(username)
	if name == "" {
		return DirectSend{}, c.refuse(username, fmt.Errorf("%w: %q", ErrNoUsername, username))
	}
	if strings.TrimSpace(text) == "" {
		return DirectSend{}, c.refuse(name, ErrEmptyText)
	}
	if length := telegramLen(text); length > maxTextLen {
		return DirectSend{}, c.refuse(name, fmt.Errorf("%w: %d symbols", ErrTextTooLong, length))
	}

	// The account stop is checked twice: here, to avoid a pointless trip to
	// Telegram, and again under the row lock, where the decision is made.
	stopped, stopReason, err := c.service.AccountStopped(ctx, c.label)
	if err != nil {
		return DirectSend{}, err
	}
	if stopped {
		return DirectSend{}, c.refuse(name, &accountStop{reason: stopReason})
	}

	peer, err := c.transport.Resolve(ctx, name)
	if err != nil {
		return DirectSend{}, c.refuse(name, c.guard(ctx, name, err))
	}
	userID, private := privateUserID(peer.Ref)
	if !private {
		return DirectSend{}, c.refuse(name, fmt.Errorf("%w: @%s", ErrNotPrivate, name))
	}

	// A service recipient is never cold: the window and the cap do not apply to
	// them, and there is no need to read the history to answer this question.
	cold := false
	if !c.service.isOwner(name) {
		cold, err = c.coldContact(ctx, name, peer)
		if err != nil {
			return DirectSend{}, c.refuse(name, c.guard(ctx, name, err))
		}
	}

	// random_id is fresh on every call, unlike the campaign: a dialog is full of
	// identical replies, and a stable id would make Telegram swallow a second
	// "ok" as a duplicate.
	randomID := newRandomID()
	id, err := c.openDirect(ctx, name, userID, text, randomID, cold)
	if err != nil {
		return DirectSend{}, c.refuse(name, err)
	}

	messageID, sendErr := c.transport.Send(ctx, peer, text, randomID)
	outcome := classifyDelivery(sendErr, c.service.cfg.FloodWaitThreshold)

	// The only question the outcome answers: is it known that the message does
	// not exist. It is known when our own gate refused - the network was never
	// reached - and when the server parsed the request and refused to execute it.
	// The opposite is unknown: a dropped connection, a timeout and any
	// unrecognized failure, above all a 5xx, after which the message may well
	// exist.
	var failure *telegram.Failure
	notCreated := errors.Is(sendErr, ErrNotAllowed) ||
		(errors.As(sendErr, &failure) && failure.NotSent())

	if outcome.stop {
		if err := c.stopAccount(ctx, name, outcome.reason); err != nil {
			return DirectSend{}, err
		}
		// A failure that stopped the account goes out as a stop: the next step is
		// set by the stop reason, not by the failure itself. Advice like "wait
		// until 14:20" would send the agent around again: it would wait, retry and
		// get "account is stopped".
		sendErr = &accountStop{reason: outcome.reason, cause: sendErr}
	}

	switch {
	case outcome.result == ResultSent:
		if err := c.closeDirect(ctx, id, name, "sent", "", userID, messageID); err != nil {
			return DirectSend{}, err
		}
		slog.Info("direct message sent",
			"account", c.label, "username", name, "direct_id", id,
			"user_id", userID, "cold", cold, "random_id", randomID, "message_id", messageID)
		return DirectSend{ID: id, Username: name, UserID: userID, Cold: cold}, nil
	// The message was certainly not created: the row goes to failed and the
	// person is available to campaigns again. A manual row has no queue to
	// return to, and pending would close the person to all campaigns forever.
	// The account stop on PEER_FLOOD and a long FLOOD_WAIT sits above and does
	// not depend on this branch: the account stops and the touch closes - two
	// consequences of one failure.
	case notCreated:
		if err := c.closeDirect(ctx, id, name, "failed", outcome.reason, userID, 0); err != nil {
			return DirectSend{}, err
		}
		slog.Warn("direct message undelivered", "username", name, "reason", outcome.reason)
	default:
		// The message may have gone out: the row stays pending and closes the
		// person. This is the safe side, and status shows its cost.
		slog.Warn("direct message delivery unknown",
			"username", name, "reason", outcome.reason, "direct_id", id)
	}
	return DirectSend{}, sendErr
}

// Read returns the dialog and, if asked to wait, waits for the other side's
// reply. Waiting is history polling, not a second update subscriber: the gotd
// handler is one per event type and is called synchronously, so a blocked
// subscriber would stop campaign reply detection.
func (c *Chat) Read(ctx context.Context, username string, depth int, waitFor time.Duration) (Conversation, error) {
	name := normalizeUsername(username)
	if name == "" {
		return Conversation{}, fmt.Errorf("%w: %q", ErrNoUsername, username)
	}
	depth = capped(depth, defaultHistory, maxHistory)
	waitFor = cappedWait(waitFor)

	peer, err := c.transport.Lookup(ctx, name)
	if err != nil {
		return Conversation{}, c.guard(ctx, name, err)
	}
	history, err := c.transport.History(ctx, peer, depth)
	if err != nil {
		return Conversation{}, c.guard(ctx, name, err)
	}
	userID, _ := privateUserID(peer.Ref)
	talk := Conversation{Username: name, UserID: userID, Messages: history}
	if waitFor == 0 {
		return talk, nil
	}

	// The wait ends with whichever comes first: the polling steps or the service
	// clock. The steps end it when the clock is frozen, the clock when the
	// polling itself turned out slow and the wait cap would otherwise be exceeded.
	deadline := c.service.Now().Add(waitFor)
	last := lastMessageID(history)
	waited := time.Duration(0)
	for waited < waitFor && c.service.Now().Before(deadline) {
		pause := pollInterval
		if left := waitFor - waited; left < pause {
			pause = left
		}
		if !wait(ctx, pause) {
			return talk, ctx.Err()
		}
		waited += pause

		fresh, err := c.transport.History(ctx, peer, depth)
		if err != nil {
			return talk, c.guard(ctx, name, err)
		}
		if incoming := newIncoming(fresh, last); len(incoming) > 0 {
			talk.Fresh = incoming
			talk.Waited = waited
			return talk, nil
		}
	}
	talk.Waited = waited
	return talk, nil
}

// Dialogs returns dialogs of all three kinds - person, group, channel - in
// Telegram order; mcp.go names the kind of each row from Ref and Broadcast.
func (c *Chat) Dialogs(ctx context.Context, limit int) ([]DialogInfo, error) {
	limit = capped(limit, defaultHistory, maxHistory)
	dialogs, err := c.transport.Dialogs(ctx, limit)
	if err != nil {
		return nil, c.guard(ctx, "", err)
	}
	return dialogs, nil
}

// coldContact answers whether this is a cold touch. A sent journal row makes the
// history check unnecessary - we have already written to this person; otherwise
// a correspondent with at least one incoming message counts as warm.
func (c *Chat) coldContact(ctx context.Context, name string, peer Peer) (bool, error) {
	var written bool
	if err := c.service.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM direct_messages WHERE username = $1 AND state = 'sent')`,
		name).Scan(&written); err != nil {
		return false, fmt.Errorf("read direct journal of %s: %w", name, err)
	}
	if written {
		return false, nil
	}
	history, err := c.transport.History(ctx, peer, defaultHistory)
	if err != nil {
		return false, fmt.Errorf("read history of %s: %w", name, err)
	}
	for _, msg := range history {
		if !msg.Outgoing {
			return false, nil
		}
	}
	return true, nil
}

// openDirect writes the journal row under the same account row lock that
// ClaimNext takes: the manual-send and mailing gates decide about one account,
// and they must decide in turn. The lock order is shared with ClaimNext, and
// accounts comes first in both - there is no deadlock.
func (c *Chat) openDirect(ctx context.Context, name string, userID int64, text string, randomID int64, cold bool) (int64, error) {
	now := c.service.Now()
	var id int64
	err := db.InTx(ctx, c.service.pool, func(tx pgx.Tx) error {
		var accountID int64
		var dailyLimit int
		var stopped bool
		var stopReason string
		err := tx.QueryRow(ctx,
			`SELECT id, daily_limit, stopped, stop_reason FROM accounts WHERE label = $1 FOR UPDATE`,
			c.label).Scan(&accountID, &dailyLimit, &stopped, &stopReason)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("account %q is not registered", c.label)
		}
		if err != nil {
			return fmt.Errorf("lock account: %w", err)
		}
		if stopped {
			return fmt.Errorf("%w: %s", ErrAccountStopped, stopReason)
		}

		var blocked bool
		if err := tx.QueryRow(ctx, directStopListSQL, name).Scan(&blocked); err != nil {
			return fmt.Errorf("check stop list: %w", err)
		}
		if blocked {
			return fmt.Errorf("%w: @%s", ErrStopList, name)
		}

		// A repeat on top of the caller's own unclosed touch. It stands before the
		// cap: the same logical touch must not charge a second unit, and the
		// pending row closes the person anyway.
		var pendingAt time.Time
		switch err := tx.QueryRow(ctx, directPendingSQL, name, c.service.staleBefore(now)).Scan(&pendingAt); {
		case err == nil:
			return &pendingWait{username: name, left: pendingAt.Add(c.service.cfg.ClaimStaleAfter).Sub(now)}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("check pending direct message: %w", err)
		}

		if cold {
			if !c.service.inSendWindow(now) {
				return fmt.Errorf("%w: @%s", ErrColdWindow, name)
			}
			var used int
			if err := tx.QueryRow(ctx, c.service.accountUsedSQL,
				startOfDay(now), c.service.staleBefore(now), accountID).Scan(&used); err != nil {
				return fmt.Errorf("count daily usage: %w", err)
			}
			if used >= dailyLimit {
				return fmt.Errorf("%w: %d of %d", ErrColdLimit, used, dailyLimit)
			}
		}

		// The hold while a campaign is running does not apply to a service
		// recipient. The accepted cost is two messages in a row, manual and
		// campaign: that is acceptable for the owner's accounts, while for
		// strangers the hold stays.
		if !c.service.isOwner(name) {
			hold := campaignHold{
				username:     name,
				queueOpensAt: c.service.nextWindow(now),
				staleAfter:   c.service.cfg.ClaimStaleAfter,
			}
			var template string
			switch err := tx.QueryRow(ctx, campaignSendingSQL, name, c.service.staleBefore(now)).
				Scan(&hold.id, &hold.title, &hold.status, &template); {
			case err == nil:
				_, broken := badBraces(template)
				hold.broken = broken > 0
				return &hold
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("check campaign queue: %w", err)
			}
		}

		// $2::bigint is an explicit type: without the cast NULLIF infers int4, and
		// a real Telegram id above 2^31 cannot be encoded at all.
		return tx.QueryRow(ctx,
			`INSERT INTO direct_messages
			 (username, tg_user_id, account_id, text, random_id, cold, state, created_at)
			 VALUES ($1, NULLIF($2::bigint, 0), $3, $4, $5, $6, 'pending', $7)
			 RETURNING id`,
			name, userID, accountID, text, randomID, cold, now).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// closeDirect closes the journal row by the send outcome. In the same pass,
// failed returns to the queue the rows this touch had removed: otherwise the
// person gets neither the manual message nor the campaign one, and the reason in
// the report would be false. message_id is stored only on success: a zero id
// means a duplicate by random_id, Telegram created no second message and there
// is no number to give it. $6::bigint is cast explicitly, like tg_user_id:
// without the cast NULLIF infers int4.
func (c *Chat) closeDirect(ctx context.Context, id int64, name, state, reason string, userID int64, messageID int) error {
	now := c.service.Now()
	return db.InTx(ctx, c.service.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE direct_messages
			 SET state = $2, reason = $3,
			     sent_at = CASE WHEN $2 = 'sent' THEN $4 ELSE sent_at END,
			     tg_user_id = COALESCE(NULLIF($5::bigint, 0), tg_user_id),
			     message_id = COALESCE(NULLIF($6::bigint, 0), message_id)
			 WHERE id = $1`,
			id, state, reason, now, userID, messageID); err != nil {
			return fmt.Errorf("close direct message %d: %w", id, err)
		}
		if state != "failed" {
			return nil
		}
		if _, err := tx.Exec(ctx, returnTouchedSQL, name); err != nil {
			return fmt.Errorf("return rows of %s: %w", name, err)
		}
		return nil
	})
}

// guard handles an account restriction whatever call it came from: a resolve, a
// history read or the send itself. The restriction belongs to the account, not
// to the method (invariant 3): contacts.resolveUsername answers FLOOD_WAIT
// before sending, and without this branch the agent would get "wait" and spend
// one more resolve per retry on a restricted account. Reading tools come here
// too: their session and limits are shared with sending, and a false stop costs
// one resume_account against the price of the account. Other failures go out
// as they are.
func (c *Chat) guard(ctx context.Context, username string, err error) error {
	if err == nil {
		return nil
	}
	outcome := classifyDelivery(err, c.service.cfg.FloodWaitThreshold)
	if !outcome.stop {
		return err
	}
	if stopErr := c.stopAccount(ctx, username, outcome.reason); stopErr != nil {
		return stopErr
	}
	return &accountStop{reason: outcome.reason, cause: err}
}

// stopAccount records the stop: the log first, then the database. It uses its
// own context, not the request context: the MCP client may drop the call right
// at PEER_FLOOD, and then the stop would exist only in the log - neither the
// healthcheck nor the mailing would see it. An active stop must be visible from
// outside, otherwise it does not exist.
func (c *Chat) stopAccount(ctx context.Context, username, reason string) error {
	slog.Error("account stopped by telegram limit",
		"account", c.label, "reason", reason, "username", username)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	return c.service.StopAccount(ctx, c.label, reason)
}

// nextWindow returns when the queue starts moving again; zero means "moving
// now". The refusal text needs it: a row held by a campaign outside the send
// window will not be moved by anyone until the next weekday morning, and the
// advice "try again in a few minutes" at 21:30 sends the agent into a wall.
func (s *Service) nextWindow(now time.Time) time.Time {
	if s.inSendWindow(now) {
		return time.Time{}
	}
	from, err := parseClock(s.cfg.SendWindowStart)
	if err != nil {
		return time.Time{}
	}
	local := now.In(moscowZone)
	open := startOfDay(local).Add(time.Duration(from) * time.Minute)
	for !open.After(local) || open.Weekday() == time.Saturday || open.Weekday() == time.Sunday {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

// refuse names a gate refusal in the log: without it the refusal goes only into
// the answer text to the agent and disappears with the dialog.
func (c *Chat) refuse(username string, err error) error {
	slog.Warn("direct send refused", "account", c.label, "username", username, "error", err)
	return err
}

// directTouched answers whether this person was written to by hand. The sender
// calls it: a reply in a manual dialog has no campaign row, and without this
// check it would turn the WARN about an unmatched incoming message into
// background noise.
func (s *Service) directTouched(ctx context.Context, userID int64, username string) (bool, error) {
	name := normalizeUsername(username)
	if name == "" && userID <= 0 {
		return false, nil
	}
	var touched bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM direct_messages
		 WHERE state <> 'failed'
		   AND (($1::bigint > 0 AND tg_user_id = $1) OR ($2 <> '' AND username = $2)))`,
		userID, name).Scan(&touched); err != nil {
		return false, fmt.Errorf("read direct journal: %w", err)
	}
	return touched, nil
}

// The stop list is queried by two keys, not one: an entry without a username
// (only amo_contact_id) would otherwise not close manual sending, while the
// invariant "a person on the stop list receives no message" is stated as
// absolute. A person's username is linked to the contact through campaign rows.
const directStopListSQL = `SELECT EXISTS (
	SELECT 1 FROM stop_list s
	WHERE s.username = $1
	   OR EXISTS (SELECT 1 FROM recipients r
	              WHERE r.username = $1 AND r.amo_contact_id = s.amo_contact_id))`

// A race with the mailing: this person's row has already been through the
// sender, and its delivery is unknown. The sign of an attempt is account_id: only
// claim sets it, and the row stays with the account forever, while claimed_at is
// cleared by the return to the queue after a temporary failure ($2 is the
// staleness bound).
//
// Only campaigns that can still send block: in cancelled and done the queue is
// already cleared, there is nothing to release that row with, and the block
// would be permanent. A live claim blocks under any status: the sender is
// carrying the row right now, and cancelling the campaign does not stop it.
// Draft is not in the list: only claim sets account_id, claim works on an active
// campaign, and the status never goes back to draft.
//
// The campaign itself is returned with its status, not a yes/no: the refusal
// must name who holds the person and what to do about it, and every status has
// its own action.
const campaignSendingSQL = `SELECT c.id, c.title, c.status, c.template_text
	FROM recipients r
	JOIN campaigns c ON c.id = r.campaign_id
	WHERE r.username = $1 AND r.state = 'planned' AND r.account_id IS NOT NULL
	  AND (c.status IN ('active', 'stopped') OR r.claimed_at >= $2)
	ORDER BY r.id
	LIMIT 1`

// directPendingSQL finds a fresh unclosed touch to this person ($2 is the
// staleness bound). While it is younger than the bound, a repeat is refused: the
// message may have gone out, and a second row would charge another unit of the
// daily cap for it.
const directPendingSQL = `SELECT created_at FROM direct_messages
	WHERE username = $1 AND state = 'pending' AND created_at >= $2
	ORDER BY id DESC LIMIT 1`

// returnTouchedSQL returns to the queue the rows this manual touch had removed:
// the send did not happen, and the person is available to campaigns again.
// Closed campaigns are not touched - their queue is already cleared.
const returnTouchedSQL = `UPDATE recipients AS r
	SET state = 'planned', reason = ''
	FROM campaigns c
	WHERE c.id = r.campaign_id AND c.status IN ('active', 'stopped', 'draft')
	  AND r.username = $1 AND r.state = 'skipped'
	  AND r.reason IN ('` + ReasonDirectSent + `', '` + ReasonDirectPending + `')`

// privateUserID is the "private chats only" gate: a channel username resolves as
// successfully as a person's. An empty ref occurs in dry-run and is not a refusal.
func privateUserID(ref telegram.Ref) (int64, bool) {
	if ref == "" {
		return 0, true
	}
	id, err := ref.UserID()
	if err != nil {
		return 0, false
	}
	return id, true
}

// capped clamps the requested size to the bounds; zero means "default".
func capped(value, fallback, limit int) int {
	switch {
	case value <= 0:
		return fallback
	case value > limit:
		return limit
	default:
		return value
	}
}

// cappedWait clamps the requested wait to the bounds. Zero means "do not wait":
// the read tool must answer immediately if no wait was requested.
func cappedWait(pause time.Duration) time.Duration {
	switch {
	case pause <= 0:
		return 0
	case pause > maxWait:
		return maxWait
	default:
		return pause
	}
}

func lastMessageID(history []ChatMessage) int {
	last := 0
	for _, msg := range history {
		if msg.ID > last {
			last = msg.ID
		}
	}
	return last
}

// newIncoming returns the other side's replies newer than the last known
// message. We wait only for incoming ones: our own send from a neighboring call
// is not a reply.
func newIncoming(history []ChatMessage, lastID int) []ChatMessage {
	var fresh []ChatMessage
	for _, msg := range history {
		if msg.ID > lastID && !msg.Outgoing {
			fresh = append(fresh, msg)
		}
	}
	return fresh
}
