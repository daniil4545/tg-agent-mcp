package app

import (
	"context"
	"errors"
	"testing"
)

// resetLiveEnv гасит настройки боевого src/.env: Makefile подключает его и
// экспортирует в окружение go test целиком, и живой транспорт с агентским
// токеном валят конфигурацию своей причиной - тест падал бы на чужой
// переменной, а не на своём предмете.
func resetLiveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TRANSPORT", "dry-run")
	t.Setenv("MCP_AGENT_TOKEN", "")
	t.Setenv("AGENT_ALLOW_LIST", "")
}

// DEV_ALLOW_LIST держит две разные роли, и разводит их контур: список служебных
// адресатов владельца читается всегда, а рубежом транспорта остаётся только в
// dev. В prod рассылка идёт живым людям, и рубеж списка не выпустил бы наружу
// ни одного из них.
func TestAllowListIsTransportLimitInDevOnly(t *testing.T) {
	ctx := context.Background()
	for _, env := range []string{"prod", "dev"} {
		t.Setenv("ENV", env)
		t.Setenv("DATABASE_URL", "postgres://localhost/test")
		t.Setenv("ACCOUNT_LABEL", "test")
		t.Setenv("DEV_ALLOW_LIST", "@Smirnov, dev_account")
		resetLiveEnv(t)

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("%s: конфигурация: %v", env, err)
		}
		if len(cfg.OwnerAccounts) != 2 || cfg.OwnerAccounts[0] != "smirnov" {
			t.Fatalf("%s: служебные адресаты %v", env, cfg.OwnerAccounts)
		}

		_, err = NewDryRunTransport(cfg).Resolve(ctx, "stranger_one")
		if env == "prod" && err != nil {
			t.Fatalf("prod: транспорт отказал постороннему: %v", err)
		}
		if env == "dev" && !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("dev: транспорт выпустил постороннего: err = %v", err)
		}
	}
}

// Ник уезжает в запросы SQL-литералом, поэтому его форму проверяет старт
// процесса: кривая строка обязана не поднять сервис, а не оказаться в запросе.
func TestAllowListRefusesBadUsername(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ACCOUNT_LABEL", "test")
	t.Setenv("DEV_ALLOW_LIST", "smirnov, o'brien")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("конфигурация принята с ником не по форме")
	}
}

// Получатель сводки вне DEV_ALLOW_LIST обязан не поднять сервис: сводка уходит
// после закрытия окна отправки, и для такого ника она стала бы холодным
// касанием, то есть не дошла бы вовсе и молча.
func TestDigestToMustBeOwners(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ACCOUNT_LABEL", "test")
	t.Setenv("DEV_ALLOW_LIST", "smirnov, manager_two, manager_one")
	resetLiveEnv(t)

	t.Setenv("DIGEST_TO", "@Smirnov, manager_two")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("конфигурация с получателями из списка: %v", err)
	}
	if len(cfg.DigestTo) != 2 || cfg.DigestTo[0] != "smirnov" {
		t.Fatalf("получатели сводки %v", cfg.DigestTo)
	}

	t.Setenv("DIGEST_TO", "smirnov, stranger_one")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("конфигурация принята с получателем сводки вне DEV_ALLOW_LIST")
	}
}

// Агентский доступ либо настроен целиком, либо отсутствует: тихо неработающая
// половина настройки или токен агента, совпавший с токеном владельца, обязаны
// не поднять сервис.
func TestAgentAccessNeedsWholeSetting(t *testing.T) {
	cases := []struct {
		name, mcp, token, list string
		ok                     bool
	}{
		{"не настроен", "true", "", "", true},
		{"настроен", "true", "agent", "@dev_account", true},
		{"токен без списка", "true", "agent", "", false},
		{"список без токена", "true", "", "dev_account", false},
		{"токен как у владельца", "true", "owner", "dev_account", false},
		{"ник не по форме", "true", "agent", "dev_account, o'brien", false},
		{"токен без MCP", "false", "agent", "dev_account", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENV", "prod")
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("ACCOUNT_LABEL", "test")
			t.Setenv("TRANSPORT", "dry-run")
			t.Setenv("MCP_ENABLED", tc.mcp)
			t.Setenv("MCP_TOKEN", "owner")
			t.Setenv("MCP_AGENT_TOKEN", tc.token)
			t.Setenv("AGENT_ALLOW_LIST", tc.list)

			cfg, err := LoadConfig()
			if (err == nil) != tc.ok {
				t.Fatalf("ждали принятие %v, ошибка %v", tc.ok, err)
			}
			if tc.ok && tc.list != "" && (len(cfg.AgentAllowList) != 1 || cfg.AgentAllowList[0] != "dev_account") {
				t.Fatalf("список агента %v", cfg.AgentAllowList)
			}
		})
	}
}
