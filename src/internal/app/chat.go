package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Chat - диалог: ручная переписка и чтение поверх той же сессии, что и
// рассылка. Форма повторяет Sender - домен, граница наружу, метка аккаунта, -
// потому что рубежи у них общие: тот же аккаунт, тот же дневной потолок, тот
// же стоп.
type Chat struct {
	service   *Service
	transport Dialog
	label     string
}

// NewChat собирает диалог на сервисе и транспорте.
func NewChat(service *Service, transport Dialog) *Chat {
	return &Chat{service: service, transport: transport, label: service.cfg.AccountLabel}
}

const (
	defaultHistory = 20  // сколько сообщений отдаётся без явной глубины
	maxHistory     = 100 // потолок глубины: дальше ответ инструмента не читается
	maxFound       = 20  // потолок находок поиска
	// maxWait - потолок ожидания реплики. Держит его не сервис, а клиент MCP: у
	// вызова инструмента свой потолок около минуты, и на живом контуре ожидания
	// от 60 секунд падали у клиента по таймауту, не дождавшись нашего ответа.
	// 45 секунд проверены и возвращаются целиком; дольше ждут повторным tg_read.
	maxWait      = 45 * time.Second
	pollInterval = 3 * time.Second // шаг опроса истории во время ожидания
	maxTextLen   = 4096            // предел одного сообщения Telegram
	stopTimeout  = 5 * time.Second // на запись стопа вне контекста запроса
)

// Отказы рубежей ручной отправки. Список закрытый: новая строка здесь - новое
// решение сервиса, а не новый текст в ответе агенту.
var (
	ErrNoUsername      = errors.New("username is empty or does not look like a telegram username")
	ErrEmptyText       = errors.New("message text is empty")
	ErrTextTooLong     = errors.New("message is longer than telegram allows")
	ErrNotPrivate      = errors.New("only private chats are supported")
	ErrAccountStopped  = errors.New("account is stopped, resume it by hand after checking it")
	ErrStopList        = errors.New("person is in the stop list")
	ErrCampaignSending = errors.New("campaign is sending to this person right now")
	ErrColdWindow      = errors.New("first message to a stranger is allowed in the send window only")
	ErrColdLimit       = errors.New("daily limit of the account is spent")
	ErrDirectPending   = errors.New("previous message to this person is in unknown state")
)

// pendingWait - отказ повтору поверх незакрытого касания вместе с остатком
// срока. Срок несёт сама ошибка: считает его сервис по CLAIM_STALE_AFTER, а
// текст ответа агенту про настройки не знает.
type pendingWait struct {
	username string
	left     time.Duration
}

func (e *pendingWait) Error() string {
	return fmt.Sprintf("%s: @%s, %s left", ErrDirectPending, e.username, e.left)
}

func (e *pendingWait) Unwrap() error { return ErrDirectPending }

// campaignHold - кампания, чья строка держит человека закрытым для ручной
// отправки. Id, название и статус несёт сама ошибка: снять блокировку можно
// только действием над конкретной кампанией, и какое действие сработает,
// решает её статус. queueOpensAt - когда очередь снова начнёт двигаться (ноль,
// если она идёт прямо сейчас); staleAfter - за сколько протухает брошенный
// claim. Оба нужны тексту отказа: без них совет «повторите через несколько
// минут» врёт в ночь и в выходные.
type campaignHold struct {
	username     string
	id           int64
	title        string
	status       string
	queueOpensAt time.Time
	staleAfter   time.Duration
	// broken - шаблон кампании не разобран, и очередь по ней стоит. Без этого
	// признака совет «очередь закроет строку сама» врал бы: идущая кампания со
	// сломанным шаблоном не отправит ничего, пока её не соберут заново.
	broken bool
}

func (e *campaignHold) Error() string {
	return fmt.Sprintf("%s: @%s, campaign %d %q (%s)", ErrCampaignSending, e.username, e.id, e.title, e.status)
}

func (e *campaignHold) Unwrap() error { return ErrCampaignSending }

// accountStop - стоп аккаунта с причиной. Причина едет вместе с ошибкой, потому
// что от неё зависит следующий шаг человека: ограничение Telegram снимается
// resume_account после проверки аккаунта, а отозванная сессия - повторным
// входом, и перепутать эти два совета значит водить агента по кругу.
type accountStop struct {
	reason string
	// cause - отказ, из-за которого стоп поставлен сейчас; пусто, если стоп уже
	// стоял в БД.
	cause error
}

func (e *accountStop) Error() string {
	if e.cause == nil {
		return fmt.Sprintf("%s: %s", ErrAccountStopped, e.reason)
	}
	return fmt.Sprintf("%s: %s: %v", ErrAccountStopped, e.reason, e.cause)
}

func (e *accountStop) Unwrap() error { return ErrAccountStopped }

// DirectSend - состоявшееся ручное касание. Cold называет цену: холодное
// касание израсходовало дневной потолок аккаунта, тёплая реплика - нет.
type DirectSend struct {
	ID       int64
	Username string
	UserID   int64
	Cold     bool
}

// Conversation - прочитанный диалог: история на момент запроса и отдельно то,
// что пришло за время ожидания. Разделены, потому что ответ инструмента
// называет их по-разному.
type Conversation struct {
	Username string
	UserID   int64
	Messages []ChatMessage
	// Fresh - входящие, дождавшиеся ответа; пусто, если ожидание истекло.
	Fresh  []ChatMessage
	Waited time.Duration
}

// Find ищет человека по нику или имени. Наружу поиск не пишет и рубежами не
// ограничен: allow-list держит отправку, а не чтение.
func (c *Chat) Find(ctx context.Context, query string) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("пустой запрос: назовите ник или имя")
	}
	found, err := c.transport.Find(ctx, query, maxFound)
	if err != nil {
		return nil, c.guard(ctx, query, err)
	}
	if len(found) > maxFound {
		found = found[:maxFound]
	}
	return found, nil
}

// Send пишет человеку руками. Рубежи те же, что у рассылки, кроме пауз: темп
// задаёт человек. Окно и дневной потолок применяются только к первому касанию
// незнакомца - реплика в идущем диалоге свободна в любое время, иначе запрет
// ответить в 21:30 делает аккаунт менее похожим на живой, а не более.
//
// Строка журнала ложится до сетевого вызова: «Telegram подтвердил - процесс
// умер - записи нет» оставило бы человека открытым для следующей кампании.
func (c *Chat) Send(ctx context.Context, username, text string) (DirectSend, error) {
	name := normalizeUsername(username)
	if name == "" {
		return DirectSend{}, c.refuse(username, fmt.Errorf("%w: %q", ErrNoUsername, username))
	}
	if strings.TrimSpace(text) == "" {
		return DirectSend{}, c.refuse(name, ErrEmptyText)
	}
	if length := telegramLen(text); length > maxTextLen {
		return DirectSend{}, c.refuse(name, fmt.Errorf("%w: %d symbols", ErrTextTooLong, length))
	}

	// Стоп аккаунта спрашивается дважды: здесь - чтобы не ходить в Telegram
	// зря, и повторно под блокировкой строки, где решение и принимается.
	stopped, stopReason, err := c.service.AccountStopped(ctx, c.label)
	if err != nil {
		return DirectSend{}, err
	}
	if stopped {
		return DirectSend{}, c.refuse(name, &accountStop{reason: stopReason})
	}

	peer, err := c.transport.Resolve(ctx, name)
	if err != nil {
		return DirectSend{}, c.refuse(name, c.guard(ctx, name, err))
	}
	userID, private := privateUserID(peer.Ref)
	if !private {
		return DirectSend{}, c.refuse(name, fmt.Errorf("%w: @%s", ErrNotPrivate, name))
	}

	// Служебный адресат холодным не бывает: окно и потолок к нему не
	// применяются, и читать историю ради ответа на этот вопрос незачем.
	cold := false
	if !c.service.isOwner(name) {
		cold, err = c.coldContact(ctx, name, peer)
		if err != nil {
			return DirectSend{}, c.refuse(name, c.guard(ctx, name, err))
		}
	}

	// random_id свежий на каждый вызов, обратно кампании: диалог полон
	// одинаковых реплик, и стабильный id заставил бы Telegram проглотить
	// второе «ок» как дубль.
	randomID := newRandomID()
	id, err := c.openDirect(ctx, name, userID, text, randomID, cold)
	if err != nil {
		return DirectSend{}, c.refuse(name, err)
	}

	messageID, sendErr := c.transport.Send(ctx, peer, text, randomID)
	outcome := classifyDelivery(sendErr, c.service.cfg.FloodWaitThreshold)

	// Единственный вопрос, на который отвечает исход: известно ли, что сообщения
	// нет. Известно, когда отказал наш собственный рубеж - до сети дело не дошло,
	// - и когда сервер разобрал запрос и выполнять его отказался. Неизвестно
	// обратное: обрыв, таймаут и любой неопознанный отказ, а это прежде всего
	// 5xx, после которого сообщение вполне может существовать.
	var failure *telegram.Failure
	notCreated := errors.Is(sendErr, ErrNotAllowed) ||
		(errors.As(sendErr, &failure) && failure.NotSent())

	if outcome.stop {
		if err := c.stopAccount(ctx, name, outcome.reason); err != nil {
			return DirectSend{}, err
		}
		// Отказ, остановивший аккаунт, наружу идёт стопом: следующий шаг задаёт
		// причина стопа, а не сам отказ. Совет «переждите до 14:20» увёл бы агента
		// на второй круг: он подождёт, повторит и получит «аккаунт остановлен».
		sendErr = &accountStop{reason: outcome.reason, cause: sendErr}
	}

	switch {
	case outcome.result == ResultSent:
		if err := c.closeDirect(ctx, id, name, "sent", "", userID, messageID); err != nil {
			return DirectSend{}, err
		}
		slog.Info("direct message sent",
			"account", c.label, "username", name, "direct_id", id,
			"user_id", userID, "cold", cold, "random_id", randomID, "message_id", messageID)
		return DirectSend{ID: id, Username: name, UserID: userID, Cold: cold}, nil
	// Сообщение заведомо не создано: строка уходит в failed, и человек снова
	// доступен кампании. Ручной строке возвращаться в очередь некуда, а pending
	// закрыл бы человека для всех кампаний навсегда. Стоп аккаунта по PEER_FLOOD
	// и длинному FLOOD_WAIT стоит выше и от этой ветки не зависит: аккаунт
	// останавливается, а касание закрывается - два следствия одного отказа.
	case notCreated:
		if err := c.closeDirect(ctx, id, name, "failed", outcome.reason, userID, 0); err != nil {
			return DirectSend{}, err
		}
		slog.Warn("direct message undelivered", "username", name, "reason", outcome.reason)
	default:
		// Сообщение могло уйти: строка остаётся pending и закрывает человека.
		// Безопасная сторона, и её цену показывает status.
		slog.Warn("direct message delivery unknown",
			"username", name, "reason", outcome.reason, "direct_id", id)
	}
	return DirectSend{}, sendErr
}

// Read отдаёт диалог и, если попросили ждать, дожидается чужой реплики.
// Ожидание - опрос истории, а не второй подписчик апдейтов: обработчик gotd
// один на тип события и вызывается синхронно, поэтому заблокированный
// подписчик остановил бы распознавание ответов кампании.
func (c *Chat) Read(ctx context.Context, username string, depth int, waitFor time.Duration) (Conversation, error) {
	name := normalizeUsername(username)
	if name == "" {
		return Conversation{}, fmt.Errorf("%w: %q", ErrNoUsername, username)
	}
	depth = capped(depth, defaultHistory, maxHistory)
	waitFor = cappedWait(waitFor)

	peer, err := c.transport.Lookup(ctx, name)
	if err != nil {
		return Conversation{}, c.guard(ctx, name, err)
	}
	history, err := c.transport.History(ctx, peer, depth)
	if err != nil {
		return Conversation{}, c.guard(ctx, name, err)
	}
	userID, _ := privateUserID(peer.Ref)
	talk := Conversation{Username: name, UserID: userID, Messages: history}
	if waitFor == 0 {
		return talk, nil
	}

	// Ожидание кончает то из двух, что раньше: шаги опроса или часы сервиса.
	// Шаги заканчивают его при неподвижных часах, часы - когда сам опрос
	// оказался долгим и потолок ожидания иначе был бы превышен.
	deadline := c.service.Now().Add(waitFor)
	last := lastMessageID(history)
	waited := time.Duration(0)
	for waited < waitFor && c.service.Now().Before(deadline) {
		pause := pollInterval
		if left := waitFor - waited; left < pause {
			pause = left
		}
		if !wait(ctx, pause) {
			return talk, ctx.Err()
		}
		waited += pause

		fresh, err := c.transport.History(ctx, peer, depth)
		if err != nil {
			return talk, c.guard(ctx, name, err)
		}
		if incoming := newIncoming(fresh, last); len(incoming) > 0 {
			talk.Fresh = incoming
			talk.Waited = waited
			return talk, nil
		}
	}
	talk.Waited = waited
	return talk, nil
}

// Dialogs отдаёт диалоги всех трёх видов - человека, группу, канал - в
// порядке Telegram; вид каждой строки называет mcp.go по Ref и Broadcast.
func (c *Chat) Dialogs(ctx context.Context, limit int) ([]DialogInfo, error) {
	limit = capped(limit, defaultHistory, maxHistory)
	dialogs, err := c.transport.Dialogs(ctx, limit)
	if err != nil {
		return nil, c.guard(ctx, "", err)
	}
	return dialogs, nil
}

// coldContact отвечает, холодное ли это касание. Строка журнала sent делает
// проверку истории ненужной - этому человеку мы уже писали; иначе тёплым
// считается собеседник, от которого есть хотя бы одно входящее.
func (c *Chat) coldContact(ctx context.Context, name string, peer Peer) (bool, error) {
	var written bool
	if err := c.service.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM direct_messages WHERE username = $1 AND state = 'sent')`,
		name).Scan(&written); err != nil {
		return false, fmt.Errorf("read direct journal of %s: %w", name, err)
	}
	if written {
		return false, nil
	}
	history, err := c.transport.History(ctx, peer, defaultHistory)
	if err != nil {
		return false, fmt.Errorf("read history of %s: %w", name, err)
	}
	for _, msg := range history {
		if !msg.Outgoing {
			return false, nil
		}
	}
	return true, nil
}

// openDirect кладёт строку журнала под той же блокировкой строки аккаунта,
// которую берёт ClaimNext: рубежи ручной отправки и рассылки решают про один
// аккаунт, и решать их они обязаны по очереди. Порядок блокировок общий с
// ClaimNext, и accounts у обеих первая - дедлока нет.
func (c *Chat) openDirect(ctx context.Context, name string, userID int64, text string, randomID int64, cold bool) (int64, error) {
	now := c.service.Now()
	var id int64
	err := db.InTx(ctx, c.service.pool, func(tx pgx.Tx) error {
		var accountID int64
		var dailyLimit int
		var stopped bool
		var stopReason string
		err := tx.QueryRow(ctx,
			`SELECT id, daily_limit, stopped, stop_reason FROM accounts WHERE label = $1 FOR UPDATE`,
			c.label).Scan(&accountID, &dailyLimit, &stopped, &stopReason)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("account %q is not registered", c.label)
		}
		if err != nil {
			return fmt.Errorf("lock account: %w", err)
		}
		if stopped {
			return fmt.Errorf("%w: %s", ErrAccountStopped, stopReason)
		}

		var blocked bool
		if err := tx.QueryRow(ctx, directStopListSQL, name).Scan(&blocked); err != nil {
			return fmt.Errorf("check stop list: %w", err)
		}
		if blocked {
			return fmt.Errorf("%w: @%s", ErrStopList, name)
		}

		// Повтор поверх собственного незакрытого касания. Стоит до потолка: то
		// же логическое касание не имеет права списать вторую единицу, а строка
		// pending и так закрывает человека.
		var pendingAt time.Time
		switch err := tx.QueryRow(ctx, directPendingSQL, name, c.service.staleBefore(now)).Scan(&pendingAt); {
		case err == nil:
			return &pendingWait{username: name, left: pendingAt.Add(c.service.cfg.ClaimStaleAfter).Sub(now)}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("check pending direct message: %w", err)
		}

		if cold {
			if !c.service.inSendWindow(now) {
				return fmt.Errorf("%w: @%s", ErrColdWindow, name)
			}
			var used int
			if err := tx.QueryRow(ctx, c.service.accountUsedSQL,
				startOfDay(now), c.service.staleBefore(now), accountID).Scan(&used); err != nil {
				return fmt.Errorf("count daily usage: %w", err)
			}
			if used >= dailyLimit {
				return fmt.Errorf("%w: %d of %d", ErrColdLimit, used, dailyLimit)
			}
		}

		// Удержание на время идущей кампании служебного адресата не касается.
		// Принятая цена - два сообщения подряд, ручное и кампанийное: для
		// аккаунтов владельца это допустимо, для незнакомых людей удержание
		// остаётся.
		if !c.service.isOwner(name) {
			hold := campaignHold{
				username:     name,
				queueOpensAt: c.service.nextWindow(now),
				staleAfter:   c.service.cfg.ClaimStaleAfter,
			}
			var template string
			switch err := tx.QueryRow(ctx, campaignSendingSQL, name, c.service.staleBefore(now)).
				Scan(&hold.id, &hold.title, &hold.status, &template); {
			case err == nil:
				_, broken := badBraces(template)
				hold.broken = broken > 0
				return &hold
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("check campaign queue: %w", err)
			}
		}

		// $2::bigint - явный тип: без приведения NULLIF выводит int4, и живой
		// id Telegram больше 2^31 не кодируется вовсе.
		return tx.QueryRow(ctx,
			`INSERT INTO direct_messages
			 (username, tg_user_id, account_id, text, random_id, cold, state, created_at)
			 VALUES ($1, NULLIF($2::bigint, 0), $3, $4, $5, $6, 'pending', $7)
			 RETURNING id`,
			name, userID, accountID, text, randomID, cold, now).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// closeDirect закрывает строку журнала по исходу отправки. failed тем же
// заходом возвращает в очередь строки, снятые этим касанием: иначе человек не
// получит ни ручного сообщения, ни кампанийного, а причина в отчёте соврёт.
// message_id ложится только при успехе: нулевой id - это дубль по random_id,
// Telegram второго сообщения не создал и называть его номер нечем. $6::bigint
// приводится явно, как и tg_user_id: без приведения NULLIF выводит int4.
func (c *Chat) closeDirect(ctx context.Context, id int64, name, state, reason string, userID int64, messageID int) error {
	now := c.service.Now()
	return db.InTx(ctx, c.service.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE direct_messages
			 SET state = $2, reason = $3,
			     sent_at = CASE WHEN $2 = 'sent' THEN $4 ELSE sent_at END,
			     tg_user_id = COALESCE(NULLIF($5::bigint, 0), tg_user_id),
			     message_id = COALESCE(NULLIF($6::bigint, 0), message_id)
			 WHERE id = $1`,
			id, state, reason, now, userID, messageID); err != nil {
			return fmt.Errorf("close direct message %d: %w", id, err)
		}
		if state != "failed" {
			return nil
		}
		if _, err := tx.Exec(ctx, returnTouchedSQL, name); err != nil {
			return fmt.Errorf("return rows of %s: %w", name, err)
		}
		return nil
	})
}

// guard - ограничение аккаунта, каким бы вызовом оно ни пришло: резолвом,
// чтением истории или самой отправкой. Ограничение принадлежит аккаунту, а не
// методу (инвариант 3): contacts.resolveUsername отвечает FLOOD_WAIT раньше
// отправки, и без этой ветки агент получал бы «переждите» и тратил на
// ограниченном аккаунте ещё один резолв за повтор. Читающие инструменты идут
// сюда же: сессия и лимиты у них общие с отправкой, а ложный стоп стоит одного
// resume_account против цены аккаунта. Прочие отказы уходят наружу как есть.
func (c *Chat) guard(ctx context.Context, username string, err error) error {
	if err == nil {
		return nil
	}
	outcome := classifyDelivery(err, c.service.cfg.FloodWaitThreshold)
	if !outcome.stop {
		return err
	}
	if stopErr := c.stopAccount(ctx, username, outcome.reason); stopErr != nil {
		return stopErr
	}
	return &accountStop{reason: outcome.reason, cause: err}
}

// stopAccount фиксирует стоп: сначала лог, потом БД. Контекст свой, а не
// контекст запроса: клиент MCP может оборвать вызов ровно на PEER_FLOOD, и
// тогда стоп остался бы только в логе - ни в healthcheck, ни в рассылке его не
// было бы. Активный стоп обязан быть виден снаружи, иначе его нет.
func (c *Chat) stopAccount(ctx context.Context, username, reason string) error {
	slog.Error("account stopped by telegram limit",
		"account", c.label, "reason", reason, "username", username)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	return c.service.StopAccount(ctx, c.label, reason)
}

// nextWindow - когда очередь снова начнёт двигаться; ноль означает «идёт
// сейчас». Нужен тексту отказа: строку, которую держит кампания, вне окна
// отправки не сдвинет никто до ближайшего буднего утра, и совет «повторите
// через несколько минут» в 21:30 бьёт агента в стену.
func (s *Service) nextWindow(now time.Time) time.Time {
	if s.inSendWindow(now) {
		return time.Time{}
	}
	from, err := parseClock(s.cfg.SendWindowStart)
	if err != nil {
		return time.Time{}
	}
	local := now.In(moscowZone)
	open := startOfDay(local).Add(time.Duration(from) * time.Minute)
	for !open.After(local) || open.Weekday() == time.Saturday || open.Weekday() == time.Sunday {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

// refuse называет отказ рубежа в логе: без этого отказ уходит только в текст
// ответа агенту и исчезает вместе с диалогом.
func (c *Chat) refuse(username string, err error) error {
	slog.Warn("direct send refused", "account", c.label, "username", username, "error", err)
	return err
}

// directTouched отвечает, писали ли этому человеку руками. Зовёт отправщик:
// ответ в ручном диалоге строки кампании не имеет, и без этой проверки он
// превратил бы WARN о несопоставленном входящем в фон.
func (s *Service) directTouched(ctx context.Context, userID int64, username string) (bool, error) {
	name := normalizeUsername(username)
	if name == "" && userID <= 0 {
		return false, nil
	}
	var touched bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM direct_messages
		 WHERE state <> 'failed'
		   AND (($1::bigint > 0 AND tg_user_id = $1) OR ($2 <> '' AND username = $2)))`,
		userID, name).Scan(&touched); err != nil {
		return false, fmt.Errorf("read direct journal: %w", err)
	}
	return touched, nil
}

// Стоп-лист спрашивается по двум ключам, а не по одному: запись без ника
// (только amo_contact_id) иначе не закрывает ручную отправку, а инвариант
// «человек из стоп-листа не получает сообщение» сформулирован абсолютно. Ник
// человека связывается с контактом через строки кампаний.
const directStopListSQL = `SELECT EXISTS (
	SELECT 1 FROM stop_list s
	WHERE s.username = $1
	   OR EXISTS (SELECT 1 FROM recipients r
	              WHERE r.username = $1 AND r.amo_contact_id = s.amo_contact_id))`

// Гонка с рассылкой: строка этого человека уже побывала у отправщика, и её
// доставка неизвестна. Признак попытки - account_id: его ставит только claim, и
// строка остаётся за аккаунтом навсегда, тогда как claimed_at снимает возврат в
// очередь после временного отказа ($2 - граница протухания).
//
// Блокируют только кампании, которые ещё могут отправить: в cancelled и done
// очередь уже разобрана, снять такую строку человеку нечем, и блокировка была
// бы вечной. Живой claim блокирует при любом статусе: отправщик несёт строку
// прямо сейчас, и отмена кампании его не останавливает. Черновика в списке нет:
// account_id ставит только claim, а claim идёт по активной кампании, и обратно в
// draft статус не возвращается.
//
// Отдаётся сама кампания со статусом, а не «да/нет»: отказ обязан назвать, кто
// держит человека и что с этим делать, а действие у каждого статуса своё.
const campaignSendingSQL = `SELECT c.id, c.title, c.status, c.template_text
	FROM recipients r
	JOIN campaigns c ON c.id = r.campaign_id
	WHERE r.username = $1 AND r.state = 'planned' AND r.account_id IS NOT NULL
	  AND (c.status IN ('active', 'stopped') OR r.claimed_at >= $2)
	ORDER BY r.id
	LIMIT 1`

// directPendingSQL - свежее незакрытое касание этому человеку ($2 - граница
// протухания). Пока оно моложе границы, повтор отказывается: сообщение могло
// уйти, и вторая строка списала бы за него ещё единицу дневного потолка.
const directPendingSQL = `SELECT created_at FROM direct_messages
	WHERE username = $1 AND state = 'pending' AND created_at >= $2
	ORDER BY id DESC LIMIT 1`

// returnTouchedSQL возвращает в очередь строки, снятые этим ручным касанием:
// отправка не состоялась, и человек снова доступен кампании. Закрытые кампании
// не трогаются - их очередь уже разобрана.
const returnTouchedSQL = `UPDATE recipients AS r
	SET state = 'planned', reason = ''
	FROM campaigns c
	WHERE c.id = r.campaign_id AND c.status IN ('active', 'stopped', 'draft')
	  AND r.username = $1 AND r.state = 'skipped'
	  AND r.reason IN ('` + ReasonDirectSent + `', '` + ReasonDirectPending + `')`

// privateUserID - рубеж «только личка»: ник канала резолвится так же успешно,
// как ник человека. Пустая ссылка бывает у dry-run и отказом не является.
func privateUserID(ref telegram.Ref) (int64, bool) {
	if ref == "" {
		return 0, true
	}
	id, err := ref.UserID()
	if err != nil {
		return 0, false
	}
	return id, true
}

// capped приводит запрошенный размер к границам; ноль означает «по умолчанию».
func capped(value, fallback, limit int) int {
	switch {
	case value <= 0:
		return fallback
	case value > limit:
		return limit
	default:
		return value
	}
}

// cappedWait приводит запрошенное ожидание к границам. Ноль означает «не
// ждать»: инструмент чтения обязан отвечать сразу, если ждать не просили.
func cappedWait(pause time.Duration) time.Duration {
	switch {
	case pause <= 0:
		return 0
	case pause > maxWait:
		return maxWait
	default:
		return pause
	}
}

func lastMessageID(history []ChatMessage) int {
	last := 0
	for _, msg := range history {
		if msg.ID > last {
			last = msg.ID
		}
	}
	return last
}

// newIncoming - чужие реплики новее последнего известного сообщения. Ждём
// только входящее: своя отправка из соседнего вызова ответом не является.
func newIncoming(history []ChatMessage, lastID int) []ChatMessage {
	var fresh []ChatMessage
	for _, msg := range history {
		if msg.ID > lastID && !msg.Outgoing {
			fresh = append(fresh, msg)
		}
	}
	return fresh
}
