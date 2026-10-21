package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	bolt "go.etcd.io/bbolt"
)

// A recipient reference lives in the DB and in queue keys, so one recipient
// must have exactly one form: otherwise they get two "first" messages.
func TestRefRejectsNonCanonicalForms(t *testing.T) {
	bad := []Ref{
		"user:007",   // leading zeros would give the same recipient a second key
		"user:0",     //
		"user:-1",    //
		"user:",      //
		"user",       //
		"group:12",   // Telegram has no group kind
		"USER:12",    //
		"user:12:34", // tail after the id
	}
	for _, ref := range bad {
		if _, _, err := ref.Split(); err == nil {
			t.Errorf("reference %q accepted", string(ref))
		}
	}

	kind, id, err := Ref("channel:1234567890").Split()
	if err != nil || kind != KindChannel || id != 1234567890 {
		t.Fatalf("parse gave %v %d %v", kind, id, err)
	}
	if got := NewRef(KindUser, 42); got != "user:42" {
		t.Fatalf("build gave %q", string(got))
	}
}

// A group recipient where the service expects a person is a data error, not
// another scenario: without this check a message for one person goes to a chat.
func TestUserIDRejectsGroups(t *testing.T) {
	if _, err := Ref("user:42").UserID(); err != nil {
		t.Fatalf("person rejected: %v", err)
	}
	for _, ref := range []Ref{"chat:42", "channel:42"} {
		if _, err := ref.UserID(); err == nil {
			t.Errorf("group reference %q accepted as a person", string(ref))
		}
	}
}

func TestNormalizeKeepsGroupsAndDropsNoise(t *testing.T) {
	cases := []struct {
		name    string
		message tg.MessageClass
		want    bool
		peer    Ref
	}{
		{
			name:    "private chat with text",
			message: &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Message: "hi"},
			want:    true,
			peer:    "user:7",
		},
		{
			// One package for all services: the one that reads channels must receive
			// their messages, and selection by recipient kind is done by the service.
			name:    "channel with text",
			message: &tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: 9}, Message: "announcement"},
			want:    true,
			peer:    "channel:9",
		},
		{
			name:    "service message",
			message: &tg.MessageService{ID: 3, PeerID: &tg.PeerUser{UserID: 7}},
			want:    false,
		},
		{
			name:    "empty without attachment",
			message: &tg.Message{ID: 4, PeerID: &tg.PeerUser{UserID: 7}, Message: "   "},
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := normalize(&tg.UpdateNewMessage{Message: c.message}, tg.Entities{})
			if ok != c.want {
				t.Fatalf("parsed=%v, expected %v", ok, c.want)
			}
			if ok && msg.Peer != c.peer {
				t.Fatalf("recipient %q, expected %q", string(msg.Peer), string(c.peer))
			}
		})
	}
}

// An attachment without a caption is a full message: a voice message and a photo
// arrive with no text at all, and a silent drop would lose them entirely.
func TestNormalizeKeepsMediaWithoutCaption(t *testing.T) {
	msg, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID:     5,
		PeerID: &tg.PeerUser{UserID: 7},
		Media:  &tg.MessageMediaDocument{},
	}}, tg.Entities{})
	if !ok {
		t.Fatal("attachment without a caption dropped")
	}
	if msg.MediaKind != "document" {
		t.Fatalf("attachment kind %q", msg.MediaKind)
	}

	// A web preview is the formatting of a link, not an attachment: treating it as
	// media sends ordinary text into the file-parsing branch.
	preview, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID:      6,
		PeerID:  &tg.PeerUser{UserID: 7},
		Message: "link",
		Media:   &tg.MessageMediaWebPage{},
	}}, tg.Entities{})
	if !ok || preview.MediaKind != "" {
		t.Fatalf("web preview parsed as an attachment: %q", preview.MediaKind)
	}
}

// In a private chat FromID is empty, and the sender is the counterpart
// themselves. Without the substitution the service would get a zero author on every incoming message.
func TestNormalizeFillsSenderInDirectMessages(t *testing.T) {
	msg, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID: 7, PeerID: &tg.PeerUser{UserID: 77}, Message: "text",
	}}, tg.Entities{})
	if !ok || msg.SenderID != 77 {
		t.Fatalf("sender %d, expected 77", msg.SenderID)
	}

	// In a group the sender and the recipient differ, and one must not be replaced by the other.
	group, ok := normalize(&tg.UpdateNewMessage{Message: withFlags(&tg.Message{
		ID: 8, PeerID: &tg.PeerChannel{ChannelID: 9},
		FromID: &tg.PeerUser{UserID: 55}, Message: "text",
	})}, tg.Entities{})
	if !ok || group.SenderID != 55 {
		t.Fatalf("sender in group %d, expected 55", group.SenderID)
	}
}

// withFlags sets the bit mask of optional fields.
//
// In gotd optional fields and flag markers are read not from struct fields but
// from Flags: GetOut returns Flags.Has(1), GetFromID checks Flags.Has(2).
// When parsing from the wire the decoder sets the mask, and a message built in a
// test without SetFlags looks incoming and without a sender, whatever lies in
// the fields. A test that forgets this call goes green on broken code.
func withFlags(message *tg.Message) *tg.Message {
	message.SetFlags()
	return message
}

// Own reply from the same account is not junk: a person also writes by hand
// from the work number, and their message is usually important to the service.
func TestNormalizeMarksOutgoing(t *testing.T) {
	msg, ok := normalize(&tg.UpdateNewMessage{Message: withFlags(&tg.Message{
		ID: 9, Out: true, PeerID: &tg.PeerUser{UserID: 7}, Message: "reply",
	})}, tg.Entities{})
	if !ok {
		t.Fatal("outgoing message dropped")
	}
	if !msg.Outgoing {
		t.Fatal("outgoing message not marked")
	}
}

func TestNormalizeTakesUsernameAndName(t *testing.T) {
	entities := tg.Entities{Users: map[int64]*tg.User{
		7: {ID: 7, FirstName: "Иван", LastName: "Петров", Usernames: []tg.Username{
			{Username: "old", Active: false},
			{Username: "ivan", Active: true},
		}},
	}}
	msg, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID: 10, PeerID: &tg.PeerUser{UserID: 7}, Message: "text",
	}}, entities)
	if !ok {
		t.Fatal("message dropped")
	}
	// An inactive username must not be taken: it already belongs to somebody else.
	if msg.Username != "ivan" {
		t.Fatalf("username %q", msg.Username)
	}
	if msg.Name != "Иван Петров" {
		t.Fatalf("name %q", msg.Name)
	}
}

// Classification decides whether to retry or give up. A mistake in it either
// loses a message forever or spins an endless retry.
func TestClassifySeparatesRetryFromPermanent(t *testing.T) {
	flood := classify(tgerr.New(420, "FLOOD_WAIT_30"))
	var failure *Failure
	if !errors.As(flood, &failure) || failure.Kind != KindFloodWait {
		t.Fatalf("flood wait parsed as %v", flood)
	}
	if failure.Permanent {
		t.Fatal("flood wait treated as permanent: the message would be lost")
	}
	if failure.RetryAt.IsZero() {
		t.Fatal("retry time not taken from the error")
	}
	if wait := time.Until(failure.RetryAt); wait < 25*time.Second {
		t.Fatalf("retry time %s, Telegram asked for 30s", wait)
	}

	permanentCases := map[string]string{
		"PEER_FLOOD":                 KindPeerFlood,
		"USER_IS_BLOCKED":            KindPeerForbidden,
		"CHAT_WRITE_FORBIDDEN":       KindPeerForbidden,
		"AUTH_KEY_UNREGISTERED":      KindAuthForbidden,
		"FROZEN_PARTICIPANT_MISSING": KindAuthForbidden,
		"MEDIA_INVALID":              KindMediaInvalid,
		// Read refusals: a retry of the same request gives the same answer, and
		// treating them as temporary means spinning it until the attempts run out.
		"MSG_ID_INVALID":         KindBadRequest,
		"QUERY_TOO_SHORT":        KindBadRequest,
		"SEARCH_QUERY_EMPTY":     KindBadRequest,
		"OFFSET_PEER_ID_INVALID": KindBadRequest,
		// Rejected text: the message is certainly not created, while "retry later"
		// leaves the journal row in pending forever and closes the person to
		// all campaigns without a single send.
		"MESSAGE_TOO_LONG": KindBadRequest,
		"MESSAGE_EMPTY":    KindBadRequest,
	}
	for code, kind := range permanentCases {
		err := classify(tgerr.New(400, code))
		if !errors.As(err, &failure) || failure.Kind != kind || !failure.Permanent {
			t.Errorf("%s parsed as %v", code, err)
		}
	}

	// An unknown error must be temporary: abandoned forever, it silently
	// loses a message, while an extra retry costs one attempt.
	unknown := classify(tgerr.New(500, "SOMETHING_NEW"))
	if !errors.As(unknown, &failure) || failure.Permanent {
		t.Fatalf("unknown error treated as permanent: %v", unknown)
	}
	// A 5xx with an unknown code is the only refusal after which delivery is
	// unknown: the server may have created the message and failed afterwards. The
	// code in Type changes nothing here, and "no message" would mean a second letter.
	if failure.NotSent() {
		t.Fatalf("5xx treated as certainly not sent: %v", unknown)
	}
	// Without the MTProto code in the text the caller sees "temporary Telegram
	// error", which tells nothing.
	if failure.Type != "SOMETHING_NEW" || !strings.Contains(unknown.Error(), "SOMETHING_NEW") {
		t.Fatalf("error code lost: %q", unknown.Error())
	}
}

func TestMediaInputDropsCaptionOnStickers(t *testing.T) {
	_, caption, err := mediaInput("photo.jpg", "caption")
	if err != nil || caption != "caption" {
		t.Fatalf("photo: caption %q, %v", caption, err)
	}

	// Telegram shows a sticker caption as a separate block, and a light touch
	// stops being light.
	_, caption, err = mediaInput("sticker.webm", "caption")
	if err != nil || caption != "" {
		t.Fatalf("sticker: caption %q, %v", caption, err)
	}

	// An unknown extension is rejected before the file upload and a retry does not cure it.
	if _, _, err := mediaInput("archive.zip", ""); err == nil {
		t.Fatal("unknown attachment accepted")
	} else if failure := new(Failure); !errors.As(err, &failure) || !failure.Permanent {
		t.Fatalf("attachment refusal is not permanent: %v", err)
	}
}

func TestResolverAcceptsOnlySocks5(t *testing.T) {
	if dialer, err := resolver(""); err != nil || dialer != nil {
		t.Fatalf("empty proxy gave %v, %v", dialer, err)
	}
	if _, err := resolver("socks5://10.0.0.1:20170"); err != nil {
		t.Fatalf("socks5 rejected: %v", err)
	}
	for _, bad := range []string{"http://10.0.0.1:3128", "socks5://10.0.0.1", "socks5://10.0.0.1:20170/path", "10.0.0.1:20170"} {
		if _, err := resolver(bad); err == nil {
			t.Errorf("proxy %q accepted", bad)
		}
	}
}

// A client built on a bad config would fail only in prod on the first send.
func TestNewValidatesConfigAndHandler(t *testing.T) {
	good := Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"}
	if _, err := New(good, func(context.Context, Message) error { return nil }); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if _, err := New(good, nil); err == nil {
		t.Fatal("client built without a handler: incoming messages would go nowhere")
	}

	bad := []Config{
		{AppHash: "h", Phone: "+7", StoragePath: "/data/s"},
		{AppID: 1, Phone: "+7", StoragePath: "/data/s"},
		{AppID: 1, AppHash: "h", StoragePath: "/data/s"},
		{AppID: 1, AppHash: "h", Phone: "+7"},
		{AppID: 1, AppHash: "h", Phone: "+7", StoragePath: "/data/s", ProxyURL: "http://p:1"},
	}
	for i, cfg := range bad {
		if _, err := New(cfg, func(context.Context, Message) error { return nil }); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
}

// The session storage is access to the live account: an open directory must
// stop the start, not fix itself silently.
func TestWithDBRefusesLooseDirectories(t *testing.T) {
	// t.TempDir returns 0755, so the session directory is created here:
	// the strictness about permissions is exactly what is checked.
	dir := t.TempDir() + "/tg"
	if err := withDB(dir+"/session.bolt", func(*bolt.DB) error { return nil }); err != nil {
		t.Fatalf("dedicated directory rejected: %v", err)
	}

	// A top-level directory means the session was put next to whatever.
	if err := withDB("session.bolt", func(*bolt.DB) error { return nil }); err == nil {
		t.Fatal("session accepted without a dedicated directory")
	}

	loose := t.TempDir() + "/loose"
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := withDB(loose+"/session.bolt", func(*bolt.DB) error { return nil }); err == nil {
		t.Fatal("directory with mode 0755 accepted")
	}
}

// A session of someone else's account in the storage would mean sending from the
// wrong number: login is skipped silently in this case, and only the number check catches it.
func TestSamePhoneComparesDigitsOnly(t *testing.T) {
	same := [][2]string{
		{"79001234567", "+7 900 123-45-67"},
		{"+79001234567", "79001234567"},
	}
	for _, pair := range same {
		if !samePhone(pair[0], pair[1]) {
			t.Errorf("numbers %q and %q considered different", pair[0], pair[1])
		}
	}

	other := [][2]string{
		{"79001234567", "79009999999"},
		{"", "79001234567"},
		{"+", "+"},
	}
	for _, pair := range other {
		if samePhone(pair[0], pair[1]) {
			t.Errorf("numbers %q and %q considered the same", pair[0], pair[1])
		}
	}
}

// The session fields are set and cleared by the Run goroutine and read by the
// MCP handlers from their own: without the lock a service stop in the middle of a
// read gives a race and a nil dereference - the `client started` check would pass and the field be zeroed right after.
func TestSessionSurvivesConcurrentStop(t *testing.T) {
	client, err := New(Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"},
		func(context.Context, Message) error { return nil })
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	api := tg.NewClient(stubInvoker{})
	peers := &stubPeers{}

	ctx := context.Background()
	stop := make(chan struct{})
	// Only an overlap catches the race: without waiting for the first read, the
	// Run goroutine sets and clears the session before the readers wake up.
	started := make(chan struct{}, 4)
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			first := true
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = client.SetTyping(ctx, "user:7")
				_, _ = client.ResolveUsername(ctx, "ivan")
				_, _ = client.Dialogs(ctx, 5)
				if first {
					started <- struct{}{}
					first = false
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-started
	}
	for i := 0; i < 2000; i++ {
		client.setSession(api, peers)
		client.setSession(nil, nil)
	}
	close(stop)
	readers.Wait()

	var failure *Failure
	if err := client.SetTyping(ctx, "user:7"); !errors.As(err, &failure) || failure.Kind != KindNotReady {
		t.Fatalf("stopped client answered %v", err)
	}
}

// Telegram returns history with newest messages first. Without reversing it, the
// service would show the agent the dialog backwards.
func TestHistoryReturnsOldestFirst(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		box, ok := output.(*tg.MessagesMessagesBox)
		if !ok {
			return tgerr.New(500, "UNEXPECTED_REQUEST")
		}
		box.Messages = &tg.MessagesMessages{
			Messages: []tg.MessageClass{
				&tg.Message{ID: 3, PeerID: &tg.PeerUser{UserID: 7}, Message: "third"},
				&tg.MessageService{ID: 2, PeerID: &tg.PeerUser{UserID: 7}},
				&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Message: "first",
					Media: &tg.MessageMediaPhoto{}},
			},
			Users: []tg.UserClass{&tg.User{ID: 7, FirstName: "Иван", Username: "ivan"}},
		}
		return nil
	}}, &tg.User{ID: 7, FirstName: "Иван", Username: "ivan"})

	history, err := client.History(context.Background(), "user:7", 20, 0)
	if err != nil {
		t.Fatalf("history not read: %v", err)
	}
	// The service message is dropped, the order is from old to new.
	if len(history) != 2 || history[0].ID != 1 || history[1].ID != 3 {
		t.Fatalf("history %v", history)
	}
	if history[0].MediaKind != "photo" || history[0].Username != "ivan" {
		t.Fatalf("first message parsed as %+v", history[0])
	}
}

// The list shows all three recipient kinds: a group has only a title, a
// channel also a username and the broadcast flag, and its access hash settles
// in storage - without it a private supergroup would become unreadable forever.
func TestDialogsTakeAllPeerKindsUpToLimit(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		box, ok := output.(*tg.MessagesDialogsBox)
		if !ok {
			return tgerr.New(500, "UNEXPECTED_REQUEST")
		}
		box.Dialogs = &tg.MessagesDialogs{
			Dialogs: []tg.DialogClass{
				&tg.Dialog{Peer: &tg.PeerChat{ChatID: 3}, TopMessage: 9, UnreadCount: 4},
				&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 5}, TopMessage: 8},
				&tg.Dialog{Peer: &tg.PeerUser{UserID: 7}, TopMessage: 5, UnreadCount: 2},
				&tg.Dialog{Peer: &tg.PeerUser{UserID: 8}, TopMessage: 6},
			},
			Messages: []tg.MessageClass{
				&tg.Message{ID: 5, PeerID: &tg.PeerUser{UserID: 7}, Message: "hi"},
			},
			Chats: []tg.ChatClass{
				&tg.Chat{ID: 3, Title: "Клуб"},
				&tg.Channel{ID: 5, AccessHash: 99, Title: "Анонсы", Username: "club_news", Broadcast: true},
			},
			Users: []tg.UserClass{&tg.User{ID: 7, FirstName: "Иван", Username: "ivan"}},
		}
		return nil
	}})

	dialogs, err := client.Dialogs(context.Background(), 3)
	if err != nil {
		t.Fatalf("dialogs not read: %v", err)
	}
	if len(dialogs) != 3 {
		t.Fatalf("limit not respected: %d dialogs", len(dialogs))
	}
	if got := dialogs[0]; got.Peer != "chat:3" || got.Name != "Клуб" || got.Username != "" || got.Unread != 4 {
		t.Fatalf("group parsed as %+v", got)
	}
	if got := dialogs[1]; got.Peer != "channel:5" || got.Name != "Анонсы" || got.Username != "club_news" || !got.Broadcast {
		t.Fatalf("channel parsed as %+v", got)
	}
	if got := dialogs[2]; got.Peer != "user:7" || got.Unread != 2 || got.Username != "ivan" || got.Last.Text != "hi" {
		t.Fatalf("private dialog parsed as %+v", got)
	}
	if peers, _ := client.peers.(*stubPeers); peers == nil || len(peers.channels) != 1 || peers.channels[0].AccessHash != 99 {
		t.Fatalf("channel did not reach the recipient storage")
	}
}

// Caching the access hash is a side benefit of the list: a failing disk deprives
// the next slice of the hash but does not cancel the already parsed result.
func TestDialogsSurviveStorageFailure(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		box, ok := output.(*tg.MessagesDialogsBox)
		if !ok {
			return tgerr.New(500, "BAD_STUB")
		}
		box.Dialogs = &tg.MessagesDialogs{
			Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 5}, TopMessage: 8}},
			Chats:   []tg.ChatClass{&tg.Channel{ID: 5, AccessHash: 99, Title: "Анонсы"}},
		}
		return nil
	}})
	peers, _ := client.peers.(*stubPeers)
	if peers == nil {
		t.Fatal("stub storage did not come up")
	}
	peers.failAdd = errors.New("disk full")

	dialogs, err := client.Dialogs(context.Background(), 5)
	if err != nil {
		t.Fatalf("write failure cancelled the result: %v", err)
	}
	if len(dialogs) != 1 || dialogs[0].Peer != "channel:5" || dialogs[0].Name != "Анонсы" {
		t.Fatalf("channel parsed as %+v", dialogs)
	}
}

// Local storage answers without a single request to Telegram, so it goes first;
// the server adds strangers, and one person must not appear in the
// answer twice.
func TestSearchUsersMergesLocalAndRemote(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		found, ok := output.(*tg.ContactsFound)
		if !ok {
			return tgerr.New(500, "UNEXPECTED_REQUEST")
		}
		*found = tg.ContactsFound{
			MyResults: []tg.PeerClass{&tg.PeerUser{UserID: 7}},
			Results:   []tg.PeerClass{&tg.PeerUser{UserID: 8}},
			Users: []tg.UserClass{
				&tg.User{ID: 7, FirstName: "Иван", Username: "ivan"},
				&tg.User{ID: 8, FirstName: "Иванов", Username: "ivanov"},
			},
		}
		return nil
	}}, &tg.User{ID: 7, FirstName: "Иван", Username: "ivan"})

	found, err := client.SearchUsers(context.Background(), "iva", 10)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %+v", found)
	}
	if found[0].Ref != "user:7" || !found[0].Known {
		t.Fatalf("acquaintance parsed as %+v", found[0])
	}
	// A stranger is not in storage: they can be written to only after resolving the username.
	if found[1].Ref != "user:8" || found[1].Known {
		t.Fatalf("stranger parsed as %+v", found[1])
	}
	if found[0].Exact || found[1].Exact {
		t.Fatalf("similar usernames marked exact: %+v", found)
	}

	// The same query by username: the local find is exact, the similar username from
	// contacts.search is not.
	found, err = client.SearchUsers(context.Background(), "ivan", 10)
	if err != nil || len(found) != 2 {
		t.Fatalf("search by username gave %+v (%v)", found, err)
	}
	if !found[0].Exact || found[1].Exact {
		t.Fatalf("exactness of the local find parsed as %+v", found)
	}
}

// A stranger by exact username is returned neither by local storage nor by
// contacts.search: without the resolve tg_find answers "nobody" about a person
// that tg_send would write to.
func TestSearchUsersResolvesExactUsername(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		switch box := output.(type) {
		case *tg.ContactsResolvedPeer:
			*box = tg.ContactsResolvedPeer{
				Peer:  &tg.PeerUser{UserID: 9},
				Users: []tg.UserClass{&tg.User{ID: 9, FirstName: "Пётр", Username: "petr_ivanov", AccessHash: 42}},
			}
		case *tg.ContactsFound:
			*box = tg.ContactsFound{}
		default:
			return tgerr.New(500, "UNEXPECTED_REQUEST")
		}
		return nil
	}})

	found, err := client.SearchUsers(context.Background(), "@Petr_Ivanov", 10)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	// Known false: the address was learned by this very resolve, and the person did not
	// become an acquaintance because of it - otherwise the report would promise a dialog that does not exist.
	if len(found) != 1 || found[0].Ref != "user:9" || found[0].Name != "Пётр" || found[0].Known || !found[0].Exact {
		t.Fatalf("exact username parsed as %+v", found)
	}

	// A failed resolve does not fail the search: the fuzzy part works as usual.
	client = runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		box, ok := output.(*tg.ContactsFound)
		if !ok {
			return tgerr.New(400, "USERNAME_NOT_OCCUPIED")
		}
		*box = tg.ContactsFound{
			Results: []tg.PeerClass{&tg.PeerUser{UserID: 9}},
			Users:   []tg.UserClass{&tg.User{ID: 9, FirstName: "Пётр", Username: "petr_ivanov"}},
		}
		return nil
	}})
	found, err = client.SearchUsers(context.Background(), "petr_ivano", 10)
	if err != nil || len(found) != 1 || found[0].Exact {
		t.Fatalf("after a failed resolve the search gave %+v (%v)", found, err)
	}

	// The same answer to an exact query: the resolve did not get through, and the exactness is visible from the username in
	// contacts.search.
	found, err = client.SearchUsers(context.Background(), "petr_ivanov", 10)
	if err != nil || len(found) != 1 || !found[0].Exact {
		t.Fatalf("exact username from contacts.search parsed as %+v (%v)", found, err)
	}

	// The resolve is broken by a limit and the fuzzy search is empty: "nobody found" here
	// is a lie - contacts.resolveUsername is limited on a live number, and the person
	// that tg_send will write to exists. The reason for the failure goes out.
	client = runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		box, ok := output.(*tg.ContactsFound)
		if !ok {
			return tgerr.New(420, "FLOOD_WAIT_30")
		}
		*box = tg.ContactsFound{}
		return nil
	}})
	found, err = client.SearchUsers(context.Background(), "petr_ivanov", 10)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != KindFloodWait || found != nil {
		t.Fatalf("failed resolve returned %+v (%v)", found, err)
	}
}

// A person changed the username, the peer cache kept the old one: the local substring search
// finds the same person, and the exact-username resolve arrives on a known id. Dropping the
// resolve entirely would answer "no exact username" about the one we searched for.
func TestSearchUsersMarksExactOnRenamedPeer(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		switch box := output.(type) {
		case *tg.ContactsResolvedPeer:
			*box = tg.ContactsResolvedPeer{
				Peer:  &tg.PeerUser{UserID: 9},
				Users: []tg.UserClass{&tg.User{ID: 9, FirstName: "Пётр", Username: "petr_ivanov", AccessHash: 42}},
			}
		case *tg.ContactsFound:
			*box = tg.ContactsFound{}
		default:
			return tgerr.New(500, "UNEXPECTED_REQUEST")
		}
		return nil
	}}, &tg.User{ID: 9, FirstName: "Пётр", Username: "petr_ivanov_old"})

	found, err := client.SearchUsers(context.Background(), "petr_ivanov", 10)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(found) != 1 || !found[0].Exact || found[0].Username != "petr_ivanov" {
		t.Fatalf("person who changed username parsed as %+v", found)
	}
}

// A client without a session must answer with a refusal, not crash: an MCP
// handler can arrive before Run comes up.
func TestDialogToolsRefuseWithoutSession(t *testing.T) {
	client, err := New(Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"},
		func(context.Context, Message) error { return nil })
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx := context.Background()
	calls := map[string]error{}
	_, calls["search"] = client.SearchUsers(ctx, "ivan", 10)
	_, calls["history"] = client.History(ctx, "user:7", 20, 0)
	_, calls["dialogs"] = client.Dialogs(ctx, 20)

	var failure *Failure
	for name, err := range calls {
		if !errors.As(err, &failure) || failure.Kind != KindNotReady {
			t.Errorf("%s without a session answered %v", name, err)
		}
	}
}

// runningClient is a client with a stub session brought up: the package tests do
// not go to the network, Telegram's answers are assembled by stubInvoker.
func runningClient(t *testing.T, invoker stubInvoker, known ...*tg.User) *Client {
	t.Helper()
	client, err := New(Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"},
		func(context.Context, Message) error { return nil })
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	client.setSession(tg.NewClient(invoker), &stubPeers{users: known})
	return client
}

// stubInvoker replaces RPC: the answer is written straight into the result box,
// as the wire decoder does.
type stubInvoker struct {
	fill func(input bin.Encoder, output bin.Decoder) error
}

func (s stubInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if s.fill == nil {
		return tgerr.New(500, "NO_STUB")
	}
	return s.fill(input, output)
}

// stubPeers is a recipient storage in memory. The lock is needed for the race
// test: four goroutines write and read it at once there.
type stubPeers struct {
	mu       sync.Mutex
	users    []*tg.User
	channels []*tg.Channel
	// failAdd is a write failure: a full disk or an unavailable bolt.
	failAdd error
}

// Add remembers the recipient for real: the exact-username search stands on it -
// ResolveUsername puts the person here, and the name is read from here too. A
// write by id replaces for both kinds, as in the live storage: a person who
// changed username and a channel seen again in the list must not lie there twice.
func (s *stubPeers) Add(_ context.Context, peer storage.Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAdd != nil {
		return s.failAdd
	}
	switch {
	case peer.User != nil:
		for i, user := range s.users {
			if user != nil && user.ID == peer.User.ID {
				s.users[i] = peer.User
				return nil
			}
		}
		s.users = append(s.users, peer.User)
	case peer.Channel != nil:
		for i, channel := range s.channels {
			if channel != nil && channel.ID == peer.Channel.ID {
				s.channels[i] = peer.Channel
				return nil
			}
		}
		s.channels = append(s.channels, peer.Channel)
	default:
		// FromChat on ChatForbidden/ChannelForbidden puts a key without an entity:
		// the stub does not store that and says so, instead of crashing the walk with nil.
		return fmt.Errorf("stub peers: recipient %v without user and channel", peer.Key)
	}
	return nil
}

func (s *stubPeers) Assign(context.Context, string, storage.Peer) error { return nil }

func (s *stubPeers) Resolve(context.Context, string) (storage.Peer, error) {
	return storage.Peer{}, storage.ErrPeerNotFound
}

func (s *stubPeers) Find(_ context.Context, key storage.PeerKey) (storage.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range s.users {
		if user != nil && user.ID == key.ID {
			return storage.Peer{Key: dialogs.DialogKey{Kind: key.Kind, ID: key.ID}, User: user}, nil
		}
	}
	return storage.Peer{}, storage.ErrPeerNotFound
}

func (s *stubPeers) Iterate(context.Context) (storage.PeerIterator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &stubPeerIterator{users: append([]*tg.User(nil), s.users...), at: -1}, nil
}

type stubPeerIterator struct {
	users []*tg.User
	at    int
}

func (s *stubPeerIterator) Next(context.Context) bool { s.at++; return s.at < len(s.users) }

func (s *stubPeerIterator) Err() error { return nil }

func (s *stubPeerIterator) Close() error { return nil }

func (s *stubPeerIterator) Value() storage.Peer {
	user := s.users[s.at]
	return storage.Peer{Key: dialogs.DialogKey{ID: user.ID}, User: user}
}

// A block in triple quotes goes out as a pre entity with an offset in UTF-16 units:
// Cyrillic is one unit, an emoji is two. Without a pair of delimiters the text is untouched.
func TestSplitPre(t *testing.T) {
	cases := []struct {
		name, text, want string
		offset, length   int
		entity           bool
	}{
		{"table", "Итог 😀:\n```\nа б\nв г\n```", "Итог 😀:\nа б\nв г", 9, 7, true},
		{"text after block", "```\nx\n```\nend", "x\nend", 0, 1, true},
		{"no closing", "```\nx", "```\nx", 0, 0, false},
		{"no block", "hi", "hi", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, entities := splitPre(c.text)
			if got != c.want {
				t.Fatalf("text %q, expected %q", got, c.want)
			}
			if !c.entity {
				if len(entities) != 0 {
					t.Fatalf("extra markup: %v", entities)
				}
				return
			}
			pre, ok := entities[0].(*tg.MessageEntityPre)
			if len(entities) != 1 || !ok || pre.Offset != c.offset || pre.Length != c.length {
				t.Fatalf("markup %v, expected pre %d+%d", entities, c.offset, c.length)
			}
		})
	}
}
