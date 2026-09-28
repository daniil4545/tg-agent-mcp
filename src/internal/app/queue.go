package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
)

// moscowZone - МСК фиксированным смещением, а не Europe/Moscow: перевода часов
// в Москве нет с 2014 года, а tzdata в контейнере может не оказаться, и тогда
// окно отправки молча уехало бы в UTC.
var moscowZone = time.FixedZone("MSK", 3*60*60)

// Recipient - строка очереди, взятая отправщиком. Текст уже собран: за
// шаблоном в базу отправщик не возвращается.
type Recipient struct {
	ID         int64
	CampaignID int64
	ContactID  int64
	LeadID     int64
	Name       string
	Username   string
	RandomID   int64
	Text       string

	// UserID - id человека в Telegram, известный после резолва. Заполняет
	// отправщик перед финализацией: по нему находится ответ человека, чей
	// апдейт пришёл без ника.
	UserID int64
}

// SendResult - исход доставки взятой строки.
type SendResult string

const (
	ResultSent         SendResult = "sent"          // доставлено, человек израсходован
	ResultUndelivered  SendResult = "undelivered"   // отказ по человеку, повтор не поможет
	ResultRetry        SendResult = "retry"         // временный отказ: строка возвращается в очередь
	ResultRetryCounted SendResult = "retry_counted" // временный отказ с расходом попытки
)

// ReasonRetryExhausted - причина ухода строки из очереди по исчерпанию бюджета
// попыток. Живёт здесь, а не среди причин отсева импорта: её ставит только
// финализация.
const ReasonRetryExhausted = "retry_exhausted"

// maxAttempts - бюджет расходуемых попыток на строку. Выборка берёт первую
// строку по id, поэтому строка со стабильной неопознанной ошибкой без бюджета
// держала бы голову очереди вечно и кампания не двигалась бы.
const maxAttempts = 5

// maxSkips - сколько негодных строк снимает один виток выборки, прежде чем
// вернуть очередь следующему. Потолок нужен ровно затем, чтобы транзакция не
// росла: она держит блокировку строки аккаунта, а её ждёт и ручная отправка.
const maxSkips = 50

// EnsureAccount заводит строку аккаунта под метку процесса при первом старте и
// отдаёт действующий потолок. daily_limit существующей строки не
// переписывается: потолок живёт в БД, и перезапуск с другой переменной
// окружения не должен его тихо поднять - расхождение называет старт процесса.
func (s *Service) EnsureAccount(ctx context.Context) (int64, int, error) {
	var id int64
	var limit int
	// DO UPDATE вместо DO NOTHING только ради RETURNING: DO NOTHING на
	// конфликте не возвращает строку вовсе.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO accounts (label, daily_limit) VALUES ($1, $2)
		 ON CONFLICT (label) DO UPDATE SET label = EXCLUDED.label
		 RETURNING id, daily_limit`,
		s.cfg.AccountLabel, s.cfg.AccountDailyLimit).Scan(&id, &limit)
	if err != nil {
		return 0, 0, fmt.Errorf("ensure account: %w", err)
	}
	return id, limit, nil
}

// ClaimNext берёт одну строку очереди под аккаунт. Транзакция короткая и без
// единого сетевого вызова: под блокировкой строки аккаунта проверяются рубежи
// (окно, стоп аккаунта, дневной потолок, активная кампания, стоп-лист,
// «получал в любой кампании»), затем строка помечается claimed_at.
//
// nil без ошибки - очередь пуста или рубеж не пускает; это штатный ответ.
func (s *Service) ClaimNext(ctx context.Context, accountLabel string) (*Recipient, error) {
	now := s.Now()
	// Ранний выход по закрытому окну остаётся, пока список служебных адресатов
	// пуст: ночью и в выходные к базе не ходим вовсе. Непустой список даёт
	// строкам владельца путь и вне окна, поэтому там транзакция всё же нужна.
	ownerOnly := !s.inSendWindow(now)
	if ownerOnly && len(s.owners) == 0 {
		return nil, nil
	}
	stale := s.staleBefore(now)

	var claimed *Recipient
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Блокировка строки аккаунта сериализует отправщиков одного аккаунта:
		// иначе два процесса пройдут проверку потолка одновременно.
		var accountID int64
		var dailyLimit int
		var stopped bool
		err := tx.QueryRow(ctx,
			`SELECT id, daily_limit, stopped FROM accounts WHERE label = $1 FOR UPDATE`,
			accountLabel).Scan(&accountID, &dailyLimit, &stopped)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("account %q is not registered", accountLabel)
		}
		if err != nil {
			return fmt.Errorf("lock account: %w", err)
		}
		if stopped {
			return nil
		}

		// Живой claim считается наравне с отправленным: строка, взятая другим
		// процессом, уже почти сообщение.
		var used int
		if err := tx.QueryRow(ctx, s.accountUsedSQL,
			startOfDay(now), stale, accountID).Scan(&used); err != nil {
			return fmt.Errorf("count daily usage: %w", err)
		}
		if used >= dailyLimit {
			if len(s.owners) == 0 {
				return nil
			}
			ownerOnly = true
		}

		var campaignID int64
		var template string
		err = tx.QueryRow(ctx, `SELECT id, template_text FROM campaigns WHERE status = 'active'`).
			Scan(&campaignID, &template)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load active campaign: %w", err)
		}

		// Шаблон с неразобранной скобкой отправляться не имеет права: кампания,
		// собранная прошлой версией, где «{{имя}}» проходил вход, дала бы живому
		// человеку «Привет, {Пётр}!». Рубеж стоит здесь, а не только в
		// StartCampaign, потому что кампания, ставшая active до обновления, через
		// start больше не проходит, а сюда приходит каждое сообщение. Состояние
		// кампании при этом не меняется: его ведёт человек, а сломанный шаблон -
		// причина простоя очереди, и называет её IdleReason.
		if _, total := badBraces(template); total > 0 {
			return nil
		}

		if _, err := tx.Exec(ctx, s.skipDisqualifiedSQL, campaignID, stale); err != nil {
			return fmt.Errorf("skip disqualified: %w", err)
		}

		// Строки с негодным именем снимаются здесь же, а не только на импорте:
		// загруженные до появления этого рубежа иначе ушли бы живым людям письмом,
		// начинающимся с запятой или с чужой скобкой. Порядок после общего отсева
		// не случаен: человеку и в стоп-листе, и без имени причиной называется
		// стоп-лист - рубеж, который его остановил бы в любом случае.
		needName := hasName(template)
		if needName {
			if _, err := tx.Exec(ctx, skipBadNamesSQL, campaignID, stale); err != nil {
				return fmt.Errorf("skip bad names: %w", err)
			}
		}

		// Последняя проверка имени - на взятой строке и тем же ответом, что на
		// импорте: длина письма зависит от шаблона и меряется в единицах UTF-16, и
		// в SQL-условии выборки её точно не выразить. Негодные строки снимаются
		// подряд, а не по одной за виток: снятая выходит из planned, и тот же
		// запрос её уже не видит, - иначе сорок таких строк разбираются часами и
		// всё это время кампания выглядит идущей. Потолок витка держит транзакцию
		// короткой: она блокирует строку аккаунта, которую ждёт и ручная отправка.
		var picked *Recipient
		for range maxSkips {
			var item Recipient
			err = tx.QueryRow(ctx, s.claimSQL, campaignID, stale, accountID, needName, ownerOnly).Scan(
				&item.ID, &item.CampaignID, &item.ContactID, &item.LeadID,
				&item.Name, &item.Username, &item.RandomID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("select recipient: %w", err)
			}

			fault := nameFault(template, cleanName(item.Name))
			if fault == "" {
				picked = &item
				break
			}
			slog.Warn("recipient skipped by name rule",
				"recipient_id", item.ID, "username", item.Username, "reason", fault)
			if _, err := tx.Exec(ctx,
				`UPDATE recipients SET state = 'skipped', reason = $2, claimed_at = NULL WHERE id = $1`,
				item.ID, fault); err != nil {
				return fmt.Errorf("skip recipient %d: %w", item.ID, err)
			}
		}
		if picked == nil {
			return nil
		}

		if _, err := tx.Exec(ctx,
			`UPDATE recipients SET claimed_at = $2, account_id = $3 WHERE id = $1`,
			picked.ID, now, accountID); err != nil {
			return fmt.Errorf("claim recipient: %w", err)
		}

		picked.Text = RenderMessage(template, picked.Name)
		claimed = picked
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim next: %w", err)
	}
	return claimed, nil
}

// Причины простоя очереди: почему отправщик сейчас молчит. Отвечают на вопрос
// «почему письмо не ушло вовремя», ради которого сервис и наблюдают.
const (
	IdleWindowClosed   = "send_window_closed"  // не будни или вне окна отправки
	IdleAccountStop    = "account_stopped"     // ограничение аккаунта, снимается человеком
	IdleDailyLimit     = "daily_limit_reached" // дневной потолок аккаунта израсходован
	IdleNoCampaign     = "no_active_campaign"  // работать не над чем
	IdleBrokenTemplate = "broken_template"     // в шаблоне кампании неразобранные скобки
	IdleQueueEmpty     = "queue_empty"         // в кампании не осталось строк в очереди
	IdleQueueBlocked   = "queue_blocked"       // строки есть, но закреплены за другим аккаунтом или взяты
	// строки есть, но все до одной снимаются рубежом: отправок по ним не будет
	IdleQueueDisqualified = "queue_disqualified"
	IdleClaimRaceLost     = "claim_race_lost" // строка доступна, но её забрал сосед между витками
)

// Idle - причина простоя с числами, без которых она не проверяется: «потолок
// израсходован» без «7 из 7» и «окно закрыто» без часа открытия заставляют
// человека искать те же цифры руками.
type Idle struct {
	Reason string
	// Used, Limit - дневной расход аккаунта и его потолок из БД: у причины
	// IdleDailyLimit.
	Used  int
	Limit int
	// OpensAt - ближайшее открытие окна отправки: у причины IdleWindowClosed.
	OpensAt time.Time
	// Left - сколько строк осталось в очереди: у причины IdleQueueDisqualified.
	// Без числа «их снимет рубеж» не проверяется по счётчикам кампании.
	Left int
	// Braces - неразобранные скобки шаблона: у причины IdleBrokenTemplate. Отчёт
	// подменяет их скобками показанной кампании - его строка говорит про «эту
	// кампанию», а не про активную.
	Braces []string
}

// IdleReason объясняет пустой ответ ClaimNext. Зовётся в простое и из отчёта, не
// на каждой отправке, и рубежом не является: это диагностика, и её расхождение с
// ClaimNext испортит лог, но не пропустит сообщение.
func (s *Service) IdleReason(ctx context.Context, accountLabel string) (Idle, error) {
	now := s.Now()
	if !s.inSendWindow(now) {
		return Idle{Reason: IdleWindowClosed, OpensAt: s.nextWindow(now)}, nil
	}

	var accountID int64
	var dailyLimit int
	var stopped bool
	err := s.pool.QueryRow(ctx,
		`SELECT id, daily_limit, stopped FROM accounts WHERE label = $1`,
		accountLabel).Scan(&accountID, &dailyLimit, &stopped)
	if err != nil {
		return Idle{}, fmt.Errorf("read account: %w", err)
	}
	if stopped {
		return Idle{Reason: IdleAccountStop}, nil
	}

	stale := s.staleBefore(now)
	var used int
	if err := s.pool.QueryRow(ctx, s.accountUsedSQL,
		startOfDay(now), stale, accountID).Scan(&used); err != nil {
		return Idle{}, fmt.Errorf("count daily usage: %w", err)
	}
	if used >= dailyLimit {
		return Idle{Reason: IdleDailyLimit, Used: used, Limit: dailyLimit}, nil
	}

	var campaignID int64
	var template string
	err = s.pool.QueryRow(ctx, `SELECT id, template_text FROM campaigns WHERE status = 'active'`).
		Scan(&campaignID, &template)
	if errors.Is(err, pgx.ErrNoRows) {
		return Idle{Reason: IdleNoCampaign}, nil
	}
	if err != nil {
		return Idle{}, fmt.Errorf("load active campaign: %w", err)
	}
	if spots, total := badBraces(template); total > 0 {
		return Idle{Reason: IdleBrokenTemplate, Braces: spots}, nil
	}

	var planned, sendable, available int
	if err := s.pool.QueryRow(ctx, s.idleQueueSQL, campaignID, stale, accountID, hasName(template)).
		Scan(&planned, &sendable, &available); err != nil {
		return Idle{}, fmt.Errorf("count queue: %w", err)
	}
	switch {
	case planned == 0:
		return Idle{Reason: IdleQueueEmpty}, nil
	case available > 0:
		return Idle{Reason: IdleClaimRaceLost}, nil
	case sendable > 0:
		return Idle{Reason: IdleQueueBlocked}, nil
	default:
		// Строки есть, но уйти не может ни одна: их снимет ближайший виток
		// выборки. Прежде этот случай назывался чужим аккаунтом - совет ждать
		// отправщика, которого нет.
		return Idle{Reason: IdleQueueDisqualified, Left: planned}, nil
	}
}

// FinishClaim закрывает строку по исходу доставки - отдельной короткой
// транзакцией уже после сетевого вызова. Стоп-лист и «получал в любой
// кампании» здесь не перепроверяются: это рубежи ClaimNext, они стоят до
// отправки, а не после.
//
// false без ошибки означает, что строка ушла из очереди, пока шла отправка
// (человек ответил первым), и исход отброшен.
func (s *Service) FinishClaim(ctx context.Context, item *Recipient, result SendResult, reason string) (bool, error) {
	now := s.Now()
	if result == ResultRetryCounted {
		var state string
		var attempts int
		err := s.pool.QueryRow(ctx, countedRetrySQL,
			item.ID, reason, ReasonCampaignGone, maxAttempts, ReasonRetryExhausted).Scan(&state, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("finish claim: %w", err)
		}
		if state == "undelivered" {
			slog.Warn("retry budget exhausted",
				"recipient_id", item.ID, "username", item.Username, "attempts", attempts, "reason", reason)
		}
		return true, nil
	}

	var query string
	var args []any
	switch result {
	case ResultSent:
		// Доставка состоялась - факт обязан лечь в БД из любого состояния, в
		// которое строку увели, пока шла отправка: без sent_at человек прошёл бы
		// импорт следующей кампании и получил второе сообщение. Ответ мог
		// обогнать запись: replied остаётся replied, но с sent_at. undelivered
		// перекрывается доставкой: отказ мог записать первый процесс той же
		// метки, а доставил второй, перехвативший протухший claim.
		// tg_user_id пишется тем же запросом: ответ без ника ищется по нему.
		// $4::bigint - явный тип: без приведения NULLIF выводит int4, и живой id
		// Telegram больше 2^31 не кодируется вовсе.
		query = `UPDATE recipients
			SET state = CASE WHEN state = 'replied' THEN 'replied' ELSE 'sent' END,
			    reason = $3, sent_at = COALESCE(sent_at, $2), claimed_at = NULL,
			    tg_user_id = COALESCE(NULLIF($4::bigint, 0), tg_user_id)
			WHERE id = $1 AND state IN ('planned', 'replied', 'skipped', 'cancelled', 'undelivered')`
		args = []any{item.ID, now, reason, item.UserID}
	case ResultUndelivered:
		query = `UPDATE recipients SET state = 'undelivered', reason = $2, claimed_at = NULL
			WHERE id = $1 AND state = 'planned'`
		args = []any{item.ID, reason}
	case ResultRetry:
		// Попытка не расходуется: снимаем claim и оставляем причину для лога.
		// Исключение - отменённая кампания: её очередь уже снята, и строка,
		// которую отмена не тронула ради живого claim, уходит в cancelled
		// здесь. Иначе она осталась бы planned навсегда - выборка берёт только
		// строки активной кампании.
		query = `UPDATE recipients AS r
			SET state = CASE WHEN c.status = 'cancelled' THEN 'cancelled' ELSE r.state END,
			    reason = CASE WHEN c.status = 'cancelled' THEN $3 ELSE $2 END,
			    claimed_at = NULL
			FROM campaigns c
			WHERE r.id = $1 AND r.state = 'planned' AND c.id = r.campaign_id`
		args = []any{item.ID, reason, ReasonCampaignGone}
	default:
		return false, fmt.Errorf("unknown send result %q", result)
	}

	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("finish claim: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkReplied закрывает строки человека, который сам вступил в диалог.
// Отправленная строка переходит в replied, ещё не отправленная - тоже, и это
// отменяет приглашение. Строки черновика не трогаются: ему ещё не слали, а
// replied отсеял бы человека из будущей рассылки с причиной «уже получал».
//
// Человек узнаётся по id Telegram и по нику: короткий апдейт ника не несёт, а
// id известен только у строк, которым уже отправляли. Оба условия в одном
// запросе - это один и тот же человек, и обе его строки закрываются вместе.
// $1::bigint - явный тип: сравнение с литералом вывело бы параметр в int4, и
// живой id Telegram больше 2^31 не закодировался бы.
//
// claimed_at не снимается: строка в работе обязана остаться в дневном счёте до
// финализации, иначе второй процесс аккаунта возьмёт лишнюю. Из выборки строка
// уходит и так - выборка берёт только planned.
func (s *Service) MarkReplied(ctx context.Context, userID int64, username string) (int, error) {
	name := normalizeUsername(username)
	if name == "" && userID <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE recipients AS r SET state = 'replied', replied_at = $3
		 FROM campaigns c
		 WHERE c.id = r.campaign_id AND c.status IN ('active', 'stopped', 'done')
		   AND r.state = 'sent'
		   AND (($1::bigint > 0 AND r.tg_user_id = $1) OR ($2 <> '' AND r.username = $2))`,
		userID, name, s.Now())
	if err != nil {
		return 0, fmt.Errorf("mark replied: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// markWroteFirst снимает с очереди человека, который написал сам до того, как
// до него дошла очередь. Его строка не уходит в replied: он отвечал не на наше
// письмо, и в метрике ответов кампании ему не место - именно эта метрика и есть
// предмет теста оффера. Отправлять ему шаблон тоже нельзя: разговор уже начат
// им самим.
//
// Строка с живым claim не трогается: её прямо сейчас несёт отправщик, и
// сообщение могло уже уйти. Снимать с неё claim тем более нельзя - строка в
// работе обязана остаться в дневном счёте до финализации, иначе второй процесс
// аккаунта возьмёт лишнюю.
func (s *Service) markWroteFirst(ctx context.Context, userID int64, username string) (int, error) {
	name := normalizeUsername(username)
	if name == "" && userID <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE recipients AS r SET state = 'skipped', reason = '`+ReasonWroteFirst+`'
		 FROM campaigns c
		 WHERE c.id = r.campaign_id AND c.status IN ('active', 'stopped')
		   AND r.state = 'planned'
		   AND (r.claimed_at IS NULL OR r.claimed_at < $3)
		   AND (($1::bigint > 0 AND r.tg_user_id = $1) OR ($2 <> '' AND r.username = $2))`,
		userID, name, s.staleBefore(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("mark wrote first: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// inSendWindow - рубеж окна: только будни и только между границами конфига по
// МСК. Кривое окно молчит, а не шлёт: конфиг проверен на старте, и сюда
// ошибка разбора попасть не должна.
func (s *Service) inSendWindow(now time.Time) bool {
	local := now.In(moscowZone)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return false
	}
	from, err := parseClock(s.cfg.SendWindowStart)
	if err != nil {
		return false
	}
	to, err := parseClock(s.cfg.SendWindowEnd)
	if err != nil {
		return false
	}
	minutes := local.Hour()*60 + local.Minute()
	return minutes >= from && minutes < to
}

func parseClock(value string) (int, error) {
	point, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("parse clock %q: %w", value, err)
	}
	return point.Hour()*60 + point.Minute(), nil
}

// startOfDay - начало суток по МСК: дневной потолок считается календарным
// днём того же пояса, что и окно отправки.
func startOfDay(now time.Time) time.Time {
	local := now.In(moscowZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, moscowZone)
}

// Отсев получателя перед отправкой. Оба условия проверяются в claim-транзакции,
// а не только при импорте: человека вносят в стоп-лист и после загрузки списка,
// а сообщение в соседней кампании он мог получить между импортом и своей
// очередью.
const inStopListSQL = `EXISTS (SELECT 1 FROM stop_list s
	WHERE s.amo_contact_id = r.amo_contact_id OR s.username = r.username)`

const alreadySentSQL = `EXISTS (SELECT 1 FROM recipients o
	WHERE (o.amo_contact_id = r.amo_contact_id OR o.username = r.username)
	  AND o.id <> r.id AND o.state IN ('sent', 'replied'))`

// Ручное касание закрывает человека для кампаний наравне с отправленной
// строкой: сообщение через tg_send в recipients не попадает, и без этого
// условия следующая кампания напишет ему второй раз. Направление одностороннее -
// журнал не пускает кампанию, но повторному tg_send не мешает, иначе диалог
// обрывался бы после первой реплики. failed не считается: при таком отказе
// сообщение заведомо не ушло.
//
// Ключ один - ник: contact_id ручному касанию взять неоткуда.
const touchedDirectSQL = `EXISTS (SELECT 1 FROM direct_messages d
	WHERE d.username = r.username AND d.state <> 'failed')`

// Причина отсева обязана отличать доставленное касание от неизвестного: pending
// значит «сообщение могло уйти», и отчёт не имеет права называть это «уже
// писали». Порог здесь не нужен и был бы враньём: свежий pending так же
// неизвестен, как старый. Тот же вопрос через checkPersonSQL отвечается этим же
// делением - два входа обязаны говорить про одного человека одно.
const sentDirectSQL = `EXISTS (SELECT 1 FROM direct_messages d
	WHERE d.username = r.username AND d.state = 'sent')`

// Снятые рубежом строки уходят в skipped, а не остаются planned: иначе очередь
// не опустеет никогда и кампания не закроется. Строка с живым claim не
// трогается - её прямо сейчас несёт другой процесс, и её судьбу решит
// финализация.
//
// Служебный адресат владельца не снимается за «уже получал» и за ручное
// касание - для него этого правила нет, - но стоп-лист снимает и его. Иначе
// такая строка висела бы planned навсегда: в отправку её не пускает
// sendableSQL, кампания не закрывается, а status называл бы простой
// queue_disqualified, то есть неверной причиной.
func skipDisqualifiedSQL(owners string) string {
	return `UPDATE recipients AS r
	SET state = 'skipped',
	    reason = CASE
	        WHEN ` + inStopListSQL + ` THEN '` + ReasonStopList + `'
	        WHEN ` + alreadySentSQL + ` THEN '` + ReasonAlreadySent + `'
	        WHEN ` + sentDirectSQL + ` THEN '` + ReasonDirectSent + `'
	        ELSE '` + ReasonDirectPending + `' END,
	    claimed_at = NULL
	WHERE r.campaign_id = $1 AND r.state = 'planned'
	  AND (r.claimed_at IS NULL OR r.claimed_at < $2)
	  AND (` + inStopListSQL + `
	       OR (r.username <> ALL(` + owners + `)
	           AND (` + alreadySentSQL + ` OR ` + touchedDirectSQL + `)))`
}

// Рубежа пустого имени в SQL больше нет: человек без имени получает письмо без
// обращения, и снимать его строку не за что (решение владельца 02.09.2026).

// bracedNameSQL - фигурная скобка в имени: она уедет в письмо как есть, а это
// тот же симптом, против которого стоит проверка шаблона.
const bracedNameSQL = `r.name ~ '[{}]'`

// skipBadNamesSQL снимает строки со скобкой в имени. Причину зовёт только
// шаблон с {имя}, поэтому запрос идёт под тем же условием, что и claim. Длина
// письма сюда не попадает - она зависит от шаблона и меряется в единицах
// UTF-16, чего SQL точно не выразит; её проверяет nameFault на взятой строке.
// Строка с живым claim не трогается - её несёт другой процесс.
//
// Пустое имя рубежом больше не является: письмо уходит без обращения
// (решение владельца 02.09.2026, восстановлено 07.09.2026).
const skipBadNamesSQL = `UPDATE recipients AS r SET state = 'skipped', claimed_at = NULL,
	    reason = '` + ReasonNameBraces + `'
	WHERE r.campaign_id = $1 AND r.state = 'planned'
	  AND ` + bracedNameSQL + `
	  AND (r.claimed_at IS NULL OR r.claimed_at < $2)`

// Условие отсева повторено в выборке намеренно: рубеж не должен зависеть от
// того, отработал ли отсев выше. Имя идёт тем же порядком: $4 говорит, зовёт ли
// шаблон активной кампании имя (самого шаблона в запросе не видно), и строка без
// имени под таким шаблоном не выбирается, даже если отсев её почему-то не снял.
//
// Строка, однажды взятая аккаунтом ($3), остаётся за ним навсегда - и под
// протухшим claim, и после возврата в очередь: повтор доставки держится на
// random_id, а Telegram дедуплицирует его в пределах одного аккаунта. Иначе
// временный отказ, при котором сообщение могло уйти, отдал бы строку соседнему
// номеру и человек получил бы второе сообщение. Строка мёртвого чужого
// аккаунта ждёт человека, и сколько таких строк - видно в status.
// sendableSQL - строка, которую рубеж пропускает: ни стоп-листа, ни второго
// сообщения, ни ручного касания, ни негодного имени под шаблоном с {имя} ($4).
// Один текст на выборку и на счёт очереди в отчёте: разъехавшись, они дали бы
// «очередь движется» там, где рубеж снимает всё до последней строки.
//
// Служебный адресат владельца проходит «уже получал» и «писали руками», но не
// стоп-лист и не негодное имя: снимается правило «одно сообщение», а не рубежи,
// которые держат человека вообще.
func sendableSQL(owners string) string {
	return `(NOT ` + inStopListSQL + `
	           AND (r.username = ANY(` + owners + `)
	                OR NOT (` + alreadySentSQL + ` OR ` + touchedDirectSQL + `))
	           AND NOT ($4 AND ` + bracedNameSQL + `))`
}

// claimSQL - выборка следующей строки. $5 - режим «только служебные адресаты»:
// его включает закрытое окно или исчерпанный потолок, и тогда очередь движется
// одними проверочными строками владельца.
func claimSQL(owners string) string {
	return `SELECT r.id, r.campaign_id, r.amo_contact_id, r.amo_lead_id, r.name, r.username, r.random_id
	FROM recipients r
	WHERE r.campaign_id = $1 AND r.state = 'planned'
	  AND (r.claimed_at IS NULL OR r.claimed_at < $2)
	  AND (r.account_id IS NULL OR r.account_id = $3)
	  AND ($5 = false OR r.username = ANY(` + owners + `))
	  AND ` + sendableSQL(owners) + `
	ORDER BY r.id
	LIMIT 1
	FOR UPDATE OF r SKIP LOCKED`
}

// idleQueueSQL - что осталось в очереди активной кампании, сколько из этого
// вообще может уйти и сколько доступно нашему аккаунту прямо сейчас. Отсев
// повторяется целиком, а не грубо: строка, которую снимет рубеж, доступной не
// является, и отчёт, посчитавший её, обещает движение очереди, которого не
// будет. Длина письма сюда не попадает - она зависит от шаблона и меряется в
// UTF-16, чего SQL не выразит; такие строки снимает nameFault на витке выборки.
func idleQueueSQL(owners string) string {
	return `SELECT count(*),
	count(*) FILTER (WHERE ` + sendableSQL(owners) + `),
	count(*) FILTER (WHERE ` + sendableSQL(owners) + `
	                   AND (r.claimed_at IS NULL OR r.claimed_at < $2)
	                   AND (r.account_id IS NULL OR r.account_id = $3))
	FROM recipients r
	WHERE r.campaign_id = $1 AND r.state = 'planned'`
}

// usedTodaySQL - дневной расход аккаунта целиком: кампанийные отправки плюс
// холодные ручные касания за сегодня ($1 - начало суток, $2 - граница протухания
// claim). Потолок общий на оба канала: инвариант говорит «дневной потолок держит
// рассылку и первое касание незнакомца», и односторонний счёт давал бы два
// потолка за сутки с одного номера.
//
// account - как назван аккаунт в запросе: $3 у рубежа, a.id у отчёта. Сумма одна
// на оба, и разъехаться им теперь нечем.
//
// Сообщения служебным адресатам в расход не входят ни в одном из подзапросов:
// потолок стоит ради незнакомых людей, и проверка рассылки на аккаунт владельца
// не имеет права его тратить. Сколько ушло им - показывает status отдельной
// строкой.
func usedTodaySQL(account, owners string) string {
	return `(SELECT count(*) FROM recipients r
		WHERE r.account_id = ` + account + ` AND r.username <> ALL(` + owners + `) AND ` + dailyUsedSQL + `)
		+ ` + coldDirectSQL(account, owners)
}

// coldDirectSQL - холодные ручные касания аккаунта за сегодня ($1 - начало
// суток). Входят в потолок наравне с рассылкой, поэтому живут внутри
// usedTodaySQL; отдельно отчёт показывает их той же строкой, а не своей.
func coldDirectSQL(account, owners string) string {
	return `(SELECT count(*) FROM direct_messages d
		WHERE d.account_id = ` + account + ` AND d.username <> ALL(` + owners + `)
		  AND d.cold AND d.state <> 'failed' AND d.created_at >= $1)`
}

// dailyUsedSQL - кампанийная часть расхода: отправленное с начала суток ($1)
// плюс живые claims ($2 - граница протухания). Один текст на рубеж потолка и на
// отчёт: разъехавшись, status показывал бы «29 из 30» там, где ClaimNext уже
// отказывает.
//
// Состояние строки в живом claim не проверяется намеренно: строка, взятая
// отправщиком, остаётся в счёте, чем бы её ни сделал параллельный ответ
// человека. Условие state = 'planned' открывало бы дыру в потолке на каждом
// replied до финализации.
const dailyUsedSQL = `(r.sent_at >= $1 OR (r.claimed_at >= $2 AND r.sent_at IS NULL))`

// countedRetrySQL - временный отказ с расходом попытки: на исчерпании бюджета
// строка уходит из очереди, иначе стабильная неопознанная ошибка держит её
// голову. Отменённая кампания разбирается здесь же, как и в обычном retry.
const countedRetrySQL = `UPDATE recipients AS r
	SET attempts = r.attempts + 1,
	    state = CASE
	        WHEN c.status = 'cancelled' THEN 'cancelled'
	        WHEN r.attempts + 1 >= $4 THEN 'undelivered'
	        ELSE r.state END,
	    reason = CASE
	        WHEN c.status = 'cancelled' THEN $3
	        WHEN r.attempts + 1 >= $4 THEN $5
	        ELSE $2 END,
	    claimed_at = NULL
	FROM campaigns c
	WHERE r.id = $1 AND r.state = 'planned' AND c.id = r.campaign_id
	RETURNING r.state, r.attempts`
