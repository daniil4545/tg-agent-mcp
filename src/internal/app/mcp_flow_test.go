package app

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCampaignFlowOverMCP drives a whole campaign the way an agent does: through
// an MCP client session, not by calling handlers. The answers asserted here are
// the ones the README quotes, so a change to a tool's wording fails this test
// before it makes the README untrue.
func TestCampaignFlowOverMCP(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	serverSession, err := NewMCPServer(service, nil).Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("start the server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	call := func(name string, args map[string]any) string {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := toolText(t, result, nil)
		if result.IsError {
			t.Fatalf("%s answered with a refusal:\n%s", name, text)
		}
		t.Logf("%s ->\n%s", name, text)
		return text
	}
	mustContain := func(name, text string, parts ...string) {
		t.Helper()
		for _, part := range parts {
			if !strings.Contains(text, part) {
				t.Errorf("%s: answer lacks %q:\n%s", name, part, text)
			}
		}
	}

	text := call("load_campaign", map[string]any{
		"title":    "Spring invitation",
		"template": "Здравствуйте, {имя}! Приглашаем вас на встречу в четверг.",
	})
	mustContain("load_campaign", text, "Draft created: Spring invitation (id 1).", "Next: add_recipients")

	text = call("add_recipients", map[string]any{"people": []map[string]any{
		{"contact_id": 101, "name": "Anna", "username": "anna_k"},
		{"contact_id": 102, "name": "Boris", "username": ""},
	}})
	mustContain("add_recipients", text, "Accepted: 1 of 2.", "no usable Telegram username")

	text = call("start", nil)
	mustContain("start", text, "Campaign started (id 1).", "10:00-19:00")

	text = call("status", nil)
	mustContain("status", text, "Spring invitation", "test: today 0 of 10")
}
