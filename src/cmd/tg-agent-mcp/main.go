// Command tg-agent-mcp - точка входа сервиса рассылки: демон отправщика,
// MCP-сервер управления и healthcheck в одном процессе.
//
// Подкоманда healthcheck - тот же бинарь, дёргающий свой же эндпоинт: образ
// distroless, и другого http-клиента внутри контейнера нет. Подкоманда
// authorize - разовый вход в живой аккаунт.
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

// dbMaxConns покрывает всех, кто одновременно держит соединение: цикл
// отправщика, обработчики MCP и healthcheck. Запас на короткие транзакции
// claim-модели: они не должны стоять в очереди за соединением.
const dbMaxConns = 8

// shutdownGrace - сколько ждём завершения http-серверов после сигнала.
const shutdownGrace = 10 * time.Second

func main() {
	// Вход в аккаунт идёт до чтения конфига сервиса: ему не нужны ни база, ни
	// метка аккаунта, и запирать вход на готовность контура незачем.
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
	// Единственное место, где заводится строка аккаунта: до http-серверов,
	// потому что healthcheck читает её сразу.
	_, dailyLimit, err := service.EnsureAccount(ctx)
	if err != nil {
		return err
	}
	// Переменная задаёт потолок только при создании строки аккаунта: без этой
	// записи оператор меняет её, перезапускает контур и уверен, что лимит другой,
	// а рассылка идёт по старому.
	if dailyLimit != cfg.AccountDailyLimit {
		logger.Warn("daily limit differs from env",
			"account", cfg.AccountLabel,
			"env_limit", cfg.AccountDailyLimit,
			"db_limit", dailyLimit,
			"note", "действует потолок из БД: ACCOUNT_DAILY_LIMIT задаёт его только при создании аккаунта")
	}
	// Служебные адресаты называются на старте предупреждением, а не строкой
	// Info: для этих ников выключены окно, потолок и правило «одно сообщение», и
	// лишний ник в DEV_ALLOW_LIST обнаружился бы иначе только ушедшим письмом.
	if len(cfg.OwnerAccounts) > 0 {
		logger.Warn("owner accounts have no send limits",
			"accounts", cfg.OwnerAccounts,
			"note", "окно, дневной потолок и правило «одно сообщение» на них не действуют; стоп-лист действует")
	}
	// Вечерняя сводка: кто её отправляет и кому. Метка отправителя видна только
	// здесь, и без этой строки опечатка в ней означала бы молчание всех
	// процессов вместо отчёта.
	logger.Info("daily digest",
		"sends_digest", cfg.DigestAccount == "" || cfg.DigestAccount == cfg.AccountLabel,
		"digest_account", cfg.DigestAccount,
		"recipients", service.DigestRecipients())

	// Зависшие ручные касания называются при старте: их доставка неизвестна, а
	// человек ими закрыт для всех кампаний, и молчание об этом равно потере.
	service.LogStuckDirect(ctx)

	// Клиент собирается до отправщика, потому что ему нужен обработчик
	// входящих, а отправщику - клиент. Разрывает круг ссылка на метод: сам
	// Sender к первому входящему уже существует.
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
	// Живой цикл идёт внутри сессии MTProto: вне неё клиент не поднят и
	// отправка вернула бы «клиент не запущен». В dry-run сессии нет вовсе.
	runSender := sender.Run
	if client != nil {
		runSender = func(ctx context.Context) error { return client.Run(ctx, sender.Run) }
	}
	// Диалог живёт на том же транспорте, что и рассылка: аккаунт один, и
	// второй сессии Telegram для ручных касаний не существует.
	chat := app.NewChat(service, transport)
	// Им же уходит вечерняя сводка: её отправляет цикл отправщика, потому что
	// вне сессии MTProto транспорт недоступен.
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

	// Остановка серверов идёт по своему контексту: ctx сервиса уже отменён
	// сигналом, и Shutdown на нём оборвал бы соединения сразу.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown", "addr", server.Addr, "error", err)
		}
	}

	// Пул закрывается сразу за этой функцией: оборванная финализация оставила бы
	// строку с живым claim до самого протухания, поэтому отправщика дожидаемся.
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

// authorize - разовый вход в живой аккаунт, руками и с терминала: Telegram
// присылает код в приложение, ввести его может только человек.
//
// Сессия ложится в TELEGRAM_STORAGE_PATH, и дальше её берёт демон. Файл держит
// один процесс, поэтому вход выполняется на отправщике в dry-run (телеграм он
// не открывает вовсе) или на остановленном контейнере.
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

// telegramConfig читается только в живом режиме: dev-контур поднимается без
// доступа к аккаунту вовсе, и требовать эти переменные там незачем.
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

// healthMux отдаёт нездоровье, когда аккаунт остановлен: активный стоп обязан
// быть виден снаружи, иначе рассылка молча стоит до следующей проверки глазами.
// Запрос заодно ходит в БД - недоступная база тоже красит контейнер.
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

// probeHealth - подкоманда для healthcheck контейнера: дёргает свой же
// эндпоинт и превращает ответ в код возврата.
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
