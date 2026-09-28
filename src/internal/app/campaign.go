package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Service - доменные операции кампании и очереди поверх пула. Состояния в
// памяти нет вовсе: счётчики, паузы и стопы переживают перезапуск только в БД.
type Service struct {
	pool *pgxpool.Pool
	cfg  Config

	// owners - служебные адресаты множеством: единственный источник ответа
	// isOwner.
	owners map[string]bool

	// Запросы с рубежами собираются один раз здесь, потому что список служебных
	// адресатов входит в них SQL-литералом, а не параметром. Параметр-массив не
	// годится: nil-слайс pgx кодирует как NULL, и `x <> ALL(NULL)` дал бы NULL -
	// потолок считался бы нулём, а отсев вешал бы строки в planned. Литерал
	// безопасен: форму ника проверил LoadConfig.
	accountUsedSQL      string
	claimSQL            string
	idleQueueSQL        string
	skipDisqualifiedSQL string
	accountsReportSQL   string

	// Now - источник времени. Поле, а не time.Now по месту: окно отправки и
	// протухание claim иначе непроверяемы тестом.
	Now func() time.Time
}

// NewService собирает сервис на открытом пуле.
func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	owners := ownersLiteral(cfg.OwnerAccounts)
	s := &Service{
		pool:                pool,
		cfg:                 cfg,
		owners:              make(map[string]bool, len(cfg.OwnerAccounts)),
		accountUsedSQL:      `SELECT ` + usedTodaySQL(`$3`, owners),
		claimSQL:            claimSQL(owners),
		idleQueueSQL:        idleQueueSQL(owners),
		skipDisqualifiedSQL: skipDisqualifiedSQL(owners),
		accountsReportSQL:   accountsReportSQL(owners),
		Now:                 time.Now,
	}
	// Тот же отбор, что в ownersLiteral: разъехавшись, Go и SQL сняли бы рубежи
	// с разных людей.
	for _, name := range cfg.OwnerAccounts {
		if telegram.IsUsername(name) {
			s.owners[name] = true
		}
	}
	return s
}

// isOwner - служебный ли это адресат: единственная точка проверки в Go. Ник
// нормализуется так же, как при импорте списка, иначе «@Smirnov» и «smirnov»
// разошлись бы на рубежах.
func (s *Service) isOwner(username string) bool {
	return s.owners[normalizeName(username)]
}

// ownerNames - служебные адресаты для отчёта, в неизменном порядке. Отчёт
// называет тех, кого рубежи действительно не держат, поэтому берёт их из того
// же множества, что и isOwner, а не из строки настроек.
func (s *Service) ownerNames() []string {
	return slices.Sorted(maps.Keys(s.owners))
}

// DigestRecipients - кому уходит вечерняя сводка: DIGEST_TO, если он задан,
// иначе все служебные адресаты, как было до разделения списков. Списки разные,
// потому что DEV_ALLOW_LIST снимает рубежи для проверок рассылки и сузить его
// до двух получателей отчёта нельзя.
func (s *Service) DigestRecipients() []string {
	if len(s.cfg.DigestTo) > 0 {
		return s.cfg.DigestTo
	}
	return s.ownerNames()
}

// ownersLiteral - список служебных адресатов SQL-литералом для сборки
// запросов; пустой список даёт ARRAY[]::text[], на котором `<> ALL` истинно, а
// `= ANY` ложно, то есть поведение сервиса неотличимо от прежнего.
//
// Форма ника проверяется здесь повторно после LoadConfig: замок стоит там, где
// строка склеивается с запросом, а не за двести строк от него. NewService
// принимает Config, собранный кем угодно, и кавычка в нике переписала бы WHERE.
func ownersLiteral(list []string) string {
	quoted := make([]string, 0, len(list))
	for _, name := range list {
		if !telegram.IsUsername(name) {
			continue
		}
		quoted = append(quoted, `'`+name+`'`)
	}
	if len(quoted) == 0 {
		return `ARRAY[]::text[]`
	}
	return `ARRAY[` + strings.Join(quoted, ",") + `]::text[]`
}

// maxBatch - потолок пачки add_recipients. Причина не в базе, а в клиенте:
// большой tool-аргумент у Клода обрезается молча.
const maxBatch = 50

// Причины отсева и снятия строки. Список закрытый: новая строка здесь -
// новое решение сервиса, а не новый текст в отчёте.
const (
	ReasonBadContact   = "bad_contact"    // нет идентификатора контакта amoCRM
	ReasonNoUsername   = "no_username"    // ник пустой или непохож на ник
	ReasonNoName       = "no_name"        // больше не выставляется, встречается в кампаниях до 07.09.2026
	ReasonTextTooLong  = "text_too_long"  // письмо с его именем длиннее предела Telegram
	ReasonNameBraces   = "name_braces"    // в имени фигурная скобка: она уедет в письмо
	ReasonDuplicate    = "duplicate"      // контакт или ник уже в этой кампании
	ReasonStopList     = "stop_list"      // человек в стоп-листе
	ReasonAlreadySent  = "already_sent"   // получал сообщение в любой кампании
	ReasonCampaignGone = "campaign_ended" // кампания отменена вместе с очередью

	// Ручное касание через tg_send закрывает человека для кампаний так же, как
	// отправленная строка. Причин две, потому что журнал знает про доставку
	// не всегда: pending значит «сообщение могло уйти», и отчёт отсева не
	// имеет права выдавать это за состоявшуюся отправку.
	ReasonDirectSent    = "direct_sent"    // писали руками, доставка подтверждена
	ReasonDirectPending = "direct_pending" // писали руками, доставка неизвестна

	// Человек написал сам до того, как до него дошла очередь: шаблон поверх
	// начатого им разговора не уходит, но ответом на письмо это не считается.
	ReasonWroteFirst = "wrote_first"
)

// Отказы по состоянию кампаний. Их пять, а не один: «подходящей кампании нет»
// одинаково звучало и там, где создавать нечего, и там, где кампания жива, -
// и совет «создайте черновик» уводил от того, что происходит.
var (
	ErrBatchTooBig     = errors.New("batch is limited to 50 people")
	ErrNoDraft         = errors.New("no draft campaign to add recipients to")
	ErrNothingToStart  = errors.New("no draft and no paused campaign to start")
	ErrNothingToStop   = errors.New("no active campaign to pause")
	ErrNothingToCancel = errors.New("no campaign to cancel")
	ErrActiveCampaign  = errors.New("another campaign is already running")
	ErrManyCampaigns   = errors.New("more than one campaign to start, cancel the extra ones")
	ErrEmptyDraft      = errors.New("draft has no eligible recipients")
	ErrNotDraft        = errors.New("recipients can be added to a draft campaign only")

	ErrUnknownPlaceholder = errors.New("template has placeholders the service does not know")
	ErrTemplateTooLong    = errors.New("rendered message does not fit the telegram limit")
)

// badTemplate - шаблон с неразобранными скобками. Куски текста вокруг них едут
// внутри отказа: без них человек ищет опечатку глазами по всему шаблону. total -
// сколько мест нашлось всего: показываются не все.
// stored - шаблон уже лежит в БД: совет тогда другой, потому что шаблон
// существующей кампании не правит ни один инструмент.
type badTemplate struct {
	spots  []string
	total  int
	stored bool
}

func (e *badTemplate) Error() string {
	return fmt.Sprintf("%s: %d spots, %s", ErrUnknownPlaceholder, e.total, strings.Join(e.spots, " | "))
}

func (e *badTemplate) Unwrap() error { return ErrUnknownPlaceholder }

// longTemplate - шаблон, не влезающий в сообщение вместе с именем. Числа едут
// внутри: без них совет «сократите» не говорит, насколько.
type longTemplate struct{ used, limit, budget int }

func (e *longTemplate) Error() string {
	return fmt.Sprintf("%s: %d of %d", ErrTemplateTooLong, e.used, e.limit)
}

func (e *longTemplate) Unwrap() error { return ErrTemplateTooLong }

// nearCampaign - живая кампания рядом с отказом. Едет внутри ошибки, потому что
// от неё зависит следующий шаг: «черновика нет» на пустой системе и «черновика
// нет» при идущей кампании разрешаются по-разному.
type nearCampaign struct {
	err    error
	id     int64
	title  string
	status string
	// draft - черновик рядом с найденной кампанией; 0, если его нет. Нужен
	// совету: start берёт черновик раньше паузы, и cancel бьёт по нему раньше
	// паузы, поэтому «продолжите start» при живом черновике отправляет агента не
	// туда.
	draft int64
	// pauses - сколько кампаний стоит на паузе. Тоже нужен совету: при двух
	// паузах без черновика start отказывает как ErrManyCampaigns, и обещание
	// «продолжит start» было бы враньём.
	pauses int
}

func (e *nearCampaign) Error() string {
	return fmt.Sprintf("%s: campaign %d %q (%s)", e.err, e.id, e.title, e.status)
}

func (e *nearCampaign) Unwrap() error { return e.err }

// rowQuerier - общая часть пула и транзакции: соседнюю кампанию ищут и снаружи
// транзакции (add_recipients), и внутри неё (stop).
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withNear навешивает на отказ первую кампанию в одном из названных состояний;
// порядок состояний задаёт приоритет ответа. Такой кампании нет - отказ уходит
// как есть.
func withNear(ctx context.Context, q rowQuerier, err error, statuses ...string) error {
	near := nearCampaign{err: err}
	scanErr := q.QueryRow(ctx,
		`SELECT c.id, c.title, c.status,
		        COALESCE((SELECT d.id FROM campaigns d
		                  WHERE d.status = 'draft' AND d.id <> c.id ORDER BY d.id LIMIT 1), 0),
		        (SELECT count(*) FROM campaigns p WHERE p.status = 'stopped')
		 FROM campaigns c WHERE c.status = ANY($1)
		 ORDER BY array_position($1, c.status), c.id LIMIT 1`,
		statuses).Scan(&near.id, &near.title, &near.status, &near.draft, &near.pauses)
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows):
		return err
	case scanErr != nil:
		return fmt.Errorf("find campaign near %w: %v", err, scanErr)
	default:
		return &near
	}
}

// Person - строка входного списка. Список собирает агент своим доступом к
// amoCRM: сам сервис в CRM не ходит.
type Person struct {
	ContactID int64
	LeadID    int64
	Name      string
	Username  string
}

// BatchReport - итог пачки: сколько принято и сколько отсеяно по каждой
// причине. Отсев - не ошибка: владелец сверяет отчёт перед стартом, а повтор
// пачки после таймаута обязан пройти без падения.
type BatchReport struct {
	Accepted int
	Skipped  map[string]int
}

// LoadCampaign создаёт новый черновик, отменяя прежний одной транзакцией.
// Гонку двух вызовов разрешает частичный уникальный индекс на draft: второй
// вызов падает на индексе, а не оставляет два черновика.
func (s *Service) LoadCampaign(ctx context.Context, template, title string) (int64, error) {
	template = strings.TrimSpace(template)
	title = strings.TrimSpace(title)
	if template == "" || title == "" {
		return 0, errors.New("campaign title and template must not be empty")
	}
	// Шаблон проверяется на входе, а не при отправке: кампания собирается один
	// раз и уходит сотням, и опечатка в подстановке или лишняя тысяча символов
	// обязаны отбиться до того, как собран список.
	if spots, total := badBraces(template); total > 0 {
		return 0, &badTemplate{spots: spots, total: total}
	}
	if used, budget := renderedLen(template); used > maxTextLen {
		return 0, &longTemplate{used: used, limit: maxTextLen, budget: budget}
	}

	now := s.Now()
	var id int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		previous, err := findCampaign(ctx, tx, "draft")
		if err != nil {
			return err
		}
		if previous != 0 {
			if err := cancelCampaign(ctx, tx, previous, now, s.staleBefore(now)); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx,
			`INSERT INTO campaigns (title, template_text, status, created_at)
			 VALUES ($1, $2, 'draft', $3) RETURNING id`,
			title, template, now).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("load campaign: %w", err)
	}
	return id, nil
}

// AddRecipients добавляет пачку в черновик и возвращает отчёт отсева.
// Отсеиваются: кривой контакт, непригодный ник, дубль внутри пачки и против
// уже вставленных, стоп-лист, «получал в любой кампании». Повтор той же пачки
// после таймаута идемпотентен - все записи уходят в дубли.
func (s *Service) AddRecipients(ctx context.Context, campaignID int64, people []Person) (BatchReport, error) {
	report := BatchReport{Skipped: map[string]int{}}
	if len(people) > maxBatch {
		return report, fmt.Errorf("%w: got %d", ErrBatchTooBig, len(people))
	}

	// Отсев по форме - до транзакции: база в нём не участвует.
	type candidate struct {
		person   Person
		username string
		name     string
	}
	candidates := make([]candidate, 0, len(people))
	seenContacts := make(map[int64]bool, len(people))
	seenNames := make(map[string]bool, len(people))
	for _, person := range people {
		if person.ContactID <= 0 {
			report.Skipped[ReasonBadContact]++
			continue
		}
		username := normalizeUsername(person.Username)
		if username == "" {
			report.Skipped[ReasonNoUsername]++
			continue
		}
		if seenContacts[person.ContactID] || seenNames[username] {
			report.Skipped[ReasonDuplicate]++
			continue
		}
		seenContacts[person.ContactID] = true
		seenNames[username] = true
		candidates = append(candidates, candidate{person: person, username: username, name: cleanName(person.Name)})
	}

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status, template string
		err := tx.QueryRow(ctx,
			`SELECT status, template_text FROM campaigns WHERE id = $1 FOR UPDATE`,
			campaignID).Scan(&status, &template)
		if err != nil {
			return fmt.Errorf("lock campaign: %w", err)
		}
		if status != "draft" {
			return fmt.Errorf("%w: campaign is %s", ErrNotDraft, status)
		}
		// Сломанный шаблон отбивается здесь же: иначе в негодный черновик можно
		// набить пятьсот человек и узнать о непригодности только на start.
		if spots, total := badBraces(template); total > 0 {
			return &badTemplate{spots: spots, total: total, stored: true}
		}

		for _, item := range candidates {
			var inStopList, alreadySent, present, directSent, directPending bool
			err := tx.QueryRow(ctx, checkPersonSQL, item.person.ContactID, item.username, campaignID).
				Scan(&inStopList, &alreadySent, &present, &directSent, &directPending)
			if err != nil {
				return fmt.Errorf("check person %d: %w", item.person.ContactID, err)
			}
			// Служебному адресату владельца пишут повторно намеренно: правило
			// «один человек - одно сообщение» стоит ради незнакомых людей.
			// Стоп-лист и дубль внутри кампании остаются и на нём.
			if s.isOwner(item.username) {
				alreadySent, directSent, directPending = false, false, false
			}
			switch {
			case present:
				report.Skipped[ReasonDuplicate]++
				continue
			case inStopList:
				report.Skipped[ReasonStopList]++
				continue
			case alreadySent:
				report.Skipped[ReasonAlreadySent]++
				continue
			case directSent:
				report.Skipped[ReasonDirectSent]++
				continue
			case directPending:
				report.Skipped[ReasonDirectPending]++
				continue
			}

			// Рубежи имени стоят после общих: человека из стоп-листа отчёт обязан
			// назвать стоп-листом (инвариант 4), а не отсутствием имени - иначе
			// самый сильный рубеж в отчёте импорта не виден вовсе.
			if fault := nameFault(template, item.name); fault != "" {
				report.Skipped[fault]++
				continue
			}

			// ON CONFLICT держит повтор пачки, обогнавший проверку выше:
			// вставка не должна ронять всю загрузку из-за одного дубля.
			tag, err := tx.Exec(ctx,
				`INSERT INTO recipients (campaign_id, amo_contact_id, amo_lead_id, name, username, state, random_id)
				 VALUES ($1, $2, $3, $4, $5, 'planned', $6)
				 ON CONFLICT (campaign_id, amo_contact_id) DO NOTHING`,
				campaignID, item.person.ContactID, item.person.LeadID,
				item.name, item.username, newRandomID())
			if err != nil {
				return fmt.Errorf("insert recipient %d: %w", item.person.ContactID, err)
			}
			if tag.RowsAffected() == 0 {
				report.Skipped[ReasonDuplicate]++
				continue
			}
			report.Accepted++
		}
		return nil
	})
	if err != nil {
		return BatchReport{Skipped: map[string]int{}}, fmt.Errorf("add recipients: %w", err)
	}
	return report, nil
}

// checkPersonSQL отвечает рубежами импорта одним заходом: стоп-лист, «получал в
// любой кампании», «уже в этой кампании» и ручное касание через tg_send.
// Человек узнаётся и по контакту amoCRM, и по нику: один человек заводится в CRM
// дважды. У журнала ключ только ник - contact_id ручному касанию взять неоткуда.
//
// Касание разложено на два флага, а не на один: pending и sent закрывают
// человека одинаково, но называются в отчёте по-разному, и вывести одно из
// другого нечем. Порог протухания здесь не нужен - любой pending значит
// «доставка неизвестна», и импорт обязан сказать именно это.
const checkPersonSQL = `SELECT
	EXISTS (SELECT 1 FROM stop_list WHERE amo_contact_id = $1 OR username = $2),
	EXISTS (SELECT 1 FROM recipients
	        WHERE (amo_contact_id = $1 OR username = $2) AND state IN ('sent', 'replied')),
	EXISTS (SELECT 1 FROM recipients
	        WHERE campaign_id = $3 AND (amo_contact_id = $1 OR username = $2)),
	EXISTS (SELECT 1 FROM direct_messages WHERE username = $2 AND state = 'sent'),
	EXISTS (SELECT 1 FROM direct_messages WHERE username = $2 AND state = 'pending')`

// StartCampaign переводит черновик или приостановленную кампанию в active.
// Отдельного инструмента продолжения нет: start работает и из draft, и из
// stopped. Когда есть и черновик, и пауза, запускается черновик: новый список
// свежее возобновления, а пауза остаётся stopped и продолжается следующим
// start. Пустой черновик не запускается - отправлять некому.
func (s *Service) StartCampaign(ctx context.Context) (int64, error) {
	now := s.Now()
	var id int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Активная кампания в системе одна, и держит это частичный индекс.
		// Проверка стоит до запуска, чтобы оператор получил внятный отказ, а не
		// текст нарушения уникального индекса от Postgres.
		active, err := findCampaign(ctx, tx, "active")
		if err != nil {
			return err
		}
		if active != 0 {
			return ErrActiveCampaign
		}

		type row struct {
			id       int64
			status   string
			template string
		}
		rows, err := tx.Query(ctx,
			`SELECT id, status, template_text FROM campaigns WHERE status IN ('draft', 'stopped') ORDER BY id FOR UPDATE`)
		if err != nil {
			return fmt.Errorf("select campaigns: %w", err)
		}
		var found []row
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.id, &item.status, &item.template); err != nil {
				rows.Close()
				return fmt.Errorf("scan campaign: %w", err)
			}
			found = append(found, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read campaigns: %w", err)
		}

		// Черновик в системе один (частичный индекс), пауз может быть несколько:
		// черновик побеждает, а неразличимые паузы разбирает человек.
		target := row{}
		for _, item := range found {
			if item.status == "draft" {
				target = item
				break
			}
		}
		switch {
		case len(found) == 0:
			return ErrNothingToStart
		case target.id != 0:
		case len(found) == 1:
			target = found[0]
		default:
			return ErrManyCampaigns
		}

		// Шаблон перепроверяется и здесь: кампания могла быть собрана прошлой
		// версией, где «{{имя}}» проходил вход. Отказ на старте - ради человека,
		// сам рубеж стоит в ClaimNext: туда приходит и кампания, ставшая active до
		// обновления, а через start она больше не проходит.
		if spots, total := badBraces(target.template); total > 0 {
			return &badTemplate{spots: spots, total: total, stored: true}
		}

		if target.status == "draft" {
			var planned int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM recipients WHERE campaign_id = $1 AND state = 'planned'`,
				target.id).Scan(&planned); err != nil {
				return fmt.Errorf("count planned: %w", err)
			}
			if planned == 0 {
				return ErrEmptyDraft
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE campaigns SET status = 'active', started_at = COALESCE(started_at, $2) WHERE id = $1`,
			target.id, now); err != nil {
			return fmt.Errorf("activate campaign: %w", err)
		}
		id = target.id
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("start campaign: %w", err)
	}
	return id, nil
}

// StopCampaign ставит кампанию на паузу или отменяет её и возвращает id
// затронутой кампании. Пауза возможна только из active; отмена бьёт по одной
// кампании в порядке active, draft, stopped. Черновик идёт раньше паузы, чтобы
// пара «пауза плюс черновик» была разрешима инструментами: первая отмена
// снимает черновик, вторая - паузу, и ни одна не уносит обе.
func (s *Service) StopCampaign(ctx context.Context, cancel bool) (int64, error) {
	now := s.Now()
	var id int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if cancel {
			var target int64
			for _, status := range []string{"active", "draft", "stopped"} {
				found, err := findCampaign(ctx, tx, status)
				if err != nil {
					return err
				}
				if found != 0 {
					target = found
					break
				}
			}
			if target == 0 {
				return ErrNothingToCancel
			}
			if err := cancelCampaign(ctx, tx, target, now, s.staleBefore(now)); err != nil {
				return err
			}
			id = target
			return nil
		}
		err := tx.QueryRow(ctx,
			`UPDATE campaigns SET status = 'stopped' WHERE status = 'active' RETURNING id`).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Паузу и черновик называет отказ: повтор stop на уже приостановленной
			// кампании иначе читается как «кампании больше нет», хотя её продолжает
			// start.
			return withNear(ctx, tx, ErrNothingToStop, "stopped", "draft")
		}
		if err != nil {
			return fmt.Errorf("pause campaign: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("stop campaign: %w", err)
	}
	return id, nil
}

// FinishIfEmpty закрывает активную кампанию, когда planned-строк не осталось.
// Взятая отправщиком строка тоже planned: кампания не закрывается, пока хоть
// одна строка в работе.
func (s *Service) FinishIfEmpty(ctx context.Context) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE campaigns SET status = 'done', finished_at = $1
		 WHERE status = 'active'
		   AND NOT EXISTS (SELECT 1 FROM recipients r WHERE r.campaign_id = campaigns.id AND r.state = 'planned')`,
		s.Now())
	if err != nil {
		return false, fmt.Errorf("finish campaign: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// findCampaign отдаёт id первой кампании в названном состоянии под блокировкой
// строки; 0 - такой кампании нет. Блокировка нужна отмене и старту: решение
// принимается по тому же состоянию, которое потом меняется.
func findCampaign(ctx context.Context, tx pgx.Tx, status string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM campaigns WHERE status = $1 ORDER BY id LIMIT 1 FOR UPDATE`,
		status).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find %s campaign: %w", status, err)
	}
	return id, nil
}

// cancelCampaign отменяет одну кампанию вместе с её очередью. Строки снимаются
// первыми: после смены статуса кампании их уже не найти по этому условию.
//
// Строка с живым claim не трогается: её прямо сейчас несёт отправщик, и отмена
// её состояния потеряла бы факт отправки - финализация такую строку уже не
// нашла бы. Её судьбу решает FinishClaim: доставленная уходит в sent, а
// временный отказ по отменённой кампании - в cancelled.
func cancelCampaign(ctx context.Context, tx pgx.Tx, campaignID int64, now, stale time.Time) error {
	if _, err := tx.Exec(ctx,
		`UPDATE recipients SET state = 'cancelled', reason = $3, claimed_at = NULL
		 WHERE campaign_id = $1 AND state = 'planned'
		   AND (claimed_at IS NULL OR claimed_at < $2)`,
		campaignID, stale, ReasonCampaignGone); err != nil {
		return fmt.Errorf("cancel recipients: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE campaigns SET status = 'cancelled', finished_at = $2 WHERE id = $1`,
		campaignID, now); err != nil {
		return fmt.Errorf("cancel campaign %d: %w", campaignID, err)
	}
	return nil
}

// staleBefore - граница протухания claim: раньше неё строку никто уже не несёт.
func (s *Service) staleBefore(now time.Time) time.Time {
	return now.Add(-s.cfg.ClaimStaleAfter)
}

// namePlaceholder - единственная подстановка шаблона. Кампания без неё законна,
// но получатель без имени в кампанию с ней не попадает.
const namePlaceholder = "{имя}"

// nameBudget - запас символов на имя при проверке длины шаблона: имя заранее
// неизвестно, а узнать о непролезающем сообщении на старте рассылки поздно.
const nameBudget = 64

// namePlaceholderPattern - единственная подстановка, которую сервис понимает:
// регистр не важен, пробелы внутри скобок допустимы. «{Имя}» в начале
// предложения - естественная опечатка, и уходить в письмо литералом она не
// должна.
var namePlaceholderPattern = regexp.MustCompile(`(?i)\{\s*имя\s*\}`)

// RenderMessage подставляет имя в шаблон. Замена литеральная: имя приходит из
// CRM, и «$1» в нём не имеет права раскрыться в кусок совпадения.
//
// Пустое имя не отсеивает человека, а убирает обращение целиком: «{имя}, текст»
// становится «текст», «Привет, {имя}!» - «Привет!». Иначе письмо начиналось бы
// с запятой. Регистр первой буквы не правится намеренно: текст заказчика
// сервис не переписывает, а читаемость обеих форм проверяется при утверждении.
func RenderMessage(template, name string) string {
	clean := cleanName(name)
	if clean == "" {
		return dropName(template)
	}
	return namePlaceholderPattern.ReplaceAllLiteralString(template, clean)
}

// namePlaceholderAfterComma и namePlaceholderBeforeComma - подстановка вместе с
// прилипшей к ней запятой. Разделены на две замены, потому что запятая стоит с
// той стороны, с какой её поставил автор шаблона, и снимать надо именно её.
var (
	namePlaceholderAfterComma  = regexp.MustCompile(`(?i),[ \t]*\{\s*имя\s*\}`)
	namePlaceholderBeforeComma = regexp.MustCompile(`(?i)\{\s*имя\s*\}[ \t]*,[ \t]*`)
)

// dropName убирает обращение из шаблона, когда имени нет.
func dropName(template string) string {
	text := namePlaceholderAfterComma.ReplaceAllLiteralString(template, "")
	text = namePlaceholderBeforeComma.ReplaceAllLiteralString(text, "")
	text = namePlaceholderPattern.ReplaceAllLiteralString(text, "")
	return strings.TrimSpace(text)
}

// hasName отвечает, зовёт ли шаблон имя получателя.
func hasName(template string) bool {
	return namePlaceholderPattern.MatchString(template)
}

// badBraces - места, где сервис не разобрал фигурную скобку. Проверяется каждая
// скобка, а не только правильно спаренные: «{{имя}}» - обычная опечатка из
// Jinja, и подстановка внутрь внешней пары дала бы человеку «Привет, {Пётр}!», а
// незакрытое «{имя!» уехало бы в письмо литералом. После разбора известных
// подстановок скобок в шаблоне остаться не должно ни одной.
// Возвращает первые maxSpots мест и общее их число: отказ, показавший три места
// из пяти, читается как полный список, и человек правит три, получает отказ
// снова и принимает его за новый дефект.
func badBraces(template string) (spots []string, total int) {
	known := namePlaceholderPattern.FindAllStringIndex(template, -1)
	runes := []rune(template)
	// shown - докуда шаблон уже показан: соседние скобки одной опечатки («{{»)
	// дают одно место в списке, а не два перекрывающихся.
	shown, pos := 0, -1
	for at, symbol := range template {
		pos++
		if symbol != '{' && symbol != '}' || covered(known, at) || pos < shown {
			continue
		}
		total++
		shown = min(pos+16, len(runes))
		if len(spots) < maxSpots {
			spots = append(spots, string(runes[max(pos-8, 0):shown]))
		}
	}
	return spots, total
}

// maxSpots - сколько мест показывает отказ: список длиннее человек всё равно
// правит по одному, а ответ инструмента он вытесняет.
const maxSpots = 3

// covered - попала ли скобка внутрь распознанной подстановки.
func covered(known [][]int, at int) bool {
	for _, span := range known {
		if at >= span[0] && at < span[1] {
			return true
		}
	}
	return false
}

// nameFault - чем имя негодно для этого шаблона: пусто, со скобкой или даёт
// письмо длиннее предела. Пустая строка - имя годится. Один ответ на импорт и на
// очередь: рубеж обязан говорить одинаково в обоих местах, а порядок причин -
// определять, что увидит человек в отчёте.
//
// Скобка в имени проверяется только там, где шаблон имя подставляет: без {имя}
// оно в письмо не входит ни одним символом, и отсеивать человека не за что.
// Пустое имя неисправностью не считается: решение владельца 02.09.2026 -
// человек без годного имени остаётся в списке, и письмо уходит без обращения.
// Подстановку убирает RenderMessage.
func nameFault(template, name string) string {
	if hasName(template) {
		switch {
		// Имя приходит из CRM и идёт в текст как есть: разметку сервис не режет
		// намеренно, но фигурная скобка в имени - тот же симптом, против которого
		// стоит проверка шаблона, вплоть до имени «{имя}».
		case strings.ContainsAny(name, "{}"):
			return ReasonNameBraces
		}
	}
	// Запас на имя в load_campaign - грубая оценка на входе, а настоящее имя
	// известно здесь: длинное имя даёт письмо длиннее предела, и Telegram отверг
	// бы его на отправке, когда список уже собран.
	if telegramLen(RenderMessage(template, name)) > maxTextLen {
		return ReasonTextTooLong
	}
	return ""
}

// telegramLen - длина сообщения так, как её меряет Telegram: в единицах UTF-16,
// где символ вне BMP (эмодзи, редкие иероглифы) идёт за два. Счёт рунами
// занижает длину, и письмо с сотней эмодзи проходило проверку, а на отправке
// получало MESSAGE_TOO_LONG, когда список уже собран.
func telegramLen(text string) int {
	length := 0
	for _, symbol := range text {
		length++
		if symbol > 0xFFFF {
			length++
		}
	}
	return length
}

// renderedLen - длина того, что реально уйдёт человеку, и заложенный в неё
// запас на имена: по nameBudget на каждую подстановку, ноль - если подстановок
// нет. Настоящее имя известно только в очереди, а отказывать надо на входе,
// поэтому запас возвращается наружу - текст отказа не имеет права называть
// запас, которого не считали.
func renderedLen(template string) (used, budget int) {
	used = telegramLen(RenderMessage(template, strings.Repeat("и", nameBudget)))
	return used, nameBudget * len(namePlaceholderPattern.FindAllStringIndex(template, -1))
}

// cleanName приводит имя из CRM к виду, пригодному для письма: переносы строк и
// повторяющиеся пробелы схлопываются, «Иван\n\nПетров» в тексте письма выглядит
// как сбой. Разметка не режется и не экранируется намеренно: отправка идёт
// plain-текстом, и <b> в имени - визуальный шум, а не инъекция. Пустая строка -
// отсев.
func cleanName(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

// normalizeUsername приводит ник к виду, в котором он лежит в БД: без пробелов,
// "@" и обёртки t.me, в нижнем регистре. Форму ника проверяет telegram.IsUsername:
// кириллица, телефон и огрызок вроде "ivan" сюда не проходят - лид без
// пригодного ника в рассылку не попадает, догадываться по имени запрещено.
// Пустая строка - отсев.
func normalizeUsername(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if index := strings.LastIndex(name, "t.me/"); index >= 0 {
		name = name[index+len("t.me/"):]
	}
	name = strings.TrimPrefix(name, "@")
	name = strings.TrimSuffix(strings.TrimSpace(name), "/")
	if !telegram.IsUsername(name) {
		return ""
	}
	return name
}

// newRandomID выдаёт random_id строки: Telegram дедуплицирует по нему, поэтому
// он генерируется один раз при вставке и переживает любой повтор доставки.
// Ноль исключён - Telegram трактует его как «без дедупликации».
func newRandomID() int64 {
	return rand.Int64N(math.MaxInt64-1) + 1
}
