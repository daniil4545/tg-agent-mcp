package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Sender - демон отправки. Один цикл на процесс и один аккаунт (ACCOUNT_LABEL):
// пула горутин нет намеренно, темп рассылки держится паузами, а не
// параллельностью, и второй аккаунт - это второй контейнер.
type Sender struct {
	service   *Service
	transport Transport
	label     string

	// chat - путь прямой отправки для вечерней сводки. Своего пути у сводки
	// нет: Chat.Send уже держит стоп аккаунта, резолв пира и журнал касаний.
	// Пусто - сводки нет вовсе.
	chat *Chat

	// lastIdle - причина простоя, о которой уже сказано в логе. Без этой памяти
	// закрытое окно писало бы одну и ту же строку каждый виток всю ночь.
	lastIdle string
}

// UseChat подключает диалог, которым уходит вечерняя сводка. Отдельным методом,
// а не аргументом конструктора: Chat строится на том же транспорте позже, уже
// после того как отправщик отдан в колбэк входящих сообщений.
func (s *Sender) UseChat(chat *Chat) { s.chat = chat }

// idleUnavailable - место причины, когда её не удалось прочитать. Значение
// заведомо не совпадает с настоящими причинами: иначе вернувшаяся БД молчала
// бы про состояние очереди.
const idleUnavailable = "idle_reason_unavailable"

// NewSender собирает отправщика на сервисе и транспорте.
func NewSender(service *Service, transport Transport) *Sender {
	return &Sender{service: service, transport: transport, label: service.cfg.AccountLabel}
}

// Run держит цикл до остановки контекста. Состояния в памяти нет: каждый виток
// заново спрашивает БД, поэтому перезапуск продолжает кампанию с места.
// Строку аккаунта заводит старт процесса (cmd), а не цикл: healthcheck
// спрашивает её раньше, чем отправщик успевает подняться.
func (s *Sender) Run(ctx context.Context) error {
	slog.Info("sender started", "account", s.label)

	for {
		if err := s.step(ctx); err != nil {
			// Остановка сервиса ошибкой не является: незакрытая строка
			// протухнет по claim и вернётся в очередь.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Сводка зовётся здесь, а не в хвосте step: вечером очередь пуста, и
		// step в этой ветке возвращается раньше. Её ошибка цикл не
		// останавливает - рассылка писем важнее отчёта о ней.
		if err := s.maybeSendDigest(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("digest failed", "error", err)
		}
		if !wait(ctx, s.pause()) {
			return nil
		}
	}
}

// step - один виток: взять строку и доставить её. Рубежи (окно, потолок, стоп
// аккаунта, стоп-лист) стоят внутри ClaimNext - пустой ответ означает, что
// отправлять сейчас нечего или нельзя.
func (s *Sender) step(ctx context.Context) error {
	item, err := s.service.ClaimNext(ctx, s.label)
	if err != nil {
		return err
	}
	if item == nil {
		s.logIdle(ctx)
		done, err := s.service.FinishIfEmpty(ctx)
		if err != nil {
			return err
		}
		if done {
			slog.Info("campaign finished")
		}
		return nil
	}
	s.lastIdle = ""
	return s.SendClaimed(ctx, item)
}

// SendClaimed доставляет взятую строку и закрывает её по исходу. Сетевые вызовы
// идут вне транзакций: claim уже закоммичен, финализация - отдельная короткая
// транзакция после ответа Telegram.
func (s *Sender) SendClaimed(ctx context.Context, item *Recipient) error {
	peer, err := s.transport.Resolve(ctx, item.Username)
	if err == nil {
		// Id человека переживает резолв в БД: короткое входящее ника не несёт,
		// и без id ответ такого человека своей строки не найдёт.
		if userID, refErr := peer.Ref.UserID(); refErr == nil {
			item.UserID = userID
		}
		// Id сообщения кампании не нужен: строка получателя опознаётся по
		// random_id, а журнал id ведёт только ручное касание.
		_, err = s.transport.Send(ctx, peer, item.Text, item.RandomID)
	}

	outcome := classifyDelivery(err, s.service.cfg.FloodWaitThreshold)
	switch {
	case outcome.stop:
		// Активный стоп обязан быть виден снаружи: рубеж, о котором никто не
		// узнал, равен его отсутствию.
		slog.Error("account stopped by telegram limit",
			"account", s.label, "reason", outcome.reason, "username", item.Username)
	case outcome.result == ResultSent:
		// След доставки: без него разбор «почему письмо не ушло вовремя» идёт
		// по базе, а лог показывает только неудачи.
		slog.Info("message sent",
			"account", s.label, "username", item.Username, "recipient_id", item.ID,
			"campaign_id", item.CampaignID, "user_id", item.UserID, "random_id", item.RandomID)
	case outcome.result == ResultUndelivered:
		slog.Warn("recipient undelivered", "username", item.Username, "reason", outcome.reason)
	case outcome.result == ResultRetry, outcome.result == ResultRetryCounted:
		slog.Warn("delivery postponed", "username", item.Username, "reason", outcome.reason)
	}

	// Стоп аккаунта пишется первым: рубеж не должен зависеть от того, удалась ли
	// вторая запись. Упавшая финализация оставит строку под протухающим claim,
	// а незаписанный стоп оставил бы аккаунт под ограничением в рассылке.
	if outcome.stop {
		if err := s.service.StopAccount(ctx, s.label, outcome.reason); err != nil {
			return err
		}
	}
	closed, err := s.service.FinishClaim(ctx, item, outcome.result, outcome.reason)
	if err != nil {
		return err
	}
	// Доставка состоялась, а записать её некуда. Строка уже sent - значит её
	// закрыл другой процесс той же метки, перехвативший протухший claim: факт
	// доставки в БД стоит, тревожить некому. Всё остальное - настоящее
	// расхождение, и единственный его след - этот лог.
	if !closed && outcome.result == ResultSent {
		var state string
		readErr := s.service.pool.QueryRow(ctx,
			`SELECT state FROM recipients WHERE id = $1`, item.ID).Scan(&state)
		if readErr == nil && state == "sent" {
			slog.Info("already recorded by another process",
				"recipient_id", item.ID, "username", item.Username)
		} else {
			slog.Error("delivered message was not recorded",
				"recipient_id", item.ID, "username", item.Username, "state", state, "error", readErr)
		}
	}
	if outcome.wait > 0 && !wait(ctx, outcome.wait) {
		return ctx.Err()
	}
	return nil
}

// OnMessage - точка подключения к живому транспорту: telegram.Client отдаёт
// сюда нормализованное входящее (сигнатура telegram.Handler). Сама связка с
// диспетчером gotd в срез не входит и впервые исполняется живьём на пилоте.
//
// Личное сообщение от человека закрывает его строку: отправленную переводит в
// replied, ещё не отправленную - тоже, и приглашение отменяется.
func (s *Sender) OnMessage(ctx context.Context, msg telegram.Message) error {
	if msg.Outgoing {
		return nil
	}
	// Только личка: реплика в группе ответом на приглашение не является.
	kind, userID, err := msg.Peer.Split()
	if err != nil || kind != telegram.KindUser {
		return nil
	}
	rows, err := s.service.MarkReplied(ctx, userID, msg.Username)
	if err != nil {
		return err
	}
	// Человек, написавший первым, снимается с очереди отдельно от ответивших:
	// шаблон поверх начатого им разговора не уходит, но и ответом на письмо,
	// которого он не получал, это не является.
	first, err := s.service.markWroteFirst(ctx, userID, msg.Username)
	if err != nil {
		return err
	}
	if first > 0 {
		slog.Info("recipient wrote first", "user_id", userID, "username", msg.Username, "rows", first)
	}
	if rows == 0 && first == 0 {
		// Ответ в ручном диалоге строки кампании не имеет и дефектом не
		// является: этот WARN означает реальную потерю ответа, и ручные
		// касания превратили бы его в фон.
		touched, err := s.service.directTouched(ctx, userID, msg.Username)
		if err != nil {
			slog.Warn("direct journal unavailable", "username", msg.Username, "error", err)
		} else if touched {
			slog.Info("inbound message in manual dialog", "user_id", userID, "username", msg.Username)
			return nil
		}
		// Несопоставленное входящее - не обязательно чужой человек: это ещё и
		// апдейт без ника от того, кому мы писали до появления tg_user_id.
		// Молчать здесь нельзя: ответ, не закрывший строку, оставляет человека
		// в очереди на второе касание.
		slog.Warn("inbound message not matched",
			"user_id", userID, "username", msg.Username)
		return nil
	}
	slog.Info("recipient replied", "user_id", userID, "username", msg.Username, "rows", rows)
	return nil
}

// logIdle называет причину молчания один раз на её появление. Сама причина
// диагностическая: ошибку её чтения хватит записать, останавливать из-за неё
// цикл нельзя - отправке она не мешает.
func (s *Sender) logIdle(ctx context.Context) {
	idle, err := s.service.IdleReason(ctx, s.label)
	if err != nil {
		// Сбой диагностики дедуплицируется наравне с причиной: недоступная БД
		// держится минутами, а витков за это время может пройти много.
		if s.lastIdle != idleUnavailable {
			s.lastIdle = idleUnavailable
			slog.Warn("idle reason unavailable", "account", s.label, "error", err)
		}
		return
	}
	if idle.Reason == s.lastIdle {
		return
	}
	s.lastIdle = idle.Reason
	// Сломанный шаблон - не штатный простой: очередь стоит, пока человек не
	// соберёт кампанию заново, и об этом обязан знать не только тот, кто откроет
	// status. Прочие причины проходят сами: окно откроется, потолок обнулится.
	if idle.Reason == IdleBrokenTemplate {
		slog.Error("sender idle", "account", s.label, "reason", idle.Reason)
		return
	}
	slog.Info("sender idle", "account", s.label, "reason", idle.Reason)
}

// pause - случайная задержка между витками из границ конфига: ровный темп
// выглядит машинным, а рассылку ведёт живой аккаунт.
func (s *Sender) pause() time.Duration {
	low, high := s.service.cfg.SendPauseMin, s.service.cfg.SendPauseMax
	if high <= low {
		return low
	}
	return low + rand.N(high-low)
}

// delivery - решение сервиса по исходу отправки.
type delivery struct {
	result SendResult
	reason string
	// stop - отказ уровня аккаунта: отправка с него прекращается целиком.
	stop bool
	// wait - переждать перед следующей строкой, если Telegram назвал срок.
	wait time.Duration
}

// classifyDelivery переводит исход отправки в решение. Разбор один на все
// отказы: FLOOD_WAIT короче порога - переждать и вернуть строку в очередь,
// длиннее - стоп аккаунта; PEER_FLOOD и мёртвая сессия - тоже стоп аккаунта,
// отправлять с него больше нечем; отказ по человеку - undelivered; всё неопознанное -
// повтор, потому что брошенная навсегда неизвестная ошибка теряет человека
// молча.
//
// Попытку расходует только неопознанный отказ: он один способен повторяться
// стабильно на одной строке. Отказ уровня аккаунта очередь не держит - отправка
// с этого аккаунта останавливается целиком, - и бюджет строки не трогает.
func classifyDelivery(err error, threshold time.Duration) delivery {
	if err == nil {
		return delivery{result: ResultSent}
	}
	if errors.Is(err, ErrNotAllowed) {
		return delivery{result: ResultUndelivered, reason: ReasonNotAllowed}
	}

	var failure *telegram.Failure
	if !errors.As(err, &failure) {
		return delivery{result: ResultRetryCounted, reason: "unknown_error"}
	}
	switch failure.Kind {
	case telegram.KindFloodWait:
		pause := time.Until(failure.RetryAt)
		if pause >= threshold {
			return delivery{result: ResultRetry, reason: failure.Kind, stop: true}
		}
		return delivery{result: ResultRetry, reason: failure.Kind, wait: pause}
	case telegram.KindAuthForbidden, telegram.KindPeerFlood:
		return delivery{result: ResultRetry, reason: failure.Kind, stop: true}
	case telegram.KindPeerForbidden, telegram.KindNotPerson, telegram.KindBadRequest, telegram.KindMediaInvalid:
		return delivery{result: ResultUndelivered, reason: failure.Kind}
	default:
		return delivery{result: ResultRetryCounted, reason: failure.Kind}
	}
}

// StopAccount останавливает аккаунт в БД. Причина первого стопа не
// переписывается: она называет, из-за чего аккаунт под ограничением, а
// повторные отказы - уже следствие.
func (s *Service) StopAccount(ctx context.Context, label, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE accounts SET stopped = true, stop_reason = $2, stopped_at = $3
		 WHERE label = $1 AND NOT stopped`,
		label, reason, s.Now())
	if err != nil {
		return fmt.Errorf("stop account %q: %w", label, err)
	}
	return nil
}

// ResumeAccount снимает стоп. Только явной командой человека: снятие по таймеру
// вернуло бы отправку с аккаунта, который Telegram всё ещё держит под
// ограничением.
//
// Причина снятого стопа возвращается вместе с признаком «стоп был»: читать
// состояние отдельным запросом нельзя - стоп, поставленный между чтением и
// снятием, снялся бы молча, а в ответ и в лог уехала бы прежняя причина.
func (s *Service) ResumeAccount(ctx context.Context, label string) (reason string, stopped bool, err error) {
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx,
			`SELECT stopped, stop_reason FROM accounts WHERE label = $1 FOR UPDATE`,
			label).Scan(&stopped, &reason)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return fmt.Errorf("account %q is not registered", label)
		}
		if scanErr != nil {
			return fmt.Errorf("lock account %q: %w", label, scanErr)
		}
		if !stopped {
			return nil
		}
		if _, execErr := tx.Exec(ctx,
			`UPDATE accounts SET stopped = false, stop_reason = '', stopped_at = NULL WHERE label = $1`,
			label); execErr != nil {
			return fmt.Errorf("resume account %q: %w", label, execErr)
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, stopped, nil
}

// AccountStopped отдаёт состояние аккаунта: на нём стоит healthcheck контейнера
// и ответ status.
func (s *Service) AccountStopped(ctx context.Context, label string) (bool, string, error) {
	var stopped bool
	var reason string
	err := s.pool.QueryRow(ctx, `SELECT stopped, stop_reason FROM accounts WHERE label = $1`, label).
		Scan(&stopped, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("account %q is not registered", label)
	}
	if err != nil {
		return false, "", fmt.Errorf("read account %q: %w", label, err)
	}
	return stopped, reason, nil
}

// wait ждёт с уважением к остановке и возвращает false, если процесс попросили
// закончить. time.Sleep здесь не годится: он не отпускает процесс по сигналу и
// не даёт тесту прогнать цикл быстро.
func wait(ctx context.Context, pause time.Duration) bool {
	if pause <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
