ifneq ($(NO_DOTENV),1)
    ENV_FILE := $(firstword $(wildcard .env ../.env))
    ifneq (,$(ENV_FILE))
        -include $(ENV_FILE)
        export
    endif
endif

GOOSE := go run github.com/pressly/goose/v3/cmd/goose@v3.27.1
MIGRATIONS_DIR := ./migrations

# Database for the integration tier: by default the same local Postgres from
# docker-compose.yml that migrate-up migrates. Without a default the whole tier
# was skipped silently and ci-check stayed green on nothing.
TEST_DATABASE_URL ?= postgres://tg_agent_mcp:tg_agent_mcp@localhost:5433/tg_agent_mcp?sslmode=disable
export TEST_DATABASE_URL

LIVE := docker compose -f docker-compose.local.yml
DUMP_DIR := ../backups

# One sender per account: the same image, one compose project per label.
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

# RUN filters tests while debugging: make test RUN=TestFoo. An empty
# TEST_DATABASE_URL is a refusal, not a skip: the guard tests live in the
# database, and a green run without them proves nothing.
test:
	@test -n "$(TEST_DATABASE_URL)" || { echo "TEST_DATABASE_URL is empty: the integration tier must not be skipped, start the database (docker compose up -d) or set the variable"; exit 1; }
	go test ./... $(if $(RUN),-run $(RUN) -count=1,)

# Fast tier for the inner loop: only what does not touch the database. A slice
# and a release are closed by a full make test; test-fast does not replace it.
test-fast:
	go test -short ./... $(if $(RUN),-run $(RUN) -count=1,)

vet:
	go vet ./...

# Nothing else catches formatting: gofmt is neither in the default golangci-lint
# set nor in the edit hook, so a mismatch would reach a release silently.
fmt-check:
	@out=$$(gofmt -l ./cmd ./internal); \
	test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

lint:
	golangci-lint run --timeout 120s

# Commit ring: seconds, no database. The full set closes a slice (ci-check).
commit-check: fmt-check vet lint test-fast

ci-check: migrate-up test vet fmt-check lint build

# The Coolify stack syntax is checked before a rollout: a typo in compose would
# otherwise surface on the host, where it is most expensive to fix.
# exclude_from_hc is a Coolify extension unknown to docker compose, so the line
# is stripped from a copy of the file.
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

# DATABASE_URL is picked up from .env silently, so migrate-* can end up in
# someone else's database. Print the target (without userinfo) and require an
# explicit ALLOW_REMOTE=1 for anything that is not localhost.
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

# The command is not echoed: it carries the whole DATABASE_URL with the
# password, and check-db has already printed the migration target without
# userinfo.
migrate-up: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

# Live stack on this machine: MCP next to Claude Code, sending from a home IP.
# Settings live in src/.env, modeled on .env.example.
live-up:
	$(LIVE) up -d --build
	@echo "MCP: http://127.0.0.1:$${MCP_PORT:-8087}/mcp"

live-down:
	$(LIVE) down

live-logs:
	$(LIVE) logs -f app

# Account login: Telegram sends a code, and only a human can enter it.
# Works on a running stack in dry-run, where nothing holds the session file.
live-authorize:
	$(LIVE) exec app /app authorize

# Dump of the campaign database. It holds the record of who was already
# messaged, which is the only "one person, one message" guard across campaigns.
# Take a dump before every campaign.
live-dump:
	@mkdir -p $(DUMP_DIR)
	@$(LIVE) exec -T postgres pg_dump -U tg_agent_mcp tg_agent_mcp \
		> $(DUMP_DIR)/campaigns-$$(date +%Y%m%d-%H%M).sql
	@ls -lh $(DUMP_DIR) | tail -1

# Additional accounts: one sender per label, as its own project on top of the
# main stack's network and database. All targets take label=<label>; account
# settings live in accounts/<label>.env (not committed to git).
accounts:
	@echo "Account files:"
	@ls $(ACCOUNTS_DIR)/*.env 2>/dev/null | sed 's|.*/||; s|\.env$$||; s|^|  |' || true
	@echo "Senders:"
	@docker ps -a --filter name=tg-agent-mcp-acc- --format '  {{.Names}}\t{{.Status}}' || true

# Label guard: two live accounts must not run under one label. The cap and the
# stop flag in the database are counted per label, so a second account would
# silently spend another account's budget. Caught here because an account file
# is created by copying a neighbouring one.
check-account:
	@test -n "$(label)" || { echo "label required: make $(MAKECMDGOALS) label=<label>"; exit 1; }
	@test -f $(ACCOUNTS_DIR)/$(label).env || \
		{ echo "no file $(ACCOUNTS_DIR)/$(label).env, create it from $(ACCOUNTS_DIR)/account.env.example"; exit 1; }
	@file_label=$$(sed -n 's/^ACCOUNT_LABEL=//p' $(ACCOUNTS_DIR)/$(label).env | tail -1 | tr -d '"'\''' | tr -d '[:space:]'); \
	test "$$file_label" = "$(label)" || \
		{ echo "$(ACCOUNTS_DIR)/$(label).env sets ACCOUNT_LABEL=$$file_label, but the command label is $(label)"; exit 1; }; \
	test "$$file_label" != "$(ACCOUNT_LABEL)" || \
		{ echo "label $$file_label is already taken by the main stack: the account needs its own"; exit 1; }

# Starts in dry-run: right after startup nobody has logged in to the account
# yet and the session file is empty. The target sets the mode, not the account
# file: a dedicated variable name is needed because the shared TRANSPORT from
# src/.env is set to live.
account-up: check-account
	LABEL=$(label) SENDER_TRANSPORT=dry-run $(SENDER) -p $(SENDER_PROJECT) up -d --build
	@echo "sender $(label) started in dry-run; log in to the account with: make account-authorize label=$(label)"

# Account login: Telegram sends a code, and only a human can enter it.
account-authorize: check-account
	LABEL=$(label) $(SENDER) -p $(SENDER_PROJECT) exec app /app authorize

# Live mode: the container is recreated with TRANSPORT=live on the same session.
# --build is required: without it up recreates the container on the old image
# and the sender silently stays on old code (caught on the v0.9.0 rollout,
# 2026-09-23).
account-live: check-account
	LABEL=$(label) SENDER_TRANSPORT=live $(SENDER) -p $(SENDER_PROJECT) up -d --build
	@echo "sender $(label) switched to live; status shows spend and the reason for any idle time"

# Stops only this sender. The session volume stays: it is erased separately
# and only when the account is replaced (docs/runbook-account.md).
account-down: check-account
	LABEL=$(label) $(SENDER) -p $(SENDER_PROJECT) down

account-logs: check-account
	LABEL=$(label) $(SENDER) -p $(SENDER_PROJECT) logs -f app
