package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// StatusReport - снимок состояния сервиса: единственный инструмент чтения
// отдаёт его целиком. Отдельных инструментов превью и построчного отчёта нет,
// поэтому здесь собрано всё, чем владелец проверяет ход кампании.
type StatusReport struct {
	// Campaign - nil, если кампаний нет вовсе.
	Campaign *CampaignSummary
	Accounts []AccountSummary
	// Owners - служебные адресаты, для которых сняты окно, потолок и правило
	// «одно сообщение». Отчёт называет их поимённо и всегда, а не только когда
	// им сегодня писали: снятый рубеж, о котором никто не узнал, равен его
	// отсутствию, а ошибка в DEV_ALLOW_LIST иначе видна только по ушедшему
	// сообщению.
	Owners []string
	// Preview - до трёх сообщений на реальных строках кампании: владелец видит
	// не шаблон, а то, что уйдёт человеку. Кампания на одного получателя не
	// падает - превью просто короче.
	Preview []MessagePreview
	// Rows заполняется только при full: построчный итог кампании.
	Rows []RecipientRow
	// Idle - почему очередь стоит на момент вызова. Кампания при этом остаётся
	// active, строки - в очереди, и без этой причины простой по окну, потолку или
	// сломанному шаблону виден только косвенно, по счётчикам.
	Idle Idle
	// Draft - подготовленный черновик, если он существует рядом с показанной
	// кампанией. Иначе владелец сверял бы список по чужой кампании.
	Draft *CampaignNote
	// Paused - кампания на паузе рядом с показанной: её продолжит start.
	Paused *CampaignNote
	// Held - строки, закреплённые за чужими аккаунтами: их не возьмёт никто,
	// кроме названного отправщика. Без этого счётчика такие строки висят
	// planned без причины и кампания не закрывается молча.
	Held []HeldRows
	// StuckDirect - ручные касания, зависшие в pending дольше протухания
	// claim. Худший исход диалоговых инструментов: такая строка закрывает
	// человека навсегда, а доставка неизвестна, и чинится она только руками.
	StuckDirect int
	// StuckClaims - строки очереди с протухшим claim: отправщик их взял и не
	// закрыл. Пока кампания активна, такую строку чистит очередь; у кампании на
	// паузе этого не происходит, и человек молча закрыт для ручной отправки.
	StuckClaims int
}

// CampaignNote - соседняя кампания в отчёте: сколько в ней строк.
type CampaignNote struct {
	ID   int64
	Rows int
}

// HeldRows - сколько строк ждут отправщика с этой меткой аккаунта.
type HeldRows struct {
	Label string
	Rows  int
}

// CampaignSummary - кампания и счётчики её строк по состояниям.
type CampaignSummary struct {
	ID     int64
	Title  string
	Status string
	Total  int
	States map[string]int
}

// AccountSummary - аккаунт с дневным расходом и активным стопом. Стоп виден в
// каждом ответе status: рубеж, о котором никто не узнал, равен его отсутствию.
type AccountSummary struct {
	Label string
	// UsedToday - весь дневной расход аккаунта тем же счётом, каким его считает
	// рубеж: usedTodaySQL, один текст на отчёт и на ClaimNext. Кампанийные
	// отправки, строки в работе и холодные касания руками уже сложены здесь.
	UsedToday int
	// DailyLimit - потолок из БД. ACCOUNT_DAILY_LIMIT задаёт его только при
	// создании строки аккаунта, поэтому отчёт показывает действующее число, а не
	// переменную окружения.
	DailyLimit int
	Stopped    bool
	StopReason string
	// ColdToday - разбивка UsedToday: сколько в нём первых касаний незнакомцев
	// руками. Человеку нужно видеть, чем израсходован потолок, а не только
	// сколько его осталось.
	ColdToday int
	// WarmToday - реплики в идущих диалогах за сегодня: в потолок не входят.
	WarmToday int
	// OwnerToday - сколько сообщений ушло сегодня служебным адресатам владельца,
	// кампанией и руками вместе. В потолок не входят, и без этой строки расход
	// на проверках рассылки не виден вовсе. Счёт тот же, каким считается сам
	// потолок: касание с неизвестной доставкой входит сюда так же, как
	// подтверждённое, иначе одно и то же сообщение считалось бы по-разному в
	// соседних строках отчёта.
	OwnerToday int
	// Idle - почему молчит этот аккаунт. Считается по каждому, а не только по
	// метке процесса с MCP: рассылку ведут несколько отправщиков, и молчание
	// одного из них - самый вероятный случай, который иначе не виден снаружи.
	Idle Idle
}

// MessagePreview - готовый текст сообщения конкретному получателю.
type MessagePreview struct {
	Username string
	Text     string
}

// RecipientRow - строка построчного итога.
type RecipientRow struct {
	Username string
	Name     string
	State    string
	Reason   string
}

// Status собирает снимок текущей кампании: активной, если она есть, иначе
// черновика. full добавляет построчный итог; всё остальное отдаётся всегда -
// счётчики, аккаунты со стопами и превью.
func (s *Service) Status(ctx context.Context, full bool) (StatusReport, error) {
	var id int64
	err := s.pool.QueryRow(ctx, currentCampaignSQL).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		accounts, err := s.accountSummaries(ctx)
		if err != nil {
			return StatusReport{}, err
		}
		stuck, err := s.stuckDirect(ctx)
		if err != nil {
			return StatusReport{}, err
		}
		claims, err := s.stuckClaims(ctx)
		if err != nil {
			return StatusReport{}, err
		}
		return StatusReport{Accounts: accounts, Owners: s.ownerNames(),
			StuckDirect: stuck, StuckClaims: claims, Idle: commonIdle(accounts)}, nil
	}
	if err != nil {
		return StatusReport{}, fmt.Errorf("status: find campaign: %w", err)
	}
	return s.StatusOf(ctx, id, full)
}

// StatusOf собирает снимок по названной кампании. Инструменты, которые только
// что изменили конкретную кампанию, отчитываются именно по ней: при живой
// активной кампании отчёт по «текущей» показал бы чужие счётчики и чужое превью.
func (s *Service) StatusOf(ctx context.Context, campaignID int64, full bool) (StatusReport, error) {
	var report StatusReport

	accounts, err := s.accountSummaries(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	report.Accounts = accounts
	report.Owners = s.ownerNames()

	report.Idle = commonIdle(accounts)

	if report.StuckDirect, err = s.stuckDirect(ctx); err != nil {
		return StatusReport{}, err
	}
	if report.StuckClaims, err = s.stuckClaims(ctx); err != nil {
		return StatusReport{}, err
	}

	summary := CampaignSummary{ID: campaignID}
	var template string
	err = s.pool.QueryRow(ctx, `SELECT title, status, template_text FROM campaigns WHERE id = $1`, campaignID).
		Scan(&summary.Title, &summary.Status, &template)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, nil
	}
	if err != nil {
		return StatusReport{}, fmt.Errorf("status: read campaign: %w", err)
	}

	summary.States = map[string]int{}
	rows, err := s.pool.Query(ctx,
		`SELECT state, count(*) FROM recipients WHERE campaign_id = $1 GROUP BY state`, summary.ID)
	if err != nil {
		return StatusReport{}, fmt.Errorf("status: count states: %w", err)
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return StatusReport{}, fmt.Errorf("status: scan state: %w", err)
		}
		summary.States[state] = count
		summary.Total += count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StatusReport{}, fmt.Errorf("status: read states: %w", err)
	}
	report.Campaign = &summary
	// Скобки показанной кампании, а не активной: строка отчёта говорит про «эту
	// кампанию», и скобки чужого шаблона указали бы человеку не на тот текст.
	report.Idle.Braces, _ = badBraces(template)

	preview, err := s.previewMessages(ctx, summary.ID, template)
	if err != nil {
		return StatusReport{}, err
	}
	report.Preview = preview

	if full {
		if report.Rows, err = s.recipientRows(ctx, summary.ID); err != nil {
			return StatusReport{}, err
		}
	}

	if report.Held, err = s.heldRows(ctx, summary.ID); err != nil {
		return StatusReport{}, err
	}

	if summary.Status != "draft" {
		if report.Draft, err = s.campaignNote(ctx, "draft"); err != nil {
			return StatusReport{}, err
		}
	}
	if summary.Status != "stopped" {
		if report.Paused, err = s.campaignNote(ctx, "stopped"); err != nil {
			return StatusReport{}, err
		}
	}
	return report, nil
}

// sharedIdle - причины, одинаковые для всех аккаунтов: окно отправки, наличие
// кампании и её шаблон общие, и повторять их в строке каждого аккаунта значит
// спрятать среди них ту единственную причину, которая отличается.
func sharedIdle(reason string) bool {
	switch reason {
	case IdleWindowClosed, IdleNoCampaign, IdleBrokenTemplate, IdleQueueEmpty:
		return true
	}
	return false
}

// commonIdle - причина простоя, которую отчёт называет одной строкой на всю
// очередь. Аккаунт один - это его причина, как и было до нескольких
// отправщиков. Аккаунтов несколько - только общая причина и только когда она
// совпала у всех: у расхода потолка и у стопа числа свои у каждого аккаунта, и
// одна строка сверху обещала бы чужой запас.
func commonIdle(accounts []AccountSummary) Idle {
	if len(accounts) == 0 {
		return Idle{}
	}
	first := accounts[0].Idle
	if len(accounts) == 1 {
		return first
	}
	if !sharedIdle(first.Reason) {
		return Idle{}
	}
	for _, item := range accounts[1:] {
		if item.Idle.Reason != first.Reason {
			return Idle{}
		}
	}
	return first
}

// campaignNote отдаёт соседнюю кампанию в названном состоянии, если она есть:
// отчёт по идущей кампании обязан назвать отдельной строкой и подготовленный
// черновик, и оставленную паузу, иначе про них забудут.
func (s *Service) campaignNote(ctx context.Context, status string) (*CampaignNote, error) {
	var note CampaignNote
	err := s.pool.QueryRow(ctx,
		`SELECT c.id, (SELECT count(*) FROM recipients r WHERE r.campaign_id = c.id)
		 FROM campaigns c WHERE c.status = $1 ORDER BY c.id LIMIT 1`, status).
		Scan(&note.ID, &note.Rows)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("status: read %s campaign: %w", status, err)
	}
	return &note, nil
}

// heldRows считает строки очереди, закреплённые за другими аккаунтами. Строка
// навсегда принадлежит взявшему её аккаунту, поэтому мёртвый или чужой
// отправщик держит её planned, а кампания не закрывается. Оператор должен
// видеть, отправщика с какой меткой ждёт очередь.
func (s *Service) heldRows(ctx context.Context, campaignID int64) ([]HeldRows, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.label, count(*) FROM recipients r
		 JOIN accounts a ON a.id = r.account_id
		 WHERE r.campaign_id = $1 AND r.state = 'planned' AND a.label <> $2
		 GROUP BY a.label ORDER BY a.label`,
		campaignID, s.cfg.AccountLabel)
	if err != nil {
		return nil, fmt.Errorf("status: count held rows: %w", err)
	}
	defer rows.Close()

	var list []HeldRows
	for rows.Next() {
		var item HeldRows
		if err := rows.Scan(&item.Label, &item.Rows); err != nil {
			return nil, fmt.Errorf("status: scan held rows: %w", err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read held rows: %w", err)
	}
	return list, nil
}

// currentCampaignSQL выбирает кампанию, о которой спрашивают: активная важнее
// черновика, черновик - приостановленной, и только потом идут закрытые по
// свежести. Среди закрытых свежесть считает finished_at, а не id: done и
// cancelled закрываются в любом порядке относительно создания, и id младшей
// кампании ничего не говорит о том, кто закрылся позже. NULLS LAST и id DESC
// остаются подстраховкой на случай пустого finished_at. Id инструменты не
// передают, поэтому порядок задан здесь, а не догадкой вызывающего.
const currentCampaignSQL = `SELECT id FROM campaigns
	ORDER BY CASE status
		WHEN 'active' THEN 0
		WHEN 'draft' THEN 1
		WHEN 'stopped' THEN 2
		ELSE 3
	END, finished_at DESC NULLS LAST, id DESC
	LIMIT 1`

// accountsReportSQL - строка аккаунта в отчёте: расход тем же счётом, что и в
// рубеже потолка (иначе status показывал бы запас там, где ClaimNext уже
// отказывает), и его разбивка. Служебные адресаты идут отдельным числом и не
// попадают ни в расход, ни в тёплые реплики: одно сообщение не имеет права быть
// посчитанным дважды. $1 - начало суток, $2 - граница протухания claim.
func accountsReportSQL(owners string) string {
	return `SELECT a.label, a.daily_limit, a.stopped, a.stop_reason,
		` + usedTodaySQL(`a.id`, owners) + `,
		` + coldDirectSQL(`a.id`, owners) + `,
		(SELECT count(*) FROM direct_messages d
		 WHERE d.account_id = a.id AND d.username <> ALL(` + owners + `)
		   AND NOT d.cold AND d.state <> 'failed' AND d.created_at >= $1),
		(SELECT count(*) FROM recipients r
		 WHERE r.account_id = a.id AND r.username = ANY(` + owners + `) AND r.sent_at >= $1)
		+ (SELECT count(*) FROM direct_messages d
		   WHERE d.account_id = a.id AND d.username = ANY(` + owners + `)
		     AND d.state <> 'failed' AND d.created_at >= $1)
	 FROM accounts a ORDER BY a.label`
}

func (s *Service) accountSummaries(ctx context.Context) ([]AccountSummary, error) {
	now := s.Now()
	rows, err := s.pool.Query(ctx, s.accountsReportSQL, startOfDay(now), s.staleBefore(now))
	if err != nil {
		return nil, fmt.Errorf("status: read accounts: %w", err)
	}
	defer rows.Close()

	var list []AccountSummary
	for rows.Next() {
		var item AccountSummary
		if err := rows.Scan(&item.Label, &item.DailyLimit, &item.Stopped, &item.StopReason,
			&item.UsedToday, &item.ColdToday, &item.WarmToday, &item.OwnerToday); err != nil {
			return nil, fmt.Errorf("status: scan account: %w", err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read accounts: %w", err)
	}
	// Причина простоя считается по каждому аккаунту, а не по метке процесса с
	// MCP: аккаунтов несколько, и вопрос «почему второй молчит» иначе не имеет
	// ответа снаружи. Расчёт идёт тем же IdleReason, что пишет причину в лог у
	// самого отправщика: второй расчёт рядом разъехался бы с первым.
	// Сбой по одному аккаунту не уносит отчёт: строка останется без причины.
	for i := range list {
		idle, err := s.IdleReason(ctx, list[i].Label)
		if err != nil {
			slog.Warn("idle reason unavailable", "account", list[i].Label, "error", err)
			continue
		}
		list[i].Idle = idle
	}
	return list, nil
}

// stuckDirect считает ручные касания, зависшие в pending дольше протухания
// claim. У строки очереди протухший claim даёт второй шанс, у журнала такого
// пути нет: pending закрывает человека навсегда, поэтому счётчик стоит в
// каждом ответе status.
func (s *Service) stuckDirect(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM direct_messages WHERE state = 'pending' AND created_at < $1`,
		s.staleBefore(s.Now())).Scan(&count); err != nil {
		return 0, fmt.Errorf("status: count stuck direct messages: %w", err)
	}
	return count, nil
}

// stuckClaims считает строки очереди, которые отправщик взял и не закрыл.
// Активная кампания разбирает их сама следующим витком, а кампания на паузе -
// нет: такая строка держит человека закрытым и для ручной отправки, поэтому
// счётчик стоит в каждом ответе status.
func (s *Service) stuckClaims(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM recipients
		 WHERE state = 'planned' AND claimed_at IS NOT NULL AND claimed_at < $1`,
		s.staleBefore(s.Now())).Scan(&count); err != nil {
		return 0, fmt.Errorf("status: count stuck claims: %w", err)
	}
	return count, nil
}

// LogStuckDirect называет зависшие касания при старте процесса. Ошибка чтения
// сама по себе процесс не останавливает: это наблюдаемость, а не рубеж, - но
// молчать о ней нельзя, иначе непрочитанный журнал выглядит как пустой.
func (s *Service) LogStuckDirect(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, username, created_at FROM direct_messages
		 WHERE state = 'pending' AND created_at < $1 ORDER BY id`,
		s.staleBefore(s.Now()))
	if err != nil {
		slog.Error("stuck direct messages unavailable", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var username string
		var created time.Time
		if err := rows.Scan(&id, &username, &created); err != nil {
			slog.Error("stuck direct messages unavailable", "error", err)
			return
		}
		slog.Error("direct message stuck pending",
			"direct_id", id, "username", username, "created_at", created)
	}
	if err := rows.Err(); err != nil {
		slog.Error("stuck direct messages unavailable", "error", err)
	}
}

func (s *Service) previewMessages(ctx context.Context, campaignID int64, template string) ([]MessagePreview, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT username, name FROM recipients WHERE campaign_id = $1 ORDER BY id LIMIT 3`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("status: read preview: %w", err)
	}
	defer rows.Close()

	var list []MessagePreview
	for rows.Next() {
		var username, name string
		if err := rows.Scan(&username, &name); err != nil {
			return nil, fmt.Errorf("status: scan preview: %w", err)
		}
		list = append(list, MessagePreview{Username: username, Text: RenderMessage(template, name)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read preview: %w", err)
	}
	return list, nil
}

func (s *Service) recipientRows(ctx context.Context, campaignID int64) ([]RecipientRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT username, name, state, reason FROM recipients WHERE campaign_id = $1 ORDER BY id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("status: read recipients: %w", err)
	}
	defer rows.Close()

	var list []RecipientRow
	for rows.Next() {
		var item RecipientRow
		if err := rows.Scan(&item.Username, &item.Name, &item.State, &item.Reason); err != nil {
			return nil, fmt.Errorf("status: scan recipient: %w", err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("status: read recipients: %w", err)
	}
	return list, nil
}
