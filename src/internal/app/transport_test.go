package app

import (
	"context"
	"errors"
	"testing"
)

// Dev allow-list - последний рубеж: он стоит в транспорте, за всеми рубежами
// БД, и человека вне списка не выпускает наружу даже с уже взятой строкой.
func TestDevAllowList(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	service.cfg.DevAllowList = []string{"allowed_one"}
	transport := NewDryRunTransport(service.cfg)
	sender := senderOn(service, transport)

	startWith(t, service, person(1, "allowed_one"), person(2, "stranger_one"))
	for range 2 {
		if err := sender.step(ctx); err != nil {
			t.Fatalf("виток отправщика: %v", err)
		}
	}

	sent := transport.Sent()
	if len(sent) != 1 || sent[0].Username != "allowed_one" {
		t.Fatalf("транспорт выпустил наружу %+v", sent)
	}
	if state, _ := recipientState(t, service.pool, "allowed_one"); state != "sent" {
		t.Fatalf("разрешённый получатель в состоянии %q", state)
	}
	state, reason := recipientState(t, service.pool, "stranger_one")
	if state != "undelivered" || reason != ReasonNotAllowed {
		t.Fatalf("получатель вне списка в состоянии %q с причиной %q", state, reason)
	}
}

// Allow-list по-прежнему валит Resolve и кампанийную отправку постороннему -
// защита от того, что ради работающего чтения кто-то снимет checkAllowed с
// Resolve. Через Resolve идёт живая отправка, и рубеж на ней обязан остаться.
func TestAllowListStillHoldsCampaignSend(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	service.cfg.DevAllowList = []string{"allowed_one"}
	transport := NewDryRunTransport(service.cfg)

	if _, err := transport.Resolve(ctx, "stranger_one"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("Resolve постороннего: err = %v, ожидали ErrNotAllowed", err)
	}
	if _, err := transport.Send(ctx, Peer{Username: "stranger_one"}, "текст", 1); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("Send постороннему: err = %v, ожидали ErrNotAllowed", err)
	}
}

// Lookup читает диалог и при непустом allow-list: рубеж стоит на Resolve и
// Send, а не на чтении. Без этого dev-контур не может прочитать диалог с
// тем, кому уже нельзя писать.
func TestLookupIgnoresAllowList(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	service.cfg.DevAllowList = []string{"allowed_one"}
	transport := NewDryRunTransport(service.cfg)

	peer, err := transport.Lookup(ctx, "stranger_one")
	if err != nil {
		t.Fatalf("Lookup постороннего вне allow-list: %v", err)
	}
	if peer.Username != "stranger_one" {
		t.Fatalf("Lookup вернул Peer %+v, ожидали ник stranger_one", peer)
	}
}

// Dry-run History отдаёт то, что было отправлено через Send: подставного
// живого Telegram нет, единственная история - собственный журнал.
func TestDryRunHistoryReturnsOwnJournal(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	transport := NewDryRunTransport(service.cfg)
	peer := Peer{Username: "ivan_petrov"}
	if _, err := transport.Send(ctx, peer, "первое", 1); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := transport.Send(ctx, peer, "второе", 2); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := transport.Send(ctx, Peer{Username: "other_one"}, "чужое", 3); err != nil {
		t.Fatalf("Send: %v", err)
	}

	history, err := transport.History(ctx, peer, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("History вернула %d сообщений, ожидали 2: %+v", len(history), history)
	}
	if history[0].Text != "первое" || !history[0].Outgoing {
		t.Fatalf("первое сообщение журнала = %+v", history[0])
	}
	if history[1].Text != "второе" || !history[1].Outgoing {
		t.Fatalf("второе сообщение журнала = %+v", history[1])
	}
}

// Живой транспорт вне prod не поднимается: процесс отказывается стартовать, а
// не поднимается с молчащей отправкой.
func TestLiveRefusedOutsideProd(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ACCOUNT_LABEL", "test")
	resetLiveEnv(t)
	// Предмет теста - живой транспорт, поэтому он ставится после сброса.
	t.Setenv("TRANSPORT", "live")

	t.Setenv("ENV", "dev")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("конфиг с TRANSPORT=live поднялся в dev")
	}

	t.Setenv("ENV", "prod")
	cfg, err := LoadConfig()
	if err != nil || cfg.Transport != "live" {
		t.Fatalf("живой транспорт в prod: %v, транспорт %q", err, cfg.Transport)
	}
}
