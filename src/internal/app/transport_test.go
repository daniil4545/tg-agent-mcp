package app

import (
	"context"
	"errors"
	"testing"
)

// The dev allow-list is the last guard: it sits in the transport, behind all
// the DB guards, and does not let a person outside the list out even with an already claimed row.
func TestDevAllowList(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	service.cfg.DevAllowList = []string{"allowed_one"}
	transport := NewDryRunTransport(service.cfg)
	sender := senderOn(service, transport)

	startWith(t, service, person(1, "allowed_one"), person(2, "stranger_one"))
	for range 2 {
		if err := sender.step(ctx); err != nil {
			t.Fatalf("sender iteration: %v", err)
		}
	}

	sent := transport.Sent()
	if len(sent) != 1 || sent[0].Username != "allowed_one" {
		t.Fatalf("transport let out %+v", sent)
	}
	if state, _ := recipientState(t, service.pool, "allowed_one"); state != "sent" {
		t.Fatalf("allowed recipient in state %q", state)
	}
	state, reason := recipientState(t, service.pool, "stranger_one")
	if state != "undelivered" || reason != ReasonNotAllowed {
		t.Fatalf("recipient outside the list in state %q with reason %q", state, reason)
	}
}

// The allow-list still fails Resolve and the campaign send to an outsider -
// protection against someone removing checkAllowed from Resolve to make
// reading work. Live sending goes through Resolve, and the guard on it must stay.
func TestAllowListStillHoldsCampaignSend(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	service.cfg.DevAllowList = []string{"allowed_one"}
	transport := NewDryRunTransport(service.cfg)

	if _, err := transport.Resolve(ctx, "stranger_one"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("Resolve of an outsider: err = %v, expected ErrNotAllowed", err)
	}
	if _, err := transport.Send(ctx, Peer{Username: "stranger_one"}, "text", 1); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("Send to an outsider: err = %v, expected ErrNotAllowed", err)
	}
}

// Lookup reads a dialog even with a non-empty allow-list: the guard sits on
// Resolve and Send, not on reading. Without this the dev stack cannot read a
// dialog with someone who can no longer be written to.
func TestLookupIgnoresAllowList(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	service.cfg.DevAllowList = []string{"allowed_one"}
	transport := NewDryRunTransport(service.cfg)

	peer, err := transport.Lookup(ctx, "stranger_one")
	if err != nil {
		t.Fatalf("Lookup of an outsider outside the allow-list: %v", err)
	}
	if peer.Username != "stranger_one" {
		t.Fatalf("Lookup returned Peer %+v, expected username stranger_one", peer)
	}
}

// Dry-run History returns what was sent through Send: there is no fake live
// Telegram, the only history is its own journal.
func TestDryRunHistoryReturnsOwnJournal(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := NewDryRunTransport(service.cfg)
	peer := Peer{Username: "ivan_petrov"}
	if _, err := transport.Send(ctx, peer, "first", 1); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := transport.Send(ctx, peer, "second", 2); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := transport.Send(ctx, Peer{Username: "other_one"}, "foreign", 3); err != nil {
		t.Fatalf("Send: %v", err)
	}

	history, err := transport.History(ctx, peer, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("History returned %d messages, expected 2: %+v", len(history), history)
	}
	if history[0].Text != "first" || !history[0].Outgoing {
		t.Fatalf("first journal message = %+v", history[0])
	}
	if history[1].Text != "second" || !history[1].Outgoing {
		t.Fatalf("second journal message = %+v", history[1])
	}
}

// The live transport does not come up outside prod: the process refuses to
// start rather than coming up with a silent send.
func TestLiveRefusedOutsideProd(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ACCOUNT_LABEL", "test")
	resetLiveEnv(t)
	// The subject of the test is the live transport, so it is set after the reset.
	t.Setenv("TRANSPORT", "live")

	t.Setenv("ENV", "dev")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("config with TRANSPORT=live came up in dev")
	}

	t.Setenv("ENV", "prod")
	cfg, err := LoadConfig()
	if err != nil || cfg.Transport != "live" {
		t.Fatalf("live transport in prod: %v, transport %q", err, cfg.Transport)
	}
}
