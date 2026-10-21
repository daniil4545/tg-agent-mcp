package telegram

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// Kind is the kind of recipient. Telegram is limited to three kinds: a person,
// a regular group, a supergroup or channel.
type Kind string

const (
	KindUser    Kind = "user"
	KindChat    Kind = "chat"    // regular group
	KindChannel Kind = "channel" // supergroup or channel
)

// Ref is a reference to a recipient in the form `user:123`, `chat:123`, `channel:123`.
//
// A string, not a struct, because this identifier lives in the DB, in queue
// keys and in logs. The canonical form has no leading zeros: otherwise one
// recipient would get two different queue keys and two "first" messages.
type Ref string

// NewRef builds a reference to a recipient.
func NewRef(kind Kind, id int64) Ref {
	return Ref(string(kind) + ":" + strconv.FormatInt(id, 10))
}

// Split parses a reference. An error means the data is corrupted on the
// service side, not a Telegram refusal.
func (r Ref) Split() (Kind, int64, error) {
	kind, rawID, ok := strings.Cut(string(r), ":")
	if !ok || rawID == "" {
		return "", 0, fmt.Errorf("invalid peer ref %q", string(r))
	}
	switch Kind(kind) {
	case KindUser, KindChat, KindChannel:
	default:
		return "", 0, fmt.Errorf("invalid peer ref %q", string(r))
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rawID {
		return "", 0, fmt.Errorf("invalid peer ref %q", string(r))
	}
	return Kind(kind), id, nil
}

// UserID parses a reference that must point to a person. It is needed where a
// group recipient is a data error, not another scenario.
func (r Ref) UserID() (int64, error) {
	kind, id, err := r.Split()
	if err != nil {
		return 0, err
	}
	if kind != KindUser {
		return 0, fmt.Errorf("peer ref %q is not a user", string(r))
	}
	return id, nil
}

// refFromPeer turns a recipient from an update into a reference.
func refFromPeer(peer tg.PeerClass) (Ref, bool) {
	switch value := peer.(type) {
	case *tg.PeerUser:
		if value.UserID <= 0 {
			return "", false
		}
		return NewRef(KindUser, value.UserID), true
	case *tg.PeerChat:
		if value.ChatID <= 0 {
			return "", false
		}
		return NewRef(KindChat, value.ChatID), true
	case *tg.PeerChannel:
		if value.ChannelID <= 0 {
			return "", false
		}
		return NewRef(KindChannel, value.ChannelID), true
	default:
		return "", false
	}
}
