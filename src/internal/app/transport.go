package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Transport is the service's boundary to the outside: find a recipient by
// username and send them text. The interface is a deliberate exception to the
// "no premature interfaces" rule: the second implementation (dry-run) has
// existed from day one and stays forever as the mode of the dev stack.
type Transport interface {
	Resolve(ctx context.Context, username string) (Peer, error)
	// Send returns the id of the sent message: it goes into the journal of manual
	// touches. Zero means "Telegram did not name an id" - that is how a duplicate
	// by random_id looks, and a value must not be invented in place of zero.
	Send(ctx context.Context, peer Peer, text string, randomID int64) (int, error)
}

// Peer is a found recipient. The username sits next to the reference because
// the last allow-list guard relies on it, and a Telegram reference no longer contains the username.
type Peer struct {
	Username string
	Ref      telegram.Ref
}

// Dialog extends Transport with reading and search over the same session:
// manual correspondence and acceptance testing of bot dialogs. Embedding,
// not a separate interface, leaves Transport untouched - Sender still sees
// only Resolve/Send, and stubTransport in sender_test.go does not grow
// empty methods.
//
// Lookup exists separately from Resolve on purpose: Resolve carries the
// checkAllowed guard (DEV_ALLOW_LIST), through it goes the campaign and manual
// send to a live person, and the guard must not be lifted. But reading a dialog
// must work with a non-empty allow-list too - otherwise the dev stack cannot
// read a dialog with someone who can no longer be written to. The split resolves
// this conflict: Lookup does not apply the guard, Resolve stays as it is. Without
// separating these two paths, the next edit either leaves reading broken in dev or
// removes checkAllowed from Resolve - and that is a hole in live sending. The test
// TestAllowListStillHoldsCampaignSend covers the second risk.
type Dialog interface {
	Transport
	// Lookup resolves a recipient for reading, without the allow-list guard.
	Lookup(ctx context.Context, username string) (Peer, error)
	Find(ctx context.Context, query string, limit int) ([]Contact, error)
	History(ctx context.Context, peer Peer, limit int) ([]ChatMessage, error)
	Dialogs(ctx context.Context, limit int) ([]DialogInfo, error)
}

// Contact is a person found by search, not necessarily a recipient. There is no
// reference to them here on purpose: sending and reading resolve the username
// anew, and a field resembling a ready address would invite writing to it without resolving.
type Contact struct {
	Username string
	Name     string
	// IsContact means the person is in the account's address book.
	IsContact bool
	// Known means the address was already known to the service. It is not a sign
	// of a dialog: a username resolved for reading lands here too.
	Known bool
	// Exact means the username equals the query, case-insensitively: this is the one
	// that was searched for, not a similar username of another person.
	Exact bool
}

// ChatMessage is a dialog message in a flat form for the answer to the agent.
type ChatMessage struct {
	ID        int
	Text      string
	MediaKind string
	SentAt    time.Time
	Outgoing  bool
}

// DialogInfo is a row of the dialog list with the last message.
type DialogInfo struct {
	Username string
	Name     string
	Ref      telegram.Ref
	Unread   int
	// Broadcast is a broadcast channel, not a place for correspondence. A bare platform fact;
	// the word for the output is assembled by mcp.go from the Ref kind and this flag.
	Broadcast bool
	Last      ChatMessage
}

// ErrNotAllowed is the refusal of the last guard: a username outside the allow-list.
var ErrNotAllowed = errors.New("recipient is not in the allow list")

// ReasonNotAllowed is the reason of a row removed by the allow-list.
const ReasonNotAllowed = "not_allowed"

// Delivery is a recorded dry-run send: what would have gone to whom live.
type Delivery struct {
	Username string
	Text     string
	RandomID int64
}

// DryRunTransport is the transport of the dev stack: it writes the send to the
// log and remembers the calls, and never goes outside. Not a single message
// can reach a live person from it, so an end-to-end campaign run is safe.
type DryRunTransport struct {
	allowed map[string]bool

	mu   sync.Mutex
	sent []Delivery
}

// NewDryRunTransport builds a dry-run with the allow-list guard from the config.
func NewDryRunTransport(cfg Config) *DryRunTransport {
	return &DryRunTransport{allowed: allowedSet(cfg.DevAllowList)}
}

func (t *DryRunTransport) Resolve(_ context.Context, username string) (Peer, error) {
	if err := checkAllowed(t.allowed, username); err != nil {
		return Peer{}, err
	}
	return Peer{Username: username}, nil
}

// Send in dry-run returns a synthetic increasing id - the send number in its
// own journal: there is no live id, and a journal row with NULL is
// indistinguishable from a write failure.
func (t *DryRunTransport) Send(_ context.Context, peer Peer, text string, randomID int64) (int, error) {
	if err := checkAllowed(t.allowed, peer.Username); err != nil {
		return 0, err
	}
	t.mu.Lock()
	t.sent = append(t.sent, Delivery{Username: peer.Username, Text: text, RandomID: randomID})
	messageID := len(t.sent)
	t.mu.Unlock()

	slog.Info("dry-run send", "username", peer.Username, "random_id", randomID, "text", text)
	return messageID, nil
}

// Sent returns a copy of the recorded sends.
func (t *DryRunTransport) Sent() []Delivery {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Delivery(nil), t.sent...)
}

// Lookup in dry-run goes nowhere - there is no live Telegram at all, so it
// returns a fake Peer by username without the guard. The honesty of the fake
// answer is held by the MCP tool with a separate line in the text.
func (t *DryRunTransport) Lookup(_ context.Context, username string) (Peer, error) {
	return Peer{Username: username}, nil
}

// Find in dry-run returns one synthetic contact for a query: there is no real
// search, and the response format must match the live transport. The username
// is normalized by the same CleanQuery as the live search, and a query shaped
// like a username is marked exact: otherwise the answer would warn "no exact
// username" about a username it returned itself.
func (t *DryRunTransport) Find(_ context.Context, query string, _ int) ([]Contact, error) {
	needle := telegram.CleanQuery(query)
	return []Contact{{Username: needle, Name: query, Exact: telegram.IsUsername(needle)}}, nil
}

// History in dry-run returns its own journal t.sent as outgoing messages -
// the only history dry-run really has, there are no incoming ones.
func (t *DryRunTransport) History(_ context.Context, peer Peer, limit int) ([]ChatMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var history []ChatMessage
	for _, delivery := range t.sent {
		if delivery.Username != peer.Username {
			continue
		}
		history = append(history, ChatMessage{Text: delivery.Text, Outgoing: true})
	}
	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}
	return history, nil
}

// Dialogs in dry-run returns empty: a dialog list of a live account cannot be
// faked honestly, unlike History, where there is its own send journal.
func (t *DryRunTransport) Dialogs(_ context.Context, _ int) ([]DialogInfo, error) {
	return nil, nil
}

// LiveTransport is live MTProto over the telegram package. It is not started
// in this slice: the config does not allow TRANSPORT=live outside prod.
type LiveTransport struct {
	allowed map[string]bool
	client  *telegram.Client
}

// NewLiveTransport builds the live transport with the same allow-list guard.
func NewLiveTransport(cfg Config, client *telegram.Client) *LiveTransport {
	return &LiveTransport{allowed: allowedSet(cfg.DevAllowList), client: client}
}

func (t *LiveTransport) Resolve(ctx context.Context, username string) (Peer, error) {
	if err := checkAllowed(t.allowed, username); err != nil {
		return Peer{}, err
	}
	ref, err := t.client.ResolveUsername(ctx, username)
	if err != nil {
		return Peer{}, err
	}
	return Peer{Username: username, Ref: ref}, nil
}

// Send returns the outcome as is: a random_id recognized by Telegram is a
// success, the client already returned it without an error, and the person did
// not get a second message. The id of such a duplicate is zero, and zero goes into the journal as is.
func (t *LiveTransport) Send(ctx context.Context, peer Peer, text string, randomID int64) (int, error) {
	if err := checkAllowed(t.allowed, peer.Username); err != nil {
		return 0, err
	}
	result, err := t.client.SendText(ctx, peer.Ref, text, randomID)
	return result.MessageID, err
}

// Lookup resolves a recipient for reading without the allow-list guard - see
// the comment on the Dialog interface. A send does not go through this Peer, so
// bypassing checkAllowed here is safe.
func (t *LiveTransport) Lookup(ctx context.Context, username string) (Peer, error) {
	ref, err := t.client.ResolveUsername(ctx, username)
	if err != nil {
		return Peer{}, err
	}
	return Peer{Username: username, Ref: ref}, nil
}

func (t *LiveTransport) Find(ctx context.Context, query string, limit int) ([]Contact, error) {
	found, err := t.client.SearchUsers(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	contacts := make([]Contact, 0, len(found))
	for _, f := range found {
		contacts = append(contacts, Contact{
			Username:  f.Username,
			Name:      f.Name,
			IsContact: f.IsContact,
			Known:     f.Known,
			Exact:     f.Exact,
		})
	}
	return contacts, nil
}

// History takes the newest page of history: beforeID=0 in the telegram client
// means "from the last message".
func (t *LiveTransport) History(ctx context.Context, peer Peer, limit int) ([]ChatMessage, error) {
	messages, err := t.client.History(ctx, peer.Ref, limit, 0)
	if err != nil {
		return nil, err
	}
	history := make([]ChatMessage, 0, len(messages))
	for _, m := range messages {
		history = append(history, chatMessageFrom(m))
	}
	return history, nil
}

func (t *LiveTransport) Dialogs(ctx context.Context, limit int) ([]DialogInfo, error) {
	dialogs, err := t.client.Dialogs(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]DialogInfo, 0, len(dialogs))
	for _, d := range dialogs {
		result = append(result, DialogInfo{
			Username:  d.Username,
			Name:      d.Name,
			Ref:       d.Peer,
			Unread:    d.Unread,
			Broadcast: d.Broadcast,
			Last:      chatMessageFrom(d.Last),
		})
	}
	return result, nil
}

// chatMessageFrom converts a telegram package message to the flat ChatMessage
// form - a step shared by History and the last message in Dialogs.
func chatMessageFrom(m telegram.Message) ChatMessage {
	return ChatMessage{ID: m.ID, Text: m.Text, MediaKind: m.MediaKind, SentAt: m.SentAt, Outgoing: m.Outgoing}
}

// allowedSet is the set of allowed usernames; nil means "no restriction".
func allowedSet(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	set := make(map[string]bool, len(list))
	for _, name := range list {
		set[name] = true
	}
	return set
}

// checkAllowed is the last guard before going outside, behind all the DB guards.
// An empty list restricts nobody (the live campaign goes by the campaign's
// list), a non-empty one lets through only those listed - both the live
// transport and dry-run alike: the guard must not depend on which transport
// the process was started with.
func checkAllowed(allowed map[string]bool, username string) error {
	if allowed == nil || allowed[normalizeUsername(username)] {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNotAllowed, username)
}
