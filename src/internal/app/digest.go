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

// digestHour is the Moscow hour from which the evening digest goes out: the
// send window closes at 19:00, and by then the campaign day is already counted.
// There is deliberately no upper bound: a process started at 21:00 still sends
// the day's digest, and a new day is a different lock key.
const digestHour = 19

// digestWarnHour is the Moscow hour from which a process that does not send
// the digest starts checking whether it went out at all. The hour of slack
// after digestHour avoids warning about a digest the sender is sending at that
// very minute.
const digestWarnHour = 20

// digestAccount is the digest line for one sender. The owner needs a
// per-account breakdown, not only the total: an account silenced by a stop is
// visible only here.
type digestAccount struct {
	Label      string
	Sent       int
	Replied    int
	UsedToday  int
	DailyLimit int
	Stopped    bool
	StopReason string
}

// maybeSendDigest sends the evening digest if its time has come and nobody has
// sent it today. It is called from the sender loop between passes: sending
// lives inside the MTProto session, and a separate goroutine cannot reach the
// transport.
//
// It returns the error, but the loop does not stop on it - the caller decides:
// sending the mailing matters more than the report about it.
func (s *Sender) maybeSendDigest(ctx context.Context) error {
	if s.chat == nil {
		return nil
	}
	now := s.service.Now().In(moscowZone)
	if !digestDue(now) {
		return nil
	}
	// In dry-run no message goes out, but the lock would eat the day: in the
	// evening of the live setup the digest would already count as sent.
	if s.service.cfg.Transport != "live" {
		return nil
	}

	// One named account sends the digest: otherwise, with several senders, it
	// arrives now from one chat, now from another. The others stay silent but
	// check whether it went out: a label without a live process would stop the
	// digest for good and without a word.
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
			// The lock is released so that the next pass of the same day tries
			// again: the direct_messages row stays failed, as for any direct
			// send.
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

// warnDigestMissing names in the log an evening when the digest reached no
// recipient. It is asked by the process that does not send the digest: it sees
// its own failure anyway, while a silent digest sender is otherwise visible to
// nobody. A read error is also a log line: this is observability, not a guard,
// and it must not stop the mailing.
func (s *Service) warnDigestMissing(ctx context.Context, now time.Time, digestLabel string) {
	missing, err := s.digestMissing(ctx, now)
	if err != nil {
		slog.Warn("digest state unavailable", "error", err)
		return
	}
	if missing {
		slog.Warn("digest not sent today", "digest_account", digestLabel,
			"note", "only the DIGEST_ACCOUNT account sends the digest: check that it is running")
	}
}

// digestMissing reports whether the digest reached no recipient today. Before
// digestWarnHour the question is meaningless: the sender is sending it then.
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

// digestDue reports whether the digest is due: a weekday and a time after
// digestHour Moscow time. Once-only delivery is held by the database lock, not
// by this condition.
func digestDue(now time.Time) bool {
	local := now.In(moscowZone)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return false
	}
	return local.Hour() >= digestHour
}

// digestCampaignSQL is the order of currentCampaignSQL without the draft:
// status ranks a draft above closed campaigns, and after a campaign closes
// with the next one loaded, the digest would go out about the draft with zeros
// instead of the result.
const digestCampaignSQL = `SELECT id FROM campaigns
	WHERE status <> 'draft'
	ORDER BY CASE status
		WHEN 'active' THEN 0
		WHEN 'stopped' THEN 1
		ELSE 2
	END, finished_at DESC NULLS LAST, id DESC
	LIMIT 1`

// digestCampaign returns the campaign the digest is about. When no campaign
// was ever started it returns zero, which is not an error.
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

// claimDigest takes the day's digest for one recipient. Zero means another
// sender has already taken it or it went out today: the insert is the lock.
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

// dropDigest releases the lock after a failed send. The error is only logged:
// the caller already carries the cause of the failure, and it must not be
// replaced by the release failure.
func (s *Service) dropDigest(ctx context.Context, id int64) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM daily_digest WHERE id = $1`, id); err != nil {
		slog.Warn("digest lock not released", "id", id, "error", err)
	}
}

// digestText builds the digest text: the campaign day, the per-account
// breakdown, the total and the forecast date of the last message, below them
// the campaign funnel by account, the campaign total and the total of all
// mailings. The text is addressed to the Russian-speaking owner and is
// deliberately in Russian.
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

// digestFunnel is the mailing funnel: taken into work, sent, delivered,
// replied. Rows removed by a guard and cancelled rows never went into work and
// are not counted.
type digestFunnel struct {
	Label     string
	Taken     int
	Sent      int
	Delivered int
	Replied   int
}

// funnelColumns counts the funnel over recipients rows r. A bad username also
// counts as sent: the message went to Telegram, but the addressee does not
// exist. Other undelivered rows are a service failure, the message never
// reached the lead.
const funnelColumns = `count(*) FILTER (WHERE r.state IN ('planned', 'sent', 'replied', 'undelivered')),
	count(*) FILTER (WHERE r.state IN ('sent', 'replied')
		OR (r.state = 'undelivered' AND r.reason IN ('` + telegram.KindPeerForbidden + `', '` + telegram.KindNotPerson + `'))),
	count(*) FILTER (WHERE r.state IN ('sent', 'replied')),
	count(*) FILTER (WHERE r.state = 'replied')`

// Table column widths in characters: the whole line fits in 34 characters and
// fits a phone screen without wrapping.
const (
	labelWidth = 10
	cellWidth  = 6
)

// rows renders the funnel as table lines: the numbers and under them the share
// of each step relative to the previous number. With nothing taken there are
// no shares, and the shares line is not needed.
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

// share returns a percentage. With a zero denominator there is no share and
// nothing to print: "0%" would read as a failed step.
func share(part, whole int) string {
	if whole == 0 {
		return ""
	}
	return fmt.Sprintf("%d%%", int(math.Round(float64(part)*100/float64(whole))))
}

// digestTotals returns the funnel over rows matching the where condition,
// without service recipients: their rows exist in every campaign and would add
// up by the number of mailings. The second value is how many campaigns took
// anyone into work: one cancelled before the start was not a mailing.
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

// digestAccountFunnels returns the campaign funnel for each account in
// summaries, in the same order as the daily breakdown; an account without rows
// keeps zeros.
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

// digestAccounts returns the daily count per account. An account without sends
// stays in the list with zero: a silent sender is exactly what the digest is
// read for.
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

// finishDay returns when the last message goes out at the current caps. It
// counts weekdays only: the send window is closed on weekends. Stopped accounts
// do not contribute to the pace - they do not send today.
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

// dayWords returns the date in words as a person writes it: "15 сентября".
// The month names are Russian because the digest text is Russian.
func dayWords(day time.Time) string {
	months := [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
		"июля", "августа", "сентября", "октября", "ноября", "декабря"}
	local := day.In(moscowZone)
	return fmt.Sprintf("%d %s", local.Day(), months[local.Month()-1])
}
