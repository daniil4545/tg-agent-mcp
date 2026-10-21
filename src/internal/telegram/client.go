// Package telegram is an MTProto userbot: one session of a live account,
// reading incoming messages and sending.
//
// A userbot, not the Bot API, because the account is real: it is a member of
// chats, writes in private to people who would not start a bot, and is visible
// in the correspondence as a person. The price is that the session is tied to a
// number and lives in exactly one process: a second process with the same
// number logs the first one out.
//
// The package does not decide what to do with a message. It turns an update
// into a structure and passes it up: selection by recipient kind, by sender and
// by content is the service's business, and differs for each service.
//
// Ported from an earlier internal project, where it ran in production.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gotd/contrib/auth/terminal"
	boltstore "github.com/gotd/contrib/bbolt"
	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram"
	tgauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	bolt "go.etcd.io/bbolt"
	bolterr "go.etcd.io/bbolt/errors"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/errgroup"
)

var (
	sessionBucket = []byte("session")
	peerBucket    = []byte("peers")
)

const sessionKey = "userbot"

// dialogsBatch is the page size of the dialog walk during warm-up. The library
// default is one, that is, one RPC per dialog.
const dialogsBatch = 100

// primeRetry is the pause between warm-up retries.
const primeRetry = time.Minute

// Config holds the client settings.
type Config struct {
	AppID   int
	AppHash string
	Phone   string
	// StoragePath is the bolt file with the session, peer storage and update
	// state. The directory must be dedicated and have mode 0700: it holds access
	// to the live account.
	StoragePath string
	// ProxyURL is `socks5://host:port` or empty. The proxy is set on the client,
	// not on the process environment: on some hosts the direct path is closed not to everything at once.
	ProxyURL string
	// PrimePeers walks the dialogs at startup to fill the peer storage.
	//
	// Needed only if the service writes to a group first. A group's access hash
	// reaches storage from an update, and a chat silent for months gives no
	// updates, so the first send there would fail with "peer not found". If the
	// service only answers incoming messages, warm-up is not needed: on a live
	// account walking all dialogs runs into FLOOD_WAIT.
	PrimePeers bool
}

func (c Config) validate() error {
	if c.AppID <= 0 {
		return errors.New("telegram: app id must be positive")
	}
	if strings.TrimSpace(c.AppHash) == "" {
		return errors.New("telegram: app hash is required")
	}
	if strings.TrimSpace(c.Phone) == "" {
		return errors.New("telegram: phone is required")
	}
	if strings.TrimSpace(c.StoragePath) == "" {
		return errors.New("telegram: storage path is required")
	}
	_, err := resolver(c.ProxyURL)
	return err
}

// Message is an incoming message reduced to a flat form.
type Message struct {
	// Peer is the chat the message is in: a person, a group or a channel.
	Peer Ref
	// SenderID is who wrote. In a private chat it equals the recipient, in a group it does not.
	SenderID int64
	// ID is the message identifier within the chat.
	ID int
	// Username and Name are not always known: short updates do not carry them.
	Username string
	Name     string
	Text     string
	// MediaKind is the attachment kind or empty if the message is text:
	// photo, document, geo, contact, poll, other. A web preview does not count as
	// an attachment, it is the formatting of a link in the text.
	MediaKind string
	SentAt    time.Time
	// Outgoing means the message was sent from our account. Such updates arrive
	// too: a person may write by hand from the same number, and their reply is
	// usually as important to the service as an incoming one.
	Outgoing bool
}

// Handler is called for every message. An error goes to the library log and the
// update is not replayed: the service's obligations live in its DB, not in
// update delivery.
type Handler func(ctx context.Context, msg Message) error

// SendResult is what came out of a send.
type SendResult struct {
	MessageID int
	// Duplicate means Telegram recognized the RandomID and did not create a second
	// message. This is a success, not an error: that is how protection against double sending works.
	Duplicate bool
}

// Media is an attachment. The kind is derived from the file name extension.
type Media struct {
	Name string
	Data []byte
}

// Client is a running userbot.
type Client struct {
	cfg     Config
	handler Handler
	// mu protects api and peers: they are set and cleared by the Run goroutine and
	// read by the MCP handlers from their own. Without the lock a service stop
	// drops a pending call on nil.
	mu    sync.RWMutex
	api   *tg.Client
	peers storage.PeerStorage
}

// session returns the live session or a refusal if the client is not started.
//
// Copies are returned: the holder of a copy survives the fields being cleared,
// and gotd on a closed connection returns an ordinary error - there is no need to wait for readers.
func (c *Client) session() (*tg.Client, storage.PeerStorage, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.api == nil || c.peers == nil {
		return nil, nil, &Failure{Kind: KindNotReady, Message: "client not started"}
	}
	return c.api, c.peers, nil
}

// setSession sets the session for the duration of Run and clears it on exit.
func (c *Client) setSession(api *tg.Client, peers storage.PeerStorage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.api, c.peers = api, peers
}

// New builds a client. The network is not touched: Run brings up the connection.
func New(cfg Config, handler Handler) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, errors.New("telegram: handler is required")
	}
	return &Client{cfg: cfg, handler: handler}, nil
}

// Run brings up the session and holds it until ctx ends or run fails.
// All of the service's work with Telegram lives inside run: outside it the
// client is not started, and a send returns KindNotReady.
func (c *Client) Run(ctx context.Context, run func(context.Context) error) error {
	if run == nil {
		return errors.New("telegram: run callback is required")
	}

	return withDB(c.cfg.StoragePath, func(db *bolt.DB) error {
		dialer, err := resolver(c.cfg.ProxyURL)
		if err != nil {
			return err
		}

		peers := boltstore.NewPeerStorage(db, peerBucket)
		dispatcher := tg.NewUpdateDispatcher()
		dispatcher.OnNewMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateNewMessage) error {
			msg, ok := normalize(update, entities)
			if !ok {
				return nil
			}
			return c.handler(ctx, msg)
		})

		manager := updates.New(updates.Config{
			Handler: storage.UpdateHook(dispatcher, peers),
			Storage: boltstore.NewStateStorage(db),
		})
		client := telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, telegram.Options{
			SessionStorage: boltstore.NewSessionStorage(db, sessionKey, sessionBucket),
			UpdateHandler:  manager,
			Resolver:       dialer,
			Middlewares:    []telegram.Middleware{updhook.UpdateHook(manager.Handle)},
		})

		return client.Run(ctx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("check telegram authorization: %w", err)
			}
			if !status.Authorized || status.User == nil {
				return errors.New("telegram account is not authorized; run the authorize command first")
			}

			c.setSession(client.API(), peers)
			defer c.setSession(nil, nil)

			return c.serve(ctx, client.API(), manager, status, run)
		})
	})
}

// serve holds three things at once: receiving updates, warming up recipients and
// the service's work. Any of them ending stops the others - otherwise the
// service would keep sending messages after it stopped hearing the replies.
func (c *Client) serve(
	ctx context.Context,
	api *tg.Client,
	manager *updates.Manager,
	status *tgauth.Status,
	run func(context.Context) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	group, groupCtx := errgroup.WithContext(ctx)
	ready := make(chan struct{})

	group.Go(func() error {
		defer cancel()
		err := manager.Run(groupCtx, api, status.User.ID, updates.AuthOptions{
			IsBot:   status.User.Bot,
			OnStart: func(context.Context) { close(ready) },
		})
		return runError(groupCtx, err)
	})

	select {
	case <-ready:
	case <-groupCtx.Done():
		return group.Wait()
	}

	// Warm-up runs before the service's work, but its failure does not cancel the start: without it
	// only sending first into a silent group does not work. The failure is retried
	// in the background, otherwise one temporary error would disable it until restart.
	if c.cfg.PrimePeers {
		if err := c.primePeers(groupCtx); err != nil {
			slog.Warn("telegram peers prime failed", "error", err)
			group.Go(func() error {
				c.retryPrime(groupCtx)
				return nil
			})
		}
	}

	group.Go(func() error {
		defer cancel()
		return runError(groupCtx, run(groupCtx))
	})
	return group.Wait()
}

// SendText sends text.
//
// randomID is the Telegram idempotency key: a retry with the same value does not
// create a second message but returns Duplicate. The value must be tied to a
// service intent (a queue job, an event), not generated on every call,
// otherwise there is no protection.
func (c *Client) SendText(ctx context.Context, peer Ref, text string, randomID int64) (SendResult, error) {
	api, peers, err := c.session()
	if err != nil {
		return SendResult{}, err
	}
	if strings.TrimSpace(text) == "" {
		return SendResult{}, permanent(KindBadRequest, "empty message text")
	}
	if randomID == 0 {
		return SendResult{}, permanent(KindBadRequest, "random_id is zero")
	}

	target, err := resolve(ctx, peers, peer)
	if err != nil {
		return SendResult{}, err
	}
	message, entities := splitPre(text)
	return sent(api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     target,
		Message:  message,
		Entities: entities,
		RandomID: randomID,
	}))
}

// splitPre turns the first block between "```\n" and "\n```" into a pre entity:
// the API, unlike the Telegram client, does not parse markup in the text, and a
// digest table needs even columns. The delimiters are removed from the text; with no pair the text
// goes out as is.
func splitPre(text string) (string, []tg.MessageEntityClass) {
	const open, closing = "```\n", "\n```"
	start := strings.Index(text, open)
	if start < 0 {
		return text, nil
	}
	body := text[start+len(open):]
	end := strings.Index(body, closing)
	if end < 0 {
		return text, nil
	}
	block := body[:end]
	pre := &tg.MessageEntityPre{Offset: utf16Len(text[:start]), Length: utf16Len(block)}
	return text[:start] + block + body[end+len(closing):], []tg.MessageEntityClass{pre}
}

// utf16Len is the length in UTF-16 units: that is how Telegram measures entity offsets.
func utf16Len(text string) int {
	return len(utf16.Encode([]rune(text)))
}

// SendMedia sends an attachment as one message. The file is uploaded anew on
// every send: a file_id cache would have to be reset when the session moves, and
// the gain is noticeable only when mass-sending the same file.
func (c *Client) SendMedia(ctx context.Context, peer Ref, media Media, caption string, randomID int64) (SendResult, error) {
	api, peers, err := c.session()
	if err != nil {
		return SendResult{}, err
	}
	if randomID == 0 {
		return SendResult{}, permanent(KindBadRequest, "random_id is zero")
	}
	// The shape is parsed before the upload: an unknown extension must be rejected
	// before the service spends a file upload.
	input, text, err := mediaInput(media.Name, caption)
	if err != nil {
		return SendResult{}, err
	}

	target, err := resolve(ctx, peers, peer)
	if err != nil {
		return SendResult{}, err
	}

	file, err := uploader.NewUploader(api).FromBytes(ctx, media.Name, media.Data)
	if err != nil {
		return SendResult{}, classify(err)
	}
	switch uploaded := input.(type) {
	case *tg.InputMediaUploadedPhoto:
		uploaded.File = file
	case *tg.InputMediaUploadedDocument:
		uploaded.File = file
	}

	return sent(api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     target,
		Media:    input,
		Message:  text,
		RandomID: randomID,
	}))
}

// SetTyping shows the recipient a typing indicator.
func (c *Client) SetTyping(ctx context.Context, peer Ref) error {
	api, peers, err := c.session()
	if err != nil {
		return err
	}
	target, err := resolve(ctx, peers, peer)
	if err != nil {
		return err
	}
	_, err = api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
		Peer:   target,
		Action: &tg.SendMessageTypingAction{},
	})
	return classify(err)
}

// MarkRead sets the read mark up to and including maxID. Idempotent.
func (c *Client) MarkRead(ctx context.Context, peer Ref, maxID int) error {
	api, peers, err := c.session()
	if err != nil {
		return err
	}
	target, err := resolve(ctx, peers, peer)
	if err != nil {
		return err
	}
	_, err = api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
		Peer:  target,
		MaxID: maxID,
	})
	return classify(err)
}

// ResolveUsername turns a username into a recipient reference and remembers the
// access hash, so the next send does not ask Telegram again.
//
// A missing username is deliberately separated from a temporary refusal: on the
// first the service decides the person cannot be found, on the second that it will try later.
func (c *Client) ResolveUsername(ctx context.Context, username string) (Ref, error) {
	api, peers, err := c.session()
	if err != nil {
		return "", err
	}
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return "", permanent(KindBadRequest, "empty username")
	}

	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		if tgerr.Is(err, "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID") {
			return "", permanent(KindPeerForbidden, "username not found")
		}
		return "", classify(err)
	}

	user, ok := resolved.Peer.(*tg.PeerUser)
	if !ok {
		return "", permanent(KindNotPerson, "username does not belong to a person")
	}
	var found storage.Peer
	for _, candidate := range resolved.Users {
		if candidate.GetID() == user.UserID && found.FromUser(candidate) {
			break
		}
	}
	if found.User == nil {
		return "", permanent(KindPeerForbidden, "resolve did not return a user")
	}
	if err := peers.Add(ctx, found); err != nil {
		return "", fmt.Errorf("persist resolved telegram peer: %w", err)
	}
	return NewRef(KindUser, user.UserID), nil
}

// resolve is the common preamble of calls to a recipient: the reference is
// parsed, the recipient is found. The storage comes as an argument, not read
// from the client fields: a service stop can clear the session mid-call.
//
// A regular group is an exception: it is addressed by an id alone, no access hash
// is needed, so we do not ask the storage at all. Otherwise a send to such a group
// would depend on warm-up, which on a live account runs into FLOOD_WAIT.
func resolve(ctx context.Context, peers storage.PeerStorage, peer Ref) (tg.InputPeerClass, error) {
	kind, id, err := peer.Split()
	if err != nil {
		return nil, permanent(KindBadRequest, "invalid recipient reference")
	}

	var key tg.PeerClass
	switch kind {
	case KindChat:
		return &tg.InputPeerChat{ChatID: id}, nil
	case KindChannel:
		key = &tg.PeerChannel{ChannelID: id}
	default:
		key = &tg.PeerUser{UserID: id}
	}
	found, err := storage.FindPeer(ctx, peers, key)
	if err != nil {
		if errors.Is(err, storage.ErrPeerNotFound) {
			return nil, classify(err)
		}
		// Our own storage failed, not Telegram: the request did not go out, and the
		// message certainly does not exist. A separate kind is needed exactly for this -
		// KindTemporary would mean "delivery unknown" and leave the touch in pending
		// forever, although the network was not touched (a bolt closed by SIGTERM answers the same way).
		return nil, &Failure{Kind: KindStorage, Message: "recipient storage unavailable: " + err.Error()}
	}
	return found.AsInputPeer(), nil
}

// sent is the common tail of both sends: a duplicate is a success, the rest is
// handled the same way.
func sent(result tg.UpdatesClass, err error) (SendResult, error) {
	if err != nil {
		if tg.IsRandomIDDuplicate(err) {
			return SendResult{Duplicate: true}, nil
		}
		return SendResult{}, classify(err)
	}
	id, err := unpack.MessageID(result, nil)
	if err != nil {
		return SendResult{}, classify(err)
	}
	return SendResult{MessageID: id}, nil
}

// The Telegram video sticker format: a 512-point square and an emoji caption for
// clients that show it instead of the picture.
const (
	stickerSide = 512
	stickerAlt  = "\U0001F431"
)

// mediaInput returns the attachment kind and the caption for it by file extension.
//
// The video duration is not declared: the server computes it, and a wrong number
// would distort the display. A sticker's caption is removed: Telegram shows it as
// a separate block, and a light touch stops being light.
func mediaInput(name, caption string) (tg.InputMediaClass, string, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png":
		return &tg.InputMediaUploadedPhoto{}, caption, nil
	case ".webm":
		return &tg.InputMediaUploadedDocument{
			MimeType: "video/webm",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeSticker{Alt: stickerAlt, Stickerset: &tg.InputStickerSetEmpty{}},
				&tg.DocumentAttributeVideo{W: stickerSide, H: stickerSide},
			},
		}, "", nil
	default:
		return nil, "", permanent(KindMediaInvalid, "unknown attachment kind: "+name)
	}
}

// primePeers fills the peer storage from the dialog list.
func (c *Client) primePeers(ctx context.Context) error {
	api, peers, err := c.session()
	if err != nil {
		return err
	}
	iter := query.GetDialogs(api).BatchSize(dialogsBatch).Iter()
	if err := storage.CollectPeers(peers).Dialogs(ctx, iter); err != nil {
		return fmt.Errorf("prime telegram peers: %w", err)
	}
	slog.Info("telegram peers primed")
	return nil
}

// retryPrime completes warm-up after a failure and exits on context cancel.
func (c *Client) retryPrime(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(primeRetry):
		}
		if err := c.primePeers(ctx); err != nil {
			slog.Warn("telegram peers prime retry failed", "error", err)
			continue
		}
		return
	}
}

// normalize turns an update into a Message. A second value of false means
// there is nothing to parse: a service update or a message without text and attachment.
//
// There is deliberately no selection by recipient kind and by sender here: a
// service that listens only to private chats and a service that reads channels
// need different rules, and the package is one.
func normalize(update *tg.UpdateNewMessage, entities tg.Entities) (Message, bool) {
	if update == nil {
		return Message{}, false
	}
	message, ok := update.Message.(*tg.Message)
	if !ok {
		return Message{}, false
	}
	return fromMessage(message, entities)
}

// fromMessage turns a message into a Message. The part shared by an update and
// a history query response: the message shape there is the same.
func fromMessage(message *tg.Message, entities tg.Entities) (Message, bool) {
	peer, ok := refFromPeer(message.GetPeerID())
	if !ok {
		return Message{}, false
	}

	text := strings.TrimSpace(message.GetMessage())
	kind := mediaKind(message.Media)
	if text == "" && kind == "" {
		return Message{}, false
	}

	// Sender: in a private chat FromID is empty, and it is the recipient themselves.
	sender := int64(0)
	if from, ok := message.GetFromID(); ok {
		if user, ok := from.(*tg.PeerUser); ok {
			sender = user.UserID
		}
	}
	if sender == 0 {
		if _, id, err := peer.Split(); err == nil {
			sender = id
		}
	}

	return Message{
		Peer:      peer,
		SenderID:  sender,
		ID:        message.GetID(),
		Username:  username(entities, sender),
		Name:      name(entities, sender),
		Text:      text,
		MediaKind: kind,
		SentAt:    time.Unix(int64(message.GetDate()), 0).UTC(),
		Outgoing:  message.GetOut(),
	}, true
}

// mediaKind is a rough attachment category or empty. A web preview and empty
// media do not count as an attachment: they are the formatting of a text message.
func mediaKind(media tg.MessageMediaClass) string {
	switch media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return ""
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		return "document"
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return "geo"
	case *tg.MessageMediaContact:
		return "contact"
	case *tg.MessageMediaPoll:
		return "poll"
	default:
		return "other"
	}
}

// username takes the public username: first the main field, then the first
// active one of the collectible ones (accounts with several usernames). Empty means the username is unknown.
func username(entities tg.Entities, userID int64) string {
	user, ok := entities.Users[userID]
	if !ok || user == nil {
		return ""
	}
	return publicUsername(user)
}

// publicUsername is the main username or the first active collectible one.
func publicUsername(user *tg.User) string {
	return pickUsername(user.Username, user.Usernames)
}

// name builds the sender name. Empty is the normal case: short updates
// carry no entities.
func name(entities tg.Entities, userID int64) string {
	user, ok := entities.Users[userID]
	if !ok || user == nil {
		return ""
	}
	return fullName(user)
}

// fullName is first and last name in one string.
func fullName(user *tg.User) string {
	return strings.TrimSpace(user.FirstName + " " + user.LastName)
}

// runError swallows context cancellation: stopping the service is not an error.
func runError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// resolver builds a dialer over SOCKS5 or nil for a direct connection.
func resolver(proxyURL string) (dcs.Resolver, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return nil, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse telegram proxy URL: %w", err)
	}
	if parsed.Scheme != "socks5" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("telegram proxy must be socks5://host:port")
	}
	if _, _, err := net.SplitHostPort(parsed.Host); err != nil {
		return nil, errors.New("telegram proxy must be socks5://host:port")
	}

	var auth *proxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &proxy.Auth{User: parsed.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create telegram SOCKS5 proxy: %w", err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("telegram SOCKS5 proxy does not support context cancellation")
	}
	return dcs.Plain(dcs.PlainOptions{Dial: contextDialer.DialContext}), nil
}

// withDB opens the session storage. Directory permissions are checked, not set
// silently: this file holds access to the live account, and an open directory
// must stop the start, not fix itself.
func withDB(path string, run func(*bolt.DB) error) (err error) {
	dir := filepath.Dir(filepath.Clean(path))
	if dir == "." || dir == string(filepath.Separator) {
		return errors.New("telegram storage must use a dedicated directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create telegram storage directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("inspect telegram storage directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("telegram storage directory %q must have permissions 0700", dir)
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		// The file is held by one process: either the sender is running or a login
		// is going on in parallel. Without this hint the error looks like "timeout".
		if errors.Is(err, bolterr.ErrTimeout) {
			return fmt.Errorf("telegram storage %q is locked by another process", path)
		}
		return fmt.Errorf("open telegram storage: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close telegram storage: %w", closeErr))
		}
	}()
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect telegram storage file: %w", err)
	}
	// Buckets are created in advance: on an empty storage reading a missing
	// bucket returns an ordinary error instead of "no session", and the very first login
	// fails with "bucket "session" does not exist".
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{sessionBucket, peerBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("initialize telegram storage: %w", err)
	}
	return run(db)
}

// Self is the account that owns the session.
type Self struct {
	ID       int64
	Username string
	Name     string
	Phone    string
}

// Authorize performs a login by a code from Telegram. It is run once by hand,
// as a separate command of the binary: after that the session lives in StoragePath.
//
// The returned Self is not a report decoration. The storage may hold the session
// of a previous account, and then the login is skipped silently: the number check
// catches the case where the campaign would go out from the wrong number.
func Authorize(ctx context.Context, cfg Config) (Self, error) {
	if err := cfg.validate(); err != nil {
		return Self{}, err
	}
	var self Self
	err := withDB(cfg.StoragePath, func(db *bolt.DB) error {
		dialer, err := resolver(cfg.ProxyURL)
		if err != nil {
			return err
		}
		client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
			SessionStorage: boltstore.NewSessionStorage(db, sessionKey, sessionBucket),
			Resolver:       dialer,
		})
		return client.Run(ctx, func(ctx context.Context) error {
			flow := tgauth.NewFlow(&authenticator{Terminal: terminal.OS(), phone: cfg.Phone}, tgauth.SendCodeOptions{})
			if err := client.Auth().IfNecessary(ctx, flow); err != nil {
				return fmt.Errorf("authorize telegram account: %w", err)
			}
			user, err := client.Self(ctx)
			if err != nil {
				return fmt.Errorf("read authorized telegram account: %w", err)
			}
			self = Self{
				ID:       user.GetID(),
				Username: user.Username,
				Name:     strings.TrimSpace(user.FirstName + " " + user.LastName),
				Phone:    user.Phone,
			}
			if !samePhone(self.Phone, cfg.Phone) {
				return fmt.Errorf("telegram storage holds a session of another account (phone %q, expected %q)",
					self.Phone, cfg.Phone)
			}
			return nil
		})
	})
	return self, err
}

// samePhone compares numbers by digits: the config and Telegram write the same
// number differently - with a plus, with parentheses, with spaces.
func samePhone(a, b string) bool {
	digits := func(phone string) string {
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, phone)
	}
	left, right := digits(a), digits(b)
	return left != "" && left == right
}

// authenticator answers the login questions. Registering a new account and
// accepting the terms are deliberately not supported: the service works with an
// existing working account, and silently creating a new one would be the worst possible outcome.
type authenticator struct {
	*terminal.Terminal
	phone string
}

func (a *authenticator) Phone(context.Context) (string, error) { return a.phone, nil }

func (a *authenticator) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("telegram: accepting terms of service is not supported for an existing account")
}

func (a *authenticator) SignUp(context.Context) (tgauth.UserInfo, error) {
	return tgauth.UserInfo{}, errors.New("telegram: sign-up is not supported; use an existing account")
}
