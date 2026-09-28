package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Пустая база - штатное состояние сервиса до первой кампании: status обязан
// ответить, а не упасть на отсутствующей кампании.
func TestStatusWithoutCampaign(t *testing.T) {
	service := newService(t)

	report, err := service.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("статус: %v", err)
	}
	if report.Campaign != nil {
		t.Fatalf("кампания взялась из пустой базы: %+v", report.Campaign)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].Label != "test" {
		t.Fatalf("аккаунты в отчёте: %+v", report.Accounts)
	}
}

// Наполненная кампания: счётчики по состояниям, дневной расход аккаунта,
// превью на реальных строках и построчный итог по full.
func TestStatusCountsStatesAndPreview(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	sender := senderOn(service, &stubTransport{})
	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток отправщика: %v", err)
	}

	report, err := service.Status(ctx, true)
	if err != nil {
		t.Fatalf("статус: %v", err)
	}
	if report.Campaign == nil || report.Campaign.Total != 2 {
		t.Fatalf("кампания в отчёте: %+v", report.Campaign)
	}
	if report.Campaign.States["sent"] != 1 || report.Campaign.States["planned"] != 1 {
		t.Fatalf("счётчики состояний: %+v", report.Campaign.States)
	}
	if report.Accounts[0].UsedToday != 1 {
		t.Fatalf("дневной расход аккаунта: %+v", report.Accounts[0])
	}
	if len(report.Preview) != 2 || !strings.Contains(report.Preview[0].Text, "Иван") {
		t.Fatalf("превью: %+v", report.Preview)
	}
	if len(report.Rows) != 2 || report.Rows[0].State != "sent" {
		t.Fatalf("построчный итог: %+v", report.Rows)
	}

	// Оставшаяся строка закреплена за чужим аккаунтом: её не возьмёт никто,
	// кроме него, и отчёт обязан назвать, чьего отправщика ждёт очередь.
	if _, err := service.pool.Exec(ctx,
		`WITH other AS (INSERT INTO accounts (label, daily_limit) VALUES ('second', 10) RETURNING id)
		 UPDATE recipients SET account_id = (SELECT id FROM other) WHERE state = 'planned'`); err != nil {
		t.Fatalf("закрепить строку за чужим аккаунтом: %v", err)
	}
	report, err = service.Status(ctx, false)
	if err != nil {
		t.Fatalf("статус: %v", err)
	}
	if len(report.Held) != 1 || report.Held[0].Label != "second" || report.Held[0].Rows != 1 {
		t.Fatalf("строки за чужим аккаунтом: %+v", report.Held)
	}
}

// Среди закрытых кампаний свежесть решает finished_at, а не id: кампания,
// закрывшаяся позже, обязана выигрывать в статусе без параметра, даже если её
// id меньше, чем у кампании, закрытой раньше.
func TestStatusPrefersLastClosedOverBiggerID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	doneID := startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	// Закрывается позже второй кампании, хотя её id меньше.
	service.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	if done, err := service.FinishIfEmpty(ctx); err != nil || !done {
		t.Fatalf("закрыть первую кампанию: %v, done=%v", err, done)
	}

	cancelledID, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("создать вторую кампанию: %v", err)
	}
	if _, err := service.AddRecipients(ctx, cancelledID, []Person{person(2, "petr_ivanov")}); err != nil {
		t.Fatalf("добавить получателей во вторую: %v", err)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("запустить вторую: %v", err)
	}
	// Закрывается раньше первой по времени, но её id больше.
	service.Now = func() time.Time { return testNow.Add(time.Hour) }
	if id, err := service.StopCampaign(ctx, true); err != nil || id != cancelledID {
		t.Fatalf("отменить вторую: id %d, err %v", id, err)
	}

	status, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("статус: %v", err)
	}
	if status.Campaign == nil || status.Campaign.ID != doneID {
		t.Fatalf("статус без параметра показал не последнюю закрытую кампанию: %+v, ожидали id %d", status.Campaign, doneID)
	}
}

// secondAccount заводит второго отправщика на том же пуле: своя метка, всё
// остальное - как у первого. Ровно так второй аккаунт появляется в жизни:
// процесс поднимается со своей меткой и заводит строку сам.
func secondAccount(t *testing.T, service *Service, label string) *Service {
	t.Helper()
	cfg := service.cfg
	cfg.AccountLabel = label
	other := NewService(service.pool, cfg)
	other.Now = service.Now
	if _, _, err := other.EnsureAccount(context.Background()); err != nil {
		t.Fatalf("завести аккаунт %q: %v", label, err)
	}
	return other
}

// Два отправщика на одной кампании: строку получает каждый, но свою. Это рубеж
// «один человек - одно сообщение» на пути, которого раньше не было: до
// нескольких аккаунтов две выборки подряд шли из одного процесса.
func TestTwoAccountsSplitQueue(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	secondAccount(t, service, "second")

	first, err := service.ClaimNext(ctx, "test")
	if err != nil || first == nil {
		t.Fatalf("выборка первым аккаунтом: %v, %+v", err, first)
	}
	other, err := service.ClaimNext(ctx, "second")
	if err != nil || other == nil {
		t.Fatalf("выборка вторым аккаунтом: %v, %+v", err, other)
	}
	if first.Username == other.Username {
		t.Fatalf("оба аккаунта взяли одного человека: %q", first.Username)
	}

	// Третьей строки нет: очередь кончилась, и третья выборка не имеет права
	// отдать кого-то из уже взятых.
	extra, err := service.ClaimNext(ctx, "second")
	if err != nil {
		t.Fatalf("третья выборка: %v", err)
	}
	if extra != nil {
		t.Fatalf("очередь отдала лишнюю строку: %+v", extra)
	}
}

// Стоп одного аккаунта не трогает соседа и виден в его строке отчёта: причина
// молчания считается по каждому аккаунту, иначе «почему второй молчит» снаружи
// не имеет ответа.
func TestStatusIdlePerAccount(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	secondAccount(t, service, "second")
	if err := service.StopAccount(ctx, "test", "PEER_FLOOD"); err != nil {
		t.Fatalf("стоп аккаунта: %v", err)
	}

	report, err := service.Status(ctx, false)
	if err != nil {
		t.Fatalf("статус: %v", err)
	}
	if len(report.Accounts) != 2 {
		t.Fatalf("аккаунты в отчёте: %+v", report.Accounts)
	}
	byLabel := map[string]AccountSummary{}
	for _, item := range report.Accounts {
		byLabel[item.Label] = item
	}
	if got := byLabel["test"].Idle.Reason; got != IdleAccountStop {
		t.Fatalf("причина простоя остановленного аккаунта: %q", got)
	}
	if got := byLabel["second"].Idle.Reason; got != IdleClaimRaceLost {
		t.Fatalf("причина простоя живого аккаунта: %q", got)
	}
	// Общей строки нет: причины разные, и одна на всех спрятала бы стоп.
	if report.Idle.Reason != "" {
		t.Fatalf("общая причина простоя при разных причинах: %+v", report.Idle)
	}

	// Живой аккаунт продолжает работу: стоп соседа его не останавливает.
	picked, err := service.ClaimNext(ctx, "second")
	if err != nil || picked == nil {
		t.Fatalf("выборка живым аккаунтом после стопа соседа: %v, %+v", err, picked)
	}
}

// Общая причина остаётся одной строкой: окно отправки, кампания и её шаблон
// одни на все аккаунты, и повторять их у каждого значит спрятать среди них ту
// единственную, которая отличается.
func TestCommonIdleCollapses(t *testing.T) {
	window := Idle{Reason: IdleWindowClosed}
	both := []AccountSummary{{Label: "a", Idle: window}, {Label: "b", Idle: window}}
	if got := commonIdle(both).Reason; got != IdleWindowClosed {
		t.Fatalf("совпавшая общая причина не схлопнулась: %q", got)
	}

	limits := []AccountSummary{
		{Label: "a", Idle: Idle{Reason: IdleDailyLimit, Used: 3, Limit: 3}},
		{Label: "b", Idle: Idle{Reason: IdleDailyLimit, Used: 7, Limit: 20}},
	}
	if got := commonIdle(limits).Reason; got != "" {
		t.Fatalf("аккаунтная причина ушла в общую строку с чужими числами: %q", got)
	}

	// Аккаунт один - строка общая, как и до нескольких отправщиков.
	single := []AccountSummary{{Label: "a", Idle: Idle{Reason: IdleDailyLimit, Used: 3, Limit: 3}}}
	if got := commonIdle(single).Reason; got != IdleDailyLimit {
		t.Fatalf("единственный аккаунт потерял общую строку: %q", got)
	}
}
