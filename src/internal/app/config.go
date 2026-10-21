// Package app holds the configuration and (as the slice grows) the domain code
// of the messaging service: campaign, queue, sender, MCP.
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

// Config holds the process settings, read once at startup. The service must
// fail to start on a broken environment, not fall over mid-campaign on the
// first access to the field it needs.
type Config struct {
	// Env is the environment: dev or prod. It holds the TRANSPORT=live guard: live
	// sending is possible only in prod.
	Env         string
	DatabaseURL string
	LogLevel    slog.Level

	// AccountLabel binds the process to an account: the accounts row is created
	// under this label on first start.
	AccountLabel string
	// AccountDailyLimit is the daily cap of a new account; it does not
	// automatically overwrite daily_limit of an existing accounts row.
	AccountDailyLimit int

	// Transport is dry-run (log instead of sending) or live.
	Transport string

	// SendWindowStart/End is the send window on weekdays, "HH:MM" Moscow time.
	SendWindowStart string
	SendWindowEnd   string
	// SendPauseMin/Max are the bounds of the random pause between sends. The
	// pause is the whole sender iteration, not only the gap between messages: it
	// is also when the sender notices a campaign start, a window opening and an empty queue.
	SendPauseMin time.Duration
	SendPauseMax time.Duration

	// FloodWaitThreshold is the threshold from section 5: a FLOOD_WAIT shorter
	// than it is just a pause, a longer one or PEER_FLOOD stops the account.
	FloodWaitThreshold time.Duration
	// ClaimStaleAfter is how long until the claim on a row taken by the sender
	// goes stale: a process dying between delivery and the write does not lose the row.
	ClaimStaleAfter time.Duration

	// OwnerAccounts are the owner's service recipients from DEV_ALLOW_LIST. The
	// window, the daily cap, the hold for the duration of a campaign and the
	// "one person - one message" rule do not apply to them: the owner tests the
	// campaign with these accounts, and each check must not spend the cap or wait for the window.
	// The stop list and the account stop apply to them like to everyone.
	OwnerAccounts []string

	// DigestAccount is the label of the account allowed to send the evening
	// digest. Empty means any sender sends it, as before the split: there are
	// several senders, and without this label the digest arrives from one
	// account or another, and that reads as a service failure.
	DigestAccount string
	// DigestTo are the recipients of the evening digest. Empty means all service
	// recipients, as before. The list must be a subset of OwnerAccounts: otherwise
	// the digest becomes a cold touch and runs into the send window, which is
	// already closed at its time.
	DigestTo []string

	// DevAllowList is the last guard of the transport: a non-empty list lets
	// nobody out except those listed. It is filled from the same DEV_ALLOW_LIST, but
	// only in dev: in prod the campaign goes to real people, and the list guard would kill it.
	// Hence one variable has two roles - service recipients everywhere, the
	// transport guard only in dev.
	DevAllowList []string

	// MCPEnabled raises the streamable HTTP endpoint; exactly one instance per
	// set of accounts.
	MCPEnabled bool
	MCPAddr    string
	MCPToken   string
	// MCPAgentToken opens agent access: only tg_send and tg_read and only to
	// AgentAllowList. The service holds the guard, so the client lets these calls
	// through without confirmation.
	MCPAgentToken  string
	AgentAllowList []string

	// HealthAddr is the healthcheck address. Separate from MCP: an account stop
	// must be visible from outside, including on a sender with MCP_ENABLED=false.
	HealthAddr string
}

// LoadConfig reads the configuration from the environment and checks its invariants.
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
	// Guard against an accidental live: a dev stack must not be able to reach
	// a live Telegram account.
	if cfg.Transport == "live" && cfg.Env != "prod" {
		return Config{}, fmt.Errorf("TRANSPORT=live is refused outside prod (ENV=%q)", cfg.Env)
	}

	cfg.SendWindowStart = env.String("SEND_WINDOW_START", "10:00")
	cfg.SendWindowEnd = env.String("SEND_WINDOW_END", "19:00")
	if err := validateWindow(cfg.SendWindowStart, cfg.SendWindowEnd); err != nil {
		return Config{}, err
	}

	// The 10-30 minute default: the message text
	// is the same for all recipients, and the pause spread is the only thing
	// that makes the campaign pace differ from a machine's. The price is
	// responsiveness: the sender notices a campaign start, a window opening and an empty queue after the same pause.
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
	// The username shape is checked at startup, not silently: the list goes into
	// SQL queries as a literal, and this is the only place where it could be something other than a username.
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
	// An entry in DEV_ALLOW_LIST also gets its username shape checked: the list
	// of service recipients has already passed IsUsername above.
	for _, name := range cfg.DigestTo {
		if !slices.Contains(cfg.OwnerAccounts, name) {
			return Config{}, fmt.Errorf("DIGEST_TO must be a subset of DEV_ALLOW_LIST, %q is not in it", name)
		}
	}
	// A named sender with no recipients would stay silent every evening, and the
	// neighboring processes would report the missing digest on every iteration: an
	// alarm that never clears is worse than none.
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

// validateWindow checks the HH:MM format and that the window start is before its end.
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

// normalizeAllowList brings DEV_ALLOW_LIST to normalized usernames: without
// "@" and lowercase, like recipient usernames at import. An empty variable
// gives nil, not a slice with one empty element.
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

// normalizeName returns a username without "@" and lowercase. One normalization
// point for the service-recipient list and for the Service.isOwner check: if
// they diverged, they would lift the guards from the wrong person.
func normalizeName(raw string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
}
