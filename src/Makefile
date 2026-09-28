ifneq ($(NO_DOTENV),1)
    ENV_FILE := $(firstword $(wildcard .env ../.env))
    ifneq (,$(ENV_FILE))
        -include $(ENV_FILE)
        export
    endif
endif

GOOSE := go run github.com/pressly/goose/v3/cmd/goose@v3.27.1
MIGRATIONS_DIR := ./migrations

# База интеграционного яруса: по умолчанию тот же локальный Postgres из
# docker-compose.yml, куда мигрирует migrate-up. Без дефолта весь ярус тихо
# скипался, и ci-check оставался зелёным ни на чём.
TEST_DATABASE_URL ?= postgres://tg_agent_mcp:tg_agent_mcp@localhost:5433/tg_agent_mcp?sslmode=disable
export TEST_DATABASE_URL

LIVE := docker compose -f docker-compose.local.yml
DUMP_DIR := ../backups

# Отправщик на один аккаунт: тот же образ, свой проект на каждую метку.
SENDER := docker compose -f docker-compose.sender.yml
ACCOUNTS_DIR := accounts
SENDER_PROJECT := tg-agent-mcp-acc-$(label)

.PHONY: run build test test-fast vet fmt-check lint commit-check ci-check deploy-check check-db migrate-up migrate-down \
        live-up live-down live-logs live-authorize live-dump \
        accounts check-account account-up account-authorize account-live account-down account-logs

run:
	go run ./cmd/tg-agent-mcp

build:
	go build ./...

# RUN фильтрует тесты при разборе: make test RUN=TestFoo. Пустой
# TEST_DATABASE_URL - отказ, а не скип: тесты рубежей живут в базе, и зелёный
# прогон без них ничего не значит.
test:
	@test -n "$(TEST_DATABASE_URL)" || { echo "TEST_DATABASE_URL is empty: интеграционный ярус пропускать нельзя, поднимите базу (docker compose up -d) или задайте переменную"; exit 1; }
	go test ./... $(if $(RUN),-run $(RUN) -count=1,)

# Быстрый ярус для петли внутри среза: только то, что не ходит в БД. Срез и
# релиз закрываются полным make test - test-fast его не заменяет.
test-fast:
	go test -short ./... $(if $(RUN),-run $(RUN) -count=1,)

vet:
	go vet ./...

# Форматирование не ловит никто другой: gofmt нет ни в дефолтном наборе
# golangci-lint, ни в хуке на правку файла - расхождение уезжало бы в релиз молча.
fmt-check:
	@out=$$(gofmt -l ./cmd ./internal); \
	test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

lint:
	golangci-lint run --timeout 120s

# Кольцо коммита: секунды, без БД. Полный набор закрывает срез (ci-check).
commit-check: fmt-check vet lint test-fast

ci-check: migrate-up test vet fmt-check lint build

# Синтаксис стека Coolify проверяется до выката: опечатка в compose обнаружится
# на хосте, где чинить её дороже всего. exclude_from_hc - расширение Coolify, и
# docker compose его не знает, поэтому строка снимается на копии файла.
deploy-check:
	@tmp=$$(mktemp ./docker-compose.coolify.XXXXXX.yml); \
	trap 'rm -f "$$tmp"' EXIT; \
	sed '/exclude_from_hc:/d' docker-compose.coolify.yml > "$$tmp"; \
	APP_IMAGE=ghcr.io/daniil4545/tg-agent-mcp:sha-check \
	POSTGRES_PASSWORD=check DATABASE_URL=postgres://tg_agent_mcp:check@postgres:5432/tg_agent_mcp?sslmode=disable \
	ACCOUNT_LABEL=check MCP_TOKEN=check \
	COMPOSE_DISABLE_ENV_FILE=1 \
	docker compose -f "$$tmp" config --quiet
	@printf 'ACCOUNT_LABEL=synthcheck\n' > $(ACCOUNTS_DIR)/synthcheck.env; \
	trap 'rm -f $(ACCOUNTS_DIR)/synthcheck.env' EXIT; \
	LABEL=synthcheck POSTGRES_PASSWORD=check $(SENDER) config --quiet

# DATABASE_URL молча подхватывается из .env, поэтому migrate-* может уехать в
# чужую базу. Печатаем цель (без userinfo) и требуем явный ALLOW_REMOTE=1 для
# всего, что не localhost.
check-db:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is not set"; exit 1; }
	@rest=$$(printf '%s' "$(DATABASE_URL)" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|^.*@||'); \
	authority=$${rest%%[/?]*}; \
	case "$$authority" in \
	"["*) host=$${authority%%]*}; host=$${host#"["}; port=$${authority##*]}; port=$${port#:} ;; \
	*) host=$${authority%%:*}; case "$$authority" in *:*) port=$${authority##*:} ;; *) port= ;; esac ;; \
	esac; \
	case "$$rest" in */*) db=$${rest#*/}; db=$${db%%[?]*} ;; *) db= ;; esac; \
	echo "migration target: host=$${host:-local} port=$${port:-default} db=$${db:-default}"; \
	case "$$host" in \
	localhost|127.0.0.1|::1|"") ;; \
	*) \
		if [ "$(ALLOW_REMOTE)" = "1" ]; then \
			echo "WARNING: remote target $$host, running because ALLOW_REMOTE=1"; \
		else \
			echo "refusing to migrate non-local host $$host; re-run with ALLOW_REMOTE=1 if this is intended"; \
			exit 1; \
		fi ;; \
	esac

# Команда не эхом: в ней целиком лежит DATABASE_URL с паролем, а цель миграции
# уже напечатал check-db без userinfo.
migrate-up: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

# Боевой контур на этой машине: MCP рядом с Claude Code, отправка с домашнего
# IP. Настройки - src/.env по образцу .env.example.
live-up:
	$(LIVE) up -d --build
	@echo "MCP: http://127.0.0.1:$${MCP_PORT:-8087}/mcp"

live-down:
	$(LIVE) down

live-logs:
	$(LIVE) logs -f app

# Вход в аккаунт: Telegram присылает код, ввести его может только человек.
# Работает на поднятом контуре в dry-run - там файл сессии никем не занят.
live-authorize:
	$(LIVE) exec app /app authorize

# Дамп базы кампаний. В ней лежит знание, кому уже писали, - единственный
# рубеж «один человек - одно сообщение» между кампаниями. Снимать перед
# каждой кампанией.
live-dump:
	@mkdir -p $(DUMP_DIR)
	@$(LIVE) exec -T postgres pg_dump -U tg_agent_mcp tg_agent_mcp \
		> $(DUMP_DIR)/campaigns-$$(date +%Y%m%d-%H%M).sql
	@ls -lh $(DUMP_DIR) | tail -1

# Дополнительные аккаунты: один отправщик на метку, своим проектом поверх сети и
# базы основного контура. Все цели принимают label=<метка>, настройки аккаунта
# лежат в accounts/<метка>.env (в гит не попадают).
accounts:
	@echo "Файлы аккаунтов:"
	@ls $(ACCOUNTS_DIR)/*.env 2>/dev/null | sed 's|.*/||; s|\.env$$||; s|^|  |' || true
	@echo "Отправщики:"
	@docker ps -a --filter name=tg-agent-mcp-acc- --format '  {{.Names}}\t{{.Status}}' || true

# Рубеж метки: под одной меткой не должны работать два живых аккаунта - потолок
# и стоп в БД считаются по метке, и второй аккаунт молча расходовал бы чужой
# счёт. Ловится здесь, потому что файл аккаунта заводят копированием соседнего.
check-account:
	@test -n "$(label)" || { echo "нужна метка: make $(MAKECMDGOALS) label=<метка>"; exit 1; }
	@test -f $(ACCOUNTS_DIR)/$(label).env || \
		{ echo "нет файла $(ACCOUNTS_DIR)/$(label).env, заведите его по $(ACCOUNTS_DIR)/account.env.example"; exit 1; }
	@file_label=$$(sed -n 's/^ACCOUNT_LABEL=//p' $(ACCOUNTS_DIR)/$(label).env | tail -1 | tr -d '"'\''' | tr -d '[:space:]'); \
	test "$$file_label" = "$(label)" || \
		{ echo "в $(ACCOUNTS_DIR)/$(label).env стоит ACCOUNT_LABEL=$$file_label, а метка команды $(label)"; exit 1; }; \
	test "$$file_label" != "$(ACCOUNT_LABEL)" || \
		{ echo "метка $$file_label уже занята основным контуром: аккаунту нужна своя"; exit 1; }

# Поднимается в dry-run: сразу после подъёма в аккаунт ещё не вошли, и файл
# сессии пуст. Режим задаёт цель, а не файл аккаунта: своё имя переменной
# нужно потому, что общий TRANSPORT из src/.env стоит живым режимом.
account-up: check-account
	LABEL=$(label) SENDER_TRANSPORT=dry-run $(SENDER) -p $(SENDER_PROJECT) up -d --build
	@echo "отправщик $(label) поднят в dry-run; вход в аккаунт - make account-authorize label=$(label)"

# Вход в аккаунт: Telegram присылает код, ввести его может только человек.
account-authorize: check-account
	LABEL=$(label) $(SENDER) -p $(SENDER_PROJECT) exec app /app authorize

# Живой режим: контейнер пересоздаётся с TRANSPORT=live на той же сессии.
# --build обязателен: без него up пересоздаёт контейнер на прежнем образе, и
# отправщик молча остаётся на старом коде (поймано на выкате v0.9.0 23.09.2026).
account-live: check-account
	LABEL=$(label) SENDER_TRANSPORT=live $(SENDER) -p $(SENDER_PROJECT) up -d --build
	@echo "отправщик $(label) переведён в live; расход и причину простоя покажет status"

# Останавливает только этот отправщик. Том с сессией остаётся: он стирается
# отдельно и только при замене аккаунта (docs/runbook-account.md).
account-down: check-account
	LABEL=$(label) $(SENDER) -p $(SENDER_PROJECT) down

account-logs: check-account
	LABEL=$(label) $(SENDER) -p $(SENDER_PROJECT) logs -f app
