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

// Ссылка на адресата живёт в БД и в ключах очереди, поэтому у одного адресата
// обязана быть ровно одна запись: иначе он получит два «первых» сообщения.
func TestRefRejectsNonCanonicalForms(t *testing.T) {
	bad := []Ref{
		"user:007",   // ведущие нули дали бы второй ключ тому же адресату
		"user:0",     //
		"user:-1",    //
		"user:",      //
		"user",       //
		"group:12",   // вида group у Telegram нет
		"USER:12",    //
		"user:12:34", // хвост после id
	}
	for _, ref := range bad {
		if _, _, err := ref.Split(); err == nil {
			t.Errorf("ссылка %q принята", string(ref))
		}
	}

	kind, id, err := Ref("channel:1234567890").Split()
	if err != nil || kind != KindChannel || id != 1234567890 {
		t.Fatalf("разбор дал %v %d %v", kind, id, err)
	}
	if got := NewRef(KindUser, 42); got != "user:42" {
		t.Fatalf("сборка дала %q", string(got))
	}
}

// Групповой адресат там, где сервис ждёт человека, - ошибка данных, а не другой
// сценарий: без этой проверки сообщение для одного человека уходит в чат.
func TestUserIDRejectsGroups(t *testing.T) {
	if _, err := Ref("user:42").UserID(); err != nil {
		t.Fatalf("человек отвергнут: %v", err)
	}
	for _, ref := range []Ref{"chat:42", "channel:42"} {
		if _, err := ref.UserID(); err == nil {
			t.Errorf("групповая ссылка %q принята как человек", string(ref))
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
			name:    "личка с текстом",
			message: &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Message: "привет"},
			want:    true,
			peer:    "user:7",
		},
		{
			// Пакет один на все сервисы: тот, что читает каналы, обязан получать
			// их сообщения, а отбор по виду адресата делает сервис.
			name:    "канал с текстом",
			message: &tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: 9}, Message: "анонс"},
			want:    true,
			peer:    "channel:9",
		},
		{
			name:    "служебное сообщение",
			message: &tg.MessageService{ID: 3, PeerID: &tg.PeerUser{UserID: 7}},
			want:    false,
		},
		{
			name:    "пустое без вложения",
			message: &tg.Message{ID: 4, PeerID: &tg.PeerUser{UserID: 7}, Message: "   "},
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := normalize(&tg.UpdateNewMessage{Message: c.message}, tg.Entities{})
			if ok != c.want {
				t.Fatalf("разобрано=%v, ожидалось %v", ok, c.want)
			}
			if ok && msg.Peer != c.peer {
				t.Fatalf("адресат %q, ожидался %q", string(msg.Peer), string(c.peer))
			}
		})
	}
}

// Вложение без подписи - полноценное сообщение: голосовое и фото приходят без
// единого символа текста, и молчаливый дроп терял бы их целиком.
func TestNormalizeKeepsMediaWithoutCaption(t *testing.T) {
	msg, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID:     5,
		PeerID: &tg.PeerUser{UserID: 7},
		Media:  &tg.MessageMediaDocument{},
	}}, tg.Entities{})
	if !ok {
		t.Fatal("вложение без подписи отброшено")
	}
	if msg.MediaKind != "document" {
		t.Fatalf("вид вложения %q", msg.MediaKind)
	}

	// Web-preview - оформление ссылки, а не вложение: считать его медиа значит
	// уводить обычный текст в ветку разбора файлов.
	preview, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID:      6,
		PeerID:  &tg.PeerUser{UserID: 7},
		Message: "ссылка",
		Media:   &tg.MessageMediaWebPage{},
	}}, tg.Entities{})
	if !ok || preview.MediaKind != "" {
		t.Fatalf("web-preview разобран как вложение: %q", preview.MediaKind)
	}
}

// В личке FromID пуст, и отправитель - сам собеседник. Без подстановки сервис
// получил бы нулевого автора у каждого входящего.
func TestNormalizeFillsSenderInDirectMessages(t *testing.T) {
	msg, ok := normalize(&tg.UpdateNewMessage{Message: &tg.Message{
		ID: 7, PeerID: &tg.PeerUser{UserID: 77}, Message: "текст",
	}}, tg.Entities{})
	if !ok || msg.SenderID != 77 {
		t.Fatalf("отправитель %d, ожидался 77", msg.SenderID)
	}

	// В группе отправитель и адресат разные, и подменять один другим нельзя.
	group, ok := normalize(&tg.UpdateNewMessage{Message: withFlags(&tg.Message{
		ID: 8, PeerID: &tg.PeerChannel{ChannelID: 9},
		FromID: &tg.PeerUser{UserID: 55}, Message: "текст",
	})}, tg.Entities{})
	if !ok || group.SenderID != 55 {
		t.Fatalf("отправитель в группе %d, ожидался 55", group.SenderID)
	}
}

// withFlags проставляет битовую маску необязательных полей.
//
// В gotd опциональные поля и флаги-признаки читаются не из полей структуры, а
// из Flags: GetOut возвращает Flags.Has(1), GetFromID проверяет Flags.Has(2).
// При разборе с провода маску ставит декодер, а собранное в тесте сообщение
// без SetFlags выглядит как входящее и без отправителя, что бы ни лежало в
// полях. Тест, забывший этот вызов, зеленеет на сломанном коде.
func withFlags(message *tg.Message) *tg.Message {
	message.SetFlags()
	return message
}

// Своя реплика с того же аккаунта - не мусор: с рабочего номера пишет и человек
// руками, и его сообщение сервису обычно важно.
func TestNormalizeMarksOutgoing(t *testing.T) {
	msg, ok := normalize(&tg.UpdateNewMessage{Message: withFlags(&tg.Message{
		ID: 9, Out: true, PeerID: &tg.PeerUser{UserID: 7}, Message: "ответ",
	})}, tg.Entities{})
	if !ok {
		t.Fatal("исходящее отброшено")
	}
	if !msg.Outgoing {
		t.Fatal("исходящее не помечено")
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
		ID: 10, PeerID: &tg.PeerUser{UserID: 7}, Message: "текст",
	}}, entities)
	if !ok {
		t.Fatal("сообщение отброшено")
	}
	// Неактивный ник брать нельзя: он уже принадлежит кому-то другому.
	if msg.Username != "ivan" {
		t.Fatalf("ник %q", msg.Username)
	}
	if msg.Name != "Иван Петров" {
		t.Fatalf("имя %q", msg.Name)
	}
}

// Классификация решает, повторять или бросить. Ошибка в ней либо теряет
// сообщение навсегда, либо крутит бесконечный повтор.
func TestClassifySeparatesRetryFromPermanent(t *testing.T) {
	flood := classify(tgerr.New(420, "FLOOD_WAIT_30"))
	var failure *Failure
	if !errors.As(flood, &failure) || failure.Kind != KindFloodWait {
		t.Fatalf("flood wait разобран как %v", flood)
	}
	if failure.Permanent {
		t.Fatal("flood wait признан постоянным: сообщение потерялось бы")
	}
	if failure.RetryAt.IsZero() {
		t.Fatal("срок повтора не взят из ошибки")
	}
	if wait := time.Until(failure.RetryAt); wait < 25*time.Second {
		t.Fatalf("срок повтора %s, Telegram просил 30s", wait)
	}

	permanentCases := map[string]string{
		"PEER_FLOOD":                 KindPeerFlood,
		"USER_IS_BLOCKED":            KindPeerForbidden,
		"CHAT_WRITE_FORBIDDEN":       KindPeerForbidden,
		"AUTH_KEY_UNREGISTERED":      KindAuthForbidden,
		"FROZEN_PARTICIPANT_MISSING": KindAuthForbidden,
		"MEDIA_INVALID":              KindMediaInvalid,
		// Отказы чтения: повтор той же просьбы даст тот же ответ, и временными
		// их считать значит крутить его до исчерпания попыток.
		"MSG_ID_INVALID":         KindBadRequest,
		"QUERY_TOO_SHORT":        KindBadRequest,
		"SEARCH_QUERY_EMPTY":     KindBadRequest,
		"OFFSET_PEER_ID_INVALID": KindBadRequest,
		// Отвергнутый текст: сообщение заведомо не создано, а «повторим позже»
		// оставляет строку журнала в pending навсегда и закрывает человека для
		// всех кампаний без единой отправки.
		"MESSAGE_TOO_LONG": KindBadRequest,
		"MESSAGE_EMPTY":    KindBadRequest,
	}
	for code, kind := range permanentCases {
		err := classify(tgerr.New(400, code))
		if !errors.As(err, &failure) || failure.Kind != kind || !failure.Permanent {
			t.Errorf("%s разобран как %v", code, err)
		}
	}

	// Неизвестная ошибка обязана быть временной: брошенная навсегда, она тихо
	// теряет сообщение, а лишний повтор стоит одной попытки.
	unknown := classify(tgerr.New(500, "SOMETHING_NEW"))
	if !errors.As(unknown, &failure) || failure.Permanent {
		t.Fatalf("неизвестная ошибка признана постоянной: %v", unknown)
	}
	// 5xx с неизвестным кодом - единственный отказ, после которого доставка
	// неизвестна: сервер мог создать сообщение и упасть уже потом. Код в Type от
	// этого ничего не меняет, и «сообщения нет» здесь означало бы второе письмо.
	if failure.NotSent() {
		t.Fatalf("5xx признан заведомо не отправленным: %v", unknown)
	}
	// Без кода MTProto в тексте вызывающий видит «временная ошибка Telegram», из
	// чего не следует ничего.
	if failure.Type != "SOMETHING_NEW" || !strings.Contains(unknown.Error(), "SOMETHING_NEW") {
		t.Fatalf("код ошибки потерян: %q", unknown.Error())
	}
}

func TestMediaInputDropsCaptionOnStickers(t *testing.T) {
	_, caption, err := mediaInput("photo.jpg", "подпись")
	if err != nil || caption != "подпись" {
		t.Fatalf("фото: подпись %q, %v", caption, err)
	}

	// Telegram показывает подпись к стикеру отдельным блоком, и лёгкое касание
	// перестаёт быть лёгким.
	_, caption, err = mediaInput("sticker.webm", "подпись")
	if err != nil || caption != "" {
		t.Fatalf("стикер: подпись %q, %v", caption, err)
	}

	// Неизвестное расширение отсекается до загрузки файла и повтором не лечится.
	if _, _, err := mediaInput("archive.zip", ""); err == nil {
		t.Fatal("неизвестное вложение принято")
	} else if failure := new(Failure); !errors.As(err, &failure) || !failure.Permanent {
		t.Fatalf("отказ по вложению не постоянный: %v", err)
	}
}

func TestResolverAcceptsOnlySocks5(t *testing.T) {
	if dialer, err := resolver(""); err != nil || dialer != nil {
		t.Fatalf("пустой прокси дал %v, %v", dialer, err)
	}
	if _, err := resolver("socks5://10.0.0.1:20170"); err != nil {
		t.Fatalf("socks5 отвергнут: %v", err)
	}
	for _, bad := range []string{"http://10.0.0.1:3128", "socks5://10.0.0.1", "socks5://10.0.0.1:20170/path", "10.0.0.1:20170"} {
		if _, err := resolver(bad); err == nil {
			t.Errorf("прокси %q принят", bad)
		}
	}
}

// Клиент, собранный на кривом конфиге, падал бы уже в проде при первой отправке.
func TestNewValidatesConfigAndHandler(t *testing.T) {
	good := Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"}
	if _, err := New(good, func(context.Context, Message) error { return nil }); err != nil {
		t.Fatalf("верный конфиг отвергнут: %v", err)
	}
	if _, err := New(good, nil); err == nil {
		t.Fatal("клиент собран без обработчика: входящие уходили бы в никуда")
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
			t.Errorf("конфиг %d принят", i)
		}
	}
}

// Хранилище сессии - это доступ к живому аккаунту: открытый каталог обязан
// останавливать старт, а не чиниться молча.
func TestWithDBRefusesLooseDirectories(t *testing.T) {
	// t.TempDir отдаёт 0755, поэтому каталог сессии создаём сами: проверяется
	// как раз строгость к правам.
	dir := t.TempDir() + "/tg"
	if err := withDB(dir+"/session.bolt", func(*bolt.DB) error { return nil }); err != nil {
		t.Fatalf("выделенный каталог отвергнут: %v", err)
	}

	// Каталог верхнего уровня означает, что сессию положили рядом с чем попало.
	if err := withDB("session.bolt", func(*bolt.DB) error { return nil }); err == nil {
		t.Fatal("сессия принята без выделенного каталога")
	}

	loose := t.TempDir() + "/loose"
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if err := withDB(loose+"/session.bolt", func(*bolt.DB) error { return nil }); err == nil {
		t.Fatal("каталог с правами 0755 принят")
	}
}

// Сессия чужого аккаунта в хранилище означала бы рассылку не с того номера:
// вход в этом случае пропускается молча, и ловит его только сверка номера.
func TestSamePhoneComparesDigitsOnly(t *testing.T) {
	same := [][2]string{
		{"79001234567", "+7 900 123-45-67"},
		{"+79001234567", "79001234567"},
	}
	for _, pair := range same {
		if !samePhone(pair[0], pair[1]) {
			t.Errorf("номера %q и %q сочтены разными", pair[0], pair[1])
		}
	}

	other := [][2]string{
		{"79001234567", "79009999999"},
		{"", "79001234567"},
		{"+", "+"},
	}
	for _, pair := range other {
		if samePhone(pair[0], pair[1]) {
			t.Errorf("номера %q и %q сочтены одним", pair[0], pair[1])
		}
	}
}

// Поля сессии ставит и снимает горутина Run, а читают их обработчики MCP из
// своих: без замка остановка сервиса посреди чтения даёт гонку и разыменование
// nil - проверка `клиент запущен` прошла бы, а поле обнулилось следом.
func TestSessionSurvivesConcurrentStop(t *testing.T) {
	client, err := New(Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"},
		func(context.Context, Message) error { return nil })
	if err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	api := tg.NewClient(stubInvoker{})
	peers := &stubPeers{}

	ctx := context.Background()
	stop := make(chan struct{})
	// Гонку ловит только перекрытие: без ожидания первого чтения горутина Run
	// успевает поставить и снять сессию раньше, чем читатели проснутся.
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
		t.Fatalf("остановленный клиент ответил %v", err)
	}
}

// Telegram отдаёт историю новыми сообщениями вперёд. Не развернув её, сервис
// показал бы агенту диалог задом наперёд.
func TestHistoryReturnsOldestFirst(t *testing.T) {
	client := runningClient(t, stubInvoker{fill: func(_ bin.Encoder, output bin.Decoder) error {
		box, ok := output.(*tg.MessagesMessagesBox)
		if !ok {
			return tgerr.New(500, "UNEXPECTED_REQUEST")
		}
		box.Messages = &tg.MessagesMessages{
			Messages: []tg.MessageClass{
				&tg.Message{ID: 3, PeerID: &tg.PeerUser{UserID: 7}, Message: "третье"},
				&tg.MessageService{ID: 2, PeerID: &tg.PeerUser{UserID: 7}},
				&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Message: "первое",
					Media: &tg.MessageMediaPhoto{}},
			},
			Users: []tg.UserClass{&tg.User{ID: 7, FirstName: "Иван", Username: "ivan"}},
		}
		return nil
	}}, &tg.User{ID: 7, FirstName: "Иван", Username: "ivan"})

	history, err := client.History(context.Background(), "user:7", 20, 0)
	if err != nil {
		t.Fatalf("история не прочитана: %v", err)
	}
	// Служебное сообщение выброшено, порядок от старых к новым.
	if len(history) != 2 || history[0].ID != 1 || history[1].ID != 3 {
		t.Fatalf("история %v", history)
	}
	if history[0].MediaKind != "photo" || history[0].Username != "ivan" {
		t.Fatalf("первое сообщение разобрано как %+v", history[0])
	}
}

// Список показывает все три вида адресата: у группы есть только название, у
// канала ещё ник и флаг вещания, а его access hash оседает в хранилище - без
// него приватная супергруппа стала бы нечитаемой навсегда.
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
				&tg.Message{ID: 5, PeerID: &tg.PeerUser{UserID: 7}, Message: "привет"},
			},
			Chats: []tg.ChatClass{
				&tg.Chat{ID: 3, Title: "Галера"},
				&tg.Channel{ID: 5, AccessHash: 99, Title: "Анонсы", Username: "club_news", Broadcast: true},
			},
			Users: []tg.UserClass{&tg.User{ID: 7, FirstName: "Иван", Username: "ivan"}},
		}
		return nil
	}})

	dialogs, err := client.Dialogs(context.Background(), 3)
	if err != nil {
		t.Fatalf("диалоги не прочитаны: %v", err)
	}
	if len(dialogs) != 3 {
		t.Fatalf("лимит не соблюдён: %d диалогов", len(dialogs))
	}
	if got := dialogs[0]; got.Peer != "chat:3" || got.Name != "Галера" || got.Username != "" || got.Unread != 4 {
		t.Fatalf("группа разобрана как %+v", got)
	}
	if got := dialogs[1]; got.Peer != "channel:5" || got.Name != "Анонсы" || got.Username != "club_news" || !got.Broadcast {
		t.Fatalf("канал разобран как %+v", got)
	}
	if got := dialogs[2]; got.Peer != "user:7" || got.Unread != 2 || got.Username != "ivan" || got.Last.Text != "привет" {
		t.Fatalf("личный диалог разобран как %+v", got)
	}
	if peers, _ := client.peers.(*stubPeers); peers == nil || len(peers.channels) != 1 || peers.channels[0].AccessHash != 99 {
		t.Fatalf("канал не попал в хранилище адресатов")
	}
}

// Кэш access hash - побочная выгода списка: отказавший диск лишает следующий
// срез хеша, но не отменяет уже разобранную выдачу.
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
		t.Fatal("подставное хранилище не поднялось")
	}
	peers.failAdd = errors.New("диск полон")

	dialogs, err := client.Dialogs(context.Background(), 5)
	if err != nil {
		t.Fatalf("отказ записи отменил выдачу: %v", err)
	}
	if len(dialogs) != 1 || dialogs[0].Peer != "channel:5" || dialogs[0].Name != "Анонсы" {
		t.Fatalf("канал разобран как %+v", dialogs)
	}
}

// Локальное хранилище отвечает без единого запроса к Telegram, поэтому идёт
// первым; сервер добирает незнакомцев, и один человек не должен попасть в
// ответ дважды.
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
		t.Fatalf("поиск не прошёл: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("найдено %+v", found)
	}
	if found[0].Ref != "user:7" || !found[0].Known {
		t.Fatalf("знакомый разобран как %+v", found[0])
	}
	// Незнакомца в хранилище нет: писать ему можно только после резолва ника.
	if found[1].Ref != "user:8" || found[1].Known {
		t.Fatalf("незнакомец разобран как %+v", found[1])
	}
	if found[0].Exact || found[1].Exact {
		t.Fatalf("похожие ники помечены точными: %+v", found)
	}

	// Тот же запрос ником: локальная находка точная, похожий ник из
	// contacts.search - нет.
	found, err = client.SearchUsers(context.Background(), "ivan", 10)
	if err != nil || len(found) != 2 {
		t.Fatalf("поиск по нику дал %+v (%v)", found, err)
	}
	if !found[0].Exact || found[1].Exact {
		t.Fatalf("точность локальной находки разобрана как %+v", found)
	}
}

// Незнакомца по точному нику не отдаёт ни локальное хранилище, ни
// contacts.search: без резолва tg_find отвечает «никого» о человеке, которому
// tg_send написал бы.
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
		t.Fatalf("поиск не прошёл: %v", err)
	}
	// Known false: адрес узнан этим же резолвом, знакомым человек от этого не
	// стал - иначе отчёт обещал бы диалог, которого нет.
	if len(found) != 1 || found[0].Ref != "user:9" || found[0].Name != "Пётр" || found[0].Known || !found[0].Exact {
		t.Fatalf("точный ник разобран как %+v", found)
	}

	// Неудачный резолв поиск не роняет: нечёткая часть отрабатывает как обычно.
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
		t.Fatalf("после неудачного резолва поиск дал %+v (%v)", found, err)
	}

	// Тот же ответ на точный запрос: резолв не дошёл, а точность видна по нику из
	// contacts.search.
	found, err = client.SearchUsers(context.Background(), "petr_ivanov", 10)
	if err != nil || len(found) != 1 || !found[0].Exact {
		t.Fatalf("точный ник из contacts.search разобран как %+v (%v)", found, err)
	}

	// Резолв сорван лимитом, а нечёткий поиск пуст: «никого не нашлось» здесь
	// враньё - contacts.resolveUsername лимитирован на живом номере, и человек,
	// которому tg_send напишет, существует. Наружу идёт причина срыва.
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
		t.Fatalf("сорванный резолв отдал %+v (%v)", found, err)
	}
}

// Человек сменил ник, в peer cache остался старый: локальный поиск по подстроке
// находит его же, и резолв точного ника приходит на знакомый id. Отбросить
// резолв целиком - ответить «точного ника нет» о том, кого искали.
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
		t.Fatalf("поиск не прошёл: %v", err)
	}
	if len(found) != 1 || !found[0].Exact || found[0].Username != "petr_ivanov" {
		t.Fatalf("сменивший ник разобран как %+v", found)
	}
}

// Клиент без сессии обязан отвечать отказом, а не падать: обработчик MCP может
// прийти раньше, чем поднимется Run.
func TestDialogToolsRefuseWithoutSession(t *testing.T) {
	client, err := New(Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"},
		func(context.Context, Message) error { return nil })
	if err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	ctx := context.Background()
	calls := map[string]error{}
	_, calls["search"] = client.SearchUsers(ctx, "ivan", 10)
	_, calls["history"] = client.History(ctx, "user:7", 20, 0)
	_, calls["dialogs"] = client.Dialogs(ctx, 20)

	var failure *Failure
	for name, err := range calls {
		if !errors.As(err, &failure) || failure.Kind != KindNotReady {
			t.Errorf("%s без сессии ответил %v", name, err)
		}
	}
}

// runningClient - клиент с поднятой подставной сессией: тесты пакета в сеть не
// ходят, ответы Telegram собирает stubInvoker.
func runningClient(t *testing.T, invoker stubInvoker, known ...*tg.User) *Client {
	t.Helper()
	client, err := New(Config{AppID: 1, AppHash: "hash", Phone: "+70000000000", StoragePath: "/data/tg/session.bolt"},
		func(context.Context, Message) error { return nil })
	if err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	client.setSession(tg.NewClient(invoker), &stubPeers{users: known})
	return client
}

// stubInvoker подменяет RPC: ответ пишется прямо в бокс результата, как это
// делает декодер провода.
type stubInvoker struct {
	fill func(input bin.Encoder, output bin.Decoder) error
}

func (s stubInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if s.fill == nil {
		return tgerr.New(500, "NO_STUB")
	}
	return s.fill(input, output)
}

// stubPeers - хранилище адресатов в памяти. Замок нужен ради теста гонки: там
// в него пишут и читают четыре горутины разом.
type stubPeers struct {
	mu       sync.Mutex
	users    []*tg.User
	channels []*tg.Channel
	// failAdd - отказ записи: полный диск или недоступный bolt.
	failAdd error
}

// Add запоминает адресата по-настоящему: на этом стоит поиск точного ника -
// ResolveUsername кладёт человека сюда, а имя читается отсюда же. Запись по id
// замещающая для обоих видов, как в боевом хранилище: сменивший ник человек и
// повторно увиденный в списке канал не должны лежать там дважды.
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
		// FromChat на ChatForbidden/ChannelForbidden кладёт ключ без сущности:
		// подставка такое не хранит и говорит об этом, а не роняет обход nil-ом.
		return fmt.Errorf("stub peers: адресат %v без user и channel", peer.Key)
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

// Блок в тройных кавычках уходит сущностью pre со смещением в единицах UTF-16:
// кириллица - одна единица, эмодзи - две. Без пары границ текст не трогается.
func TestSplitPre(t *testing.T) {
	cases := []struct {
		name, text, want string
		offset, length   int
		entity           bool
	}{
		{"таблица", "Итог 😀:\n```\nа б\nв г\n```", "Итог 😀:\nа б\nв г", 9, 7, true},
		{"текст после блока", "```\nx\n```\nконец", "x\nконец", 0, 1, true},
		{"без закрытия", "```\nx", "```\nx", 0, 0, false},
		{"без блока", "привет", "привет", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, entities := splitPre(c.text)
			if got != c.want {
				t.Fatalf("текст %q, ожидали %q", got, c.want)
			}
			if !c.entity {
				if len(entities) != 0 {
					t.Fatalf("лишняя разметка: %v", entities)
				}
				return
			}
			pre, ok := entities[0].(*tg.MessageEntityPre)
			if len(entities) != 1 || !ok || pre.Offset != c.offset || pre.Length != c.length {
				t.Fatalf("разметка %v, ожидали pre %d+%d", entities, c.offset, c.length)
			}
		})
	}
}
