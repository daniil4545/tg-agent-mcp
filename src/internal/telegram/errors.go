package telegram

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/tgerr"
)

// Failure is a Telegram error parsed into "retry or not".
//
// The service cares not about the error text but about the decision: postpone
// the work, abandon it forever or raise an alarm. So the parsing lives here,
// once, and does not spread across call sites as string comparisons.
type Failure struct {
	// Kind is the reason in one word, suitable for a log and a metric.
	Kind string
	// Message is what happened, in human language.
	Message string
	// Type is the MTProto error code (MSG_ID_INVALID, FLOOD_WAIT) or empty if the
	// error is not from Telegram. Without it the caller sees "temporary Telegram
	// error" and does not know what exactly the server answered.
	Type string
	// Permanent means a retry will not help: the same attempt gives the same error.
	Permanent bool
	// RetryAt is not before this moment, if Telegram named the time itself.
	RetryAt time.Time
}

// NotSent answers the only question the caller has after a refusal: is it known
// that the message does not exist. The answer comes from the kind of refusal,
// not from the presence of a code: either the server parsed the request and
// refused to execute it (a restriction, a closed private chat, a bad request,
// a dead session), or the matter never reached the server - the client decided
// itself or the local recipient storage failed.
//
// Exactly one case is unknown - KindTemporary. A connection drop, a timeout and
// any unrecognized code land there, and an unrecognized code is first of all a
// 5xx (RPC_CALL_FAIL, INTERNAL_SERVER_ERROR, -503 Timeout): the server may have
// created the message and failed only after that. Treating such a refusal as
// "no message" means writing to the person a second time.
func (f *Failure) NotSent() bool {
	return f.Kind != KindTemporary
}

func (f *Failure) Error() string {
	if f.Type == "" {
		return f.Kind + ": " + f.Message
	}
	return f.Kind + ": " + f.Message + " (" + f.Type + ")"
}

// Error kinds. The list is closed: a new line here means a new service
// decision, not a new text in the log.
const (
	KindNotReady      = "telegram_not_ready"      // client not started
	KindFloodWait     = "telegram_flood_wait"     // Telegram asks to wait
	KindPeerFlood     = "telegram_peer_flood"     // account restricted from messaging strangers
	KindPeerForbidden = "telegram_peer_forbidden" // writing to this recipient is not allowed
	KindNotPerson     = "telegram_not_person"     // username is taken by a group or channel
	KindMediaInvalid  = "telegram_media_invalid"  // attachment rejected
	KindAuthForbidden = "telegram_auth_forbidden" // session is no longer authorized
	KindBadRequest    = "telegram_bad_request"    // malformed request from the service
	KindStorage       = "telegram_storage"        // own recipient storage failed before the call
	KindTemporary     = "telegram_temporary"      // anything else, cured by a retry
)

func permanent(kind, message string) *Failure {
	return &Failure{Kind: kind, Message: message, Permanent: true}
}

// classify turns a gotd error into a decision. Anything unrecognized is treated
// as temporary: an unknown error abandoned forever silently loses a message,
// and an extra retry costs one attempt.
func classify(err error) error {
	if err == nil {
		return nil
	}
	// Stopping the service is not a Telegram error.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	failure := reason(err)
	if rpc, ok := tgerr.As(err); ok {
		failure.Type = rpc.Type
	}
	return failure
}

// reason is the service decision on an error, without the MTProto code: classify
// sets it, once for all branches.
func reason(err error) *Failure {
	// PEER_FLOOD gets its own branch: AsFloodWait does not recognize it (it matches
	// only FLOOD_WAIT and FLOOD_PREMIUM_WAIT), Telegram names no time for it, and it
	// is not a pause but a restriction of the account on messages to strangers -
	// it cannot be solved by a retry.
	if tgerr.Is(err, "PEER_FLOOD") {
		return permanent(KindPeerFlood, "account restricted from messaging strangers")
	}
	if wait, ok := tgerr.AsFloodWait(err); ok {
		return &Failure{
			Kind:    KindFloodWait,
			Message: "Telegram asks to postpone the retry",
			RetryAt: time.Now().Add(wait),
		}
	}
	if errors.Is(err, storage.ErrPeerNotFound) || tgerr.Is(err,
		"PEER_ID_INVALID",
		"CHAT_WRITE_FORBIDDEN",
		"USER_IS_BLOCKED",
		"USER_PRIVACY_RESTRICTED",
		"INPUT_USER_DEACTIVATED",
	) {
		return permanent(KindPeerForbidden, "recipient is unavailable for sending")
	}
	// A refusal about the file itself is not cured by a retry: the bytes are the
	// same from attempt to attempt. Without this branch the send would spend three
	// file uploads and the pauses between them to arrive at the same result.
	if tgerr.Is(err,
		"MEDIA_INVALID",
		"MEDIA_EMPTY",
		"PHOTO_INVALID_DIMENSIONS",
		"PHOTO_EXT_INVALID",
		"STICKER_FILE_INVALID",
		"VIDEO_FILE_INVALID",
		"FILE_PARTS_INVALID",
	) {
		return permanent(KindMediaInvalid, "Telegram rejected the attachment")
	}
	// A bad request: an empty or too short search, a nonexistent message id, a
	// rejected text. A retry of the same request gives the same refusal, and
	// treating it as temporary means spinning it until the attempts run out.
	//
	// A refusal about the text belongs here too, and it is not minor: the message
	// is certainly not created, while "retry later" leaves the journal row in
	// pending forever, closing the person to all campaigns. MESSAGE_TOO_LONG comes
	// on a live length: our own check counts runes, Telegram counts UTF-16 units.
	if tgerr.Is(err,
		"MSG_ID_INVALID",
		"QUERY_TOO_SHORT",
		"SEARCH_QUERY_EMPTY",
		"OFFSET_PEER_ID_INVALID",
		"MESSAGE_TOO_LONG",
		"MESSAGE_EMPTY",
	) {
		return permanent(KindBadRequest, "Telegram rejected the request")
	}
	// A dead session is not a "temporary network error": the service must stop and
	// call a human, otherwise it will silently retry forever.
	if tgerr.Is(err,
		"AUTH_KEY_UNREGISTERED",
		"AUTH_KEY_INVALID",
		"SESSION_EXPIRED",
		"SESSION_REVOKED",
		"USER_DEACTIVATED",
		"USER_DEACTIVATED_BAN",
		"FROZEN_PARTICIPANT_MISSING",
	) {
		return permanent(KindAuthForbidden, "session is no longer authorized")
	}
	return &Failure{Kind: KindTemporary, Message: "temporary Telegram error"}
}
