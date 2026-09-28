package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// senderOn собирает отправщика на тестовом сервисе: паузы почти нулевые, чтобы
// цикл гонялся быстро, порог FLOOD_WAIT - минута.
func senderOn(service *Service, transport Transport) *Sender {
	service.cfg.SendPauseMin = time.Millisecond
	service.cfg.SendPauseMax = 2 * time.Millisecond
	service.cfg.FloodWaitThreshold = time.Minute
	return NewSender(service, transport)
}

// stubTransport - подменный транспорт с заданным отказом: живого Telegram в
// срезе нет, отказы приходят из разбора ошибок шаблона.
type stubTransport struct {
	fail func(username string) error
	// peerID - id, который резолв отдаёт как ссылку на человека; 0 - ссылки нет,
	// как в dry-run.
	peerID int64
	// messageID - id, который отправка называет успехом; 0 повторяет дубль по
	// random_id, где Telegram номера не даёт.
	messageID int

	mu   sync.Mutex
	sent []Delivery
}

func (t *stubTransport) Resolve(_ context.Context, username string) (Peer, error) {
	peer := Peer{Username: username}
	if t.peerID != 0 {
		peer.Ref = telegram.NewRef(telegram.KindUser, t.peerID)
	}
	return peer, nil
}

func (t *stubTransport) Send(_ context.Context, peer Peer, text string, randomID int64) (int, error) {
	if t.fail != nil {
		if err := t.fail(peer.Username); err != nil {
			return 0, err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, Delivery{Username: peer.Username, Text: text, RandomID: randomID})
	return t.messageID, nil
}

func (t *stubTransport) Sent() []Delivery {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Delivery(nil), t.sent...)
}

// floodWait - отказ Telegram «подождите столько-то».
func floodWait(pause time.Duration) error {
	return &telegram.Failure{Kind: telegram.KindFloodWait, RetryAt: time.Now().Add(pause)}
}

// Смерть между доставкой и записью: строка отправлена, но не финализирована.
// Протухший claim возвращает её в очередь, повтор идёт с тем же random_id -
// Telegram дедуплицирует, второго сообщения человек не получает.
func TestResendAfterCrashKeepsRandomID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "ivan_petrov"))

	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	peer, err := transport.Resolve(ctx, claimed.Username)
	if err != nil {
		t.Fatalf("резолв: %v", err)
	}
	if _, err := transport.Send(ctx, peer, claimed.Text, claimed.RandomID); err != nil {
		t.Fatalf("отправка: %v", err)
	}

	// Процесс убит до финализации: claim протухает, следующий виток берёт ту же
	// строку.
	service.Now = func() time.Time { return testNow.Add(10 * time.Minute) }
	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток после перезапуска: %v", err)
	}

	sent := transport.Sent()
	if len(sent) != 2 || sent[0].RandomID != sent[1].RandomID {
		t.Fatalf("повтор ушёл с другим random_id: %+v", sent)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("строка в состоянии %q, ожидалось sent", state)
	}
}

// Временная ошибка: FLOOD_WAIT короче порога - переждать, строка остаётся
// planned, попытка не расходуется, аккаунт не останавливается.
func TestFloodWaitKeepsRowPlanned(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{fail: func(string) error { return floodWait(20 * time.Millisecond) }}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "ivan_petrov"))

	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток отправщика: %v", err)
	}

	state, reason := recipientState(t, service.pool, "ivan_petrov")
	if state != "planned" || reason != telegram.KindFloodWait {
		t.Fatalf("строка в состоянии %q с причиной %q", state, reason)
	}
	if stopped, _, err := service.AccountStopped(ctx, "test"); err != nil || stopped {
		t.Fatalf("короткий FLOOD_WAIT остановил аккаунт: %v, %v", stopped, err)
	}
	again, err := service.ClaimNext(ctx, "test")
	if err != nil || again == nil {
		t.Fatalf("строка не вернулась в очередь: %v, %+v", err, again)
	}
}

// PEER_FLOOD - ограничение уровня аккаунта, а не отказ по человеку: рассылка с
// него прекращается целиком, а строка возвращается в очередь неизрасходованной.
func TestPeerFloodStopsAccount(t *testing.T) {
	outcome := classifyDelivery(&telegram.Failure{Kind: telegram.KindPeerFlood, Permanent: true}, time.Minute)
	if !outcome.stop || outcome.result != ResultRetry {
		t.Fatalf("PEER_FLOOD разобран как %+v", outcome)
	}
}

// Постоянная ошибка по человеку: строка undelivered с причиной, кампания идёт
// дальше и закрывается на пустой очереди.
func TestPeerForbiddenMarksUndelivered(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{fail: func(username string) error {
		if username == "closed_one" {
			return &telegram.Failure{Kind: telegram.KindPeerForbidden, Permanent: true}
		}
		return nil
	}}
	sender := senderOn(service, transport)
	id := startWith(t, service, person(1, "closed_one"), person(2, "open_one"))

	for range 3 {
		if err := sender.step(ctx); err != nil {
			t.Fatalf("виток отправщика: %v", err)
		}
	}

	state, reason := recipientState(t, service.pool, "closed_one")
	if state != "undelivered" || reason != telegram.KindPeerForbidden {
		t.Fatalf("закрытая личка дала состояние %q с причиной %q", state, reason)
	}
	if state, _ := recipientState(t, service.pool, "open_one"); state != "sent" {
		t.Fatalf("кампания не продолжилась: второй получатель в состоянии %q", state)
	}
	if status := campaignStatus(t, service.pool, id); status != "done" {
		t.Fatalf("кампания в состоянии %q", status)
	}
}

// Постоянная ошибка аккаунта: FLOOD_WAIT длиннее порога останавливает аккаунт в
// БД, строка возвращается в planned, отправок с него больше нет, а стоп виден
// снаружи - на нём стоит healthcheck.
func TestAccountStopOnLongFloodWait(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{fail: func(string) error { return floodWait(2 * time.Minute) }}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "ivan_petrov"))

	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток отправщика: %v", err)
	}

	stopped, reason, err := service.AccountStopped(ctx, "test")
	if err != nil || !stopped || reason != telegram.KindFloodWait {
		t.Fatalf("аккаунт не остановлен: %v, %q, %v", stopped, reason, err)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "planned" {
		t.Fatalf("строка в состоянии %q, ожидалось planned", state)
	}
	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed != nil {
		t.Fatalf("остановленный аккаунт взял строку: %+v, %v", claimed, err)
	}

	// Снятие - только командой человека.
	if _, _, err := service.ResumeAccount(ctx, "test"); err != nil {
		t.Fatalf("снять стоп: %v", err)
	}
	if claimed, err := service.ClaimNext(ctx, "test"); err != nil || claimed == nil {
		t.Fatalf("после снятия стопа строка не взялась: %+v, %v", claimed, err)
	}
}

// Перезапуск посреди кампании: новый процесс продолжает с planned, никому не
// шлёт второй раз и доводит кампанию до done; цикл уходит по отмене контекста.
func TestRestartContinuesCampaign(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	first := &stubTransport{}
	id := startWith(t, service, person(1, "first_one"), person(2, "second_one"))
	if err := senderOn(service, first).step(ctx); err != nil {
		t.Fatalf("виток до перезапуска: %v", err)
	}

	second := &stubTransport{}
	sender := senderOn(service, second)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sender.Run(runCtx) }()

	deadline := time.Now().Add(10 * time.Second)
	for campaignStatus(t, service.pool, id) != "done" {
		if time.Now().After(deadline) {
			t.Fatal("кампания не закрылась после перезапуска")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("цикл вернул ошибку: %v", err)
	}

	delivered := map[string]int{}
	for _, item := range append(first.Sent(), second.Sent()...) {
		delivered[item.Username]++
	}
	if len(delivered) != 2 || delivered["first_one"] != 1 || delivered["second_one"] != 1 {
		t.Fatalf("отправки после перезапуска: %+v", delivered)
	}
}

// Входящее личное сообщение закрывает строку человека, но по-разному:
// отправленную переводит в replied, ещё не отправленную - в skipped с причиной
// «написал сам». Приглашение не уходит ни в том, ни в другом случае, а в
// метрику ответов попадает только тот, кому письмо действительно ушло.
func TestInboundEventMarksReplied(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := &stubTransport{}
	sender := senderOn(service, transport)
	startWith(t, service, person(1, "answered_one"), person(2, "wrote_first"))
	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток отправщика: %v", err)
	}

	want := map[string]struct{ state, reason string }{
		"answered_one": {"replied", ""},
		"wrote_first":  {"skipped", ReasonWroteFirst},
	}
	for _, name := range []string{"answered_one", "wrote_first"} {
		event := telegram.Message{Peer: telegram.NewRef(telegram.KindUser, 100), Username: name, Text: "привет"}
		if err := sender.OnMessage(ctx, event); err != nil {
			t.Fatalf("входящее от %s: %v", name, err)
		}
		state, reason := recipientState(t, service.pool, name)
		if state != want[name].state || reason != want[name].reason {
			t.Fatalf("строка %s в состоянии %q (%s), ожидалось %q (%s)",
				name, state, reason, want[name].state, want[name].reason)
		}
	}

	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток после ответов: %v", err)
	}
	if sent := transport.Sent(); len(sent) != 1 || sent[0].Username != "answered_one" {
		t.Fatalf("написавшему первым ушло приглашение: %+v", sent)
	}
}

// Короткое входящее ника не несёт: человек узнаётся по id Telegram,
// сохранённому при отправке.
func TestReplyMatchedByUserID(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	sender := senderOn(service, &stubTransport{peerID: 777})
	startWith(t, service, person(1, "ivan_petrov"))
	if err := sender.step(ctx); err != nil {
		t.Fatalf("виток отправщика: %v", err)
	}

	event := telegram.Message{Peer: telegram.NewRef(telegram.KindUser, 777), Text: "привет"}
	if err := sender.OnMessage(ctx, event); err != nil {
		t.Fatalf("входящее без ника: %v", err)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "replied" {
		t.Fatalf("строка в состоянии %q, ожидалось replied", state)
	}
}

// Строку увели из очереди, пока шла отправка: доставка всё равно записывается,
// а не теряется в ERROR-логе - без sent_at человек получил бы второе сообщение
// в следующей кампании.
func TestDeliveredRowRecordedAfterSkip(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	sender := senderOn(service, &stubTransport{})
	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.pool.Exec(ctx,
		`UPDATE recipients SET state = 'skipped' WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("увести строку из очереди: %v", err)
	}

	logged := captureLogs(t)
	if err := sender.SendClaimed(ctx, claimed); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if text := logged.String(); strings.Contains(text, "delivered message was not recorded") {
		t.Fatalf("доставка не записана:\n%s", text)
	}
	if state, _ := recipientState(t, service.pool, "ivan_petrov"); state != "sent" {
		t.Fatalf("строка в состоянии %q, ожидалось sent", state)
	}
}

// Строку закрыл второй процесс той же метки, перехвативший протухший claim:
// доставка в БД уже записана, и ERROR здесь был бы ложной тревогой на рубеже
// наблюдаемости - настоящий стоп потерялся бы среди таких записей.
func TestDeliveryRecordedByOtherProcessIsNotError(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	sender := senderOn(service, &stubTransport{})
	startWith(t, service, person(1, "ivan_petrov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("запись доставки вторым процессом: %v", err)
	}

	logged := captureLogs(t)
	if err := sender.SendClaimed(ctx, claimed); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if text := logged.String(); strings.Contains(text, "delivered message was not recorded") {
		t.Fatalf("ложная тревога на записанной доставке:\n%s", text)
	}
}

// captureLogs перехватывает записи уровня WARN и выше на время теста: рубеж,
// единственный след которого - лог, иначе проверить нечем.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}
