package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// stubDialog - подменный транспорт диалога поверх существующего stubTransport:
// отправка и резолв берутся оттуда, чтение подставляется здесь.
type stubDialog struct {
	stubTransport

	found   []Contact
	dialogs []DialogInfo
	// poll отдаёт историю по номеру опроса: ожидание нового сообщения иначе
	// непроверяемо - оно и есть «история изменилась между опросами».
	poll func(n int) []ChatMessage
	// ref подменяет ссылку на собеседника: групповая ссылка - единственный
	// способ проверить рубеж «только личка», dry-run отдаёт пустую.
	ref telegram.Ref
	// resolveErr - отказ резолва и чтения: ограничение аккаунта приходит от них
	// раньше, чем от самой отправки.
	resolveErr error

	polls        int
	findLimit    int
	historyDepth int
}

func (d *stubDialog) Resolve(ctx context.Context, username string) (Peer, error) {
	if d.resolveErr != nil {
		return Peer{}, d.resolveErr
	}
	if d.ref != "" {
		return Peer{Username: username, Ref: d.ref}, nil
	}
	return d.stubTransport.Resolve(ctx, username)
}

func (d *stubDialog) Lookup(_ context.Context, username string) (Peer, error) {
	return d.Resolve(context.Background(), username)
}

func (d *stubDialog) Find(_ context.Context, _ string, limit int) ([]Contact, error) {
	d.findLimit = limit
	return d.found, nil
}

func (d *stubDialog) History(_ context.Context, _ Peer, limit int) ([]ChatMessage, error) {
	d.historyDepth = limit
	d.polls++
	if d.poll == nil {
		return nil, nil
	}
	return d.poll(d.polls), nil
}

func (d *stubDialog) Dialogs(_ context.Context, _ int) ([]DialogInfo, error) {
	return d.dialogs, nil
}

// chatOn собирает диалог на тестовом сервисе: порог FLOOD_WAIT - минута, как у
// отправщика.
func chatOn(service *Service, transport Dialog) *Chat {
	service.cfg.FloodWaitThreshold = time.Minute
	return NewChat(service, transport)
}

// offlineChat - диалог без базы: чтение, поиск и список диалогов в неё не
// ходят, и быстрый ярус проверяет их без Postgres.
func offlineChat(transport Dialog) *Chat {
	return NewChat(&Service{cfg: Config{AccountLabel: "test"}, Now: time.Now}, transport)
}

// sendOK - ручная отправка, которая обязана пройти.
func sendOK(t *testing.T, chat *Chat, username, text string) DirectSend {
	t.Helper()
	send, err := chat.Send(context.Background(), username, text)
	if err != nil {
		t.Fatalf("отправка @%s: %v", username, err)
	}
	return send
}

// directLog - последняя строка журнала ручных касаний по нику.
type directLog struct {
	state     string
	cold      bool
	messageID *int64
}

func directRow(t *testing.T, service *Service, username string) directLog {
	t.Helper()
	var row directLog
	err := service.pool.QueryRow(context.Background(),
		`SELECT state, cold, message_id FROM direct_messages WHERE username = $1 ORDER BY id DESC LIMIT 1`,
		username).Scan(&row.state, &row.cold, &row.messageID)
	if err != nil {
		t.Fatalf("прочитать журнал %q: %v", username, err)
	}
	return row
}

func directCount(t *testing.T, service *Service) int {
	t.Helper()
	var count int
	if err := service.pool.QueryRow(context.Background(), `SELECT count(*) FROM direct_messages`).Scan(&count); err != nil {
		t.Fatalf("посчитать журнал: %v", err)
	}
	return count
}

// runSQL ставит рубеж прямо SQL: стоп-лист и потолок аккаунта задаются мимо
// домена, иначе тест рубежа зависит от инструментов.
func runSQL(t *testing.T, service *Service, sql string, args ...any) {
	t.Helper()
	if _, err := service.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("подготовка (%s): %v", sql, err)
	}
}

// claimOne отдаёт строку получателя отправщику: с этого момента кампания несёт
// её в Telegram.
func claimOne(t *testing.T, service *Service) *Recipient {
	t.Helper()
	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(context.Background(), "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	return claimed
}

// stuckClaim оставляет строку взятой навсегда: отправщик умер до финала,
// кампанию поставили на паузу, чистить строку больше некому.
func stuckClaim(t *testing.T, service *Service) {
	t.Helper()
	claimOne(t, service)
	if _, err := service.StopCampaign(context.Background(), false); err != nil {
		t.Fatalf("пауза кампании: %v", err)
	}
	service.Now = func() time.Time { return testNow.Add(time.Hour) }
}

// Запрошенные величины приводятся к границам: неограниченная глубина разносит
// ответ инструмента, неограниченное ожидание вешает вызов. Часы сервиса прыгают
// на половину потолка за опрос, поэтому запрошенные десять минут кончаются за
// два шага, а не за шестьдесят.
func TestReadCapsDepthAndWait(t *testing.T) {
	transport := &stubDialog{}
	chat := offlineChat(transport)
	ctx := context.Background()

	if _, err := chat.Read(ctx, "ivan_petrov", 500, 0); err != nil {
		t.Fatalf("чтение с большой глубиной: %v", err)
	}
	if transport.historyDepth != maxHistory {
		t.Fatalf("глубина %d, ожидали потолок %d", transport.historyDepth, maxHistory)
	}
	if _, err := chat.Read(ctx, "ivan_petrov", 0, 0); err != nil {
		t.Fatalf("чтение без глубины: %v", err)
	}
	if transport.historyDepth != defaultHistory {
		t.Fatalf("глубина по умолчанию %d, ожидали %d", transport.historyDepth, defaultHistory)
	}
	if _, err := chat.Read(ctx, "Иван Петров", 0, 0); !errors.Is(err, ErrNoUsername) {
		t.Fatalf("чтение по имени: err = %v, ожидали ErrNoUsername", err)
	}

	start := time.Now()
	step := maxWait / 2
	chat.service.Now = func() time.Time { return start.Add(time.Duration(transport.polls) * step) }
	transport.polls = 0
	if _, err := chat.Read(ctx, "ivan_petrov", 0, 10*time.Minute); err != nil {
		t.Fatalf("чтение с ожиданием: %v", err)
	}
	if transport.polls != 3 {
		t.Fatalf("опросов %d, ожидали 3: десять минут не уложились в потолок %s", transport.polls, maxWait)
	}

	// Отрицательное ожидание - «не ждать вовсе»: ответ уходит первым же опросом.
	transport.polls = 0
	talk, err := chat.Read(ctx, "ivan_petrov", 0, -time.Second)
	if err != nil {
		t.Fatalf("чтение без ожидания: %v", err)
	}
	if transport.polls != 1 || talk.Waited != 0 {
		t.Fatalf("отрицательное ожидание дало %d опросов и ожидание %s", transport.polls, talk.Waited)
	}
}

// Групповые и канальные диалоги доходят до выдачи наравне с личными, в
// порядке транспорта: список больше не отсеивает и не обрезает ничего сам.
func TestDialogsIncludeGroupsAndChannels(t *testing.T) {
	chat := offlineChat(&stubDialog{dialogs: []DialogInfo{
		{Username: "ivan_petrov", Ref: telegram.NewRef(telegram.KindUser, 10)},
		{Name: "Чат клуба", Ref: telegram.NewRef(telegram.KindChat, 11)},
		{Name: "Анонсы", Ref: telegram.NewRef(telegram.KindChannel, 12), Broadcast: true},
	}})

	list, err := chat.Dialogs(context.Background(), 0)
	if err != nil {
		t.Fatalf("список диалогов: %v", err)
	}
	if len(list) != 3 || list[1].Name != "Чат клуба" || list[2].Name != "Анонсы" || !list[2].Broadcast {
		t.Fatalf("группа или канал не дошли до выдачи: %+v", list)
	}
}

// Поиск отдаёт не больше своего потолка и просит у транспорта столько же:
// длинный список имён в ответе инструмента не читается вовсе.
func TestFindNamesSearchLimits(t *testing.T) {
	transport := &stubDialog{}
	for i := range maxFound + 10 {
		transport.found = append(transport.found, Contact{Name: string(rune('а' + i%32))})
	}
	chat := offlineChat(transport)

	found, err := chat.Find(context.Background(), "Иван")
	if err != nil {
		t.Fatalf("поиск: %v", err)
	}
	if len(found) != maxFound || transport.findLimit != maxFound {
		t.Fatalf("найдено %d, у транспорта просили %d", len(found), transport.findLimit)
	}
	if _, err := chat.Find(context.Background(), "  "); err == nil {
		t.Fatal("пустой запрос ушёл в Telegram")
	}
}

// У ожидания два выхода, и проверяются оба. Чужая реплика отдаётся отдельно от
// истории, потому что ответ инструмента называет её по-другому; молчание
// собеседника - штатный ответ, а не ошибка. Вложение доезжает пометкой вида:
// содержимое не скачивается, но сказать «здесь было фото» инструмент обязан.
func TestReadWaitEndsWithReplyOrTimeout(t *testing.T) {
	ctx := context.Background()
	history := []ChatMessage{{ID: 1, Text: "смотрите", MediaKind: "photo", Outgoing: true}}

	talk, err := offlineChat(&stubDialog{poll: func(n int) []ChatMessage {
		if n >= 2 {
			return append(history, ChatMessage{ID: 2, Text: "ага, понял"})
		}
		return history
	}}).Read(ctx, "ivan_petrov", 0, maxWait)
	if err != nil {
		t.Fatalf("чтение с ожиданием: %v", err)
	}
	if len(talk.Fresh) != 1 || talk.Fresh[0].Text != "ага, понял" {
		t.Fatalf("новое сообщение не дождались: %+v", talk)
	}
	if len(talk.Messages) != 1 || talk.Messages[0].MediaKind != "photo" || talk.Waited <= 0 {
		t.Fatalf("история смешалась с новым, потеряла вложение или ожидание не измерено: %+v", talk)
	}

	quiet, err := offlineChat(&stubDialog{poll: func(int) []ChatMessage { return history }}).
		Read(ctx, "ivan_petrov", 0, time.Second)
	if err != nil {
		t.Fatalf("ожидание истекло ошибкой: %v", err)
	}
	if len(quiet.Fresh) != 0 || quiet.Waited != time.Second {
		t.Fatalf("ответ по таймауту: %+v", quiet)
	}
}

// Строка журнала ложится до сетевого вызова: «Telegram подтвердил - процесс
// умер - записи нет» оставило бы человека открытым для следующей кампании.
func TestDirectSendRecordedBeforeDelivery(t *testing.T) {
	service := newService(t)

	var stateAtSend string
	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		stateAtSend = directRow(t, service, "ivan_petrov").state
		return nil
	}}}

	send := sendOK(t, chatOn(service, transport), "ivan_petrov", "привет")
	row := directRow(t, service, "ivan_petrov")
	if stateAtSend != "pending" || row.state != "sent" || !row.cold || !send.Cold {
		t.Fatalf("во время отправки строка была %q, после - %q, cold %v, ответ %+v",
			stateAtSend, row.state, row.cold, send)
	}
}

// Отказ, при котором сообщение заведомо не ушло, снимает касание: человек
// снова доступен кампании, иначе он не получит ничего. FLOOD_WAIT здесь же:
// сервер отказался выполнить запрос, сообщение не создано, и pending закрыл бы
// человека для всех кампаний навсегда.
func TestPermanentFailureDoesNotCloseCampaigns(t *testing.T) {
	cases := map[string]error{
		"вне allow-list":      ErrNotAllowed,
		"личка закрыта":       &telegram.Failure{Kind: telegram.KindPeerForbidden, Permanent: true},
		"ник занят каналом":   &telegram.Failure{Kind: telegram.KindNotPerson, Permanent: true},
		"запрос отвергнут":    &telegram.Failure{Kind: telegram.KindBadRequest, Permanent: true},
		"короткий FLOOD_WAIT": floodWait(10 * time.Second),
		// Отказ сервера остаётся отказом сервера, чем бы он ни был: PEER_FLOOD
		// заодно останавливает аккаунт, и это не мешает закрыть строку журнала.
		"PEER_FLOOD": &telegram.Failure{
			Kind: telegram.KindPeerFlood, Type: "PEER_FLOOD", Permanent: true},
		// Сессии нет - запрос не отправлялся вовсе: SIGTERM между резолвом и
		// отправкой.
		"сессии нет": &telegram.Failure{Kind: telegram.KindNotReady},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			service := newService(t)
			ctx := context.Background()

			transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error { return failure }}}
			if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
				t.Fatal("отказ отдал успех")
			}
			if row := directRow(t, service, "ivan_petrov"); row.state != "failed" {
				t.Fatalf("строка журнала в состоянии %q, ожидалось failed", row.state)
			}

			draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "после отказа")
			if err != nil {
				t.Fatalf("создать кампанию: %v", err)
			}
			report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
			if err != nil {
				t.Fatalf("импорт: %v", err)
			}
			if report.Accepted != 1 {
				t.Fatalf("человек остался закрытым: %+v", report)
			}
		})
	}
}

// Строку получателя увели в skipped, пока шла ручная отправка, а она не
// удалась: строка возвращается в очередь, иначе человек не получит ничего, а
// причина в отчёте соврёт.
func TestPermanentFailureReturnsSkippedRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		// Очередь видит уже записанное касание и снимает строку получателя -
		// ровно та гонка, ради которой возврат и делается.
		if _, err := service.ClaimNext(ctx, "test"); err != nil {
			t.Fatalf("виток очереди во время отправки: %v", err)
		}
		return &telegram.Failure{Kind: telegram.KindPeerForbidden, Permanent: true}
	}}}

	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
		t.Fatal("закрытая личка отдала успех")
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "planned" || reason != "" {
		t.Fatalf("строка получателя осталась %q с причиной %q", state, reason)
	}
}

// Рубеж отказывает одинаково, откуда бы отказ ни пришёл: наружу ничего не
// уходит и строки журнала не прибавляется. Механизм один, входов много -
// поэтому таблица, а не двенадцать почти одинаковых тестов.
func TestDirectSendRefusedByGuards(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, service *Service, transport *stubDialog)
		// dryRun: allow-list живёт в транспорте dev-контура, а не в базе.
		dryRun   bool
		username string
		text     string
		want     error
		// advice: что отказ обязан подсказать агенту, когда выход неочевиден.
		advice string
	}{
		{name: "ник не похож на ник", username: "Иван Петров", want: ErrNoUsername},
		{name: "пустой текст", username: "ivan_petrov", want: ErrEmptyText, text: ""},
		{name: "текст из одних пробелов", username: "ivan_petrov", want: ErrEmptyText, text: "   "},
		{name: "текст длиннее предела Telegram", username: "ivan_petrov", want: ErrTextTooLong,
			text: strings.Repeat("а", maxTextLen+1)},
		// Ник группы резолвится так же успешно, как ник человека, и без проверки
		// ссылки сообщение ушло бы в чат.
		{name: "групповой чат вместо лички", username: "club_chat", want: ErrNotPrivate,
			prepare: func(_ *testing.T, _ *Service, transport *stubDialog) {
				transport.ref = telegram.NewRef(telegram.KindChat, 11)
			}},
		// Инвариант говорит «с этого аккаунта целиком», а не «в рассылке».
		{name: "аккаунт остановлен", username: "ivan_petrov", want: ErrAccountStopped,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				if err := service.StopAccount(context.Background(), "test", telegram.KindPeerFlood); err != nil {
					t.Fatalf("остановить аккаунт: %v", err)
				}
			}},
		{name: "стоп-лист по нику", username: "ivan_petrov", want: ErrStopList,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				runSQL(t, service, `INSERT INTO stop_list (username) VALUES ('ivan_petrov')`)
			}},
		// Запись стоп-листа без ника: ник связывается с контактом amoCRM через
		// строки кампаний, иначе рубеж обходится ником.
		{name: "стоп-лист по контакту amoCRM", username: "ivan_petrov", want: ErrStopList,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				startWith(t, service, person(1, "ivan_petrov"))
				runSQL(t, service, `INSERT INTO stop_list (amo_contact_id) VALUES (1)`)
			}},
		// Dev allow-list - последний рубеж перед выходом наружу.
		{name: "ник вне dev allow-list", username: "stranger_one", want: ErrNotAllowed, dryRun: true,
			prepare: func(_ *testing.T, service *Service, _ *stubDialog) {
				service.cfg.DevAllowList = []string{"allowed_one"}
			}},
		// Ночная рассылка по списку из tg_dialogs - обход всей машинерии кампании.
		{name: "первое касание незнакомца ночью", username: "ivan_petrov", want: ErrColdWindow,
			prepare: func(_ *testing.T, service *Service, _ *stubDialog) {
				service.Now = func() time.Time { return time.Date(2026, 9, 2, 3, 0, 0, 0, moscowZone) }
			}},
		// Потолок общий с рассылкой: кампанийная отправка расходует его наравне с
		// ручной, иначе ручной канал обходит дневной предел аккаунта.
		{name: "дневной потолок израсходован кампанией", username: "stranger_one", want: ErrColdLimit,
			prepare: func(t *testing.T, service *Service, transport *stubDialog) {
				runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
				startWith(t, service, person(1, "ivan_petrov"))
				if err := senderOn(service, transport).step(context.Background()); err != nil {
					t.Fatalf("виток рассылки: %v", err)
				}
			}},
		// Отправщик уже несёт строку в Telegram: ручное касание дало бы второе.
		{name: "кампания пишет этому человеку прямо сейчас", username: "ivan_petrov", want: ErrCampaignSending,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) { claimOne(t, service) }},
		// Строка вернулась в очередь после неоднозначного отказа: claim снят, но
		// попытка была и её доставка неизвестна - ручное касание дало бы второе
		// сообщение.
		{name: "строка вернулась в очередь после отказа", username: "ivan_petrov", want: ErrCampaignSending,
			prepare: func(t *testing.T, service *Service, _ *stubDialog) {
				claimed := claimOne(t, service)
				if _, err := service.FinishClaim(context.Background(), claimed, ResultRetry, "unknown_error"); err != nil {
					t.Fatalf("вернуть строку в очередь: %v", err)
				}
			}},
		// Протухший claim рубеж не снимает: доставка такой строки неизвестна. Сама
		// строка не чинится, поэтому отказ обязан назвать выход.
		{name: "протухший claim кампании на паузе", username: "ivan_petrov", want: ErrCampaignSending, advice: "cancel",
			prepare: func(t *testing.T, service *Service, _ *stubDialog) { stuckClaim(t, service) }},
		// Доставка первого касания неизвестна, и вторая строка списала бы за него
		// ещё единицу потолка. Неопознанный отказ - единственный такой случай:
		// названные Telegram отказы означают «запрос не выполнен». Рубеж держит
		// сервис, а не транспорт: к моменту повтора Telegram уже не отказывает.
		{name: "повтор поверх незакрытого касания", username: "ivan_petrov", want: ErrDirectPending, advice: "5 минут",
			prepare: func(t *testing.T, service *Service, transport *stubDialog) {
				transport.fail = func(string) error { return errors.New("сеть отвалилась") }
				if _, err := chatOn(service, transport).Send(context.Background(), "ivan_petrov", "привет"); err == nil {
					t.Fatal("неопознанный отказ отдал успех")
				}
				transport.fail = nil
			}},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			service := newService(t)
			transport := &stubDialog{}
			if item.prepare != nil {
				item.prepare(t, service, transport)
			}
			chat := chatOn(service, transport)
			if item.dryRun {
				chat = chatOn(service, NewDryRunTransport(service.cfg))
			}
			text := item.text
			// "" по умолчанию значит «текст неважен для этого рубежа»; для рубежа
			// пустого текста именно "" - сам проверяемый ввод.
			if text == "" && !errors.Is(item.want, ErrEmptyText) {
				text = "привет"
			}
			// Подготовка сама могла писать и наружу, и в журнал: рубеж проверяется
			// приростом, а не абсолютом.
			sent, journaled := len(transport.Sent()), directCount(t, service)

			_, err := chat.Send(context.Background(), item.username, text)
			if !errors.Is(err, item.want) {
				t.Fatalf("отказ рубежа: err = %v, ожидали %v", err, item.want)
			}
			if len(transport.Sent()) != sent {
				t.Fatal("сообщение ушло наружу вопреки рубежу")
			}
			if count := directCount(t, service); count != journaled {
				t.Fatalf("рубеж отказал, а строк журнала стало %d вместо %d", count, journaled)
			}
			if item.advice != "" && !strings.Contains(toolError(err).Error(), item.advice) {
				t.Fatalf("отказ не называет выход %q: %s", item.advice, toolError(err))
			}
		})
	}
}

// Строка вернулась в очередь после временного отказа, а кампанию поставили на
// паузу: claimed_at пуст, счётчик зависших claim такую строку не видит, и сама
// она не разберётся никогда. Отказ обязан назвать кампанию и исполнимое
// действие, иначе человек закрыт бессрочно, а агент ищет несуществующий признак.
func TestPausedCampaignHoldNamesCampaign(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	claimed := claimOne(t, service)
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "unknown_error"); err != nil {
		t.Fatalf("вернуть строку в очередь: %v", err)
	}
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("пауза кампании: %v", err)
	}

	_, err := chatOn(service, &stubDialog{}).Send(ctx, "ivan_petrov", "привет")
	if !errors.Is(err, ErrCampaignSending) {
		t.Fatalf("отказ гарда: err = %v, ожидали ErrCampaignSending", err)
	}
	answer := toolError(err).Error()
	for _, want := range []string{fmt.Sprintf("кампании %d", claimed.CampaignID), `"тест"`, "start", "cancel"} {
		if !strings.Contains(answer, want) {
			t.Fatalf("отказ не называет %q: %s", want, answer)
		}
	}
}

// Строка отменённой кампании человека не держит: отправлять по ней больше
// некому, чинить её человеку нечем - stop с cancel на отменённой кампании
// отказывает, - и вечная блокировка ручной отправки была бы дороже риска.
func TestCancelledCampaignDoesNotBlockDirectSend(t *testing.T) {
	service := newService(t)

	claimOne(t, service)
	if _, err := service.StopCampaign(context.Background(), true); err != nil {
		t.Fatalf("отмена кампании: %v", err)
	}
	// Отправщик умер, claim протух: строка осталась planned навсегда.
	service.Now = func() time.Time { return testNow.Add(time.Hour) }

	sendOK(t, chatOn(service, &stubDialog{}), "ivan_petrov", "привет")
}

// Журнал закрывает человека для кампаний, но не для второй реплики: иначе
// диалог обрывался бы после первого сообщения. Вторая реплика уже тёплая и
// уходит своим random_id - стабильный id Telegram проглотил бы как дубль.
func TestDirectSendTwiceToSamePersonAllowed(t *testing.T) {
	service := newService(t)

	transport := &stubDialog{stubTransport: stubTransport{messageID: 4242}}
	chat := chatOn(service, transport)
	sendOK(t, chat, "ivan_petrov", "ок")
	if send := sendOK(t, chat, "ivan_petrov", "ок"); send.Cold {
		t.Fatal("вторая реплика посчиталась холодным касанием")
	}
	if sent := transport.Sent(); len(sent) != 2 || sent[0].RandomID == sent[1].RandomID {
		t.Fatalf("одинаковые реплики ушли с одним random_id: %+v", sent)
	}
	row := directRow(t, service, "ivan_petrov")
	if count := directCount(t, service); row.state != "sent" || row.cold || count != 2 {
		t.Fatalf("вторая строка журнала %q, cold %v, всего строк %d", row.state, row.cold, count)
	}
	// Без message_id строка журнала не связывается с сообщением в диалоге, и
	// разбор ручного касания идёт по тексту.
	if row.messageID == nil || *row.messageID != 4242 {
		t.Fatalf("в журнале message_id %v, ожидали 4242", row.messageID)
	}
}

// Реплика в идущем диалоге окну и потолку не подчиняется: запрет ответить в
// 21:30 делает аккаунт менее похожим на живой, а не более.
func TestWarmReplyIgnoresWindowAndCeiling(t *testing.T) {
	service := newService(t)

	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
	service.Now = func() time.Time { return time.Date(2026, 9, 2, 21, 30, 0, 0, moscowZone) }
	// Тёплым делает входящее: собеседник написал сам.
	transport := &stubDialog{poll: func(int) []ChatMessage {
		return []ChatMessage{{ID: 1, Text: "здравствуйте"}}
	}}

	send := sendOK(t, chatOn(service, transport), "ivan_petrov", "добрый вечер")
	row := directRow(t, service, "ivan_petrov")
	if send.Cold || row.state != "sent" || row.cold {
		t.Fatalf("реплика посчиталась холодной: ответ %+v, строка %q, cold %v", send, row.state, row.cold)
	}
}

// Служебному адресату владельца пишется вне окна и при исчерпанном потолке:
// это проверка своего аккаунта, а не касание незнакомца, - и расход аккаунта
// такая отправка не увеличивает. Незнакомцу в тех же условиях отказ остаётся.
func TestOwnerDirectSendIgnoresWindowAndLimit(t *testing.T) {
	service := newService(t, "owner_one")
	ctx := context.Background()

	// Потолок в единицу и одно холодное касание за сегодня: расход исчерпан.
	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
	runSQL(t, service, `INSERT INTO direct_messages
		 (username, account_id, text, random_id, cold, state, created_at)
		 VALUES ('stranger_zero', (SELECT id FROM accounts WHERE label = 'test'), 'привет', 777, true, 'sent', $1)`,
		testNow)
	service.Now = func() time.Time { return time.Date(2026, 9, 2, 21, 30, 0, 0, moscowZone) }
	chat := chatOn(service, &stubDialog{})

	if _, err := chat.Send(ctx, "stranger_one", "привет"); !errors.Is(err, ErrColdWindow) {
		t.Fatalf("незнакомцу вне окна: err = %v, ожидали ErrColdWindow", err)
	}
	// Ник в неканоническом виде: служебного адресата узнаёт та же нормализация,
	// что и остальные рубежи.
	sendOK(t, chat, "@Owner_One", "привет")
	if row := directRow(t, service, "owner_one"); row.state != "sent" || row.cold {
		t.Fatalf("строка журнала служебного адресата %q, cold %v", row.state, row.cold)
	}

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if used := status.Accounts[0].UsedToday; used != 1 {
		t.Fatalf("расход после служебной отправки %d, ожидали прежний 1", used)
	}
}

// Удержание на время идущей кампании для служебного адресата снято: строка его
// кампании уже взята отправщиком, а ручное касание всё равно уходит. Для
// незнакомца тот же случай остаётся ErrCampaignSending (таблица выше), и цена
// снятия названа в спеке: владельцу может прийти два сообщения подряд.
func TestOwnerDirectSendIgnoresCampaignHold(t *testing.T) {
	service := newService(t, "owner_one")
	ctx := context.Background()

	startWith(t, service, person(1, "owner_one"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку служебного адресата: %v, %+v", err, claimed)
	}

	sendOK(t, chatOn(service, &stubDialog{}), "owner_one", "привет")
	if row := directRow(t, service, "owner_one"); row.state != "sent" {
		t.Fatalf("строка журнала служебного адресата %q, ожидали sent", row.state)
	}
}

// Ограничение Telegram на ручной отправке останавливает аккаунт целиком, а
// строка журнала уходит в failed: запрос не выполнен, сообщение не создано.
func TestFloodWaitOnDirectSendStopsAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		return floodWait(2 * time.Minute)
	}}}
	logged := captureLogs(t)
	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
		t.Fatal("длинный FLOOD_WAIT отдал успех")
	}
	stopped, reason, err := service.AccountStopped(ctx, "test")
	if err != nil || !stopped || reason != telegram.KindFloodWait {
		t.Fatalf("аккаунт не остановлен: %v, %q, %v", stopped, reason, err)
	}
	if row := directRow(t, service, "ivan_petrov"); row.state != "failed" {
		t.Fatalf("строка журнала в состоянии %q, ожидалось failed", row.state)
	}
	if !strings.Contains(logged.String(), "account stopped by telegram limit") {
		t.Fatalf("стоп аккаунта не виден в логе:\n%s", logged.String())
	}
}

// Отказ 5xx с кодом Telegram доставку не отменяет: сервер мог создать сообщение
// и упасть уже после этого. Строка журнала остаётся pending и держит человека
// закрытым - иначе следующая кампания напишет ему второй раз своим random_id, и
// дедупликация Telegram по random_id не спасёт.
func TestUnknownServerFailureKeepsTouchPending(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubDialog{stubTransport: stubTransport{fail: func(string) error {
		return &telegram.Failure{Kind: telegram.KindTemporary, Type: "RPC_CALL_FAIL"}
	}}}
	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); err == nil {
		t.Fatal("отказ 500 отдал успех")
	}
	if row := directRow(t, service, "ivan_petrov"); row.state != "pending" {
		t.Fatalf("строка журнала в состоянии %q, ожидалось pending", row.state)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "после 500")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("импорт: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonDirectPending] != 1 {
		t.Fatalf("человек открыт для кампании после 500: %+v", report)
	}
}

// Ограничение принадлежит аккаунту, а не вызову: contacts.resolveUsername
// отвечает FLOOD_WAIT раньше отправки, и без стопа агент повторял бы tg_send,
// тратя на ограниченном аккаунте ещё один резолв за каждый повтор.
func TestLimitFromResolveStopsAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubDialog{resolveErr: floodWait(2 * time.Minute)}
	if _, err := chatOn(service, transport).Send(ctx, "ivan_petrov", "привет"); !errors.Is(err, ErrAccountStopped) {
		t.Fatalf("отказ резолва: err = %v, ожидали стоп аккаунта", err)
	}
	stopped, reason, err := service.AccountStopped(ctx, "test")
	if err != nil || !stopped || reason != telegram.KindFloodWait {
		t.Fatalf("аккаунт не остановлен резолвом: %v, %q, %v", stopped, reason, err)
	}
	if count := directCount(t, service); count != 0 {
		t.Fatalf("строк журнала %d: отказ до записи журнала их не заводит", count)
	}

	// Читающие инструменты ходят той же сессией и под теми же лимитами, поэтому
	// останавливают аккаунт наравне с отправкой: ложный стоп стоит одного
	// resume_account, пропущенный - аккаунта.
	if _, _, err := service.ResumeAccount(ctx, "test"); err != nil {
		t.Fatalf("снять стоп: %v", err)
	}
	if _, err := chatOn(service, transport).Read(ctx, "ivan_petrov", 0, 0); !errors.Is(err, ErrAccountStopped) {
		t.Fatalf("чтение при ограничении: err = %v, ожидали стоп аккаунта", err)
	}
	if stopped, _, _ := service.AccountStopped(ctx, "test"); !stopped {
		t.Fatal("ограничение, пришедшее от чтения, аккаунт не остановило")
	}
}

// Ручные касания видны в строке аккаунта, а зависшие строки - отдельным счётом:
// и pending в журнале, и протухший claim кампании молча закрывают человека
// навсегда, чинятся руками и потому обязаны быть названы снаружи.
func TestStatusShowsManualTouchesAndStuckRows(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	stuckClaim(t, service)
	insertDirect(t, service, "ivan_petrov", "sent", testNow)
	insertDirect(t, service, "lost_one", "pending", testNow.Add(-time.Hour))

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].WarmToday != 2 || status.Accounts[0].ColdToday != 0 {
		t.Fatalf("ручные касания в отчёте: %+v", status.Accounts)
	}
	if status.StuckDirect != 1 || status.StuckClaims != 1 {
		t.Fatalf("зависших касаний %d и claim %d, ожидали по одному", status.StuckDirect, status.StuckClaims)
	}
	var text strings.Builder
	writeAccounts(&text, status)
	if !strings.Contains(text.String(), "Строк с зависшим claim: 1") {
		t.Fatalf("зависший claim не назван в отчёте:\n%s", text.String())
	}

	logged := captureLogs(t)
	service.LogStuckDirect(ctx)
	if !strings.Contains(logged.String(), "direct message stuck pending") {
		t.Fatalf("зависшее касание не названо при старте:\n%s", logged.String())
	}
}

// Строка аккаунта называет потолок так же, как его считает рубеж: холодные
// касания руками входят в дневной предел, тёплые - нет. Проверяется текст, а не
// счётчики: рубеж, о котором никто не узнал, равен его отсутствию.
func TestStatusTellsTruthAboutDailyLimit(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	runSQL(t, service, `UPDATE accounts SET daily_limit = 2 WHERE label = 'test'`)
	chat := chatOn(service, &stubDialog{})
	for _, name := range []string{"stranger_one", "stranger_one", "stranger_two"} {
		sendOK(t, chat, name, "привет")
	}
	// Два холодных касания израсходовали потолок целиком.
	if _, err := chat.Send(ctx, "stranger_three", "привет"); !errors.Is(err, ErrColdLimit) {
		t.Fatalf("третье холодное касание: err = %v, ожидали ErrColdLimit", err)
	}

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var text strings.Builder
	writeAccounts(&text, status)
	report := text.String()
	for _, want := range []string{"сегодня 2 из 2", "из них первых касаний руками: 2", "тёплых ответов сегодня: 1"} {
		if !strings.Contains(report, want) {
			t.Fatalf("в строке аккаунта нет %q:\n%s", want, report)
		}
	}
}

// Ник нормализуется на входе: без этого @Ivan_Petrov промахивается мимо
// стоп-листа, мимо гарда гонки и мимо собственного журнала.
func TestUsernameNormalizedOnDirectSend(t *testing.T) {
	service := newService(t)

	chat := chatOn(service, &stubDialog{})
	sendOK(t, chat, "@Ivan_Petrov", "привет")
	if row := directRow(t, service, "ivan_petrov"); row.state != "sent" {
		t.Fatalf("строка журнала по нормализованному нику: %q", row.state)
	}

	runSQL(t, service, `INSERT INTO stop_list (username) VALUES ('petr_sidorov')`)
	if _, err := chat.Send(context.Background(), "https://t.me/Petr_Sidorov", "привет"); !errors.Is(err, ErrStopList) {
		t.Fatalf("ник в обёртке t.me промахнулся мимо стоп-листа: err = %v", err)
	}
}
