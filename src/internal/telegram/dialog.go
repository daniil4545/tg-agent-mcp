package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
)

// Found is a person found by search.
type Found struct {
	Ref      Ref
	Username string
	Name     string
	// IsContact means the person is in the account's address book.
	IsContact bool
	// Known means the person's address was already in local storage: we
	// corresponded with them, resolved their username or they got into the warm-up.
	// It does not mean a dialog exists - resolving a username for reading puts it there too.
	Known bool
	// Exact means the person's username equals the query, case-insensitively. A prefix
	// search returns similar usernames of other people, and without the mark they
	// are indistinguishable from the one searched for.
	Exact bool
}

// usernameForm is what Telegram accepts as a username at all. Without the check,
// a search by name would spend a resolve on a certain non-username, and the queue a minute on a row.
var usernameForm = regexp.MustCompile(`^[a-z][a-z0-9_]{3,30}[a-z0-9]$`)

// IsUsername reports whether a string has the shape of a Telegram username: 5-32
// Latin letters, digits and underscore, not at the end and not doubled. The
// leading @ and the case are removed by CleanQuery, there is no second normalization here.
func IsUsername(name string) bool {
	return usernameForm.MatchString(name) && !strings.Contains(name, "__")
}

// CleanQuery brings a search query to the form in which it is compared with a
// username: without spaces and the leading @, lowercase. The answer to the agent
// shows the same form that was searched for, otherwise "@Ivan" would give "@@" in the text.
// Spaces are removed twice: "@ ivan_petrov" after removing @ is left with a
// leading space and stops being a username by shape.
func CleanQuery(query string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), "@")))
}

// Dialog is a row of the dialog list: a recipient and the last message.
type Dialog struct {
	Peer     Ref
	Username string
	Name     string
	Unread   int
	// Broadcast is the channel flag on the Telegram side: broadcasting, not
	// correspondence. A bare platform fact without interpretation: the word for the
	// output is assembled by the caller from the reference kind and this flag.
	Broadcast bool
	Last      Message
}

// SearchUsers looks for a person by username or name.
//
// First the local recipient storage - zero requests to Telegram and a full
// answer for those already known; then an exact username resolve, if it was not
// found locally; then a search on the server.
func (c *Client) SearchUsers(ctx context.Context, query string, limit int) ([]Found, error) {
	api, peers, err := c.session()
	if err != nil {
		return nil, err
	}
	needle := CleanQuery(query)
	if needle == "" {
		return nil, permanent(KindBadRequest, "empty search query")
	}
	if limit <= 0 {
		return nil, permanent(KindBadRequest, "search limit must be positive")
	}

	found := make([]Found, 0, limit)
	seen := make(map[int64]bool, limit)
	var exact bool

	iter, err := peers.Iterate(ctx)
	if err != nil {
		return nil, fmt.Errorf("iterate telegram peers: %w", err)
	}
	for len(found) < limit && iter.Next(ctx) {
		user := iter.Value().User
		if user == nil || seen[user.GetID()] || !matches(user, needle) {
			continue
		}
		seen[user.GetID()] = true
		hit := strings.ToLower(publicUsername(user)) == needle
		exact = exact || hit
		found = append(found, Found{
			Ref:       NewRef(KindUser, user.GetID()),
			Username:  publicUsername(user),
			Name:      fullName(user),
			IsContact: user.GetContact(),
			Known:     true,
			Exact:     hit,
		})
	}
	iterErr := iter.Err()
	if closeErr := iter.Close(); closeErr != nil {
		slog.Warn("close telegram peer iterator", "error", closeErr)
	}
	if iterErr != nil {
		return nil, fmt.Errorf("iterate telegram peers: %w", iterErr)
	}

	// doubt means the exact-username resolve failed, and "no such person" does not
	// follow from it: contacts.resolveUsername is hard-limited on a live number and
	// answers FLOOD_WAIT while contacts.search works and returns empty.
	var doubt error
	if !exact && IsUsername(needle) {
		hit, id, err := c.resolveExact(ctx, peers, needle)
		switch {
		case err != nil:
			// "Username not taken" is a normal outcome of searching by a name that looks like
			// a username, not a failure: a WARN on it becomes background noise in exactly the
			// log where account stops are supposed to be noticed. Only a missing answer is logged.
			if unchecked(err) {
				slog.Warn("exact username resolve failed", "username", needle, "error", err)
				doubt = err
			}
		case !seen[id]:
			seen[id] = true
			found = append([]Found{hit}, found...)
		default:
			// The same person was already found locally by a stale username from the peer
			// cache: they changed the username, the id stayed. Dropping the resolve
			// entirely would say "no exact username" about the one we searched for.
			ref := NewRef(KindUser, id)
			for i := range found {
				if found[i].Ref == ref {
					found[i].Exact = true
					found[i].Username = hit.Username
					break
				}
			}
		}
	}
	if len(found) >= limit {
		return found[:limit], nil
	}

	result, err := api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: needle, Limit: limit})
	if err != nil {
		return nil, classify(err)
	}
	users := tg.UserClassArray(result.Users).UserToMap()
	// MyResults are matches from the address book: the server separates them from
	// the general search, and this is the only contact sign for an answer unknown to us.
	mine := make(map[int64]bool, len(result.MyResults))
	for _, item := range result.MyResults {
		if peer, ok := item.(*tg.PeerUser); ok {
			mine[peer.UserID] = true
		}
	}
	for _, batch := range [][]tg.PeerClass{result.MyResults, result.Results} {
		for _, item := range batch {
			if len(found) >= limit {
				break
			}
			peer, ok := item.(*tg.PeerUser)
			if !ok || seen[peer.UserID] {
				continue
			}
			user, ok := users[peer.UserID]
			if !ok {
				continue
			}
			seen[peer.UserID] = true
			found = append(found, Found{
				Ref:       NewRef(KindUser, peer.UserID),
				Username:  publicUsername(user),
				Name:      fullName(user),
				IsContact: user.GetContact() || mine[peer.UserID],
				// A prefix search returns the exact username too, if the resolve did not
				// reach it or failed.
				Exact: strings.ToLower(publicUsername(user)) == needle,
			})
		}
	}
	// Checking the exact username failed and the fuzzy search found nothing: "nobody
	// found" would be a lie here - the person may exist, and tg_send will write to
	// them the same second. The reason for the failure goes out.
	if len(found) == 0 && doubt != nil {
		return nil, doubt
	}
	return found, nil
}

// unchecked means the username resolve gave no answer about the person: a limit,
// a drop or a dead session. A refusal on the merits ("username not taken",
// "username is not a person") is an answer, and an empty result after it is
// truthful; a limit, a drop and a 5xx mean nothing, and passing them off as
// "nobody found" is not allowed.
func unchecked(err error) bool {
	var failure *Failure
	if !errors.As(err, &failure) {
		return true
	}
	switch failure.Kind {
	case KindFloodWait, KindPeerFlood, KindNotReady, KindAuthForbidden:
		return true
	}
	return !failure.NotSent()
}

// resolveExact is an exact username match through ResolveUsername. A separate
// request is needed because a stranger is neither in local storage nor in the
// contacts.search results: without the resolve tg_find answers "nobody" about a
// person that tg_send would write to.
func (c *Client) resolveExact(ctx context.Context, peers storage.PeerStorage, needle string) (Found, int64, error) {
	ref, err := c.ResolveUsername(ctx, needle)
	if err != nil {
		return Found{}, 0, err
	}
	id, err := ref.UserID()
	if err != nil {
		return Found{}, 0, err
	}
	// We take the name from storage: ResolveUsername has just put the person there,
	// and no second request is needed for the name.
	peer, err := storage.FindPeer(ctx, peers, &tg.PeerUser{UserID: id})
	if err != nil {
		return Found{}, 0, fmt.Errorf("find resolved peer @%s: %w", needle, err)
	}
	if peer.User == nil {
		return Found{}, 0, permanent(KindNotPerson, "@"+needle+" is not a person")
	}
	return Found{
		Ref:       ref,
		Username:  publicUsername(peer.User),
		Name:      fullName(peer.User),
		IsContact: peer.User.GetContact(),
		Exact:     true,
	}, id, nil
}

// History returns a page of correspondence from old messages to new. beforeID
// limits the result to messages older than it, zero means "from the last".
func (c *Client) History(ctx context.Context, peer Ref, limit, beforeID int) ([]Message, error) {
	api, peers, err := c.session()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, permanent(KindBadRequest, "history depth must be positive")
	}
	target, err := resolve(ctx, peers, peer)
	if err != nil {
		return nil, err
	}

	response, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     target,
		Limit:    limit,
		OffsetID: beforeID,
	})
	if err != nil {
		return nil, classify(err)
	}
	modified, ok := response.AsModified()
	if !ok {
		return nil, permanent(KindBadRequest, "Telegram did not return the history")
	}

	entities := tg.Entities{Users: tg.UserClassArray(modified.GetUsers()).UserToMap()}
	items := modified.GetMessages()
	// Telegram returns history newest first, but it is read top to bottom, so the
	// walk goes from the end. Service and empty messages are skipped.
	history := make([]Message, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(*tg.Message)
		if !ok {
			continue
		}
		message, ok := fromMessage(item, entities)
		if !ok {
			continue
		}
		history = append(history, message)
	}
	return history, nil
}

// Dialogs returns dialogs of all three kinds - person, group, channel - newest
// on top.
func (c *Client) Dialogs(ctx context.Context, limit int) ([]Dialog, error) {
	api, peers, err := c.session()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, permanent(KindBadRequest, "dialog limit must be positive")
	}

	// The counter is manual: the library's Collect walks all the account's dialogs
	// to the end, and there are hundreds on a live number.
	iter := query.GetDialogs(api).BatchSize(dialogsBatch).Iter()
	dialogs := make([]Dialog, 0, limit)
	for len(dialogs) < limit && iter.Next(ctx) {
		item := iter.Value()
		// The conversion is required: DialogClass has three kinds, and not every
		// one has a recipient.
		dialog, ok := item.Dialog.(*tg.Dialog)
		if !ok {
			continue
		}
		ref, ok := refFromPeer(dialog.Peer)
		if !ok {
			continue
		}

		entities := tg.Entities{
			Users:    item.Entities.Users(),
			Chats:    item.Entities.Chats(),
			Channels: item.Entities.Channels(),
		}
		found := Dialog{Peer: ref, Unread: dialog.UnreadCount}
		switch peer := dialog.Peer.(type) {
		case *tg.PeerUser:
			found.Username = username(entities, peer.UserID)
			found.Name = name(entities, peer.UserID)
		case *tg.PeerChat:
			// A regular group has neither a username nor an access hash: it is addressed by an id alone.
			if chat, ok := entities.Chats[peer.ChatID]; ok {
				found.Name = chat.Title
			}
		case *tg.PeerChannel:
			channel, ok := entities.Channels[peer.ChannelID]
			if !ok {
				break
			}
			found.Name, found.Broadcast = channel.Title, channel.Broadcast
			found.Username = pickUsername(channel.Username, channel.Usernames)
			// The only place where a channel's access hash is taken: a private
			// supergroup has no username, and it cannot be fetched later by a resolve.
			// FromChat answers false on a min constructor - such a hash is unusable.
			var stored storage.Peer
			if stored.FromChat(channel) {
				// Caching the hash is a side benefit of the list, not what the tool was
				// called for: a failed write does not cancel the already parsed result.
				if err := peers.Add(ctx, stored); err != nil {
					slog.Warn("persist dialog channel", "channel_id", peer.ChannelID, "error", err)
				}
			}
		}
		if last, ok := item.Last.(*tg.Message); ok {
			if message, ok := fromMessage(last, entities); ok {
				found.Last = message
			}
		}
		dialogs = append(dialogs, found)
	}
	if err := iter.Err(); err != nil {
		return nil, classify(err)
	}
	return dialogs, nil
}

// pickUsername returns the public username from a pair of Telegram fields: the
// main field, then the first active one of the collectible ones. The fields are
// laid out the same for a person and a channel, so one parser serves both kinds.
func pickUsername(main string, extra []tg.Username) string {
	if main != "" {
		return main
	}
	for _, candidate := range extra {
		if candidate.Active && candidate.Username != "" {
			return candidate.Username
		}
	}
	return ""
}

// matches is a rough match by username and name, case-insensitive. The exact
// username is looked up by ResolveUsername, and here a person is recalled by a fragment.
func matches(user *tg.User, needle string) bool {
	return strings.Contains(strings.ToLower(publicUsername(user)), needle) ||
		strings.Contains(strings.ToLower(fullName(user)), needle)
}
