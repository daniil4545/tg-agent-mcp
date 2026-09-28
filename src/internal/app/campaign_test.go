package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
)

// testNow - среда, середина окна отправки. Время инъектируется в Service, а не
// берётся из часов: иначе прогон в субботу красит тесты рубежей в красный.
var testNow = time.Date(2026, 9, 2, 12, 0, 0, 0, moscowZone)

// newService поднимает сервис на пустых таблицах тестовой базы. Служебные
// адресаты передаются сюда, а не в cfg готового сервиса: список входит в тексты
// запросов, собранные в NewService.
func newService(t *testing.T, owners ...string) *Service {
	t.Helper()

	if testing.Short() {
		t.Skip("-short: интеграционный ярус пропущен")
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, url, 4)
	if err != nil {
		t.Fatalf("открыть пул: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE direct_messages, recipients, campaigns, accounts, stop_list RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("очистить таблицы: %v", err)
	}

	service := NewService(pool, Config{
		AccountLabel:      "test",
		AccountDailyLimit: 10,
		SendWindowStart:   "10:00",
		SendWindowEnd:     "19:00",
		ClaimStaleAfter:   5 * time.Minute,
		OwnerAccounts:     owners,
	})
	service.Now = func() time.Time { return testNow }
	if _, _, err := service.EnsureAccount(ctx); err != nil {
		t.Fatalf("завести аккаунт: %v", err)
	}
	return service
}

func person(contactID int64, username string) Person {
	return Person{ContactID: contactID, LeadID: contactID * 10, Name: "Иван", Username: username}
}

// startWith - обычная подготовка: черновик с людьми и запуск кампании.
func startWith(t *testing.T, service *Service, people ...Person) int64 {
	t.Helper()
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, people); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("запустить кампанию: %v", err)
	}
	return id
}

func recipientState(t *testing.T, pool *pgxpool.Pool, username string) (string, string) {
	t.Helper()
	var state, reason string
	err := pool.QueryRow(context.Background(),
		`SELECT state, reason FROM recipients WHERE username = $1 ORDER BY id DESC LIMIT 1`, username).
		Scan(&state, &reason)
	if err != nil {
		t.Fatalf("прочитать строку %q: %v", username, err)
	}
	return state, reason
}

func campaignStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM campaigns WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("прочитать кампанию %d: %v", id, err)
	}
	return status
}

// Подстановка узнаёт имя в любом регистре и с пробелами внутри скобок: «{Имя}»
// в начале предложения - естественная опечатка, и уходить в письмо литералом
// она не должна. Чужие скобки остаются как есть - их отбивает load_campaign.
func TestRenderMessage(t *testing.T) {
	got := RenderMessage("{Имя}, ждём {дата} в {ИМЯ}-клубе, { имя }", " Пётр\n")
	want := "Пётр, ждём {дата} в Пётр-клубе, Пётр"
	if got != want {
		t.Fatalf("подстановка дала %q, ожидалось %q", got, want)
	}
}

// Шаблон проверяется на входе: кампания собирается один раз и уходит сотням,
// поэтому чужая подстановка и непролезающая длина обязаны отбиться до сбора
// списка, а не на первой отправке.
func TestLoadCampaignChecksTemplate(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	// Скобка проверяется каждая, а не только правильно спаренная: «{{имя}}» -
	// опечатка из Jinja, и подстановка внутрь внешней пары дала бы человеку
	// «Привет, {Пётр}!», а незакрытая скобка уехала бы в письмо литералом.
	for _, template := range []string{
		"Привет, {имя}! Ждём в {город}, {name}",
		"Привет, {{имя}}!",
		"Привет, {имя! Как дела?",
		"Привет, имя}!",
	} {
		if _, err := service.LoadCampaign(ctx, template, "тест"); !errors.Is(err, ErrUnknownPlaceholder) {
			t.Fatalf("шаблон %q принят: %v", template, err)
		}
	}

	long := "Привет, {имя}! " + strings.Repeat("а", maxTextLen)
	if _, err := service.LoadCampaign(ctx, long, "тест"); !errors.Is(err, ErrTemplateTooLong) {
		t.Fatalf("шаблон длиннее предела Telegram принят: %v", err)
	}

	// По рунам такой шаблон влезает, по счёту Telegram - нет: эмодзи идёт за два.
	emoji := strings.Repeat("а", maxTextLen-150) + strings.Repeat("🙂", 100)
	if _, err := service.LoadCampaign(ctx, emoji, "тест"); !errors.Is(err, ErrTemplateTooLong) {
		t.Fatalf("шаблон с эмодзи принят: %v", err)
	}

	if _, err := service.LoadCampaign(ctx, "Привет, {Имя}!", "тест"); err != nil {
		t.Fatalf("законный шаблон отбит: %v", err)
	}
}

// Шаблон с {имя} и человек без имени: получатель принимается, а обращение
// снимается вместе с запятой - письмо, начинающееся с запятой, не уходит
// никому, но и человек из кампании не пропадает (решение владельца 02.09.2026).
// Имя из CRM при этом схлопывается: «Иван\n\nПетров» в тексте письма выглядит
// как сбой.
func TestNamelessRecipientKeptWithoutGreeting(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	nameless := Person{ContactID: 1, Name: "\n\t", Username: "nameless_one"}

	id, err := service.LoadCampaign(ctx, "Привет, {имя}! Это проверка", "с именем")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{
		nameless,
		{ContactID: 2, Name: "Иван\n\nПетров", Username: "ivan_petrov"},
	})
	if err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	if report.Accepted != 2 || len(report.Skipped) != 0 {
		t.Fatalf("отчёт отсева %+v", report)
	}

	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("запустить кампанию: %v", err)
	}
	texts := map[string]string{
		"nameless_one": "Привет! Это проверка",
		"ivan_petrov":  "Привет, Иван Петров! Это проверка",
	}
	for range texts {
		claimed, err := service.ClaimNext(ctx, "test")
		if err != nil || claimed == nil {
			t.Fatalf("взять строку: %v, %+v", err, claimed)
		}
		if want := texts[claimed.Username]; claimed.Text != want {
			t.Fatalf("текст для %s: %q, ожидалось %q", claimed.Username, claimed.Text, want)
		}
		if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
			t.Fatalf("финализация: %v", err)
		}
	}

	free, err := service.LoadCampaign(ctx, "Приглашаем в клуб", "без имени")
	if err != nil {
		t.Fatalf("создать кампанию без подстановки: %v", err)
	}
	// Человек новый: первому безымянному письмо уже ушло, и во второй кампании
	// его закрывает правило «один человек - одно сообщение», а не имя.
	second, err := service.AddRecipients(ctx, free,
		[]Person{{ContactID: 3, Name: " ", Username: "nameless_two"}})
	if err != nil {
		t.Fatalf("добавить безымянного: %v", err)
	}
	if second.Accepted != 1 {
		t.Fatalf("шаблон без {имя} отсеял безымянного: %+v", second)
	}
}

// Имя из CRM - последний путь фигурной скобки в письмо: «Ирина {тест}» и вовсе
// «{имя}» дали бы человеку ровно тот текст, против которого стоит проверка
// шаблона. Длина считается счётом Telegram, а не рунами: эмодзи идёт за два, и
// имя из эмодзи переполняет письмо там, где рунный счёт видел запас.
func TestBadNamesSkippedAtImport(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}! "+strings.Repeat("а", maxTextLen-200), "тест")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{
		{ContactID: 1, Name: "Ирина {тест}", Username: "irina_petrova"},
		{ContactID: 2, Name: "{имя}", Username: "petr_ivanov"},
		{ContactID: 3, Name: strings.Repeat("🙂", 100), Username: "anna_smirnova"},
		{ContactID: 4, Name: "Иван", Username: "ivan_petrov"},
	})
	if err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	if report.Accepted != 1 || report.Skipped[ReasonNameBraces] != 2 || report.Skipped[ReasonTextTooLong] != 1 {
		t.Fatalf("отчёт отсева %+v", report)
	}

	// Шаблон без подстановки имя в письмо не берёт ни одним символом, и скобка в
	// имени отсевом быть не может.
	free, err := service.LoadCampaign(ctx, "Приглашаем в клуб", "без имени")
	if err != nil {
		t.Fatalf("создать кампанию без подстановки: %v", err)
	}
	second, err := service.AddRecipients(ctx, free, []Person{
		{ContactID: 1, Name: "Ирина {Галера}", Username: "irina_petrova"},
	})
	if err != nil {
		t.Fatalf("добавить получателя: %v", err)
	}
	if second.Accepted != 1 {
		t.Fatalf("скобка в имени отсеяла человека из кампании без подстановки: %+v", second)
	}
}

// Лид без пригодного ника в рассылку не попадает: догадываться по имени
// запрещено, поэтому кириллица, телефон и пустая строка обязаны давать отсев.
func TestNormalizeUsername(t *testing.T) {
	cases := map[string]string{
		"  @Ivan_Petrov ":       "ivan_petrov",
		"https://t.me/Ivan/":    "",
		"t.me/ivan_petrov":      "ivan_petrov",
		"Иван Петров":           "",
		"+7 999 123-45-67":      "",
		"":                      "",
		"ab":                    "",
		"1ivan":                 "",
		"ivan petrov":           "",
		"ivanivanivanivanivan1": "ivanivanivanivanivan1",
		// Telegram таких ников не выдаёт: короче пяти символов, с хвостовым и с
		// двойным подчёркиванием. Раньше они проходили, и очередь тратила по минуте
		// на каждого.
		"abcd":          "",
		"abcde":         "abcde",
		"zzz_trailing_": "",
		"zzz__double":   "",
		"a_b_c":         "a_b_c",
	}
	for raw, want := range cases {
		if got := normalizeUsername(raw); got != want {
			t.Errorf("normalizeUsername(%q) = %q, ожидалось %q", raw, got, want)
		}
	}
}

// Перезаливка списка: вычеркнутого человека не должно остаться в очереди, а
// черновик обязан остаться один.
func TestReloadCancelsOldDraft(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	first, err := service.LoadCampaign(ctx, "Привет, {имя}!", "первый")
	if err != nil {
		t.Fatalf("первая загрузка: %v", err)
	}
	if _, err := service.AddRecipients(ctx, first, []Person{person(1, "striked_out")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}

	second, err := service.LoadCampaign(ctx, "Привет, {имя}!", "второй")
	if err != nil {
		t.Fatalf("перезаливка: %v", err)
	}

	if status := campaignStatus(t, service.pool, first); status != "cancelled" {
		t.Fatalf("прежний черновик в состоянии %q, ожидалось cancelled", status)
	}
	if state, _ := recipientState(t, service.pool, "striked_out"); state != "cancelled" {
		t.Fatalf("вычеркнутый остался в состоянии %q", state)
	}
	var drafts int
	if err := service.pool.QueryRow(ctx, `SELECT count(*) FROM campaigns WHERE status = 'draft'`).Scan(&drafts); err != nil {
		t.Fatalf("счёт черновиков: %v", err)
	}
	if drafts != 1 || campaignStatus(t, service.pool, second) != "draft" {
		t.Fatalf("черновиков после перезаливки: %d", drafts)
	}
}

// Повтор пачки после таймаута MCP-вызова: дубли уходят в отсев с причиной, а
// не в ошибку, и второй строки не появляется.
func TestRepeatBatchSkipsDuplicates(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	batch := []Person{person(1, "ivan_petrov"), person(2, "petr_ivanov")}

	first, err := service.AddRecipients(ctx, id, batch)
	if err != nil {
		t.Fatalf("первая пачка: %v", err)
	}
	second, err := service.AddRecipients(ctx, id, batch)
	if err != nil {
		t.Fatalf("повтор пачки: %v", err)
	}

	if first.Accepted != 2 || second.Accepted != 0 || second.Skipped[ReasonDuplicate] != 2 {
		t.Fatalf("первая пачка %+v, повтор %+v", first, second)
	}
	var rows int
	if err := service.pool.QueryRow(ctx, `SELECT count(*) FROM recipients WHERE campaign_id = $1`, id).Scan(&rows); err != nil {
		t.Fatalf("счёт строк: %v", err)
	}
	if rows != 2 {
		t.Fatalf("строк в кампании %d, ожидалось 2", rows)
	}
}

// Ноль пригодных получателей: старт отказывает, отчёт отсева называет причину.
func TestStartRefusesEmptyDraft(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "Иван"), person(2, "+79991234567")})
	if err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonNoUsername] != 2 {
		t.Fatalf("отчёт отсева %+v", report)
	}

	if _, err := service.StartCampaign(ctx); !errors.Is(err, ErrEmptyDraft) {
		t.Fatalf("старт пустого черновика вернул %v", err)
	}
}

// Повтор старта после паузы: отправленные строки не трогаются, кампания
// продолжается с planned.
func TestRestartAfterPauseKeepsSent(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	id := startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("пауза: %v", err)
	}

	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("повтор старта: %v", err)
	}
	if status := campaignStatus(t, service.pool, id); status != "active" {
		t.Fatalf("кампания в состоянии %q", status)
	}
	next, err := service.ClaimNext(ctx, "test")
	if err != nil || next == nil {
		t.Fatalf("после старта строка не взялась: %v, %+v", err, next)
	}
	if next.ID == claimed.ID {
		t.Fatalf("повторно взята уже отправленная строка %d", next.ID)
	}
}

// Старт при живой активной кампании отказывает внятно, а не падает на
// уникальном индексе: оператор должен увидеть, что делать дальше.
func TestStartRefusedWhileActive(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"))
	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "второй")
	if err != nil {
		t.Fatalf("создать черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(2, "second_one")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}

	if _, err := service.StartCampaign(ctx); !errors.Is(err, ErrActiveCampaign) {
		t.Fatalf("старт при активной кампании вернул %v", err)
	}
	if status := campaignStatus(t, service.pool, draft); status != "draft" {
		t.Fatalf("черновик после отказа в состоянии %q", status)
	}
}

// pausedAndDraft - неудобная пара: кампания на паузе и подготовленный черновик
// рядом. Инструменты обязаны разбирать её однозначно.
func pausedAndDraft(t *testing.T, service *Service) (paused, draft int64) {
	t.Helper()
	ctx := context.Background()

	paused = startWith(t, service, person(1, "first_one"))
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("пауза: %v", err)
	}
	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "черновик")
	if err != nil {
		t.Fatalf("создать черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(2, "second_one")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	return paused, draft
}

// Пара «пауза плюс черновик»: start запускает черновик - новый список свежее
// возобновления, а пауза остаётся возобновимой.
func TestStartPrefersDraftOverPause(t *testing.T) {
	service := newService(t)
	paused, draft := pausedAndDraft(t, service)

	started, err := service.StartCampaign(context.Background())
	if err != nil || started != draft {
		t.Fatalf("запущена кампания %d вместо черновика %d: %v", started, draft, err)
	}
	if status := campaignStatus(t, service.pool, paused); status != "stopped" {
		t.Fatalf("пауза после запуска черновика в состоянии %q", status)
	}
}

// Та же пара под отменой: stop с cancel снимает черновик, пауза цела и
// продолжается следующим start. Вторая отмена снимает уже её.
func TestCancelHitsDraftBeforePause(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	paused, draft := pausedAndDraft(t, service)

	cancelled, err := service.StopCampaign(ctx, true)
	if err != nil || cancelled != draft {
		t.Fatalf("отменена кампания %d вместо черновика %d: %v", cancelled, draft, err)
	}
	if status := campaignStatus(t, service.pool, paused); status != "stopped" {
		t.Fatalf("пауза снята вместе с черновиком: состояние %q", status)
	}

	second, err := service.StopCampaign(ctx, true)
	if err != nil || second != paused {
		t.Fatalf("вторая отмена сняла кампанию %d вместо паузы %d: %v", second, paused, err)
	}
}

// Отмена бьёт по одной кампании: идущей, если она есть, иначе по черновику.
// Подготовленный черновик не исчезает вместе с отменённой рассылкой.
func TestCancelHitsOneCampaign(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	active := startWith(t, service, person(1, "first_one"))
	draft, err := service.LoadCampaign(ctx, "Привет, {имя}!", "второй")
	if err != nil {
		t.Fatalf("создать черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, draft, []Person{person(2, "second_one")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}

	cancelled, err := service.StopCampaign(ctx, true)
	if err != nil || cancelled != active {
		t.Fatalf("отменена кампания %d вместо %d: %v", cancelled, active, err)
	}
	if status := campaignStatus(t, service.pool, draft); status != "draft" {
		t.Fatalf("черновик исчез вместе с активной: состояние %q", status)
	}
	if state, _ := recipientState(t, service.pool, "second_one"); state != "planned" {
		t.Fatalf("строка черновика в состоянии %q", state)
	}

	// Идущей кампании больше нет - вторая отмена снимает черновик.
	second, err := service.StopCampaign(ctx, true)
	if err != nil || second != draft {
		t.Fatalf("вторая отмена сняла кампанию %d вместо %d: %v", second, draft, err)
	}
	if state, _ := recipientState(t, service.pool, "second_one"); state != "cancelled" {
		t.Fatalf("строка снятого черновика в состоянии %q", state)
	}
}

// Правило «один человек - одно сообщение» снято для служебных адресатов
// владельца и осталось для всех остальных: ручное касание закрывает незнакомца
// и не закрывает аккаунт владельца, которым эту же рассылку и проверяют.
func TestOwnerPassesImportAfterDirectTouch(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	insertDirect(t, service, "smirnov", "sent", testNow)
	insertDirect(t, service, "ivan_petrov", "sent", testNow)

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "проверка")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "smirnov"), person(2, "ivan_petrov")})
	if err != nil {
		t.Fatalf("импорт: %v", err)
	}
	if report.Accepted != 1 || report.Skipped[ReasonDirectSent] != 1 {
		t.Fatalf("отчёт отсева %+v", report)
	}
	if state, _ := recipientState(t, service.pool, "smirnov"); state != "planned" {
		t.Fatalf("служебный адресат в состоянии %q", state)
	}
}

// Стоп-лист держит и служебного адресата: срез снимает правило «одно
// сообщение», а не рубеж, который запрещает писать человеку вообще.
func TestOwnerStillHeldByStopList(t *testing.T) {
	service := newService(t, "smirnov")
	ctx := context.Background()

	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('smirnov')`); err != nil {
		t.Fatalf("стоп-лист: %v", err)
	}
	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "проверка")
	if err != nil {
		t.Fatalf("создать кампанию: %v", err)
	}
	report, err := service.AddRecipients(ctx, id, []Person{person(1, "smirnov")})
	if err != nil {
		t.Fatalf("импорт: %v", err)
	}
	if report.Accepted != 0 || report.Skipped[ReasonStopList] != 1 {
		t.Fatalf("отчёт отсева %+v", report)
	}
}
