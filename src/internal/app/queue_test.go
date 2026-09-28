package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// insertDirect кладёт строку журнала ручных касаний прямо SQL: журнал пишет
// диалог, а отсев обязан проверяться отдельно от записи.
func insertDirect(t *testing.T, service *Service, username, state string, created time.Time) {
	t.Helper()
	if _, err := service.pool.Exec(context.Background(),
		`INSERT INTO direct_messages (username, account_id, text, random_id, state, created_at)
		 VALUES ($1, (SELECT id FROM accounts WHERE label = 'test'), 'привет', $2, $3, $4)`,
		username, newRandomID(), state, created); err != nil {
		t.Fatalf("вставить касание %q: %v", state, err)
	}
}

// Ручная отправка закрывает человека для кампаний: строка в журнале - такое же
// сообщение, как строка очереди. Проверяются оба входа: импорт следующей
// кампании и очередь уже идущей.
func TestDirectSendClosesPersonForCampaigns(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	insertDirect(t, service, "ivan_petrov", "sent", testNow)

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("человек с ручным касанием взят в отправку: %+v", claimed)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonDirectSent {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("вторая кампания: %v", err)
	}
	report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("импорт во вторую кампанию: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonDirectSent] != 1 {
		t.Fatalf("отчёт отсева %+v", report)
	}
}

// Незакрытое касание закрывает человека, но называется своей причиной: pending
// значит «сообщение могло уйти», и отчёт не имеет права выдавать это за
// состоявшуюся отправку. Возраст касания на причину не влияет: свежий pending
// так же неизвестен, как старый, и оба входа - очередь и импорт - обязаны
// сказать о нём одно.
func TestPendingTouchReportedSeparately(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	insertDirect(t, service, "ivan_petrov", "pending", testNow)

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("человек с зависшим касанием взят в отправку: %+v", claimed)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonDirectPending {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}

	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("вторая кампания: %v", err)
	}
	report, err := service.AddRecipients(ctx, draft, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("импорт во вторую кампанию: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonDirectPending] != 1 {
		t.Fatalf("отчёт отсева %+v", report)
	}
}

// Отказ, при котором сообщение заведомо не ушло, возвращает человека кампании:
// иначе неудачная ручная попытка молча вычёркивала бы его навсегда.
func TestFailedTouchDoesNotClosePerson(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	insertDirect(t, service, "ivan_petrov", "failed", testNow)

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	if report.Accepted != 1 {
		t.Fatalf("неудавшееся касание отсеяло человека: %+v", report)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("запустить кампанию: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("строка не взята: %v, %+v", err, claimed)
	}
}

// Кампания не active - ни одной отправки: выборка требует активной кампании.
func TestNoSendWhenCampaignNotActive(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("строка взята из черновика: %+v", claimed)
	}
}

// Перезапуск после отправки: строка уже sent, протухание claim не возвращает
// её в очередь.
func TestNoResendAfterRestart(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}

	service.Now = func() time.Time { return testNow.Add(time.Hour) }
	again, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("повторная выборка: %v", err)
	}
	if again != nil {
		t.Fatalf("отправленная строка взята повторно: %+v", again)
	}
}

// Ответ человека закрывает строку: после replied касаний больше нет.
func TestNoTouchAfterReply(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}

	rows, err := service.MarkReplied(ctx, 0, "@Ivan_Petrov")
	if err != nil || rows != 1 {
		t.Fatalf("ответ не записан: %v, строк %d", err, rows)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "replied" {
		t.Fatalf("строка в состоянии %q", state)
	}
	service.Now = func() time.Time { return testNow.Add(time.Hour) }
	again, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if again != nil {
		t.Fatalf("ответившего снова взяли в отправку: %+v", again)
	}
}

// Человек написал первым: приглашение отменяется, но ответом это не считается.
// Строка уходит в skipped с причиной «написал сам»: письма он не получал, и
// место ему не в метрике ответов, а в отчёте отсева.
func TestInboundBeforeSendCancelsTouch(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	replied, err := service.MarkReplied(ctx, 0, "ivan_petrov")
	if err != nil || replied != 0 {
		t.Fatalf("неотправленную строку записали в ответы: %v, строк %d", err, replied)
	}
	rows, err := service.markWroteFirst(ctx, 0, "ivan_petrov")
	if err != nil || rows != 1 {
		t.Fatalf("входящее не записано: %v, строк %d", err, rows)
	}

	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonWroteFirst {
		t.Fatalf("строка в состоянии %q (%s), ожидалось skipped/%s", state, reason, ReasonWroteFirst)
	}
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("приглашение всё-таки взято в отправку: %+v", claimed)
	}
}

// Вне окна 10:00-19:00 буднего дня отправок нет: ни рано утром, ни в выходной.
func TestNoSendOutsideWindow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	moments := map[string]time.Time{
		"до окна": time.Date(2026, 9, 2, 9, 30, 0, 0, moscowZone),
		"после":   time.Date(2026, 9, 2, 19, 0, 0, 0, moscowZone),
		"суббота": time.Date(2026, 9, 5, 12, 0, 0, 0, moscowZone),
		"по UTC в окне, по МСК нет": time.Date(2026, 9, 2, 17, 30, 0, 0, time.UTC),
	}
	for name, moment := range moments {
		service.Now = func() time.Time { return moment }
		claimed, err := service.ClaimNext(ctx, "test")
		if err != nil {
			t.Fatalf("%s: выборка: %v", name, err)
		}
		if claimed != nil {
			t.Fatalf("%s: строка взята вне окна", name)
		}
	}
}

// Дневной потолок держится под конкуренцией: два процесса на один аккаунт не
// перебирают его вдвоём - счёт и взятие строки идут под блокировкой аккаунта.
func TestNoSendOverDailyLimit(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	if _, err := service.pool.Exec(ctx, `UPDATE accounts SET daily_limit = 1`); err != nil {
		t.Fatalf("выставить потолок: %v", err)
	}

	var wait sync.WaitGroup
	results := make([]*Recipient, 2)
	errs := make([]error, 2)
	for i := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[i], errs[i] = service.ClaimNext(ctx, "test")
		}()
	}
	wait.Wait()

	claimed := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("выборка %d: %v", i, errs[i])
		}
		if results[i] != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("взято строк %d при потолке 1", claimed)
	}
}

// Дневной потолок общий на оба канала: холодное касание руками расходует его
// наравне с рассылкой. Иначе агент и кампания за те же сутки дают с одного
// номера два потолка холодных касаний.
func TestColdDirectSpendsCampaignLimit(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)
	sendOK(t, chatOn(service, &stubDialog{}), "stranger_one", "привет")

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("кампания взяла строку поверх израсходованного потолка: %+v", claimed)
	}
	idle, err := service.IdleReason(ctx, "test")
	if err != nil || idle.Reason != IdleDailyLimit {
		t.Fatalf("причина простоя %q (%v), ожидалась %q", idle.Reason, err, IdleDailyLimit)
	}
}

// Стоп аккаунта снимается только человеком: пока флаг стоит, отправок нет.
func TestNoSendWhenAccountStopped(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE accounts SET stopped = true, stop_reason = 'PEER_FLOOD', stopped_at = now()`); err != nil {
		t.Fatalf("остановить аккаунт: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("остановленный аккаунт взял строку: %+v", claimed)
	}
}

// Человек без имени получает письмо без обращения, а не выпадает из кампании
// (решение владельца 02.09.2026). Имя из одного перевода строки Go схлопывает в
// пустое, и обращение снимается целиком - иначе письмо начиналось бы с запятой.
func TestNamelessRowGoesWithoutGreeting(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, amo_lead_id, name, username, state, random_id)
		 SELECT id, 2, 20, E'\n', 'petr_ivanov', 'planned', 777 FROM campaigns WHERE status = 'active'`); err != nil {
		t.Fatalf("вставить строку без имени: %v", err)
	}

	first, err := service.ClaimNext(ctx, "test")
	if err != nil || first == nil || first.Username != "ivan_petrov" {
		t.Fatalf("первая строка: %v, %+v", err, first)
	}
	if _, err := service.FinishClaim(ctx, first, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}

	next, err := service.ClaimNext(ctx, "test")
	if err != nil || next == nil {
		t.Fatalf("строка без имени не ушла в отправку: %v, %+v", err, next)
	}
	if next.Username != "petr_ivanov" {
		t.Fatalf("взята строка %q, ожидалась petr_ivanov", next.Username)
	}
	// Шаблон кампании - «Привет, {имя}!»: без имени обращение снимается вместе
	// с запятой, остальной текст остаётся дословным.
	if next.Text != "Привет!" {
		t.Fatalf("текст письма %q, ожидалось «Привет!»", next.Text)
	}
}

// Кампания с неразобранной скобкой в шаблоне не отправляет ничего, даже если
// прошла вход прошлой версии, но и состояния своего не меняет: сломанный шаблон
// - причина простоя очереди, а состояние кампании ведёт человек. Шаблон
// портится прямо в базе - через load_campaign такой уже не проходит.
func TestBrokenTemplateStopsTheQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET template_text = 'Привет, {{имя}}!' WHERE id = $1`, id); err != nil {
		t.Fatalf("испортить шаблон: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("кампания со сломанным шаблоном отдала строку: %+v", claimed)
	}
	if status := campaignStatus(t, service.pool, id); status != "active" {
		t.Fatalf("сервис сам сменил состояние кампании на %q", status)
	}
	idle, err := service.IdleReason(ctx, "test")
	if err != nil || idle.Reason != IdleBrokenTemplate {
		t.Fatalf("причина простоя %q (%v), ожидали %q", idle.Reason, err, IdleBrokenTemplate)
	}
	status, err := service.Status(ctx, false)
	if err != nil || len(status.Idle.Braces) == 0 {
		t.Fatalf("status не назвал сломанный шаблон: %+v, %v", status.Idle, err)
	}

	// Пауза и продолжение остаются за человеком, но продолжить негодную кампанию
	// start не даёт: список придётся собрать заново.
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("пауза: %v", err)
	}
	if _, err := service.StartCampaign(ctx); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("старт кампании со сломанным шаблоном вернул %v", err)
	}
}

// Негодное имя не уходит в отправку и тогда, когда попало в очередь мимо
// импорта: скобка в имени и письмо длиннее предела Telegram - те же рубежи, что
// на импорте, и причина у строки та же.
func TestBadNamesNeverLeaveTheQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx,
		`UPDATE campaigns SET template_text = 'Привет, {имя}! ' || repeat('а', $2) WHERE id = $1`,
		id, maxTextLen-200); err != nil {
		t.Fatalf("удлинить шаблон: %v", err)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, amo_lead_id, name, username, state, random_id)
		 VALUES ($1, 2, 20, 'Ирина {Галера}', 'irina_petrova', 'planned', 777),
		        ($1, 3, 30, repeat('🙂', 100), 'anna_smirnova', 'planned', 778),
		        ($1, 4, 40, repeat('🙂', 100), 'olga_orlova', 'planned', 779)`, id); err != nil {
		t.Fatalf("вставить строки: %v", err)
	}

	// Первой уходит годная строка, дальше очередь обязана опустеть.
	first, err := service.ClaimNext(ctx, "test")
	if err != nil || first == nil || first.Username != "ivan_petrov" {
		t.Fatalf("первая строка: %v, %+v", err, first)
	}
	if _, err := service.FinishClaim(ctx, first, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	// Один виток, а не три: негодные снимаются подряд, пока не найдётся годная.
	// По одной за виток очередь из сорока таких строк разбирается часами, и всё
	// это время кампания выглядит идущей.
	next, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if next != nil {
		t.Fatalf("строка с негодным именем ушла в отправку: %+v", next)
	}

	if state, reason := recipientState(t, service.pool, "irina_petrova"); state != "skipped" || reason != ReasonNameBraces {
		t.Fatalf("имя со скобкой в состоянии %q (%s)", state, reason)
	}
	for _, username := range []string{"anna_smirnova", "olga_orlova"} {
		if state, reason := recipientState(t, service.pool, username); state != "skipped" || reason != ReasonTextTooLong {
			t.Fatalf("длинное имя @%s в состоянии %q (%s)", username, state, reason)
		}
	}
}

// Стоп-лист проверяется в момент отправки, а не только при импорте: человека
// вносят в список и после загрузки.
func TestStopListCheckedAtSendTime(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('ivan_petrov')`); err != nil {
		t.Fatalf("стоп-лист: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed != nil {
		t.Fatalf("строка из стоп-листа взята в отправку: %+v", claimed)
	}
	state, reason := recipientState(t, service.pool, "ivan_petrov")
	if state != "skipped" || reason != ReasonStopList {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}
}

// Один человек - одно сообщение и между кампаниями: отсев при импорте и тот же
// рубеж в выборке, если строка попала в новую кампанию мимо импорта.
func TestNoSecondMessageAcrossCampaigns(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	first := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}

	second, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("вторая кампания: %v", err)
	}
	report, err := service.AddRecipients(ctx, second, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("импорт во вторую кампанию: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonAlreadySent] != 1 {
		t.Fatalf("отчёт отсева %+v", report)
	}

	// Тот же человек, заведённый в CRM вторым контактом: импорт узнаёт его по
	// нику, но проверяем и рубеж выборки - строку кладём мимо импорта.
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO recipients (campaign_id, amo_contact_id, name, username, random_id)
		 VALUES ($1, 999, 'Иван', 'ivan_petrov', 42)`, second); err != nil {
		t.Fatalf("вставить строку мимо импорта: %v", err)
	}
	if _, err := service.FinishIfEmpty(ctx); err != nil {
		t.Fatalf("закрыть первую кампанию: %v", err)
	}
	if status := campaignStatus(t, service.pool, first); status != "done" {
		t.Fatalf("первая кампания в состоянии %q", status)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("запустить вторую: %v", err)
	}

	again, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if again != nil {
		t.Fatalf("второе сообщение тому же человеку: %+v", again)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "skipped" || reason != ReasonAlreadySent {
		t.Fatalf("строка второй кампании в состоянии %q с причиной %q", state, reason)
	}
}

// Кампания закрывается только пустой очередью: строка в работе - ещё planned.
func TestCampaignDoneWhenQueueEmpty(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}

	done, err := service.FinishIfEmpty(ctx)
	if err != nil {
		t.Fatalf("проверка пустоты: %v", err)
	}
	if done || campaignStatus(t, service.pool, id) != "active" {
		t.Fatalf("кампания закрыта со строкой в работе")
	}

	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	done, err = service.FinishIfEmpty(ctx)
	if err != nil {
		t.Fatalf("проверка пустоты: %v", err)
	}
	if !done || campaignStatus(t, service.pool, id) != "done" {
		t.Fatalf("кампания не закрылась на пустой очереди")
	}
}

// Временный отказ не расходует попытку: строка возвращается в очередь со
// снятым claim и берётся снова.
func TestRetryReturnsRowToQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "flood_wait"); err != nil {
		t.Fatalf("возврат в очередь: %v", err)
	}

	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil {
		t.Fatalf("строка не вернулась в очередь: %v, %+v", err, again)
	}
	if again.ID != claimed.ID || again.RandomID != claimed.RandomID {
		t.Fatalf("вернулась другая строка или другой random_id: %+v против %+v", again, claimed)
	}
}

// Отмена кампании не трогает строку с живым claim: её прямо сейчас несёт
// отправщик. Иначе доставка не записалась бы - и человек прошёл бы импорт в
// следующую кампанию и не попал бы в дневной счёт.
func TestCancelKeepsClaimedRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}

	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("отмена кампании: %v", err)
	}
	if campaignStatus(t, service.pool, id) != "cancelled" {
		t.Fatalf("кампания не отменена")
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "planned" {
		t.Fatalf("взятая строка отменена вместе с кампанией: состояние %q", state)
	}

	closed, err := service.FinishClaim(ctx, claimed, ResultSent, "")
	if err != nil || !closed {
		t.Fatalf("финализация доставки: %v, закрыто %v", err, closed)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("строка в состоянии %q, ожидалось sent", state)
	}
	var sentAt *time.Time
	if err := service.pool.QueryRow(ctx, `SELECT sent_at FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&sentAt); err != nil {
		t.Fatalf("прочитать sent_at: %v", err)
	}
	if sentAt == nil {
		t.Fatal("sent_at не проставлен: отправка не попадёт в дневной счёт")
	}
}

// Временный отказ по отменённой кампании уводит строку в cancelled: очередь
// отменённой кампании не разбирает никто, и planned осталось бы навсегда.
func TestRetryOnCancelledCampaignCancelsRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("отмена кампании: %v", err)
	}

	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "flood_wait"); err != nil {
		t.Fatalf("возврат в очередь: %v", err)
	}
	if state, reason := recipientState(t, service.pool, "ivan_petrov"); state != "cancelled" || reason != ReasonCampaignGone {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}
}

// Живой id Telegram больше 2^31: финализация обязана его записать, а не упасть
// на кодировании параметра - иначе первая же настоящая отправка валит цикл.
func TestFinishClaimStoresBigUserID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	claimed.UserID = 7123456789

	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация с живым id: %v", err)
	}
	var stored int64
	if err := service.pool.QueryRow(ctx, `SELECT tg_user_id FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&stored); err != nil {
		t.Fatalf("прочитать tg_user_id: %v", err)
	}
	if stored != claimed.UserID {
		t.Fatalf("записан tg_user_id %d, ожидался %d", stored, claimed.UserID)
	}
}

// Ответ человека с живым id Telegram закрывает строку: рубеж «написал первым»
// не имеет права ломаться ровно на настоящих id.
func TestMarkRepliedByBigUserID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	const userID int64 = 7123456789
	// Ответить может только тот, кому письмо ушло: строка переводится в sent
	// вместе с id, под которым его узнают во входящем.
	if _, err := service.pool.Exec(ctx, `UPDATE recipients SET tg_user_id = $1, state = 'sent'`, userID); err != nil {
		t.Fatalf("проставить tg_user_id: %v", err)
	}

	rows, err := service.MarkReplied(ctx, userID, "")
	if err != nil || rows != 1 {
		t.Fatalf("ответ не записан: %v, строк %d", err, rows)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "replied" {
		t.Fatalf("строка в состоянии %q", state)
	}
}

// Черновику ещё не слали: входящее от его человека не переводит строку в
// replied, иначе он отсеялся бы из будущей рассылки с причиной «уже получал».
func TestMarkRepliedSkipsDraft(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "черновик")
	if err != nil {
		t.Fatalf("создать черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}

	rows, err := service.MarkReplied(ctx, 0, "ivan_petrov")
	if err != nil {
		t.Fatalf("входящее: %v", err)
	}
	if rows != 0 {
		t.Fatalf("строка черновика закрыта ответом: строк %d", rows)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "planned" {
		t.Fatalf("строка черновика в состоянии %q", state)
	}
}

// Строка ушла из очереди, пока шла отправка: доставка всё равно записывается -
// иначе человек прошёл бы импорт следующей кампании и получил второе сообщение.
func TestSentRecordedFromSkippedRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`UPDATE recipients SET state = 'skipped' WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("увести строку из очереди: %v", err)
	}

	closed, err := service.FinishClaim(ctx, claimed, ResultSent, "")
	if err != nil || !closed {
		t.Fatalf("финализация доставки: %v, закрыто %v", err, closed)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("строка в состоянии %q, ожидалось sent", state)
	}
	var sentAt *time.Time
	if err := service.pool.QueryRow(ctx, `SELECT sent_at FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&sentAt); err != nil {
		t.Fatalf("прочитать sent_at: %v", err)
	}
	if sentAt == nil {
		t.Fatal("sent_at не проставлен: доставка потеряна")
	}

	next, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("вторая кампания: %v", err)
	}
	report, err := service.AddRecipients(ctx, next, []Person{person(1, "ivan_petrov")})
	if err != nil {
		t.Fatalf("импорт во вторую кампанию: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonAlreadySent] != 1 {
		t.Fatalf("получивший сообщение прошёл импорт: %+v", report)
	}
}

// Протухший claim перехватил второй процесс той же метки: первый успел записать
// отказ, второй доставил. Факт доставки обязан победить - иначе человек без
// sent_at пройдёт импорт следующей кампании и получит второе сообщение.
func TestSentRecordedFromUndeliveredRow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultUndelivered, ReasonNotAllowed); err != nil {
		t.Fatalf("записать отказ: %v", err)
	}

	closed, err := service.FinishClaim(ctx, claimed, ResultSent, "")
	if err != nil || !closed {
		t.Fatalf("финализация доставки: %v, закрыто %v", err, closed)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("строка в состоянии %q, ожидалось sent", state)
	}
	var sentAt *time.Time
	if err := service.pool.QueryRow(ctx, `SELECT sent_at FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&sentAt); err != nil {
		t.Fatalf("прочитать sent_at: %v", err)
	}
	if sentAt == nil {
		t.Fatal("sent_at не проставлен: доставка потеряна")
	}
}

// Ответ человека не открывает дыру в потолке: строка в работе остаётся в
// дневном счёте до финализации, иначе второй процесс аккаунта возьмёт лишнюю.
func TestReplyKeepsRowInDailyCount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	if _, err := service.pool.Exec(ctx, `UPDATE accounts SET daily_limit = 1`); err != nil {
		t.Fatalf("выставить потолок: %v", err)
	}

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	rows, err := service.MarkReplied(ctx, 0, claimed.Username)
	if err != nil || rows != 1 {
		t.Fatalf("ответ не записан: %v, строк %d", err, rows)
	}

	next, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if next != nil {
		t.Fatalf("потолок превышен: взята вторая строка %+v", next)
	}
}

// Бюджет попыток: строка со стабильной неопознанной ошибкой уходит из очереди на
// исчерпании бюджета и не держит её голову. FLOOD_WAIT попытку не расходует -
// переждать не значит потратить.
func TestRetryBudgetExhausted(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "flood_wait"); err != nil {
		t.Fatalf("возврат в очередь: %v", err)
	}
	var attempts int
	if err := service.pool.QueryRow(ctx, `SELECT attempts FROM recipients WHERE id = $1`, claimed.ID).
		Scan(&attempts); err != nil {
		t.Fatalf("прочитать attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("FLOOD_WAIT израсходовал попытку: attempts %d", attempts)
	}

	for i := range maxAttempts {
		again, err := service.ClaimNext(ctx, "test")
		if err != nil || again == nil {
			t.Fatalf("строка не вернулась в очередь на попытке %d: %v, %+v", i+1, err, again)
		}
		if _, err := service.FinishClaim(ctx, again, ResultRetryCounted, "unknown_error"); err != nil {
			t.Fatalf("попытка %d: %v", i+1, err)
		}
	}

	state, reason := recipientState(t, service.pool, "ivan_petrov")
	if state != "undelivered" || reason != ReasonRetryExhausted {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}
	if next, err := service.ClaimNext(ctx, "test"); err != nil || next != nil {
		t.Fatalf("исчерпанная строка осталась в очереди: %+v, %v", next, err)
	}
}

// Аккаунт держит взятую строку и после возврата в очередь: временный отказ мог
// прилететь на уже ушедшее сообщение, а дедуп Telegram по random_id действует
// только в пределах отправителя.
func TestRetryKeepsRowWithAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultRetry, "unknown_error"); err != nil {
		t.Fatalf("возврат в очередь: %v", err)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ('second', 10)`); err != nil {
		t.Fatalf("завести второй аккаунт: %v", err)
	}

	if other, err := service.ClaimNext(ctx, "second"); err != nil || other != nil {
		t.Fatalf("чужой аккаунт взял возвращённую строку: %+v, %v", other, err)
	}
	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil || again.ID != claimed.ID {
		t.Fatalf("свой аккаунт не вернул строку: %v, %+v", err, again)
	}
}

// Протухший claim перехватывает только тот же аккаунт: Telegram дедуплицирует
// random_id в пределах аккаунта, и повтор с соседнего дал бы человеку второе
// сообщение.
func TestStaleClaimStaysWithAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ('second', 10)`); err != nil {
		t.Fatalf("завести второй аккаунт: %v", err)
	}

	// Процесс первого аккаунта умер, claim протух.
	service.Now = func() time.Time { return testNow.Add(10 * time.Minute) }
	if other, err := service.ClaimNext(ctx, "second"); err != nil || other != nil {
		t.Fatalf("чужой аккаунт перехватил строку: %+v, %v", other, err)
	}
	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil {
		t.Fatalf("свой аккаунт не вернул строку: %v, %+v", err, again)
	}
	if again.ID != claimed.ID {
		t.Fatalf("вернулась другая строка: %d против %d", again.ID, claimed.ID)
	}
}

// Простой очереди объясняется причиной: разбор «почему письмо не ушло вовремя»
// идёт по логу, и причина обязана следовать за состоянием сервиса. Сценарий
// идёт одной историей, потому что причины перекрывают друг друга по порядку
// рубежей: окно, стоп, потолок, кампания, очередь.
func TestIdleReasonFollowsState(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	assertIdle := func(want string) {
		t.Helper()
		got, err := service.IdleReason(ctx, "test")
		if err != nil {
			t.Fatalf("причина простоя: %v", err)
		}
		if got.Reason != want {
			t.Fatalf("причина %q, ожидалась %q", got.Reason, want)
		}
	}

	assertIdle(IdleNoCampaign)

	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	assertIdle(IdleQueueEmpty)

	if _, err := service.pool.Exec(ctx,
		`UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`); err != nil {
		t.Fatalf("опустить потолок: %v", err)
	}
	assertIdle(IdleDailyLimit)

	if err := service.StopAccount(ctx, "test", "PEER_FLOOD"); err != nil {
		t.Fatalf("остановить аккаунт: %v", err)
	}
	assertIdle(IdleAccountStop)

	// Суббота той же недели: окно закрыто и перекрывает все причины ниже.
	service.Now = func() time.Time { return testNow.AddDate(0, 0, 3) }
	assertIdle(IdleWindowClosed)
}

// Строки, которые снимет рубеж, доступными не считаются: отчёт, посчитавший их,
// говорит «очередь движется, отправщик берёт строки по одной» при кампании, по
// которой не уйдёт ни одного письма. Головное обещание релиза - отчёт сам
// называет, почему очередь стоит, - на таком счёте врёт.
func TestIdleReasonCountsOnlyRowsThatCanGo(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	// Имя портится в базе: так строка и попадает в очередь - загруженной до
	// появления рубежа, а не через импорт. Скобка в имени уехала бы в письмо,
	// и это тот же симптом, против которого стоит проверка шаблона.
	startWith(t, service, person(1, "ivan_petrov"))
	runSQL(t, service, `UPDATE recipients SET name = '{Пётр}' WHERE username = 'ivan_petrov'`)

	idle, err := service.IdleReason(ctx, "test")
	if err != nil {
		t.Fatalf("причина простоя: %v", err)
	}
	if idle.Reason != IdleQueueDisqualified || idle.Left != 1 {
		t.Fatalf("причина %q (осталось %d), ожидали %q", idle.Reason, idle.Left, IdleQueueDisqualified)
	}

	handlers := &tools{service: service}
	result, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, result, err)
	if strings.Contains(text, "Очередь движется") {
		t.Fatalf("отчёт обещает движение стоящей очереди:\n%s", text)
	}
	if !strings.Contains(text, "снимает рубеж") {
		t.Fatalf("отчёт не назвал причину простоя:\n%s", text)
	}

	// Виток выборки закрывает такую очередь, и причина меняется на пустую: без
	// этого «снимает рубеж» осталось бы висеть на закончившейся кампании.
	if next, err := service.ClaimNext(ctx, "test"); err != nil || next != nil {
		t.Fatalf("строка без имени ушла в отправку: %v, %+v", err, next)
	}
	if idle, err := service.IdleReason(ctx, "test"); err != nil || idle.Reason != IdleQueueEmpty {
		t.Fatalf("после витка причина %q (%v), ожидали %q", idle.Reason, err, IdleQueueEmpty)
	}
}

// Занятая очередь отличается от пустой: строка, закреплённая за соседним
// аккаунтом, ждёт своего процесса, и кампания не закончена. Спутать их значит
// показать «работы нет» там, где работа стоит.
func TestIdleReasonSeesBlockedQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"))
	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ('second', 10)`); err != nil {
		t.Fatalf("завести второй аккаунт: %v", err)
	}

	idle, err := service.IdleReason(ctx, "second")
	if err != nil {
		t.Fatalf("причина простоя: %v", err)
	}
	if idle.Reason != IdleQueueBlocked {
		t.Fatalf("причина %q, ожидалась %q", idle.Reason, IdleQueueBlocked)
	}
}

// Окно и дневной потолок держат рассылку незнакомым людям и не держат
// служебных адресатов владельца: проверка рассылки идёт в воскресенье на
// исчерпанном потолке и не тратит его ни на единицу.
func TestOwnerLeavesQueueOutsideWindowAndOverLimit(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	// Воскресенье, глубокая ночь: окно закрыто по обоим основаниям.
	night := time.Date(2026, 9, 6, 3, 0, 0, 0, moscowZone)
	startWith(t, service, person(1, "ivan_petrov"), person(2, "smirnov"), person(3, "petr_sidorov"))
	// Потолок в единицу и одна отправка незнакомцу за сегодня: расход исчерпан.
	if _, err := service.pool.Exec(ctx,
		`UPDATE recipients SET state = 'sent', sent_at = $1,
		        account_id = (SELECT id FROM accounts WHERE label = 'test')
		 WHERE username = 'petr_sidorov'`, night); err != nil {
		t.Fatalf("израсходовать потолок: %v", err)
	}
	if _, err := service.pool.Exec(ctx, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`); err != nil {
		t.Fatalf("опустить потолок: %v", err)
	}
	service.Now = func() time.Time { return night }

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	if claimed == nil || claimed.Username != "smirnov" {
		t.Fatalf("выборка отдала %+v, ожидалась строка служебного адресата", claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("закрыть строку: %v", err)
	}

	if next, err := service.ClaimNext(ctx, "test"); err != nil || next != nil {
		t.Fatalf("вне окна взята строка незнакомца: %+v, %v", next, err)
	}

	report, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("отчёт: %v", err)
	}
	account := report.Accounts[0]
	if account.UsedToday != 1 || account.OwnerToday != 1 {
		t.Fatalf("расход %d при потолке %d, служебных %d: отправка владельцу потратила потолок",
			account.UsedToday, account.DailyLimit, account.OwnerToday)
	}
}

// Стоп-лист снимает из очереди и служебного адресата: человека, внесённого в
// стоп-лист после импорта, отправка не берёт, а строка обязана уйти из очереди
// причиной stop_list - иначе кампания не закроется никогда, а простой назывался
// бы отсевом.
func TestStopListLeavesQueueForOwner(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	startWith(t, service, person(1, "smirnov"))
	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('smirnov')`); err != nil {
		t.Fatalf("стоп-лист: %v", err)
	}

	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed != nil {
		t.Fatalf("выборка взяла человека из стоп-листа: %+v, %v", claimed, err)
	}
	if state, reason := recipientState(t, service.pool, "smirnov"); state != "skipped" || reason != ReasonStopList {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}
	if done, err := service.FinishIfEmpty(ctx); err != nil || !done {
		t.Fatalf("кампания не закрылась на опустевшей очереди: %v, %v", done, err)
	}
}
