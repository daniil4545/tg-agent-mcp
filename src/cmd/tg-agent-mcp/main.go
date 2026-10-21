// Command tg-agent-mcp is the entry point of the messaging service: the sender
// daemon, the MCP control server and the healthcheck in one process.
//
// The healthcheck subcommand is the same binary calling its own endpoint: the
// image is distroless and there is no other http client inside the container.
// The authorize subcommand is a one-off login into the live account.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/app"
	"github.com/daniil4545/tg-agent-mcp/internal/db"
	"github.com/daniil4545/tg-agent-mcp/internal/env"
	"github.com/daniil4545/tg-agent-mcp/internal/logs"
	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// dbMaxConns covers everyone who holds a connection at the same time: the
// sender loop, the MCP handlers and the healthcheck. It leaves headroom for the
// short transactions of the claim model: they must not queue behind a connection.
const dbMaxConns = 8

// shutdownGrace is how long we wait for the http servers to finish after a signal.
const shutdownGrace = 10 * time.Second

func main() {
	// Login happens before the service config is read: it needs neither the
	// database nor the account label, and tying login to the readiness of the rest is pointless.
	if len(os.Args) > 1 && os.Args[1] == "authorize" {
		if err := authorize(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cfg, err := app.LoadConfig()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probeHealth(cfg.HealthAddr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	logger := logs.New("tg-agent-mcp", cfg.Env, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger, cfg); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown")
}

func run(ctx context.Context, logger *slog.Logger, cfg app.Config) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL, dbMaxConns)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	service := app.NewService(pool, cfg)
	// The only place where the account row is created: before the http servers,
	// because the healthcheck reads it immediately.
	_, dailyLimit, err := service.EnsureAccount(ctx)
	if err != nil {
		return err
	}
	// The variable sets the cap only when the account row is created: without
	// this warning an operator changes it, restarts the stack and believes the
	// limit is different while the campaign runs on the old one.
	if dailyLimit != cfg.AccountDailyLimit {
		logger.Warn("daily limit differs from env",
			"account", cfg.AccountLabel,
			"env_limit", cfg.AccountDailyLimit,
			"db_limit", dailyLimit,
			"note", "the cap from the database applies: ACCOUNT_DAILY_LIMIT sets it only when the account is created")
	}
	// Service recipients are named at startup with a warning, not an Info line:
	// the window, the cap and the one-message rule are off for these usernames,
	// and an extra username in DEV_ALLOW_LIST would otherwise surface only as a sent message.
	if len(cfg.OwnerAccounts) > 0 {
		logger.Warn("owner accounts have no send limits",
			"accounts", cfg.OwnerAccounts,
			"note", "the send window, the daily cap and the one-message-per-person rule do not apply to them; the stop list does")
	}
	// Evening digest: who sends it and to whom. The sender label is visible only
	// here, and without this line a typo in it would mean every process stays
	// silent instead of sending the report.
	logger.Info("daily digest",
		"sends_digest", cfg.DigestAccount == "" || cfg.DigestAccount == cfg.AccountLabel,
		"digest_account", cfg.DigestAccount,
		"recipients", service.DigestRecipients())

	// Stuck manual touches are named at startup: their delivery is unknown, and
	// the person is closed to all campaigns because of them, so silence about it equals a loss.
	service.LogStuckDirect(ctx)

	// The client is built before the sender because it needs the incoming
	// message handler, and the sender needs the client. A method reference breaks
	// the cycle: by the first incoming message the Sender already exists.
	var sender *app.Sender
	var transport app.Dialog
	var client *telegram.Client
	if cfg.Transport == "dry-run" {
		transport = app.NewDryRunTransport(cfg)
	} else {
		tgCfg, err := telegramConfig()
		if err != nil {
			return err
		}
		client, err = telegram.New(tgCfg, func(ctx context.Context, msg telegram.Message) error {
			return sender.OnMessage(ctx, msg)
		})
		if err != nil {
			return err
		}
		transport = app.NewLiveTransport(cfg, client)
	}
	sender = app.NewSender(service, transport)
	// The live loop runs inside the MTProto session: outside it the client is not
	// up and a send would return "client not started". In dry-run there is no session at all.
	runSender := sender.Run
	if client != nil {
		runSender = func(ctx context.Context) error { return client.Run(ctx, sender.Run) }
	}
	// The dialog lives on the same transport as the campaign: there is one
	// account, and no second Telegram session exists for manual touches.
	chat := app.NewChat(service, transport)
	// The evening digest also goes through it: the sender loop sends it because
	// the transport is unavailable outside the MTProto session.
	sender.UseChat(chat)

	logger.Info("service started",
		"account_label", cfg.AccountLabel,
		"transport", cfg.Transport,
		"mcp_enabled", cfg.MCPEnabled,
		"health_addr", cfg.HealthAddr,
	)

	servers := []*http.Server{{Addr: cfg.HealthAddr, Handler: healthMux(service, cfg.AccountLabel)}}
	if cfg.MCPEnabled {
		mux := http.NewServeMux()
		mux.Handle("/mcp", app.MCPHandler(service, chat))
		servers = append(servers, &http.Server{Addr: cfg.MCPAddr, Handler: mux})
	}

	listenFailed := make(chan error, len(servers))
	for _, server := range servers {
		go func(server *http.Server) {
			logger.Info("http listening", "addr", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				listenFailed <- fmt.Errorf("listen %s: %w", server.Addr, err)
			}
		}(server)
	}
	senderDone := make(chan error, 1)
	go func() { senderDone <- runSender(ctx) }()

	var runErr error
	senderStopped := false
	select {
	case <-ctx.Done():
	case runErr = <-listenFailed:
	case runErr = <-senderDone:
		senderStopped = true
	}

	// Servers stop under their own context: the service ctx is already cancelled
	// by the signal, and Shutdown on it would cut connections at once.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown", "addr", server.Addr, "error", err)
		}
	}

	// The pool is closed right after this function returns: an interrupted
	// finalization would leave a row with a live claim until it expires, so we wait for the sender.
	if !senderStopped {
		select {
		case err := <-senderDone:
			if runErr == nil {
				runErr = err
			}
		case <-shutdownCtx.Done():
			logger.Warn("sender did not stop in time")
		}
	}
	return runErr
}

// authorize is a one-off login into the live account, done by hand from a
// terminal: Telegram sends the code to the app and only a human can enter it.
//
// The session lands in TELEGRAM_STORAGE_PATH and the daemon takes it from
// there. One process holds the file, so login runs on a sender in dry-run
// (it does not open Telegram at all) or on a stopped container.
func authorize() error {
	cfg, err := telegramConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	self, err := telegram.Authorize(ctx, cfg)
	if err != nil {
		return err
	}
	name := self.Name
	if self.Username != "" {
		name += " @" + self.Username
	}
	fmt.Printf("authorized: %s (id %d, phone %s)\n", name, self.ID, self.Phone)
	return nil
}

// telegramConfig is read only in live mode: the dev stack comes up without
// access to the account at all, and requiring these variables there is pointless.
func telegramConfig() (telegram.Config, error) {
	appID, err := env.RequireInt("TELEGRAM_APP_ID")
	if err != nil {
		return telegram.Config{}, err
	}
	appHash, err := env.RequireString("TELEGRAM_APP_HASH")
	if err != nil {
		return telegram.Config{}, err
	}
	phone, err := env.RequireString("TELEGRAM_PHONE")
	if err != nil {
		return telegram.Config{}, err
	}
	storagePath, err := env.RequireString("TELEGRAM_STORAGE_PATH")
	if err != nil {
		return telegram.Config{}, err
	}
	return telegram.Config{
		AppID:       appID,
		AppHash:     appHash,
		Phone:       phone,
		StoragePath: storagePath,
		ProxyURL:    env.String("TELEGRAM_PROXY_URL", ""),
	}, nil
}

// healthMux reports unhealthy when the account is stopped: an active stop must
// be visible from outside, otherwise the campaign stands silently until the next
// manual check. The request also queries the DB - an unreachable database turns the container red too.
func healthMux(service *app.Service, label string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		stopped, reason, err := service.AccountStopped(ctx, label)
		switch {
		case err != nil:
			http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		case stopped:
			http.Error(w, "account stopped: "+reason, http.StatusServiceUnavailable)
		default:
			_, _ = io.WriteString(w, "ok")
		}
	})
	return mux
}

// probeHealth is the subcommand for the container healthcheck: it calls the
// service's own endpoint and turns the response into an exit code.
func probeHealth(addr string) error {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + addr + "/health")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned %s", resp.Status)
	}
	return nil
}
