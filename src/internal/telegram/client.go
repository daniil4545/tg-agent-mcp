// Package telegram - userbot на MTProto: одна сессия живого аккаунта, чтение
// входящих и отправка.
//
// Userbot, а не Bot API, потому что аккаунт настоящий: он состоит в чатах,
// пишет в личку людям, которые бота бы не запустили, и виден в переписке как
// человек. Цена - сессия привязана к номеру и живёт ровно в одном процессе:
// второй процесс с тем же номером разлогинивает первый.
//
// Пакет не решает, что делать с сообщением. Он приводит апдейт к структуре и
// отдаёт наверх: отбор по виду адресата, по отправителю и по содержанию - дело
// сервиса, и у каждого он свой.
//
// Перенесено из qualifier, где отработало в проде.
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

// dialogsBatch - страница обхода диалогов при прогреве. Дефолт библиотеки
// равен единице, то есть один RPC на каждый диалог.
const dialogsBatch = 100

// primeRetry - пауза между повторами прогрева.
const primeRetry = time.Minute

// Config - настройки клиента.
type Config struct {
	AppID   int
	AppHash string
	Phone   string
	// StoragePath - файл bolt с сессией, peer storage и состоянием апдейтов.
	// Каталог обязан быть выделенным и с правами 0700: там лежит доступ к
	// живому аккаунту.
	StoragePath string
	// ProxyURL - `socks5://host:port` или пусто. Прокси задаётся клиенту, а не
	// окружению процесса: на хостах закрыт прямой путь не ко всему сразу.
	ProxyURL string
	// PrimePeers - обойти диалоги при старте, чтобы наполнить peer storage.
	//
	// Нужен, только если сервис пишет в группу первым. Access hash группы
	// попадает в хранилище из апдейта, а молчащий месяцами чат апдейтов не
	// даёт, и первая отправка туда упала бы «peer не найден». Если сервис
	// только отвечает на входящие, прогрев не нужен: на боевом аккаунте обход
	// всех диалогов упирается в FLOOD_WAIT.
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

// Message - входящее сообщение, приведённое к плоскому виду.
type Message struct {
	// Peer - чат, в котором сообщение: человек, группа или канал.
	Peer Ref
	// SenderID - кто написал. В личке совпадает с адресатом, в группе нет.
	SenderID int64
	// ID - идентификатор сообщения внутри чата.
	ID int
	// Username и Name известны не всегда: короткие апдейты их не несут.
	Username string
	Name     string
	Text     string
	// MediaKind - вид вложения или пусто, если сообщение текстовое:
	// photo, document, geo, contact, poll, other. Web-preview вложением не
	// считается, это оформление ссылки в тексте.
	MediaKind string
	SentAt    time.Time
	// Outgoing - сообщение отправлено с нашего аккаунта. Такие апдейты приходят
	// тоже: с того же номера может писать человек руками, и его реплика обычно
	// важна сервису не меньше входящей.
	Outgoing bool
}

// Handler вызывается на каждое сообщение. Ошибка попадает в лог библиотеки и
// апдейт не переигрывается: обязательства сервиса живут в его БД, а не в
// доставке апдейтов.
type Handler func(ctx context.Context, msg Message) error

// SendResult - что вышло из отправки.
type SendResult struct {
	MessageID int
	// Duplicate - Telegram узнал RandomID и второе сообщение не создал. Это
	// успех, а не ошибка: так и работает защита от двойной отправки.
	Duplicate bool
}

// Media - вложение. Вид выводится из расширения имени.
type Media struct {
	Name string
	Data []byte
}

// Client - запущенный userbot.
type Client struct {
	cfg     Config
	handler Handler
	// mu защищает api и peers: их ставит и обнуляет горутина Run, а читают
	// обработчики MCP из своих. Без замка остановка сервиса роняет висящий
	// вызов на nil.
	mu    sync.RWMutex
	api   *tg.Client
	peers storage.PeerStorage
}

// session отдаёт живую сессию или отказ, если клиент не запущен.
//
// Возвращаются копии: держатель копии переживёт обнуление полей, а gotd на
// закрытом соединении вернёт обычную ошибку - ждать читателей не нужно.
func (c *Client) session() (*tg.Client, storage.PeerStorage, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.api == nil || c.peers == nil {
		return nil, nil, &Failure{Kind: KindNotReady, Message: "клиент не запущен"}
	}
	return c.api, c.peers, nil
}

// setSession ставит сессию на время работы Run и снимает её по выходу.
func (c *Client) setSession(api *tg.Client, peers storage.PeerStorage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.api, c.peers = api, peers
}

// New собирает клиента. Сеть при этом не трогается: соединение поднимает Run.
func New(cfg Config, handler Handler) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, errors.New("telegram: handler is required")
	}
	return &Client{cfg: cfg, handler: handler}, nil
}

// Run поднимает сессию и держит её, пока не кончится ctx или не упадёт run.
// Вся работа сервиса с Telegram живёт внутри run: снаружи клиент не запущен, и
// отправка вернёт KindNotReady.
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

// serve держит три вещи разом: приём апдейтов, прогрев адресатов и работу
// сервиса. Любая из них, кончившись, останавливает остальные - иначе сервис
// продолжал бы слать сообщения, перестав слышать ответы.
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

	// Прогрев идёт до работы сервиса, но его сбой старт не отменяет: без него
	// не работает только отправка первым в молчащую группу. Неудачу добираем
	// фоном, иначе одна временная ошибка выключала бы её до рестарта.
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

// SendText отправляет текст.
//
// randomID - ключ идемпотентности Telegram: повтор с тем же значением не создаёт
// второго сообщения, а возвращает Duplicate. Значение обязано быть привязано к
// намерению сервиса (работа очереди, событие), а не сгенерировано на каждый
// вызов, иначе защиты нет.
func (c *Client) SendText(ctx context.Context, peer Ref, text string, randomID int64) (SendResult, error) {
	api, peers, err := c.session()
	if err != nil {
		return SendResult{}, err
	}
	if strings.TrimSpace(text) == "" {
		return SendResult{}, permanent(KindBadRequest, "пустой текст сообщения")
	}
	if randomID == 0 {
		return SendResult{}, permanent(KindBadRequest, "random_id равен нулю")
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

// splitPre переводит первый блок между "```\n" и "\n```" в сущность pre:
// API, в отличие от клиента Telegram, разметку в тексте не разбирает, а таблице
// сводки нужны ровные колонки. Границы из текста убираются; пары нет - текст
// уходит как есть.
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

// utf16Len - длина в единицах UTF-16: так Telegram меряет смещения сущностей.
func utf16Len(text string) int {
	return len(utf16.Encode([]rune(text)))
}

// SendMedia отправляет вложение одним сообщением. Файл грузится на каждую
// отправку заново: кеш file_id пришлось бы сбрасывать при переезде сессии, а
// выигрыш заметен только при массовой рассылке одного и того же файла.
func (c *Client) SendMedia(ctx context.Context, peer Ref, media Media, caption string, randomID int64) (SendResult, error) {
	api, peers, err := c.session()
	if err != nil {
		return SendResult{}, err
	}
	if randomID == 0 {
		return SendResult{}, permanent(KindBadRequest, "random_id равен нулю")
	}
	// Разбор формы идёт до аплоада: неизвестное расширение обязано отсечься
	// раньше, чем сервис потратит загрузку файла.
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

// SetTyping показывает адресату индикатор набора.
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

// MarkRead ставит галочку прочтения до maxID включительно. Идемпотентен.
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

// ResolveUsername переводит ник в ссылку на адресата и запоминает access hash,
// поэтому следующая отправка уже не спрашивает Telegram.
//
// Отсутствующий ник отделён от временного отказа намеренно: по первому сервис
// решает, что человека не найти, по второму - что попробует позже.
func (c *Client) ResolveUsername(ctx context.Context, username string) (Ref, error) {
	api, peers, err := c.session()
	if err != nil {
		return "", err
	}
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return "", permanent(KindBadRequest, "пустой username")
	}

	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		if tgerr.Is(err, "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID") {
			return "", permanent(KindPeerForbidden, "username не найден")
		}
		return "", classify(err)
	}

	user, ok := resolved.Peer.(*tg.PeerUser)
	if !ok {
		return "", permanent(KindNotPerson, "username принадлежит не человеку")
	}
	var found storage.Peer
	for _, candidate := range resolved.Users {
		if candidate.GetID() == user.UserID && found.FromUser(candidate) {
			break
		}
	}
	if found.User == nil {
		return "", permanent(KindPeerForbidden, "resolve не вернул пользователя")
	}
	if err := peers.Add(ctx, found); err != nil {
		return "", fmt.Errorf("persist resolved telegram peer: %w", err)
	}
	return NewRef(KindUser, user.UserID), nil
}

// resolve - общая преамбула обращений к адресату: ссылка разобрана, адресат
// найден. Хранилище приходит аргументом, а не читается из полей клиента: снять
// сессию может остановка сервиса посреди вызова.
//
// Обычная группа - исключение: она адресуется одним id, access hash ей не
// нужен, поэтому хранилище не спрашиваем вовсе. Иначе отправка в такую группу
// зависела бы от прогрева, а он на боевом аккаунте упирается в FLOOD_WAIT.
func resolve(ctx context.Context, peers storage.PeerStorage, peer Ref) (tg.InputPeerClass, error) {
	kind, id, err := peer.Split()
	if err != nil {
		return nil, permanent(KindBadRequest, "некорректная ссылка на адресата")
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
		// Отказало своё хранилище, а не Telegram: запрос не уходил, и сообщения
		// заведомо нет. Отдельный вид нужен именно поэтому - KindTemporary означал
		// бы «доставка неизвестна» и оставил бы касание в pending навсегда, хотя
		// сеть не трогали (закрытый по SIGTERM bolt отвечает так же).
		return nil, &Failure{Kind: KindStorage, Message: "хранилище адресатов недоступно: " + err.Error()}
	}
	return found.AsInputPeer(), nil
}

// sent - общий хвост обеих отправок: дубль это успех, остальное разбирается
// одинаково.
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

// Формат видеостикера Telegram: квадрат 512 точек и эмодзи-подпись для
// клиентов, которые показывают её вместо картинки.
const (
	stickerSide = 512
	stickerAlt  = "\U0001F431"
)

// mediaInput - вид вложения и подпись к нему по расширению файла.
//
// Длительность видео не заявляем: её посчитает сервер, а неверное число
// исказило бы показ. У стикера подпись снимается: Telegram показывает её
// отдельным блоком, и лёгкое касание перестаёт быть лёгким.
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
		return nil, "", permanent(KindMediaInvalid, "неизвестный вид вложения: "+name)
	}
}

// primePeers наполняет peer storage из списка диалогов.
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

// retryPrime добирает прогрев после неудачи и уходит по отмене контекста.
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

// normalize приводит апдейт к Message. Второе значение false означает, что
// разбирать нечего: служебный апдейт или сообщение без текста и вложения.
//
// Отбора по виду адресата и по отправителю здесь нет намеренно: сервису,
// который слушает только личку, и сервису, который читает каналы, нужны разные
// правила, а пакет один.
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

// fromMessage приводит сообщение к Message. Общая часть апдейта и ответа на
// запрос истории: форма сообщения там одна и та же.
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

	// Отправитель: в личке FromID пуст, и им является сам адресат.
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

// mediaKind - грубая категория вложения или пусто. Web-preview и пустое медиа
// вложением не считаются: это оформление текстового сообщения.
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

// username берёт публичный ник: сначала основное поле, затем первый активный
// из коллекционных (аккаунты с несколькими никами). Пусто - ник неизвестен.
func username(entities tg.Entities, userID int64) string {
	user, ok := entities.Users[userID]
	if !ok || user == nil {
		return ""
	}
	return publicUsername(user)
}

// publicUsername - основной ник или первый активный из коллекционных.
func publicUsername(user *tg.User) string {
	return pickUsername(user.Username, user.Usernames)
}

// name собирает имя отправителя. Пусто - штатный случай: короткие апдейты
// entities не несут.
func name(entities tg.Entities, userID int64) string {
	user, ok := entities.Users[userID]
	if !ok || user == nil {
		return ""
	}
	return fullName(user)
}

// fullName - имя и фамилия одной строкой.
func fullName(user *tg.User) string {
	return strings.TrimSpace(user.FirstName + " " + user.LastName)
}

// runError гасит отмену контекста: остановка сервиса не является ошибкой.
func runError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// resolver собирает диалер через SOCKS5 или nil для прямого соединения.
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

// withDB открывает хранилище сессии. Права каталога проверяются, а не
// выставляются молча: в этом файле лежит доступ к живому аккаунту, и открытый
// каталог обязан останавливать старт, а не чиниться сам.
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
		// Файл держит один процесс: либо запущен отправщик, либо параллельно
		// идёт вход. Без этой подсказки ошибка выглядит как «timeout».
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
	// Бакеты заводятся заранее: на пустом хранилище чтение отсутствующего
	// бакета возвращает обычную ошибку вместо «сессии нет», и первый же вход
	// падает с «bucket "session" does not exist».
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

// Self - аккаунт, которому принадлежит сессия.
type Self struct {
	ID       int64
	Username string
	Name     string
	Phone    string
}

// Authorize проводит вход по коду из Telegram. Запускается один раз руками,
// отдельной командой бинаря: сессия после этого живёт в StoragePath.
//
// Вернувшийся Self - не украшение отчёта. В хранилище может лежать сессия
// прежнего аккаунта, и тогда вход пропускается молча: сверка номера ловит
// случай, когда рассылка ушла бы не с того номера.
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

// samePhone сравнивает номера по цифрам: конфиг и Telegram пишут один и тот же
// номер по-разному - с плюсом, со скобками, с пробелами.
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

// authenticator отвечает на вопросы входа. Регистрация нового аккаунта и приём
// условий не поддержаны намеренно: сервис работает существующим рабочим
// аккаунтом, и молчаливое создание нового было бы худшим исходом из возможных.
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
