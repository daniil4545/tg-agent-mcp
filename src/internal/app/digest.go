package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// digestHour - час МСК, с которого уходит вечерняя сводка: окно отправки
// закрывается в 19:00, и к этому моменту день кампании уже посчитан. Верхней
// границы нет намеренно: процесс, поднятый в 21:00, сводку за день ещё
// отправит, а новые сутки - это уже другой ключ замка.
const digestHour = 19

// digestWarnHour - с какого часа МСК процесс, который сводку не отправляет,
// начинает проверять, ушла ли она вообще. Час запаса после digestHour нужен,
// чтобы не предупреждать о сводке, которую отправитель в эту минуту и шлёт.
const digestWarnHour = 20

// digestAccount - строка сводки по одному отправщику. Заказчику нужен разрез по
// аккаунтам, а не только итог: аккаунт, замолчавший из-за стопа, виден только
// здесь.
type digestAccount struct {
	Label      string
	Sent       int
	Replied    int
	UsedToday  int
	DailyLimit int
	Stopped    bool
	StopReason string
}

// maybeSendDigest отправляет вечернюю сводку, если её время пришло и никто её
// сегодня не отправил. Зовётся из цикла отправщика между витками: отправка
// живёт внутри сессии MTProto, и отдельной горутине транспорт недоступен.
//
// Ошибку возвращает, но цикл на ней не останавливается - решает вызывающий:
// рассылка писем важнее отчёта о ней.
func (s *Sender) maybeSendDigest(ctx context.Context) error {
	if s.chat == nil {
		return nil
	}
	now := s.service.Now().In(moscowZone)
	if !digestDue(now) {
		return nil
	}
	// В dry-run письмо не уходит, а замок съел бы день: вечером живого контура
	// сводка оказалась бы уже отправленной.
	if s.service.cfg.Transport != "live" {
		return nil
	}

	// Сводку отправляет один названный аккаунт: иначе при нескольких
	// отправщиках она приходит то из одного чата, то из другого. Остальные
	// молчат, но смотрят, ушла ли она: метка без живого процесса остановила бы
	// сводку насовсем и молча.
	if digestLabel := s.service.cfg.DigestAccount; digestLabel != "" && digestLabel != s.label {
		s.service.warnDigestMissing(ctx, now, digestLabel)
		return nil
	}

	campaignID, err := s.service.digestCampaign(ctx)
	if err != nil || campaignID == 0 {
		return err
	}

	var text string
	for _, owner := range s.service.DigestRecipients() {
		id, err := s.service.claimDigest(ctx, now, owner, campaignID, s.label)
		if err != nil {
			return err
		}
		if id == 0 {
			continue
		}
		if text == "" {
			if text, err = s.service.digestText(ctx, campaignID, now); err != nil {
				s.service.dropDigest(ctx, id)
				return err
			}
		}
		if _, err := s.chat.Send(ctx, owner, text); err != nil {
			// Замок снимается, чтобы следующий виток того же дня попробовал
			// снова: строка direct_messages при этом остаётся failed, как у
			// любой прямой отправки.
			s.service.dropDigest(ctx, id)
			return fmt.Errorf("digest to @%s: %w", owner, err)
		}
		if err := s.service.finishDigest(ctx, id); err != nil {
			return err
		}
		slog.Info("digest sent", "owner", owner, "campaign_id", campaignID)
	}
	return nil
}

// warnDigestMissing называет в журнале вечер, в который сводка не ушла ни
// одному получателю. Спрашивает тот, кто её не отправляет: свой отказ он видит
// и так, а молчащий отправитель сводки иначе не виден никому. Ошибка чтения -
// тоже строка журнала: это наблюдаемость, а не рубеж, и останавливать ею
// рассылку нельзя.
func (s *Service) warnDigestMissing(ctx context.Context, now time.Time, digestLabel string) {
	missing, err := s.digestMissing(ctx, now)
	if err != nil {
		slog.Warn("digest state unavailable", "error", err)
		return
	}
	if missing {
		slog.Warn("digest not sent today", "digest_account", digestLabel,
			"note", "сводку отправляет только аккаунт DIGEST_ACCOUNT: проверьте, поднят ли он")
	}
}

// digestMissing - не ушла ли сегодня сводка ни одному получателю. Раньше
// digestWarnHour вопрос не имеет смысла: отправитель в это время её и шлёт.
func (s *Service) digestMissing(ctx context.Context, now time.Time) (bool, error) {
	if now.In(moscowZone).Hour() < digestWarnHour {
		return false, nil
	}
	var sent int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM daily_digest WHERE day = $1 AND sent_at IS NOT NULL`,
		now.In(moscowZone).Format(time.DateOnly)).Scan(&sent); err != nil {
		return false, fmt.Errorf("digest: count sent: %w", err)
	}
	return sent == 0, nil
}

// digestDue - настало ли время сводки: будний день и время после digestHour по
// МСК. Однократность держит замок в БД, а не это условие.
func digestDue(now time.Time) bool {
	local := now.In(moscowZone)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return false
	}
	return local.Hour() >= digestHour
}

// digestCampaignSQL - порядок currentCampaignSQL без черновика: status ставит
// черновик выше закрытых, и после закрытия кампании с загруженной следующей
// сводка ушла бы о черновике с нулями вместо итога.
const digestCampaignSQL = `SELECT id FROM campaigns
	WHERE status <> 'draft'
	ORDER BY CASE status
		WHEN 'active' THEN 0
		WHEN 'stopped' THEN 1
		ELSE 2
	END, finished_at DESC NULLS LAST, id DESC
	LIMIT 1`

// digestCampaign - о какой кампании сводка. Запущенных кампаний нет вовсе -
// ноль, и это не ошибка.
func (s *Service) digestCampaign(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, digestCampaignSQL).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("digest: find campaign: %w", err)
	}
	return id, nil
}

// claimDigest берёт сводку дня для одного адресата. Ноль означает, что её уже
// взял другой отправщик или она сегодня ушла: вставка и есть замок.
func (s *Service) claimDigest(ctx context.Context, now time.Time, owner string, campaignID int64, label string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO daily_digest (day, username, campaign_id, account_id, created_at)
		 VALUES ($1, $2, $3, (SELECT id FROM accounts WHERE label = $4), $5)
		 ON CONFLICT (day, username) DO NOTHING
		 RETURNING id`,
		now.In(moscowZone).Format(time.DateOnly), owner, campaignID, label, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("digest: claim: %w", err)
	}
	return id, nil
}

func (s *Service) finishDigest(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `UPDATE daily_digest SET sent_at = $2 WHERE id = $1`, id, s.Now()); err != nil {
		return fmt.Errorf("digest: finish: %w", err)
	}
	return nil
}

// dropDigest снимает замок после неудачной отправки. Ошибку только логируем:
// вызывающий уже несёт причину отказа, и подменять её отказом снятия нельзя.
func (s *Service) dropDigest(ctx context.Context, id int64) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM daily_digest WHERE id = $1`, id); err != nil {
		slog.Warn("digest lock not released", "id", id, "error", err)
	}
}

// digestText собирает текст сводки: день по кампании, разрез по аккаунтам,
// итог и прогноз даты последнего письма, под ними воронка кампании по
// аккаунтам, итог кампании и итог всех рассылок.
func (s *Service) digestText(ctx context.Context, campaignID int64, now time.Time) (string, error) {
	report, err := s.StatusOf(ctx, campaignID, false)
	if err != nil {
		return "", err
	}
	if report.Campaign == nil {
		return "", fmt.Errorf("digest: campaign %d not found", campaignID)
	}

	from := dayStart(now)
	accounts, err := s.digestAccounts(ctx, campaignID, from, report.Accounts)
	if err != nil {
		return "", err
	}

	var todaySent, todayReplied int
	for _, a := range accounts {
		todaySent += a.Sent
		todayReplied += a.Replied
	}

	states := report.Campaign.States
	totalSent := states["sent"] + states["replied"]
	queued := report.Campaign.Total - totalSent - states["undelivered"] - states["skipped"] - states["cancelled"]

	var b strings.Builder
	fmt.Fprintf(&b, "Кампания «%s», %s.\n", report.Campaign.Title, dayWords(now))
	fmt.Fprintf(&b, "Сегодня ушло %d, ответили %d.\n", todaySent, todayReplied)
	b.WriteString("По аккаунтам сегодня:\n")
	for _, a := range accounts {
		fmt.Fprintf(&b, "  %s - ушло %d, ответили %d, расход %d из %d", a.Label, a.Sent, a.Replied, a.UsedToday, a.DailyLimit)
		if a.Stopped {
			fmt.Fprintf(&b, ", остановлен: %s", a.StopReason)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Всего ушло %d из %d, ответили %d, не доставлено %d.\n",
		totalSent, report.Campaign.Total, states["replied"], states["undelivered"])
	if queued > 0 {
		fmt.Fprintf(&b, "Осталось %d, при нынешнем темпе последнее письмо уйдёт %s.\n",
			queued, dayWords(finishDay(now, queued, accounts)))
	} else {
		b.WriteString("Очередь пуста.\n")
	}
	if stopped := stoppedLabels(accounts); len(stopped) > 0 {
		fmt.Fprintf(&b, "Остановлены аккаунты: %s.\n", strings.Join(stopped, ", "))
	} else {
		b.WriteString("Аккаунты в порядке, стопов нет.\n")
	}

	byAccount, err := s.digestAccountFunnels(ctx, campaignID, report.Accounts)
	if err != nil {
		return "", err
	}
	campaign, _, err := s.digestTotals(ctx, `r.campaign_id = $1`, campaignID)
	if err != nil {
		return "", err
	}
	all, campaigns, err := s.digestTotals(ctx, `c.status <> 'draft'`)
	if err != nil {
		return "", err
	}
	b.WriteString("\nИтог за кампанию по аккаунтам, по кампании и по всем рассылкам. Под числом - доля от предыдущего:\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "%-*s%*s%*s%*s%*s\n", labelWidth, "", cellWidth, "взято", cellWidth, "отпр.", cellWidth, "дост.", cellWidth, "отв.")
	for _, f := range byAccount {
		b.WriteString(f.rows(f.Label))
	}
	b.WriteString(campaign.rows("Кампания"))
	b.WriteString(all.rows(fmt.Sprintf("Все (%d)", campaigns)))
	b.WriteString("```")
	return b.String(), nil
}

// digestFunnel - воронка рассылки: взято в работу, отправлено, доставлено,
// ответили. Снятые рубежом и отменённые строки в работу не шли и не считаются.
type digestFunnel struct {
	Label     string
	Taken     int
	Sent      int
	Delivered int
	Replied   int
}

// funnelColumns считает воронку по строкам recipients r. Отправкой считается и
// брак ника: письмо ушло в Telegram, но адресата нет. Прочие undelivered - сбой
// сервиса, до лида письмо не дошло.
const funnelColumns = `count(*) FILTER (WHERE r.state IN ('planned', 'sent', 'replied', 'undelivered')),
	count(*) FILTER (WHERE r.state IN ('sent', 'replied')
		OR (r.state = 'undelivered' AND r.reason IN ('` + telegram.KindPeerForbidden + `', '` + telegram.KindNotPerson + `'))),
	count(*) FILTER (WHERE r.state IN ('sent', 'replied')),
	count(*) FILTER (WHERE r.state = 'replied')`

// Ширина колонок таблицы в символах: вся строка укладывается в 34 знака и
// влезает в экран телефона без переноса.
const (
	labelWidth = 10
	cellWidth  = 6
)

// rows - воронка строками таблицы: числа и под ними доля каждого шага от
// предыдущего числа. Без взятых долей нет ни одной, и строка долей не нужна.
func (f digestFunnel) rows(label string) string {
	if runes := []rune(label); len(runes) > labelWidth {
		label = string(runes[:labelWidth])
	}
	out := fmt.Sprintf("%-*s%*d%*d%*d%*d\n",
		labelWidth, label, cellWidth, f.Taken, cellWidth, f.Sent, cellWidth, f.Delivered, cellWidth, f.Replied)
	if f.Taken == 0 {
		return out
	}
	return out + fmt.Sprintf("%-*s%*s%*s%*s\n", labelWidth+cellWidth, "",
		cellWidth, share(f.Sent, f.Taken), cellWidth, share(f.Delivered, f.Sent), cellWidth, share(f.Replied, f.Delivered))
}

// share - доля в процентах. При нулевом знаменателе доли нет, и печатать
// нечего: «0%» читался бы как провал шага.
func share(part, whole int) string {
	if whole == 0 {
		return ""
	}
	return fmt.Sprintf("%d%%", int(math.Round(float64(part)*100/float64(whole))))
}

// digestTotals - воронка по строкам под условием where, без служебных
// адресатов: их строки есть в каждой кампании и сложились бы по числу
// рассылок. Второе значение - сколько кампаний хоть кого-то взяли в работу:
// отменённая до старта рассылкой не была.
func (s *Service) digestTotals(ctx context.Context, where string, args ...any) (digestFunnel, int, error) {
	var f digestFunnel
	var campaigns int
	err := s.pool.QueryRow(ctx,
		`SELECT count(DISTINCT r.campaign_id) FILTER (WHERE r.state IN ('planned', 'sent', 'replied', 'undelivered')), `+funnelColumns+`
		 FROM recipients r
		 JOIN campaigns c ON c.id = r.campaign_id
		 WHERE r.username <> ALL(`+ownersLiteral(s.cfg.OwnerAccounts)+`) AND `+where,
		args...).Scan(&campaigns, &f.Taken, &f.Sent, &f.Delivered, &f.Replied)
	if err != nil {
		return f, 0, fmt.Errorf("digest: funnel: %w", err)
	}
	return f, campaigns, nil
}

// digestAccountFunnels - воронка кампании по каждому аккаунту из summaries, в
// том же порядке, что и дневной разрез; аккаунт без строк остаётся с нулями.
func (s *Service) digestAccountFunnels(ctx context.Context, campaignID int64, summaries []AccountSummary) ([]digestFunnel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.label, `+funnelColumns+`
		 FROM recipients r
		 JOIN accounts a ON a.id = r.account_id
		 WHERE r.campaign_id = $1 AND r.username <> ALL(`+ownersLiteral(s.cfg.OwnerAccounts)+`)
		 GROUP BY a.label`,
		campaignID)
	if err != nil {
		return nil, fmt.Errorf("digest: account funnels: %w", err)
	}
	defer rows.Close()

	found := make(map[string]digestFunnel)
	for rows.Next() {
		var f digestFunnel
		if err := rows.Scan(&f.Label, &f.Taken, &f.Sent, &f.Delivered, &f.Replied); err != nil {
			return nil, fmt.Errorf("digest: account funnels scan: %w", err)
		}
		found[f.Label] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digest: account funnels rows: %w", err)
	}

	out := make([]digestFunnel, 0, len(summaries))
	for _, a := range summaries {
		f := found[a.Label]
		f.Label = a.Label
		out = append(out, f)
	}
	return out, nil
}

// digestAccounts - дневной счёт по каждому аккаунту. Аккаунт без отправок
// остаётся в списке с нулём: молчащий отправщик и есть то, ради чего сводку
// читают.
func (s *Service) digestAccounts(ctx context.Context, campaignID int64, from time.Time, summaries []AccountSummary) ([]digestAccount, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.label,
		        count(*) FILTER (WHERE r.state IN ('sent', 'replied')) AS sent,
		        count(*) FILTER (WHERE r.state = 'replied') AS replied
		 FROM recipients r
		 JOIN accounts a ON a.id = r.account_id
		 WHERE r.campaign_id = $1 AND r.sent_at >= $2 AND r.sent_at < $3
		 GROUP BY a.label`,
		campaignID, from, from.Add(24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("digest: accounts: %w", err)
	}
	defer rows.Close()

	sent := make(map[string][2]int)
	for rows.Next() {
		var label string
		var day, replied int
		if err := rows.Scan(&label, &day, &replied); err != nil {
			return nil, fmt.Errorf("digest: accounts scan: %w", err)
		}
		sent[label] = [2]int{day, replied}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digest: accounts rows: %w", err)
	}

	out := make([]digestAccount, 0, len(summaries))
	for _, a := range summaries {
		counts := sent[a.Label]
		out = append(out, digestAccount{
			Label: a.Label, Sent: counts[0], Replied: counts[1],
			UsedToday: a.UsedToday, DailyLimit: a.DailyLimit,
			Stopped: a.Stopped, StopReason: a.StopReason,
		})
	}
	return out, nil
}

func stoppedLabels(accounts []digestAccount) []string {
	var out []string
	for _, a := range accounts {
		if a.Stopped {
			out = append(out, a.Label)
		}
	}
	return out
}

// finishDay - когда уйдёт последнее письмо при нынешних потолках. Считается по
// будням: в выходные окно отправки закрыто. Остановленные аккаунты в темп не
// входят - они сегодня не отправляют.
func finishDay(now time.Time, queued int, accounts []digestAccount) time.Time {
	var pace int
	for _, a := range accounts {
		if !a.Stopped {
			pace += a.DailyLimit
		}
	}
	day := dayStart(now)
	if pace <= 0 {
		return day
	}
	for left := queued; left > 0; {
		day = day.AddDate(0, 0, 1)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		left -= pace
	}
	return day
}

func dayStart(now time.Time) time.Time {
	local := now.In(moscowZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, moscowZone)
}

// dayWords - дата словами, как её пишет человек: «15 сентября».
func dayWords(day time.Time) string {
	months := [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
		"июля", "августа", "сентября", "октября", "ноября", "декабря"}
	local := day.In(moscowZone)
	return fmt.Sprintf("%d %s", local.Day(), months[local.Month()-1])
}
