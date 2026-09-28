// Package app - конфигурация и (по мере роста среза) доменный код сервиса
// рассылки: кампания, очередь, отправщик, MCP.
package app

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/env"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Config - настройки процесса, читаются один раз на старте. Сервис обязан не
// подняться на кривом окружении, а не упасть посреди кампании на первом же
// обращении к нужному полю.
type Config struct {
	// Env - контур: dev или prod. Держит рубеж TRANSPORT=live: живая отправка
	// возможна только в prod.
	Env         string
	DatabaseURL string
	LogLevel    slog.Level

	// AccountLabel привязывает процесс к аккаунту: строка accounts заводится
	// под эту метку при первом старте.
	AccountLabel string
	// AccountDailyLimit - дневной потолок нового аккаунта; в daily_limit
	// существующей строки accounts не переписывается автоматически.
	AccountDailyLimit int

	// Transport - dry-run (лог вместо отправки) или live.
	Transport string

	// SendWindowStart/End - окно отправки по будням, "HH:MM" по МСК.
	SendWindowStart string
	SendWindowEnd   string
	// SendPauseMin/Max - границы случайной паузы между отправками. Пауза - это
	// весь виток отправщика, а не только промежуток между письмами: на ней же
	// он замечает старт кампании, открытие окна и опустевшую очередь.
	SendPauseMin time.Duration
	SendPauseMax time.Duration

	// FloodWaitThreshold - порог из раздела 5: FLOOD_WAIT короче него - просто
	// пауза, дольше или PEER_FLOOD - стоп аккаунта.
	FloodWaitThreshold time.Duration
	// ClaimStaleAfter - через сколько протухает claim строки, взятой
	// отправщиком: смерть процесса между доставкой и записью не теряет строку.
	ClaimStaleAfter time.Duration

	// OwnerAccounts - служебные адресаты владельца из DEV_ALLOW_LIST. Для них
	// не действуют окно, дневной потолок, удержание на время кампании и правило
	// «один человек - одно сообщение»: этими аккаунтами владелец проверяет
	// рассылку, и каждая проверка не должна тратить потолок и ждать окна.
	// Стоп-лист и стоп аккаунта на них распространяются как на всех.
	OwnerAccounts []string

	// DigestAccount - метка аккаунта, которому разрешено отправлять вечернюю
	// сводку. Пусто - отправляет любой отправщик, как было до разделения:
	// отправщиков несколько, и без этой метки сводка приходит то с одного
	// аккаунта, то с другого, а читается это как сбой сервиса.
	DigestAccount string
	// DigestTo - получатели вечерней сводки. Пусто - все служебные адресаты,
	// как было раньше. Список обязан быть подмножеством OwnerAccounts: иначе
	// сводка станет холодным касанием и упрётся в окно отправки, которое к её
	// времени уже закрыто.
	DigestTo []string

	// DevAllowList - последний рубеж транспорта: непустой список не выпускает
	// наружу никого, кроме перечисленных. Заполняется тем же DEV_ALLOW_LIST, но
	// только в dev: в prod рассылка идёт живым людям, и рубеж списка её убил бы.
	// Отсюда двойная роль одной переменной - служебные адресаты везде, рубеж
	// транспорта только в dev.
	DevAllowList []string

	// MCPEnabled поднимает streamable HTTP эндпоинт; ровно один экземпляр на
	// портфель аккаунтов.
	MCPEnabled bool
	MCPAddr    string
	MCPToken   string
	// MCPAgentToken открывает агентский доступ: только tg_send и tg_read и только
	// по AgentAllowList. Рубеж держит сервис, поэтому клиент пускает эти вызовы
	// без подтверждения.
	MCPAgentToken  string
	AgentAllowList []string

	// HealthAddr - адрес healthcheck. Отдельный от MCP: стоп аккаунта обязан
	// быть виден снаружи и у отправщика с MCP_ENABLED=false.
	HealthAddr string
}

// LoadConfig читает конфигурацию из окружения и проверяет её инварианты.
func LoadConfig() (Config, error) {
	var cfg Config
	var err error

	cfg.Env = env.String("ENV", "dev")
	if cfg.Env != "dev" && cfg.Env != "prod" {
		return Config{}, fmt.Errorf("ENV must be dev or prod, got %q", cfg.Env)
	}
	if cfg.DatabaseURL, err = env.RequireString("DATABASE_URL"); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = env.Level("LOG_LEVEL", slog.LevelInfo); err != nil {
		return Config{}, err
	}
	if cfg.AccountLabel, err = env.RequireString("ACCOUNT_LABEL"); err != nil {
		return Config{}, err
	}
	if cfg.AccountDailyLimit, err = env.Int("ACCOUNT_DAILY_LIMIT", 20); err != nil {
		return Config{}, err
	}

	cfg.Transport = env.String("TRANSPORT", "dry-run")
	if cfg.Transport != "dry-run" && cfg.Transport != "live" {
		return Config{}, fmt.Errorf("TRANSPORT must be dry-run or live, got %q", cfg.Transport)
	}
	// Защита от случайного live: dev-контур не должен получить возможность
	// достучаться до живого Telegram-аккаунта.
	if cfg.Transport == "live" && cfg.Env != "prod" {
		return Config{}, fmt.Errorf("TRANSPORT=live is refused outside prod (ENV=%q)", cfg.Env)
	}

	cfg.SendWindowStart = env.String("SEND_WINDOW_START", "10:00")
	cfg.SendWindowEnd = env.String("SEND_WINDOW_END", "19:00")
	if err := validateWindow(cfg.SendWindowStart, cfg.SendWindowEnd); err != nil {
		return Config{}, err
	}

	// Дефолт 10-30 минут задан пилотом 07.09.2026: текст письма у всех
	// получателей один, и разброс паузы - единственное, чем ход рассылки
	// отличается от машинного. Цена - отзывчивость: старт кампании, открытие
	// окна и закрытие пустой очереди отправщик замечает через ту же паузу.
	if cfg.SendPauseMin, err = env.Duration("SEND_PAUSE_MIN", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.SendPauseMax, err = env.Duration("SEND_PAUSE_MAX", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.SendPauseMin >= cfg.SendPauseMax {
		return Config{}, fmt.Errorf("SEND_PAUSE_MIN must be shorter than SEND_PAUSE_MAX, got %s and %s",
			cfg.SendPauseMin, cfg.SendPauseMax)
	}

	if cfg.FloodWaitThreshold, err = env.Duration("FLOOD_WAIT_THRESHOLD", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.ClaimStaleAfter, err = env.Duration("CLAIM_STALE_AFTER", 5*time.Minute); err != nil {
		return Config{}, err
	}

	cfg.OwnerAccounts = normalizeAllowList(env.String("DEV_ALLOW_LIST", ""))
	// Форма ника проверяется на старте, а не молча: список уезжает в запросы
	// SQL-литералом, и это единственное место, где он может оказаться не ником.
	for _, name := range cfg.OwnerAccounts {
		if !telegram.IsUsername(name) {
			return Config{}, fmt.Errorf("DEV_ALLOW_LIST must hold telegram usernames, got %q", name)
		}
	}
	if cfg.Env == "dev" {
		cfg.DevAllowList = cfg.OwnerAccounts
	}

	cfg.DigestAccount = strings.TrimSpace(env.String("DIGEST_ACCOUNT", ""))
	cfg.DigestTo = normalizeAllowList(env.String("DIGEST_TO", ""))
	// Вхождение в DEV_ALLOW_LIST проверяет форму ника заодно: список служебных
	// адресатов уже прошёл IsUsername выше.
	for _, name := range cfg.DigestTo {
		if !slices.Contains(cfg.OwnerAccounts, name) {
			return Config{}, fmt.Errorf("DIGEST_TO must be a subset of DEV_ALLOW_LIST, %q is not in it", name)
		}
	}
	// Названный отправитель без единого получателя молчал бы каждый вечер, а
	// соседние процессы каждый виток писали бы о пропавшей сводке: тревога,
	// которая не снимется никогда, хуже её отсутствия.
	if cfg.DigestAccount != "" && len(cfg.DigestTo) == 0 && len(cfg.OwnerAccounts) == 0 {
		return Config{}, fmt.Errorf("DIGEST_ACCOUNT is set while nobody receives the digest: fill DIGEST_TO or DEV_ALLOW_LIST")
	}

	if cfg.MCPEnabled, err = env.Bool("MCP_ENABLED", false); err != nil {
		return Config{}, err
	}
	cfg.MCPAddr = env.String("MCP_ADDR", ":8080")
	cfg.MCPToken = env.String("MCP_TOKEN", "")
	if cfg.MCPEnabled && cfg.MCPToken == "" {
		return Config{}, fmt.Errorf("MCP_TOKEN is required when MCP_ENABLED=true")
	}

	cfg.MCPAgentToken = env.String("MCP_AGENT_TOKEN", "")
	cfg.AgentAllowList = normalizeAllowList(env.String("AGENT_ALLOW_LIST", ""))
	switch {
	case cfg.MCPAgentToken == "" && len(cfg.AgentAllowList) > 0:
		return Config{}, fmt.Errorf("AGENT_ALLOW_LIST is set without MCP_AGENT_TOKEN")
	case cfg.MCPAgentToken == "":
	case !cfg.MCPEnabled:
		return Config{}, fmt.Errorf("MCP_AGENT_TOKEN is set while MCP_ENABLED=false")
	case cfg.MCPAgentToken == cfg.MCPToken:
		return Config{}, fmt.Errorf("MCP_AGENT_TOKEN must differ from MCP_TOKEN")
	case len(cfg.AgentAllowList) == 0:
		return Config{}, fmt.Errorf("AGENT_ALLOW_LIST is required with MCP_AGENT_TOKEN")
	}
	for _, name := range cfg.AgentAllowList {
		if !telegram.IsUsername(name) {
			return Config{}, fmt.Errorf("AGENT_ALLOW_LIST must hold telegram usernames, got %q", name)
		}
	}

	cfg.HealthAddr = env.String("HEALTH_ADDR", ":8081")

	return cfg, nil
}

// validateWindow проверяет формат HH:MM и что начало окна раньше конца.
func validateWindow(start, end string) error {
	from, err := time.Parse("15:04", start)
	if err != nil {
		return fmt.Errorf("SEND_WINDOW_START must be HH:MM, got %q", start)
	}
	to, err := time.Parse("15:04", end)
	if err != nil {
		return fmt.Errorf("SEND_WINDOW_END must be HH:MM, got %q", end)
	}
	if !from.Before(to) {
		return fmt.Errorf("SEND_WINDOW_START must be before SEND_WINDOW_END, got %q and %q", start, end)
	}
	return nil
}

// normalizeAllowList приводит DEV_ALLOW_LIST к нормализованным никам: без
// "@" и в нижнем регистре, как и ники получателей при импорте. Пустая
// переменная даёт nil, а не пустой слайс с одним пустым элементом.
func normalizeAllowList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if name := normalizeName(part); name != "" {
			list = append(list, name)
		}
	}
	return list
}

// normalizeName - ник без "@" и в нижнем регистре. Одна точка нормализации на
// список служебных адресатов и на проверку Service.isOwner: разъехавшись, они
// сняли бы рубежи не с того человека.
func normalizeName(raw string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
}
