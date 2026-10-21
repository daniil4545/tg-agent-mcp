package app

import (
	"context"
	"errors"
	"testing"
)

// resetLiveEnv clears the settings of the live src/.env: the Makefile includes
// it and exports it whole into the go test environment, and the live transport
// with an agent token fails the configuration for its own reason - the test
// would fail on a foreign variable instead of on its own subject.
func resetLiveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TRANSPORT", "dry-run")
	t.Setenv("MCP_AGENT_TOKEN", "")
	t.Setenv("AGENT_ALLOW_LIST", "")
}

// DEV_ALLOW_LIST holds two different roles, and the environment separates them:
// the list of the owner's service recipients is always read, but it stays the
// transport guard only in dev. In prod the campaign goes to real people, and
// the list guard would let none of them out.
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
			t.Fatalf("%s: configuration: %v", env, err)
		}
		if len(cfg.OwnerAccounts) != 2 || cfg.OwnerAccounts[0] != "smirnov" {
			t.Fatalf("%s: service recipients %v", env, cfg.OwnerAccounts)
		}

		_, err = NewDryRunTransport(cfg).Resolve(ctx, "stranger_one")
		if env == "prod" && err != nil {
			t.Fatalf("prod: transport refused an outsider: %v", err)
		}
		if env == "dev" && !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("dev: transport let an outsider out: err = %v", err)
		}
	}
}

// The username goes into SQL queries as a literal, so process startup checks
// its shape: a bad string must stop the service, not end up in a query.
func TestAllowListRefusesBadUsername(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ACCOUNT_LABEL", "test")
	t.Setenv("DEV_ALLOW_LIST", "smirnov, o'brien")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("configuration accepted with a malformed username")
	}
}

// A digest recipient outside DEV_ALLOW_LIST must stop the service: the digest
// goes out after the send window closes, and for such a username it would be
// a cold touch, that is, it would not arrive at all, and silently.
func TestDigestToMustBeOwners(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ACCOUNT_LABEL", "test")
	t.Setenv("DEV_ALLOW_LIST", "smirnov, manager_two, manager_one")
	resetLiveEnv(t)

	t.Setenv("DIGEST_TO", "@Smirnov, manager_two")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("configuration with recipients from the list: %v", err)
	}
	if len(cfg.DigestTo) != 2 || cfg.DigestTo[0] != "smirnov" {
		t.Fatalf("digest recipients %v", cfg.DigestTo)
	}

	t.Setenv("DIGEST_TO", "smirnov, stranger_one")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("configuration accepted with a digest recipient outside DEV_ALLOW_LIST")
	}
}

// Agent access is either configured whole or absent: a silently dead half of
// the setting, or an agent token equal to the owner token, must stop the
// service.
func TestAgentAccessNeedsWholeSetting(t *testing.T) {
	cases := []struct {
		name, mcp, token, list string
		ok                     bool
	}{
		{"not configured", "true", "", "", true},
		{"configured", "true", "agent", "@dev_account", true},
		{"token without list", "true", "agent", "", false},
		{"list without token", "true", "", "dev_account", false},
		{"token same as owner", "true", "owner", "dev_account", false},
		{"malformed username", "true", "agent", "dev_account, o'brien", false},
		{"token without MCP", "false", "agent", "dev_account", false},
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
				t.Fatalf("expected acceptance %v, got error %v", tc.ok, err)
			}
			if tc.ok && tc.list != "" && (len(cfg.AgentAllowList) != 1 || cfg.AgentAllowList[0] != "dev_account") {
				t.Fatalf("agent list %v", cfg.AgentAllowList)
			}
		})
	}
}
