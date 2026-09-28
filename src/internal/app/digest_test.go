package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// digestEvening - будний вечер после закрытия окна: время, в которое сводка и
// уходит.
var digestEvening = time.Date(2026, 9, 2, 19, 30, 0, 0, moscowZone)

// digestSenderOn собирает отправщика со сводкой на подменном диалоге и ставит
// время на вечер: сводка живёт во времени, а не в состоянии процесса.
func digestSenderOn(t *testing.T, service *Service, dialog *stubDialog) *Sender {
	t.Helper()
	service.cfg.Transport = "live"
	service.Now = func() time.Time { return digestEvening }
	sender := senderOn(service, dialog)
	sender.UseChat(chatOn(service, dialog))
	return sender
}

// sentToday помечает часть строк кампании отправленными сегодня: сводка
// считает день по sent_at, а не по состоянию очереди.
func sentToday(t *testing.T, service *Service, campaignID int64, sent, replied int) {
	t.Helper()
	if _, err := service.pool.Exec(context.Background(),
		`UPDATE recipients SET state = 'sent', sent_at = $2,
		        account_id = (SELECT id FROM accounts WHERE label = 'test')
		 WHERE id IN (SELECT id FROM recipients WHERE campaign_id = $1 ORDER BY id LIMIT $3)`,
		campaignID, digestEvening, sent+replied); err != nil {
		t.Fatalf("пометить отправленные: %v", err)
	}
	if replied == 0 {
		return
	}
	if _, err := service.pool.Exec(context.Background(),
		`UPDATE recipients SET state = 'replied', replied_at = $2
		 WHERE id IN (SELECT id FROM recipients WHERE campaign_id = $1 AND state = 'sent' ORDER BY id LIMIT $3)`,
		campaignID, digestEvening, replied); err != nil {
		t.Fatalf("пометить ответивших: %v", err)
	}
}

// Время сводки: будний вечер после 19:00. До 19:00 и в выходной сводки нет,
// иначе заказчик получил бы отчёт о неполном дне.
func TestDigestDue(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"до окна", time.Date(2026, 9, 2, 18, 59, 0, 0, moscowZone), false},
		{"ровно в 19", time.Date(2026, 9, 2, 19, 0, 0, 0, moscowZone), true},
		{"поздний вечер", time.Date(2026, 9, 2, 22, 40, 0, 0, moscowZone), true},
		{"суббота", time.Date(2026, 9, 5, 19, 30, 0, 0, moscowZone), false},
	}
	for _, c := range cases {
		if got := digestDue(c.when); got != c.want {
			t.Errorf("%s: digestDue = %v, ожидали %v", c.name, got, c.want)
		}
	}
}

// Сводка уходит каждому служебному адресату один раз за день: отправщиков
// несколько, и без замка в базе каждый отправил бы свою.
func TestDigestSentOncePerDay(t *testing.T) {
	service := newService(t, "owner_one", "owner_two")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	sentToday(t, service, campaignID, 1, 1)

	for range 3 {
		if err := sender.maybeSendDigest(ctx); err != nil {
			t.Fatalf("сводка: %v", err)
		}
	}

	if got := len(dialog.Sent()); got != 2 {
		t.Fatalf("ушло %d сводок, ожидали по одной каждому адресату", got)
	}
	var rows int
	if err := service.pool.QueryRow(ctx,
		`SELECT count(*) FROM daily_digest WHERE sent_at IS NOT NULL`).Scan(&rows); err != nil {
		t.Fatalf("журнал сводок: %v", err)
	}
	if rows != 2 {
		t.Fatalf("в журнале %d строк, ожидали 2", rows)
	}
}

// Сводку отправляет только названный аккаунт: при двух отправщиках она иначе
// приходит то из одного чата, то из другого, и читается как сбой сервиса.
func TestDigestSentOnlyByDigestAccount(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.DigestAccount = "other"
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка чужого аккаунта: %v", err)
	}
	if got := len(dialog.Sent()); got != 0 {
		t.Fatalf("ушло %d сводок, ожидали ни одной: отправитель - другой аккаунт", got)
	}

	service.cfg.DigestAccount = "test"
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка своего аккаунта: %v", err)
	}
	if got := len(dialog.Sent()); got != 1 {
		t.Fatalf("ушло %d сводок, ожидали одну", got)
	}
}

// Получатели сводки - свой список, а не все служебные адресаты: DEV_ALLOW_LIST
// снимает рубежи для проверок рассылки и шире круга читателей отчёта.
func TestDigestGoesToDigestTo(t *testing.T) {
	service := newService(t, "owner_one", "owner_two", "owner_three")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.DigestTo = []string{"owner_one", "owner_two"}
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка: %v", err)
	}
	got := make(map[string]bool)
	for _, item := range dialog.Sent() {
		got[item.Username] = true
	}
	if len(got) != 2 || !got["owner_one"] || !got["owner_two"] {
		t.Fatalf("сводка ушла %v, ожидали owner_one и owner_two", got)
	}
}

// Метка отправителя без живого процесса остановила бы сводку насовсем и молча.
// Видит это тот, кто её не отправляет, и говорит журналом - в нём единственный
// след такой поломки. До запаса в час он молчит: в это время отправитель сводку
// и шлёт.
func TestDigestMissingWarnedByOtherSender(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.DigestAccount = "other"
	log := captureLogs(t)
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("вечер до запаса: %v", err)
	}
	if strings.Contains(log.String(), "digest not sent today") {
		t.Fatalf("тревога раньше %d:00 МСК:\n%s", digestWarnHour, log)
	}

	service.Now = func() time.Time { return digestEvening.Add(2 * time.Hour) }
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("поздний вечер: %v", err)
	}
	if !strings.Contains(log.String(), "digest not sent today") {
		t.Fatalf("вечер без сводки прошёл молча:\n%s", log)
	}
	if len(dialog.Sent()) != 0 {
		t.Fatal("сводку отправил процесс с чужой меткой")
	}

	// Сводка ушла - тревоги больше нет: сигнал обязан сниматься сам, иначе его
	// перестают читать.
	service.cfg.DigestAccount = "test"
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка своего аккаунта: %v", err)
	}
	service.cfg.DigestAccount = "other"
	log.Reset()
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("вечер после сводки: %v", err)
	}
	if strings.Contains(log.String(), "digest not sent today") {
		t.Fatalf("тревога об отправленной сводке:\n%s", log)
	}
}

// Текст сводки несёт день, разрез по аккаунту и итог кампании: заказчику нужны
// числа, а не факт отправки.
func TestDigestTextCounts(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()

	campaignID := startWith(t, service,
		person(1, "ivan_petrov"), person(2, "petr_ivanov"), person(3, "anna_sidorova"))
	sentToday(t, service, campaignID, 1, 1)

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка: %v", err)
	}
	sent := dialog.Sent()
	if len(sent) != 1 {
		t.Fatalf("ушло %d сводок, ожидали одну", len(sent))
	}
	text := sent[0].Text
	for _, want := range []string{"2 сентября", "Сегодня ушло 2, ответили 1", "test - ушло 2, ответили 1", "Всего ушло 2 из 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("в сводке нет %q:\n%s", want, text)
		}
	}
}

// Отказ на одном адресате не лишает сводки остальных и не рождает второй
// отправки тому, кому она уже ушла.
func TestDigestRetriesOnlyFailed(t *testing.T) {
	service := newService(t, "owner_one", "owner_two")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	dialog.fail = func(username string) error {
		if username == "owner_two" {
			return errors.New("telegram недоступен")
		}
		return nil
	}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()

	campaignID := startWith(t, service, person(1, "ivan_petrov"))
	sentToday(t, service, campaignID, 1, 0)

	if err := sender.maybeSendDigest(ctx); err == nil {
		t.Fatal("отказ отправки не назван ошибкой")
	}
	// Повтор приходит следующим витком, а не сразу: после неудачи Chat держит
	// карантин на адресата - сообщение в неизвестном состоянии дублировать
	// нельзя. Пауза цикла 10-30 минут этот карантин перекрывает.
	dialog.fail = nil
	later := digestEvening.Add(10 * time.Minute)
	service.Now = func() time.Time { return later }
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("повтор сводки: %v", err)
	}

	got := map[string]int{}
	for _, d := range dialog.Sent() {
		got[d.Username]++
	}
	if got["owner_one"] != 1 || got["owner_two"] != 1 {
		t.Fatalf("сводки ушли %v, ожидали по одной каждому", got)
	}
}

// В dry-run сводка не уходит вовсе: письмо не ушло бы, а замок съел бы день, и
// на живом контуре сводка оказалась бы уже отправленной.
func TestDigestSilentInDryRun(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	service.cfg.Transport = "dry-run"
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка: %v", err)
	}
	if got := len(dialog.Sent()); got != 0 {
		t.Fatalf("в dry-run ушло %d сводок", got)
	}
}

// setRecipient ставит строке кампании состояние, причину и аккаунт: воронка
// считается по ним, а довести каждую строку до состояния честным путём тест не
// обязан. Пустая метка оставляет строку без аккаунта.
func setRecipient(t *testing.T, service *Service, campaignID, contactID int64, state, reason, label string) {
	t.Helper()
	tag, err := service.pool.Exec(context.Background(),
		`UPDATE recipients SET state = $3, reason = $4, sent_at = CASE WHEN $3 IN ('sent', 'replied') THEN $6::timestamptz END,
		        account_id = (SELECT id FROM accounts WHERE label = $5)
		 WHERE campaign_id = $1 AND amo_contact_id = $2`,
		campaignID, contactID, state, reason, label, digestEvening)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("строка %d кампании %d: %v, строк %d", contactID, campaignID, err, tag.RowsAffected())
	}
}

// Воронка в трёх разрезах сходится с состояниями строк: брак ника - отправка,
// сбой сервиса - нет; снятые рубежом, отменённые, служебный адресат и черновик
// не считаются, отменённая до старта кампания рассылкой не числится; аккаунт
// без строк виден нулями без долей.
func TestDigestFunnel(t *testing.T) {
	service := newService(t, "owner_one")
	dialog := &stubDialog{stubTransport: stubTransport{peerID: 100, messageID: 7}}
	sender := digestSenderOn(t, service, dialog)
	ctx := context.Background()
	for _, label := range []string{"second", "idle"} {
		if _, err := service.pool.Exec(ctx,
			`INSERT INTO accounts (label, daily_limit) VALUES ($1, 30)`, label); err != nil {
			t.Fatalf("аккаунт %s: %v", label, err)
		}
	}

	past := startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"), person(3, "owner_one"))
	setRecipient(t, service, past, 1, "sent", "", "test")
	setRecipient(t, service, past, 2, "replied", "", "test")
	setRecipient(t, service, past, 3, "sent", "", "test")
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET status = 'done', finished_at = $2 WHERE id = $1`, past, digestEvening); err != nil {
		t.Fatalf("закрыть прошлую кампанию: %v", err)
	}

	current := startWith(t, service,
		person(11, "anna_sidorova"), person(12, "oleg_smirnov"), person(13, "gone_user"),
		person(14, "broken_send"), person(15, "back_in_queue"), person(16, "seen_before"),
		person(17, "was_cancelled"), person(18, "not_claimed"), person(19, "maria_orlova"),
		person(20, "owner_one"), person(21, "group_nick"))
	setRecipient(t, service, current, 11, "sent", "", "test")
	setRecipient(t, service, current, 12, "replied", "", "test")
	setRecipient(t, service, current, 13, "undelivered", "telegram_peer_forbidden", "test")
	setRecipient(t, service, current, 14, "undelivered", ReasonRetryExhausted, "second")
	setRecipient(t, service, current, 15, "planned", "", "second")
	setRecipient(t, service, current, 16, "skipped", ReasonAlreadySent, "second")
	setRecipient(t, service, current, 17, "cancelled", ReasonCampaignGone, "second")
	setRecipient(t, service, current, 19, "sent", "", "second")
	setRecipient(t, service, current, 20, "replied", "", "test")
	setRecipient(t, service, current, 21, "undelivered", "telegram_not_person", "test")

	var cancelled int64
	if err := service.pool.QueryRow(ctx,
		`INSERT INTO campaigns (title, template_text, status, finished_at)
		 VALUES ('отменённая', 'Привет!', 'cancelled', $1) RETURNING id`, digestEvening).Scan(&cancelled); err != nil {
		t.Fatalf("отменённая кампания: %v", err)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, username, state, random_id)
		 VALUES ($1, 40, 'never_started', 'cancelled', 40)`, cancelled); err != nil {
		t.Fatalf("строка отменённой кампании: %v", err)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "черновик")
	if err != nil {
		t.Fatalf("черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(30, "future_lead")}); err != nil {
		t.Fatalf("получатели черновика: %v", err)
	}

	if err := sender.maybeSendDigest(ctx); err != nil {
		t.Fatalf("сводка: %v", err)
	}
	sent := dialog.Sent()
	if len(sent) != 1 {
		t.Fatalf("ушло %d сводок, ожидали одну", len(sent))
	}
	text := sent[0].Text
	for _, want := range []string{
		"Аккаунты в порядке, стопов нет.\n\nИтог за кампанию",
		"```\n           взято отпр. дост.  отв.\n",
		"idle           0     0     0     0\nsecond",
		"second         3     1     1     0\n                   33%  100%    0%\n",
		"test           4     4     2     1\n                  100%   50%   50%\n",
		"Кампания       8     5     3     1\n                   63%   60%   33%\n",
		"Все (2)       10     7     5     2\n                   70%   71%   40%\n```",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в сводке нет %q:\n%s", want, text)
		}
	}
}

// Черновик сводку не зовёт: один черновик - сводки нет, закрытая кампания с
// загруженным после неё черновиком - сводка об итоге закрытой.
func TestDigestCampaignSkipsDraft(t *testing.T) {
	service := newService(t, "owner_one")
	ctx := context.Background()

	if _, err := service.LoadCampaign(ctx, "Привет, {имя}!", "первая"); err != nil {
		t.Fatalf("первый черновик: %v", err)
	}
	if id, err := service.digestCampaign(ctx); err != nil || id != 0 {
		t.Fatalf("есть только черновик: id %d, ошибка %v", id, err)
	}
	done := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET status = 'done', finished_at = $2 WHERE id = $1`, done, digestEvening); err != nil {
		t.Fatalf("закрыть кампанию: %v", err)
	}
	if _, err := service.LoadCampaign(ctx, "Привет, {имя}!", "следующая"); err != nil {
		t.Fatalf("черновик: %v", err)
	}

	// Здесь черновик проигрывает и порядку по finished_at: проверка держит сценарий
	// тикета, а условие на черновик ловит проверка выше.
	id, err := service.digestCampaign(ctx)
	if err != nil {
		t.Fatalf("выбор кампании: %v", err)
	}
	if id != done {
		t.Fatalf("сводка о кампании %d, ожидали закрытую %d", id, done)
	}
}
