package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Sender is the sending daemon. One loop per process and one account (ACCOUNT_LABEL):
// there is deliberately no goroutine pool, the campaign pace is held by pauses,
// not by parallelism, and a second account is a second container.
type Sender struct {
	service   *Service
	transport Transport
	label     string

	// chat is the direct-send path for the evening digest. The digest has no path
	// of its own: Chat.Send already holds the account stop, peer resolution and
	// the touch journal. Empty means no digest at all.
	chat *Chat

	// lastIdle is the idle reason already reported in the log. Without this memory
	// a closed window would write the same line every iteration all night.
	lastIdle string
}

// UseChat attaches the dialog that carries the evening digest. A separate
// method, not a constructor argument: Chat is built on the same transport later,
// after the sender has already been handed to the incoming-message callback.
func (s *Sender) UseChat(chat *Chat) { s.chat = chat }

// idleUnavailable stands in for the reason when it could not be read. The value
// deliberately differs from the real reasons: otherwise a returning database
// would stay silent about the queue state.
const idleUnavailable = "idle_reason_unavailable"

// NewSender builds a sender on a service and a transport.
func NewSender(service *Service, transport Transport) *Sender {
	return &Sender{service: service, transport: transport, label: service.cfg.AccountLabel}
}

// Run holds the loop until the context is stopped. There is no in-memory state:
// each iteration asks the database again, so a restart resumes the campaign in
// place. The account row is created by process startup (cmd), not by the loop:
// the healthcheck asks for it before the sender manages to come up.
func (s *Sender) Run(ctx context.Context) error {
	slog.Info("sender started", "account", s.label)

	for {
		if err := s.step(ctx); err != nil {
			// Stopping the service is not an error: an unclosed row
			// goes stale by claim and returns to the queue.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// The digest is called here, not at the tail of step: in the evening the queue
		// is empty and step returns earlier on that branch. Its error does not stop
		// the loop - sending the messages matters more than the report about them.
		if err := s.maybeSendDigest(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("digest failed", "error", err)
		}
		if !wait(ctx, s.pause()) {
			return nil
		}
	}
}

// step is one iteration: take a row and deliver it. The guards (window, cap,
// account stop, stop list) sit inside ClaimNext - an empty answer means there
// is nothing to send now or it is not allowed.
func (s *Sender) step(ctx context.Context) error {
	item, err := s.service.ClaimNext(ctx, s.label)
	if err != nil {
		return err
	}
	if item == nil {
		s.logIdle(ctx)
		done, err := s.service.FinishIfEmpty(ctx)
		if err != nil {
			return err
		}
		if done {
			slog.Info("campaign finished")
		}
		return nil
	}
	s.lastIdle = ""
	return s.SendClaimed(ctx, item)
}

// SendClaimed delivers a claimed row and closes it by outcome. Network calls
// run outside transactions: the claim is already committed, and finalization is
// a separate short transaction after Telegram's answer.
func (s *Sender) SendClaimed(ctx context.Context, item *Recipient) error {
	peer, err := s.transport.Resolve(ctx, item.Username)
	if err == nil {
		// The person's id survives resolution into the DB: a short incoming update
		// carries no username, and without the id this person's reply would not find its row.
		if userID, refErr := peer.Ref.UserID(); refErr == nil {
			item.UserID = userID
		}
		// The campaign does not need the message id: the recipient row is identified by
		// random_id, and only the manual touch keeps an id journal.
		_, err = s.transport.Send(ctx, peer, item.Text, item.RandomID)
	}

	outcome := classifyDelivery(err, s.service.cfg.FloodWaitThreshold)
	switch {
	case outcome.stop:
		// An active stop must be visible from outside: a guard nobody
		// learned about equals no guard.
		slog.Error("account stopped by telegram limit",
			"account", s.label, "reason", outcome.reason, "username", item.Username)
	case outcome.result == ResultSent:
		// Delivery trail: without it the analysis of "why the message did not go out on
		// time" goes through the database, and the log shows only failures.
		slog.Info("message sent",
			"account", s.label, "username", item.Username, "recipient_id", item.ID,
			"campaign_id", item.CampaignID, "user_id", item.UserID, "random_id", item.RandomID)
	case outcome.result == ResultUndelivered:
		slog.Warn("recipient undelivered", "username", item.Username, "reason", outcome.reason)
	case outcome.result == ResultRetry, outcome.result == ResultRetryCounted:
		slog.Warn("delivery postponed", "username", item.Username, "reason", outcome.reason)
	}

	// The account stop is written first: the guard must not depend on whether the
	// second write succeeded. A failed finalization leaves the row under a
	// going-stale claim, while an unwritten stop would leave the account under a Telegram restriction in the campaign.
	if outcome.stop {
		if err := s.service.StopAccount(ctx, s.label, outcome.reason); err != nil {
			return err
		}
	}
	closed, err := s.service.FinishClaim(ctx, item, outcome.result, outcome.reason)
	if err != nil {
		return err
	}
	// The delivery happened but there is nowhere to record it. If the row is
	// already sent, another process with the same label closed it after taking
	// over a stale claim: the delivery fact is in the DB, nobody to alert. Anything
	// else is a real divergence, and this log line is its only trace.
	if !closed && outcome.result == ResultSent {
		var state string
		readErr := s.service.pool.QueryRow(ctx,
			`SELECT state FROM recipients WHERE id = $1`, item.ID).Scan(&state)
		if readErr == nil && state == "sent" {
			slog.Info("already recorded by another process",
				"recipient_id", item.ID, "username", item.Username)
		} else {
			slog.Error("delivered message was not recorded",
				"recipient_id", item.ID, "username", item.Username, "state", state, "error", readErr)
		}
	}
	if outcome.wait > 0 && !wait(ctx, outcome.wait) {
		return ctx.Err()
	}
	return nil
}

// OnMessage is the connection point to the live transport: telegram.Client
// hands the normalized incoming message here (signature telegram.Handler). The
// link with the gotd dispatcher itself is not part of the slice and runs live
// only against a real account.
//
// A private message from a person closes their row: a sent one moves to
// replied, a not yet sent one too, and the invitation is cancelled.
func (s *Sender) OnMessage(ctx context.Context, msg telegram.Message) error {
	if msg.Outgoing {
		return nil
	}
	// Private chats only: a reply in a group is not an answer to the invitation.
	kind, userID, err := msg.Peer.Split()
	if err != nil || kind != telegram.KindUser {
		return nil
	}
	rows, err := s.service.MarkReplied(ctx, userID, msg.Username)
	if err != nil {
		return err
	}
	// A person who wrote first is taken off the queue separately from those who
	// replied: the template does not go over a conversation they started, but it is
	// not a reply to a message they never received either.
	first, err := s.service.markWroteFirst(ctx, userID, msg.Username)
	if err != nil {
		return err
	}
	if first > 0 {
		slog.Info("recipient wrote first", "user_id", userID, "username", msg.Username, "rows", first)
	}
	if rows == 0 && first == 0 {
		// A reply in a manual dialog has no campaign row and is not a defect:
		// this WARN means a real loss of a reply, and manual touches
		// would turn it into background noise.
		touched, err := s.service.directTouched(ctx, userID, msg.Username)
		if err != nil {
			slog.Warn("direct journal unavailable", "username", msg.Username, "error", err)
		} else if touched {
			slog.Info("inbound message in manual dialog", "user_id", userID, "username", msg.Username)
			return nil
		}
		// An unmatched incoming message is not necessarily a stranger: it may also
		// be an update without a username from someone we wrote to before tg_user_id
		// appeared. Staying silent here is not allowed: a reply that did not close a
		// row leaves the person in the queue for a second touch.
		slog.Warn("inbound message not matched",
			"user_id", userID, "username", msg.Username)
		return nil
	}
	slog.Info("recipient replied", "user_id", userID, "username", msg.Username, "rows", rows)
	return nil
}

// logIdle names the reason for silence once per appearance. The reason itself
// is diagnostic: it is enough to log the error of reading it, stopping the
// loop over it is not allowed - it does not hinder sending.
func (s *Sender) logIdle(ctx context.Context) {
	idle, err := s.service.IdleReason(ctx, s.label)
	if err != nil {
		// A diagnostics failure is deduplicated like a reason: an unreachable DB
		// lasts for minutes, and many iterations can pass meanwhile.
		if s.lastIdle != idleUnavailable {
			s.lastIdle = idleUnavailable
			slog.Warn("idle reason unavailable", "account", s.label, "error", err)
		}
		return
	}
	if idle.Reason == s.lastIdle {
		return
	}
	s.lastIdle = idle.Reason
	// A broken template is not a normal idle: the queue stands until a person
	// rebuilds the campaign, and not only the one who opens status must know.
	// Other reasons pass by themselves: the window opens, the cap resets.
	if idle.Reason == IdleBrokenTemplate {
		slog.Error("sender idle", "account", s.label, "reason", idle.Reason)
		return
	}
	slog.Info("sender idle", "account", s.label, "reason", idle.Reason)
}

// pause is a random delay between iterations within the config bounds: an even
// pace looks machine-made, while the campaign runs through a live account.
func (s *Sender) pause() time.Duration {
	low, high := s.service.cfg.SendPauseMin, s.service.cfg.SendPauseMax
	if high <= low {
		return low
	}
	return low + rand.N(high-low)
}

// delivery is the service's decision on a send outcome.
type delivery struct {
	result SendResult
	reason string
	// stop is an account-level refusal: sending from it ceases entirely.
	stop bool
	// wait means wait before the next row if Telegram named a time.
	wait time.Duration
}

// classifyDelivery turns a send outcome into a decision. One analysis for all
// refusals: FLOOD_WAIT shorter than the threshold - wait and return the row to
// the queue, longer - account stop; PEER_FLOOD and a dead session are also an
// account stop, there is nothing left to send from it; a refusal about the
// person - undelivered; anything unrecognized - retry, because an unknown
// error abandoned forever loses a person silently.
//
// Only an unrecognized refusal spends an attempt: it alone can repeat stably
// on one row. An account-level refusal does not hold the queue - sending from
// this account stops entirely - and does not touch the row's budget.
func classifyDelivery(err error, threshold time.Duration) delivery {
	if err == nil {
		return delivery{result: ResultSent}
	}
	if errors.Is(err, ErrNotAllowed) {
		return delivery{result: ResultUndelivered, reason: ReasonNotAllowed}
	}

	var failure *telegram.Failure
	if !errors.As(err, &failure) {
		return delivery{result: ResultRetryCounted, reason: "unknown_error"}
	}
	switch failure.Kind {
	case telegram.KindFloodWait:
		pause := time.Until(failure.RetryAt)
		if pause >= threshold {
			return delivery{result: ResultRetry, reason: failure.Kind, stop: true}
		}
		return delivery{result: ResultRetry, reason: failure.Kind, wait: pause}
	case telegram.KindAuthForbidden, telegram.KindPeerFlood:
		return delivery{result: ResultRetry, reason: failure.Kind, stop: true}
	case telegram.KindPeerForbidden, telegram.KindNotPerson, telegram.KindBadRequest, telegram.KindMediaInvalid:
		return delivery{result: ResultUndelivered, reason: failure.Kind}
	default:
		return delivery{result: ResultRetryCounted, reason: failure.Kind}
	}
}

// StopAccount stops the account in the DB. The reason of the first stop is not
// overwritten: it names why the account is under restriction, and repeated
// refusals are only a consequence.
func (s *Service) StopAccount(ctx context.Context, label, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE accounts SET stopped = true, stop_reason = $2, stopped_at = $3
		 WHERE label = $1 AND NOT stopped`,
		label, reason, s.Now())
	if err != nil {
		return fmt.Errorf("stop account %q: %w", label, err)
	}
	return nil
}

// ResumeAccount lifts the stop. Only by an explicit human command: lifting on a
// timer would bring back sending from an account that Telegram still holds
// under restriction.
//
// The reason of the lifted stop is returned together with the "was stopped"
// flag: reading the state with a separate query is not allowed - a stop set
// between the read and the lift would be lifted silently, and the old reason
// would go into the response and the log.
func (s *Service) ResumeAccount(ctx context.Context, label string) (reason string, stopped bool, err error) {
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx,
			`SELECT stopped, stop_reason FROM accounts WHERE label = $1 FOR UPDATE`,
			label).Scan(&stopped, &reason)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return fmt.Errorf("account %q is not registered", label)
		}
		if scanErr != nil {
			return fmt.Errorf("lock account %q: %w", label, scanErr)
		}
		if !stopped {
			return nil
		}
		if _, execErr := tx.Exec(ctx,
			`UPDATE accounts SET stopped = false, stop_reason = '', stopped_at = NULL WHERE label = $1`,
			label); execErr != nil {
			return fmt.Errorf("resume account %q: %w", label, execErr)
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, stopped, nil
}

// AccountStopped returns the account state: the container healthcheck and the
// status response rely on it.
func (s *Service) AccountStopped(ctx context.Context, label string) (bool, string, error) {
	var stopped bool
	var reason string
	err := s.pool.QueryRow(ctx, `SELECT stopped, stop_reason FROM accounts WHERE label = $1`, label).
		Scan(&stopped, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("account %q is not registered", label)
	}
	if err != nil {
		return false, "", fmt.Errorf("read account %q: %w", label, err)
	}
	return stopped, reason, nil
}

// wait waits while respecting shutdown and returns false if the process was
// asked to finish. time.Sleep is unsuitable here: it does not release the
// process on a signal and does not let a test run the loop fast.
func wait(ctx context.Context, pause time.Duration) bool {
	if pause <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
