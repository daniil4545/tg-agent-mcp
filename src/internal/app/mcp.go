package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// tools holds two groups of MCP tools over a single account: six campaign tools
// that touch only the database, and four dialog tools that go through Chat into
// the live session. There is one campaign in the system, so no id is passed
// around - partial database indexes enforce the uniqueness.
type tools struct {
	service *Service
	chat    *Chat
}

type loadArgs struct {
	Template string `json:"template" jsonschema:"invitation text; the Russian placeholder {имя} is replaced with the recipient's name"`
	Title    string `json:"title" jsonschema:"campaign title used in reports"`
}

type personArgs struct {
	ContactID int64  `json:"contact_id" jsonschema:"amoCRM contact id"`
	LeadID    int64  `json:"lead_id,omitempty" jsonschema:"amoCRM lead id"`
	Name      string `json:"name" jsonschema:"name substituted into the template"`
	Username  string `json:"username" jsonschema:"Telegram username; a person without one is not added to the campaign"`
}

type addArgs struct {
	People []personArgs `json:"people" jsonschema:"batch of recipients, at most 50 per call"`
}

// The omitempty on the flags is not cosmetic: without it the schema declares the
// field required, and a call with no arguments is rejected by schema validation
// before it reaches the handler.
type stopArgs struct {
	Cancel bool `json:"cancel,omitempty" jsonschema:"true cancels the campaign together with its queue; false (default) pauses it, and start resumes it"`
}

type resumeArgs struct {
	Label string `json:"label" jsonschema:"label of the account whose stop is cleared"`
}

type statusArgs struct {
	Full       bool  `json:"full,omitempty" jsonschema:"add a per-recipient breakdown; when omitted the report has none"`
	CampaignID int64 `json:"campaign_id,omitempty" jsonschema:"campaign id from load_campaign; when omitted the current campaign is shown: the running one, else the draft, else the paused one, else the last closed one"`
}

type findArgs struct {
	Query string `json:"query" jsonschema:"a Telegram username is matched exactly, and the exact match is listed first with a mark; if there is none, similar usernames are listed with a warning. A name is searched among contacts and existing dialogs"`
}

type sendArgs struct {
	Username string `json:"username" jsonschema:"Telegram username without @"`
	Text     string `json:"text" jsonschema:"message text: non-empty, at most 4096 characters"`
}

type readArgs struct {
	Username    string `json:"username" jsonschema:"Telegram username without @"`
	Depth       int    `json:"depth,omitempty" jsonschema:"how many of the latest messages to show; default twenty, maximum one hundred"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"how many seconds to wait for a reply from the other side; default is not to wait at all, maximum 45 seconds because the client does not keep a longer call alive. To wait longer, call tg_read again: the wait continues from the same point"`
}

type dialogsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many dialogs to show; default twenty, maximum one hundred"`
}

// hint returns a pointer for MCP annotation fields where the SDK tells "not set" from false.
func hint(value bool) *bool { return &value }

// NewMCPServer builds the MCP server that manages the campaign and the dialogs.
func NewMCPServer(service *Service, chat *Chat) *mcp.Server {
	handlers := &tools{service: service, chat: chat}
	server := mcp.NewServer(&mcp.Implementation{Name: "tg-agent-mcp", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_campaign",
		Description: "Create a campaign draft with a message template. The previous draft is cancelled.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(true), IdempotentHint: false, OpenWorldHint: hint(false)},
	}, handlers.loadCampaign)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_recipients",
		Description: "Add a batch of recipients (up to 50) to the draft and get a screening report with a message preview. For service recipients from DEV_ALLOW_LIST the one-message rule does not apply; the stop list does.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(false), IdempotentHint: false, OpenWorldHint: hint(false)},
	}, handlers.addRecipients)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "start",
		Description: "Start the draft, or resume a paused campaign.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(false), IdempotentHint: false, OpenWorldHint: hint(true)},
	}, handlers.start)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "stop",
		Description: "Pause the campaign, or cancel it together with its queue.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(true), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, handlers.stop)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "resume_account",
		Description: "Clear the stop on an account after a human has checked it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, handlers.resumeAccount)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "status",
		Description: "Campaign counters, accounts with stops and a message preview; campaign_id shows a named campaign, full adds a per-recipient breakdown.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(false)},
	}, handlers.status)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_find",
		Description: "Find a person: an exact username, or a name among contacts and existing dialogs. An exact username match is listed first with a mark; without one, similar usernames are listed with a warning. The search sends nothing to anyone.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(true)},
	}, handlers.find)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_send",
		Description: "Send a direct message to a person by hand. The first touch of a stranger goes through the send window and the daily cap; after sending, the person is closed to campaigns. For service recipients from DEV_ALLOW_LIST the window, the cap and the one-message rule do not apply; the stop list does.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(false), IdempotentHint: false, OpenWorldHint: hint(true)},
	}, handlers.send)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_read",
		Description: "Show a private dialog and, if asked, wait for a reply from the other side; groups and channels are not read.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(true)},
	}, handlers.read)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_dialogs",
		Description: "List dialogs - private chats, groups and channels - with unread counts and the last message; each row names its kind, and only a person can be written to (tg_send).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(true)},
	}, handlers.dialogs)

	return server
}

// Token roles. UserID must be non-empty: the SDK binds a session to its token
// through it, and an empty value switches that binding off.
const (
	roleOwner = "owner"
	roleAgent = "agent"
)

// MCPHandler is the management endpoint: streamable HTTP behind a bearer token.
// There are two roles: the owner sees every tool and confirms mutations on the
// MCP client side, the agent sees the two dialog tools, and the service itself
// guards the agent's recipients.
func MCPHandler(service *Service, chat *Chat) http.Handler {
	owner := NewMCPServer(service, chat)
	agent := NewAgentServer(service, chat)
	streamable := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		info := auth.TokenInfoFromContext(req.Context())
		if info == nil {
			return nil
		}
		switch info.UserID {
		case roleOwner:
			return owner
		case roleAgent:
			return agent
		}
		return nil
	}, nil)
	// AllowMissingExpiration: the token is static and carries no expiry;
	// without this flag the middleware rejects it as expired.
	verify := staticTokens(service.cfg.MCPToken, service.cfg.MCPAgentToken)
	guard := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	return guard(streamable)
}

// staticTokens compares the presented token with the configured ones in
// constant time: a character-by-character comparison leaks the token through
// response timing. An empty agent token opens no role.
func staticTokens(owner, agent string) auth.TokenVerifier {
	return func(_ context.Context, presented string, _ *http.Request) (*auth.TokenInfo, error) {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(owner)) == 1 {
			return &auth.TokenInfo{UserID: roleOwner}, nil
		}
		if agent != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(agent)) == 1 {
			return &auth.TokenInfo{UserID: roleAgent}, nil
		}
		// An authorization failure is visible only from outside: the client gets
		// a 401 with no explanation, and without this entry the cause cannot be
		// told from a service that is down. The token is not logged, only its
		// length - a mismatched token is usually empty or truncated.
		slog.Warn("mcp token rejected", "presented_len", len(presented))
		return nil, auth.ErrInvalidToken
	}
}

// errAgentRecipient is the refusal of agent access: the recipient is not a test account.
var errAgentRecipient = errors.New("username is outside AGENT_ALLOW_LIST: agent access writes to and reads from test accounts only")

// agentTools are the dialog tools behind the list of test accounts. The
// username is checked before Chat with the same normalization Chat uses:
// otherwise a form with @ or a link would diverge from the list.
type agentTools struct {
	tools   *tools
	allowed map[string]bool
}

func (a *agentTools) check(username string) error {
	if !a.allowed[normalizeUsername(username)] {
		slog.Warn("agent recipient refused", "username", username)
		return errAgentRecipient
	}
	return nil
}

func (a *agentTools) send(ctx context.Context, req *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
	if err := a.check(args.Username); err != nil {
		return nil, nil, err
	}
	return a.tools.send(ctx, req, args)
}

func (a *agentTools) read(ctx context.Context, req *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
	if err := a.check(args.Username); err != nil {
		return nil, nil, err
	}
	return a.tools.read(ctx, req, args)
}

// NewAgentServer builds the agent-access server. A new tool does not land here
// by itself: it is opened to the agent only deliberately.
func NewAgentServer(service *Service, chat *Chat) *mcp.Server {
	handlers := &agentTools{
		tools:   &tools{service: service, chat: chat},
		allowed: allowedSet(service.cfg.AgentAllowList),
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "tg-agent-mcp-agent", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_send",
		Description: "Send a direct message to a test account from AGENT_ALLOW_LIST to check the bot. The service rejects any other username.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: hint(false), IdempotentHint: false, OpenWorldHint: hint(true)},
	}, handlers.send)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_read",
		Description: "Show the private dialog with a test account from AGENT_ALLOW_LIST and, if asked, wait for its reply.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: hint(false), IdempotentHint: true, OpenWorldHint: hint(true)},
	}, handlers.read)

	return server
}

func (t *tools) loadCampaign(ctx context.Context, _ *mcp.CallToolRequest, args loadArgs) (*mcp.CallToolResult, any, error) {
	id, err := t.service.LoadCampaign(ctx, args.Template, args.Title)
	if err != nil {
		return nil, nil, toolError(err)
	}
	slog.Info("campaign loaded", "campaign_id", id, "title", strings.TrimSpace(args.Title))

	// The template is printed generously, with line breaks: reading "what the
	// person will receive" by eye is the only way to catch a doubled brace on the
	// third line. It is cut only on long messages, and the length is always stated.
	template := strings.TrimSpace(args.Template)
	var text strings.Builder
	// The length stated is the one the refusal checks: with a reserve for the
	// name and counted the way Telegram counts. The bare template length would
	// promise headroom that does not exist - fifty more characters would be
	// refused with a different, larger number.
	used, budget := renderedLen(template)
	fmt.Fprintf(&text, "Draft created: %s (id %d).\n", strings.TrimSpace(args.Title), id)
	if budget > 0 {
		fmt.Fprintf(&text, "The template with the name substituted takes %d of %d characters by Telegram's count (%d of them reserved for the name):\n%s\n\n",
			used, maxTextLen, budget, cutRunes(template, templateShown))
	} else {
		fmt.Fprintf(&text, "The template takes %d of %d characters by Telegram's count:\n%s\n\n",
			used, maxTextLen, cutRunes(template, templateShown))
	}
	// A template without a placeholder is legal, but a mailing with no form of
	// address is not what is usually meant, and it must be seen before start.
	if hasName(template) {
		fmt.Fprintf(&text, "The template has the %s placeholder: a recipient without a name gets the message with no form of address, comma included. Check that the text reads well that way.\n", namePlaceholder)
	} else {
		fmt.Fprintf(&text, "The template has no %s placeholder: messages go out with no form of address by name.\n", namePlaceholder)
	}
	fmt.Fprintf(&text, "Next: add_recipients in batches of up to %d people, then start.", maxBatch)
	return textResult(text.String())
}

// addRecipients works with the single draft: the tool takes no campaign id,
// because there is one draft in the system and a partial index enforces it.
func (t *tools) addRecipients(ctx context.Context, _ *mcp.CallToolRequest, args addArgs) (*mcp.CallToolResult, any, error) {
	campaignID, err := t.service.draftCampaign(ctx)
	if err != nil {
		return nil, nil, toolError(err)
	}

	// personArgs repeats Person field for field and differs only in schema tags:
	// the domain type need not know the format of the tool arguments.
	people := make([]Person, 0, len(args.People))
	for _, item := range args.People {
		people = append(people, Person(item))
	}
	report, err := t.service.AddRecipients(ctx, campaignID, people)
	if err != nil {
		return nil, nil, toolError(err)
	}

	slog.Info("recipients added", "campaign_id", campaignID,
		"offered", len(args.People), "accepted", report.Accepted, "skipped", report.Skipped)

	var text strings.Builder
	fmt.Fprintf(&text, "Accepted: %d of %d.\n", report.Accepted, len(args.People))
	writeSkipped(&text, report.Skipped)

	// The report is built for the draft that was inserted into, not for the
	// "current" campaign: with a live active campaign, checking the list would
	// go by another campaign's counters.
	status, err := t.service.StatusOf(ctx, campaignID, false)
	if err != nil {
		return nil, nil, err
	}
	writeCampaign(&text, status)
	writePreview(&text, status)
	return textResult(text.String())
}

func (t *tools) start(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	id, err := t.service.StartCampaign(ctx)
	if err != nil {
		return nil, nil, toolError(err)
	}
	slog.Info("campaign started", "campaign_id", id)
	status, err := t.service.StatusOf(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}

	// The report below names what exactly was started: with a "draft plus pause"
	// pair, start takes the draft, and the operator must see which campaign went
	// and what is left waiting.
	var text strings.Builder
	fmt.Fprintf(&text, "Campaign started (id %d). Sending runs in the window %s-%s on weekdays, within the account's daily cap.\n",
		id, t.service.cfg.SendWindowStart, t.service.cfg.SendWindowEnd)
	writeCampaign(&text, status)
	// The line above is the rule, this one is the state of things now: a start at
	// 22:10 on a Friday would otherwise look like sending in progress. The reason
	// is already computed in status, so no queries are added.
	writeIdle(&text, status)
	writeAccounts(&text, status)
	return textResult(text.String())
}

func (t *tools) stop(ctx context.Context, _ *mcp.CallToolRequest, args stopArgs) (*mcp.CallToolResult, any, error) {
	id, err := t.service.StopCampaign(ctx, args.Cancel)
	if err != nil {
		return nil, nil, toolError(err)
	}
	slog.Info("campaign stopped", "campaign_id", id, "cancelled", args.Cancel)
	// The report is about the campaign that was stopped: a draft may sit next to
	// it, and the "current" campaign in the answer would already be the draft.
	status, err := t.service.StatusOf(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}

	var text strings.Builder
	if args.Cancel {
		text.WriteString("Campaign cancelled, queue cleared: it cannot be resumed.\n")
	} else {
		text.WriteString("Campaign paused. Nothing is being sent; start resumes from the same place.\n")
	}
	writeCampaign(&text, status)
	return textResult(text.String())
}

func (t *tools) resumeAccount(ctx context.Context, _ *mcp.CallToolRequest, args resumeArgs) (*mcp.CallToolResult, any, error) {
	label := strings.TrimSpace(args.Label)
	// Reading the state and clearing it are one transaction under a row lock: a
	// stop placed between the read and the clear would otherwise be cleared
	// silently, while the answer and the log named the previous reason. The
	// answer "stop cleared" on a live account is impossible too - the same
	// transaction decides whether to clear.
	reason, stopped, err := t.service.ResumeAccount(ctx, label)
	if err != nil {
		return nil, nil, t.unknownAccount(ctx, label, err)
	}
	if !stopped {
		// Nothing is promised about sending here: the transaction looked at one
		// stop, and the queue can be quiet because of the window, the cap or the
		// lack of a campaign.
		return textResult(fmt.Sprintf("Account %q has no stop: nothing to clear. status shows whether sending is running.", label))
	}
	// Clearing a stop is the only action a human takes by hand after reviewing
	// an account: in the log it must stand next to the stop itself.
	slog.Info("account resumed", "account", label, "stop_reason", reason)
	return textResult(fmt.Sprintf("Stop cleared on account %q (it was set because of %s). Sending resumes in the next window.",
		label, reason))
}

func (t *tools) status(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
	var status StatusReport
	var err error
	// Any non-zero id goes to StatusOf, a negative one included: "parameter not
	// passed" is only zero, and a report on the current campaign in answer to an
	// explicit typo in the id reads as a report on the requested one.
	if args.CampaignID != 0 {
		status, err = t.service.StatusOf(ctx, args.CampaignID, args.Full)
	} else {
		status, err = t.service.Status(ctx, args.Full)
	}
	if err != nil {
		return nil, nil, err
	}
	// StatusOf on an unknown id returns a report without a campaign: accounts in
	// place of the requested totals read as "the campaign is empty", not as a
	// typo in the id.
	if args.CampaignID != 0 && status.Campaign == nil {
		return nil, nil, fmt.Errorf("there is no campaign with id %d: ids come from the load_campaign answer, and without campaign_id status shows the current one",
			args.CampaignID)
	}

	var text strings.Builder
	writeCampaign(&text, status)
	writeIdle(&text, status)
	writePreview(&text, status)
	writeAccounts(&text, status)
	if args.Full {
		writeRows(&text, status)
	}
	return textResult(text.String())
}

func (t *tools) find(ctx context.Context, _ *mcp.CallToolRequest, args findArgs) (*mcp.CallToolResult, any, error) {
	found, err := t.chat.Find(ctx, args.Query)
	if err != nil {
		return nil, nil, toolError(err)
	}
	var text strings.Builder
	t.writeContour(&text)
	// The query is brought to the same form telegram.SearchUsers searched with:
	// a raw "@Ivan" would fail the username shape check and print "@@" in the text.
	needle := telegram.CleanQuery(args.Query)
	writeContacts(&text, needle, found)
	return textResult(text.String())
}

// send reports the price of the touch, not the fact of sending: the journal
// closes the person to all future campaigns, and the caller makes that decision.
func (t *tools) send(ctx context.Context, _ *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
	sent, err := t.chat.Send(ctx, args.Username, args.Text)
	if err != nil {
		return nil, nil, toolError(err)
	}

	var text strings.Builder
	t.writeContour(&text)
	fmt.Fprintf(&text, "Sent to @%s", sent.Username)
	if sent.UserID > 0 {
		fmt.Fprintf(&text, " (id %d)", sent.UserID)
	}
	text.WriteString(".\nTouch recorded: this person will not be included in any campaign from now on.\n")
	if sent.Cold {
		text.WriteString("This is the first touch of a stranger: it counted toward the account's daily cap.\n")
	}
	text.WriteString("Next: tg_read with wait_seconds waits for the reply.\n")
	return textResult(text.String())
}

func (t *tools) read(ctx context.Context, _ *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
	talk, err := t.chat.Read(ctx, args.Username, args.Depth, time.Duration(args.WaitSeconds)*time.Second)
	if err != nil {
		return nil, nil, toolError(err)
	}
	var text strings.Builder
	t.writeContour(&text)
	// The count goes by the history, not by everything printed: depth cut the
	// history, and a reply that arrived during the wait comes on top of the cut.
	writeHistoryCap(&text, args.Depth, len(talk.Messages))
	// This line says nothing about the wait itself: how long it really lasted is
	// told by writeConversation, and "waited 45" next to "reply after 6 seconds"
	// would be two mutually exclusive numbers in a row.
	if args.WaitSeconds > int(maxWait.Seconds()) {
		fmt.Fprintf(&text, "Asked to wait %d seconds, the limit of one call is %d: the client does not keep a longer call alive.\n",
			args.WaitSeconds, int(maxWait.Seconds()))
	}
	writeConversation(&text, talk)
	return textResult(text.String())
}

func (t *tools) dialogs(ctx context.Context, _ *mcp.CallToolRequest, args dialogsArgs) (*mcp.CallToolResult, any, error) {
	list, err := t.chat.Dialogs(ctx, args.Limit)
	if err != nil {
		return nil, nil, toolError(err)
	}
	var text strings.Builder
	t.writeContour(&text)
	writeDialogsCap(&text, args.Limit, len(list))
	writeDialogs(&text, list)
	return textResult(text.String())
}

// writeHistoryCap names the cut of a dialog read. The caveat is needed not only
// when an explicit depth exceeds the limit: the most common call has no
// argument and is cut by the default, and a silently truncated dialog reads as
// a whole one. It stays silent where the history is shorter than the depth: the
// dialog is shown in full, and "not everything is shown" would send the agent
// looking for a remainder that does not exist.
func writeHistoryCap(text *strings.Builder, asked, shown int) {
	if asked > maxHistory {
		fmt.Fprintf(text, "Asked for the last %d messages, the limit is %d.\n", asked, maxHistory)
	}
	depth := capped(asked, defaultHistory, maxHistory)
	if shown < depth {
		return
	}
	if depth < maxHistory {
		fmt.Fprintf(text, "Not everything is shown: the depth of %d is used up, earlier messages are beyond the cut. "+
			"To go further, call tg_read with depth up to %d.\n", depth, maxHistory)
		return
	}
	fmt.Fprintf(text, "Not everything is shown: the depth of %d is used up, earlier messages are beyond the cut, "+
		"and the tool does not read deeper than the limit.\n", depth)
}

// writeDialogsCap names the cut of the dialog list: without this line "shown
// 12" reads as "12 in total", and the agent concludes "the person is not in the
// dialogs" from the head of the list. It stays silent where the list is shorter
// than the limit: all the account's dialogs are shown, and advice to call
// tg_dialogs with a larger limit would be false.
func writeDialogsCap(text *strings.Builder, asked, shown int) {
	if asked > maxHistory {
		fmt.Fprintf(text, "Asked for %d dialogs, the limit is %d.\n", asked, maxHistory)
	}
	limit := capped(asked, defaultHistory, maxHistory)
	if shown < limit {
		return
	}
	fmt.Fprintf(text, "Showing the %d most recent dialogs; there may be more further down the list.", limit)
	if limit < maxHistory {
		fmt.Fprintf(text, " To see more, call tg_dialogs with limit up to %d.\n", maxHistory)
		return
	}
	text.WriteString(" The tool does not look deeper than the limit: look for a person who is not visible by exact username with tg_find.\n")
}

// unknownAccount translates the refusal of an account read. An unknown label is
// its only cause that depends on the caller, and it must say where to find the
// right one; other failures (database unavailable) pass through as they are,
// not disguised as a typo in the label.
func (t *tools) unknownAccount(ctx context.Context, label string, err error) error {
	var known int
	if scanErr := t.service.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE label = $1`, label).Scan(&known); scanErr != nil || known > 0 {
		return toolError(err)
	}
	return fmt.Errorf("the service does not know an account labelled %q: status lists the account labels, call resume_account with one of them",
		label)
}

// writeContour names the stand-in environment on the first line: dry-run
// returns its own send journal as history, and without this line the agent
// would take it for a real dialog.
func (t *tools) writeContour(text *strings.Builder) {
	if t.service.cfg.Transport != "live" {
		text.WriteString("Dry-run mode: there is no live Telegram, the answer is a stand-in.\n")
	}
}

// draftCampaign finds the single draft. The campaign tools pass no id: there
// is one draft in the system, and a partial unique index enforces it, not an
// agreement by the caller.
func (s *Service) draftCampaign(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM campaigns WHERE status = 'draft'`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, withNear(ctx, s.pool, ErrNoDraft, "active", "stopped")
	}
	if err != nil {
		return 0, fmt.Errorf("find draft: %w", err)
	}
	return id, nil
}

// toolError translates a domain refusal into text from which the operator can
// tell what to do next: otherwise the MCP client shows the owner a wrapped
// query error.
func toolError(err error) error {
	switch {
	case errors.Is(err, ErrNoDraft):
		return noDraftError(err)
	case errors.Is(err, ErrNothingToStart):
		return errors.New("nothing to start: there is neither a draft nor a paused campaign. Create a draft with load_campaign")
	case errors.Is(err, ErrNothingToStop):
		return nothingToStopError(err)
	case errors.Is(err, ErrNothingToCancel):
		return errors.New("nothing to cancel: there is no running campaign, no draft and no paused campaign")
	case errors.Is(err, ErrActiveCampaign):
		return errors.New("a campaign is already running: stop it with stop first, then start the next one")
	case errors.Is(err, ErrManyCampaigns):
		return errors.New("more than one campaign can be started: remove the extra ones with stop and cancel")
	case errors.Is(err, ErrEmptyDraft):
		return errors.New("the draft has no usable recipients: add them with add_recipients")
	case errors.Is(err, ErrUnknownPlaceholder):
		return unknownPlaceholderError(err)
	case errors.Is(err, ErrTemplateTooLong):
		return longTemplateError(err)
	case errors.Is(err, ErrNotDraft):
		return errors.New("only a draft accepts recipients: a started campaign does not take additions to its list")
	case errors.Is(err, ErrBatchTooBig):
		return fmt.Errorf("the batch has more than %d people: split the list into parts", maxBatch)
	case errors.Is(err, ErrNoUsername):
		return errors.New("the username is empty or does not look like a Telegram username: 5-32 characters, Latin letters, digits and underscore, not at the end and not doubled; no @")
	case errors.Is(err, ErrEmptyText):
		return errors.New("the text is empty: say what to send")
	case errors.Is(err, ErrTextTooLong):
		return fmt.Errorf("the message is longer than %d characters: split it into parts", maxTextLen)
	case errors.Is(err, ErrNotPrivate):
		return errors.New("this is not a private chat: the tools work with private dialogs only, groups and channels are not supported")
	case errors.Is(err, ErrAccountStopped):
		return accountStopError(err)
	case errors.Is(err, ErrStopList):
		return errors.New("the person is on the stop list: nothing is sent to them - neither by a campaign nor by hand")
	case errors.Is(err, ErrCampaignSending):
		return campaignHoldError(err)
	case errors.Is(err, ErrDirectPending):
		return fmt.Errorf("the previous message to this person is in an unknown state: it may have gone out. "+
			"Wait %d minutes and look at the dialog with tg_read", waitMinutes(err))
	case errors.Is(err, ErrColdWindow):
		return errors.New("the first touch of a stranger goes out only in the send window on weekdays: wait for the window; a reply in an ongoing dialog is not limited by time")
	case errors.Is(err, ErrColdLimit):
		return errors.New("the account's daily cap is used up: the first touch of a stranger waits until tomorrow; a reply in an ongoing dialog goes through")
	case errors.Is(err, ErrNotAllowed):
		return errors.New("username is outside DEV_ALLOW_LIST: in this environment only the listed usernames can be written to")
	default:
		var failure *telegram.Failure
		if errors.As(err, &failure) {
			return telegramError(failure)
		}
		return err
	}
}

// unknownPlaceholderError lists the foreign placeholders: without the list the
// person hunts for the typo by eye across the whole template.
func unknownPlaceholderError(err error) error {
	var bad *badTemplate
	if !errors.As(err, &bad) {
		return fmt.Errorf("the template has curly braces the service does not understand: it knows one placeholder - %s",
			namePlaceholder)
	}
	quoted := make([]string, 0, len(bad.spots))
	for _, spot := range bad.spots {
		quoted = append(quoted, fmt.Sprintf("%q", spot))
	}
	// Not all the places are shown: without this caveat the person fixes three,
	// gets the refusal again and reads it as a new defect.
	shown := ""
	if bad.total > len(bad.spots) {
		shown = fmt.Sprintf(" (first %d of %d)", len(bad.spots), bad.total)
	}
	// The advice depends on where the template lives: text in an argument is
	// fixed in place, but the template of an existing campaign is edited by no
	// tool, and promising "remove the braces" there would advise the impossible.
	if bad.stored {
		return fmt.Errorf("the service could not parse this campaign's template%s: %s. Nothing will be sent for it. "+
			"No tool edits the template of an existing campaign: rebuild the campaign - "+
			"load_campaign with the corrected text, then add_recipients (the list has to be collected again), then start; "+
			"remove the broken one with stop and cancel",
			shown, strings.Join(quoted, ", "))
	}
	return fmt.Errorf("the template has curly braces the service does not understand%s: %s. "+
		"It knows one placeholder - %s; everything else, including double braces and an unclosed brace, goes into the message as is, "+
		"so remove the extra braces or close the placeholder",
		shown, strings.Join(quoted, ", "), namePlaceholder)
}

// longTemplateError names both numbers: without them "shorten it" does not say
// how much to cut.
func longTemplateError(err error) error {
	var long *longTemplate
	if !errors.As(err, &long) {
		return fmt.Errorf("the template does not fit into a Telegram message: the limit is %d characters", maxTextLen)
	}
	if long.budget == 0 {
		return fmt.Errorf("the template does not fit into a Telegram message: it takes %d of %d characters. Excess characters: %d",
			long.used, long.limit, long.used-long.limit)
	}
	return fmt.Errorf("the template does not fit into a Telegram message: it takes %d of %d characters, %d of them a rough reserve for the name "+
		"(%d per placeholder; a real name is shorter or longer, and the service checks its length on import). "+
		"Excess characters: %d",
		long.used, long.limit, long.budget, nameBudget, long.used-long.limit)
}

// noDraftError tells an empty system from a live campaign next to it. The
// advice "create a draft" is right in both cases, but with a running campaign
// it sounds like "everything is lost": the agent must read that the campaign is
// intact and that it is not the one that takes additions to its list.
func noDraftError(err error) error {
	var near *nearCampaign
	if !errors.As(err, &near) {
		return errors.New("there is no draft: create one with load_campaign, then add recipients")
	}
	return fmt.Errorf("there is no draft, and only a draft accepts recipients. Next to it is campaign %d %q (%s), whose list can no longer be extended: "+
		"load_campaign creates a draft next to it and does not touch the campaign itself",
		near.id, near.title, near.status)
}

// nothingToStopError answers from what stands nearby: a repeated stop on an
// already paused campaign would otherwise read as "the campaign is gone",
// although start resumes it.
func nothingToStopError(err error) error {
	var near *nearCampaign
	if !errors.As(err, &near) {
		return errors.New("nothing to stop: no campaign is running")
	}
	if near.status != "stopped" {
		return fmt.Errorf("nothing to stop: no campaign is running, there is only draft %d %q next to it - "+
			"stop with cancel removes it", near.id, near.title)
	}
	// The advice must take the draft nearby into account: start takes the draft
	// before the pause, cancel hits it before the pause, and "resume with start"
	// would launch the wrong campaign, while "remove with cancel" would wipe the
	// collected list.
	if near.draft != 0 {
		return fmt.Errorf("nothing to stop: no campaign is running, campaign %d %q is paused. "+
			"But there is also draft %d, and both commands act on it: start will launch the draft, stop with cancel will cancel the draft. "+
			"The queue reaches campaign %d only after the started campaign is stopped: start refuses while a campaign is running",
			near.id, near.title, near.draft, near.id)
	}
	// Neighbouring pauses change the advice too: with several stopped campaigns
	// and no draft, start does not choose by itself and refuses - "start will
	// resume it" cannot be promised.
	if near.pauses > 1 {
		return fmt.Errorf("nothing to stop: no campaign is running, and several campaigns are paused (%d), the first is %d %q. "+
			"With several pauses start refuses to choose: remove the extra ones with stop and cancel, then start resumes the remaining one",
			near.pauses, near.id, near.title)
	}
	return fmt.Errorf("nothing to stop: no campaign is running, and campaign %d %q is already paused - "+
		"start resumes it, and stop with cancel removes it together with its queue",
		near.id, near.title)
}

// campaignHoldError names the state of the campaign that holds the person and
// the action that will work against it. There are three states, not two. A
// running campaign sorts itself out, but only while the queue moves: outside
// the send window the row does not move until the next weekday morning, and
// "retry in a few minutes" sends the agent into a wall. A paused campaign never
// sorts itself out, and cancel hits campaigns in the order active, draft,
// stopped - with a live neighbour it removes that one, not this one. A closed
// campaign (cancelled or finished) gets here only through a live claim: the
// sender is carrying the row right now, and there is no action on it at all.
//
// "A stuck claim in status" is not mentioned here: a row returned to the queue
// has an empty claimed_at, and that counter does not see it - the advice would
// send the agent after a sign that does not exist.
func campaignHoldError(err error) error {
	var hold *campaignHold
	if !errors.As(err, &hold) {
		return errors.New("a campaign is writing to this person: retry later, otherwise they will get two messages")
	}
	switch hold.status {
	case "active":
		// A running campaign with a broken template does not move the queue at
		// all, and "retry in a few minutes" would send the agent in a second circle.
		if hold.broken {
			return fmt.Errorf("the person is held by a row of campaign %d %q, but its queue is stalled: the campaign's template could not be parsed. "+
				"It will not sort itself out - remove the campaign with stop and cancel, and the person is freed",
				hold.id, hold.title)
		}
		if hold.queueOpensAt.IsZero() {
			return fmt.Errorf("campaign %d %q is writing to this person right now: the queue will close the row itself, retry in a few minutes",
				hold.id, hold.title)
		}
		return fmt.Errorf("the person is held by a row of running campaign %d %q, but the queue is stalled outside the send window: "+
			"the row will not move before %s MSK, retrying until then is useless",
			hold.id, hold.title, hold.queueOpensAt.In(moscowZone).Format("02.01 15:04"))
	case "stopped":
		// start does not resume a campaign with a broken template - the same guard
		// that stands for a running one - and the advice "resume with start" would
		// run into a refusal. One way out is left: remove it together with the queue.
		if hold.broken {
			return fmt.Errorf("the person is held by a row of paused campaign %d %q, and the campaign's template could not be parsed: "+
				"start will not resume it, and it will not sort itself out - remove it together with its queue with stop and cancel; "+
				"cancel hits campaigns in the order active, draft, stopped, so with a live active campaign or a draft call it again until the queue reaches campaign %d",
				hold.id, hold.title, hold.id)
		}
		return fmt.Errorf("the person is held by a row of paused campaign %d %q, and it will not sort itself out: "+
			"resume the campaign with start, or remove it together with its queue with stop and cancel - "+
			"cancel hits campaigns in the order active, draft, stopped, so with a live active campaign or a draft call it again until the queue reaches campaign %d",
			hold.id, hold.title, hold.id)
	default:
		return fmt.Errorf("the person is held by a row of campaign %d %q, which is already closed (%s), but the sender is carrying the row right now: "+
			"there is no action on it - the sender will close it itself, and an abandoned claim goes stale in no more than %d minutes",
			hold.id, hold.title, hold.status, minutesUp(hold.staleAfter))
	}
}

// accountStopError follows the reason of the stop. A Telegram restriction is
// cleared by a human with resume_account after checking the account, but for a
// revoked session the same advice goes in circles: the stop is cleared, the
// daemon gets the same error, the stop is set again. There is one way out of a
// revoked session - signing in again with the authorize subcommand.
func accountStopError(err error) error {
	var stop *accountStop
	if !errors.As(err, &stop) {
		return errors.New("the account is stopped: check it by hand, then clear the stop with resume_account")
	}
	if stop.reason == telegram.KindAuthForbidden {
		return fmt.Errorf("the account is stopped: the Telegram session is no longer authorized (%s). "+
			"resume_account will not help here - the stop will be set again; sign in again with the authorize subcommand (docs/first-launch.md), then clear the stop",
			stop.reason)
	}
	return fmt.Errorf("the account is stopped by a Telegram restriction (%s): check it by hand, then clear the stop with resume_account",
		stop.reason)
}

// waitMinutes is how many more minutes the delivery of the previous touch is
// still counted as unknown. The period is set by a service setting, so it
// travels inside the error.
func waitMinutes(err error) int {
	var pending *pendingWait
	if !errors.As(err, &pending) {
		return 1
	}
	return minutesUp(pending.left)
}

// minutesUp is a period in minutes rounded up: the advice "wait 0 minutes"
// would allow an immediate retry.
func minutesUp(left time.Duration) int {
	value := int((left + time.Minute - time.Nanosecond) / time.Minute)
	if value < 1 {
		return 1
	}
	return value
}

// telegramError translates a Telegram refusal into a decision for the caller.
// The MTProto code goes into the text: without it the agent sees "temporary
// Telegram error", which implies neither a retry nor a refusal.
func telegramError(failure *telegram.Failure) error {
	switch failure.Kind {
	case telegram.KindNotReady:
		return withCode("the Telegram session is not up yet, retry in a few seconds; if it persists, see make live-logs", failure)
	case telegram.KindPeerForbidden:
		return withCode("direct messages are closed, the username does not exist, or we are blocked", failure)
	case telegram.KindNotPerson:
		return withCode("the username belongs to a group or channel: the service writes only to people. The dialog list shows them, but sending there is closed by a guard", failure)
	case telegram.KindFloodWait:
		// A long FLOOD_WAIT does not get here: it stops the account, and tg_send
		// answers with the stop. What remains is a short pause that can really be
		// waited out - it is said outright, otherwise the agent waits, retries and
		// gets "account stopped".
		if failure.RetryAt.IsZero() {
			return withCode("Telegram asks to wait and retry, the account is not stopped", failure)
		}
		return withCode(fmt.Sprintf("Telegram asks to wait until %s MSK and retry, the account is not stopped",
			failure.RetryAt.In(moscowZone).Format("15:04")), failure)
	case telegram.KindPeerFlood:
		return withCode("the account is restricted from messaging strangers: sending from it is stopped and is cleared only by resume_account after a human check", failure)
	case telegram.KindBadRequest:
		return withCode("Telegram rejected the request itself: repeating it gets the same refusal. "+
			"No message was created - fix the text (too long or empty) or the search query and call the tool again", failure)
	default:
		return failure
	}
}

func withCode(text string, failure *telegram.Failure) error {
	if failure.Type == "" {
		return errors.New(text)
	}
	return fmt.Errorf("%s (Telegram answered %s)", text, failure.Type)
}

func textResult(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

// stateOrder fixes the order of counters in the answer: map order is random,
// and two calls in a row would look like different reports.
var stateOrder = []string{"planned", "sent", "replied", "undelivered", "skipped", "cancelled"}

var stateLabels = map[string]string{
	"planned":     "queued",
	"sent":        "sent",
	"replied":     "replied",
	"undelivered": "undelivered",
	"skipped":     "removed by a guard",
	"cancelled":   "cancelled",
}

var reasonLabels = map[string]string{
	ReasonBadContact:   "no amoCRM contact id",
	ReasonNoUsername:   "no usable Telegram username",
	ReasonNoName:       "no name (campaigns before 07.09.2026; now the message goes out with no form of address)",
	ReasonWroteFirst:   "wrote first, before their turn in the queue came",
	ReasonTextTooLong:  "the message with their name is longer than the Telegram limit",
	ReasonNameBraces:   "the name contains a curly brace, which would end up in the message",
	ReasonDuplicate:    "duplicate: already in the campaign or twice in the batch",
	ReasonStopList:     "stop list",
	ReasonAlreadySent:  "already received a message",
	ReasonCampaignGone: "campaign cancelled",
	ReasonNotAllowed:   "outside the dev allow list",
	// Two reasons instead of one: the report may not claim delivery where the
	// service does not know it.
	ReasonDirectSent:    "already messaged by hand through tg_send",
	ReasonDirectPending: "messaged by hand, delivery unknown",
}

func label(labels map[string]string, key string) string {
	if text, ok := labels[key]; ok {
		return text
	}
	return key
}

func writeSkipped(text *strings.Builder, skipped map[string]int) {
	reasons := make([]string, 0, len(skipped))
	for reason, count := range skipped {
		if count > 0 {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return
	}
	sort.Strings(reasons)
	text.WriteString("Screened out:\n")
	for _, reason := range reasons {
		fmt.Fprintf(text, "  %s: %d\n", label(reasonLabels, reason), skipped[reason])
	}
}

func writeCampaign(text *strings.Builder, status StatusReport) {
	if status.Campaign == nil {
		text.WriteString("No campaigns: start with load_campaign.\n")
		return
	}
	fmt.Fprintf(text, "Campaign: %s (id %d), status %s.\n",
		status.Campaign.Title, status.Campaign.ID, status.Campaign.Status)
	fmt.Fprintf(text, "Rows total: %d", status.Campaign.Total)
	for _, state := range stateOrder {
		if count := status.Campaign.States[state]; count > 0 {
			fmt.Fprintf(text, ", %s %d", label(stateLabels, state), count)
		}
	}
	text.WriteString("\n")
	// A broken template is a cause of idling, not a campaign state: the status
	// stays what the person left it, so the queue stands silently, and this very
	// line has to say so.
	if len(status.Idle.Braces) > 0 {
		fmt.Fprintf(text, "This campaign's template could not be parsed (%q): nothing will be sent for it. "+
			"Rebuild the campaign with load_campaign and add_recipients",
			status.Idle.Braces[0])
		// Removing through stop is advised only for a live campaign: on a closed
		// one stop refuses - nothing to remove, and the advice would send the
		// person after a refusal.
		switch status.Campaign.Status {
		case "active", "draft", "stopped":
			text.WriteString(", remove the broken one with stop and cancel")
		}
		text.WriteString(".\n")
	}
	// A row once taken by an account stays with it for good, so with several
	// senders pinned rows are the normal course of a mailing, not a signal. Only
	// the case where an account that is not sending now holds them is alarming:
	// there is no other account that would not meet them earlier.
	for _, held := range status.Held {
		if heldStalled(status, held.Label) {
			fmt.Fprintf(text, "Pinned to account %s: %d rows - only it will take them, and it is not sending now.\n",
				held.Label, held.Rows)
			continue
		}
		fmt.Fprintf(text, "Pinned to account %s: %d rows - it will send them in its own time.\n",
			held.Label, held.Rows)
	}
	if status.Draft != nil {
		fmt.Fprintf(text, "There is a draft (id %d): %d rows, not started.\n",
			status.Draft.ID, status.Draft.Rows)
	}
	if status.Paused != nil {
		fmt.Fprintf(text, "A campaign is paused (id %d): %d rows, it can be resumed.\n",
			status.Paused.ID, status.Paused.Rows)
	}
}

// writeIdle names a queue idle in words: the campaign is active, the rows stand
// planned, and such a picture is indistinguishable from healthy work - the
// reason is visible only indirectly, from the spend counter or the window
// bounds. The numbers go in the line itself: "cap used up" without "7 of 7"
// has nothing to check against.
func writeIdle(text *strings.Builder, status StatusReport) {
	body := idleBody(status.Idle, status)
	if body == "" {
		return
	}
	fmt.Fprintf(text, "%s: %s\n", idleLead(status.Idle.Reason), body)
}

// heldStalled reports whether the account holding the pinned rows is not
// sending now: it is absent from the accounts altogether or it is stopped. A
// live neighbour with the same rows is the normal picture of a mailing from
// several accounts.
func heldStalled(status StatusReport, label string) bool {
	for _, item := range status.Accounts {
		if item.Label == label {
			return item.Stopped
		}
	}
	return true
}

// idleLead is what opens the idle line: an empty queue and a busy sender do not
// "stand", and one word for all the reasons would lie in two cases out of nine.
func idleLead(reason string) string {
	switch reason {
	case IdleQueueEmpty:
		return "Queue is empty"
	case IdleClaimRaceLost:
		return "Queue is moving"
	default:
		return "Queue is stalled"
	}
}

// idleBody is the reason for the idle in words. One wording serves two places:
// the general report line and the account line, which is needed where there are
// several senders and only one of them is quiet. An empty string means the
// reason is already named nearby and there is nothing to repeat.
func idleBody(idle Idle, status StatusReport) string {
	switch idle.Reason {
	case IdleWindowClosed:
		if idle.OpensAt.IsZero() {
			return "the send window is closed."
		}
		return fmt.Sprintf("the send window is closed, next opening %s MSK.",
			idle.OpensAt.In(moscowZone).Format("02.01 15:04"))
	case IdleAccountStop:
		return "the account is stopped, only resume_account clears the stop, after a human check."
	case IdleDailyLimit:
		return fmt.Sprintf("the account's daily cap is used up (%d of %d), sending resumes tomorrow in the window.",
			idle.Used, idle.Limit)
	case IdleNoCampaign:
		// The advice to call start is given only where there is something to
		// start: in an empty system the line above already said "no campaigns",
		// and on a closed campaign start refuses. The Draft and Paused notes alone
		// are not enough: StatusOf fills them only when a different campaign is
		// shown, and a lone draft would be left without advice.
		shown := status.Campaign != nil &&
			(status.Campaign.Status == "draft" || status.Campaign.Status == "stopped")
		switch {
		case shown || status.Draft != nil || status.Paused != nil:
			return "no campaign is running - launch one with start."
		case status.Campaign != nil:
			return "the campaign is closed; load_campaign creates a new one."
		}
		return ""
	case IdleBrokenTemplate:
		// The braces of the shown campaign are already named by the line above,
		// together with the advice; this line is needed where the report is about
		// another campaign.
		if len(status.Idle.Braces) == 0 {
			return "the template of the running campaign could not be parsed, nothing will be sent for it."
		}
		return ""
	case IdleQueueEmpty:
		return "no rows left in work."
	case IdleQueueBlocked:
		return "the remaining rows are pinned to another account, only it will take them."
	case IdleQueueDisqualified:
		return fmt.Sprintf("a guard removes all the remaining rows (%d) - stop list, already messaged, or an unusable name. "+
			"Nothing more will be sent for this campaign, the next sender cycle will close it; status with full shows the per-row breakdown.",
			idle.Left)
	case IdleClaimRaceLost:
		return "rows are available, the sender takes them one at a time with pauses."
	}
	return ""
}

func writePreview(text *strings.Builder, status StatusReport) {
	if len(status.Preview) == 0 || status.Campaign == nil {
		return
	}
	// The preview hits the display cap: without the campaign's row count, three
	// rows read as the whole list. Where there is no cut, there is no caveat
	// either - "first 2 of 2" makes one look for a remainder that does not exist.
	if len(status.Preview) < status.Campaign.Total {
		fmt.Fprintf(text, "Message preview (first %d of %d):\n", len(status.Preview), status.Campaign.Total)
	} else {
		fmt.Fprintf(text, "Message preview (%d):\n", len(status.Preview))
	}
	// The text is shortened the same way as a reply in the dialog list: the
	// preview shows the substitution, it does not retell the whole message.
	for _, item := range status.Preview {
		fmt.Fprintf(text, "  @%s: %s\n", item.Username, shorten(item.Text))
	}
}

// writeAccounts prints the account's spend with the same number the guard
// counts: UsedToday is built by one usedTodaySQL and already includes first
// touches by hand, which are named on a separate line - the person needs to see
// what the cap was spent on. Warm replies and service recipients do not count
// toward the cap and stand apart, otherwise the report line would promise
// headroom where tg_send already refuses.
func writeAccounts(text *strings.Builder, status StatusReport) {
	if len(status.Accounts) > 0 {
		text.WriteString("Accounts:\n")
		for _, item := range status.Accounts {
			fmt.Fprintf(text, "  %s: today %d of %d (cap from the database, counting rows in work",
				item.Label, item.UsedToday, item.DailyLimit)
			if item.ColdToday > 0 {
				fmt.Fprintf(text, ", of them first touches by hand: %d", item.ColdToday)
			}
			text.WriteString(")")
			if item.WarmToday > 0 {
				fmt.Fprintf(text, ", warm replies today: %d (not counted toward the cap)", item.WarmToday)
			}
			if item.OwnerToday > 0 {
				fmt.Fprintf(text, ", service recipients today: %d (not counted toward the cap)", item.OwnerToday)
			}
			if item.Stopped {
				fmt.Fprintf(text, ", STOPPED (%s), cleared only by resume_account", item.StopReason)
			}
			// The reason for the silence goes into the account line only when there
			// is no general line: there are several senders and they are quiet for
			// different reasons. For a stopped account it is not printed - the stop
			// is mentioned right here.
			if status.Idle.Reason == "" && item.Idle.Reason != IdleAccountStop {
				if body := idleBody(item.Idle, status); body != "" {
					fmt.Fprintf(text, ", quiet: %s", strings.TrimSuffix(body, "."))
				}
			}
			text.WriteString("\n")
		}
	}
	// The list of service recipients is named by username and always, not only
	// when someone was written to today: the window, the cap and the one-message
	// rule are off for these usernames, and an extra username in it would
	// otherwise be discovered only by a message that went out.
	if len(status.Owners) > 0 {
		text.WriteString("Service recipients (the window, the cap and the one-message rule do not apply to them):")
		for _, name := range status.Owners {
			fmt.Fprintf(text, " @%s", name)
		}
		text.WriteString("\n")
	}
	// A stuck journal row closes the person for good, and delivery is unknown:
	// it is fixed by hand, so it must catch the eye.
	if status.StuckDirect > 0 {
		fmt.Fprintf(text, "Manual touches with unknown delivery: %d\n", status.StuckDirect)
	}
	// A row with a stale claim in a paused campaign does not fix itself: it keeps
	// the person closed to both the mailing and tg_send.
	if status.StuckClaims > 0 {
		fmt.Fprintf(text, "Rows with a stuck claim: %d\n", status.StuckClaims)
	}
}

// writeContacts prints the search results. needle is the query in the form
// Telegram searched with: no @, no surrounding spaces, lowercase.
func writeContacts(text *strings.Builder, needle string, found []Contact) {
	byUsername := telegram.IsUsername(needle)
	if len(found) == 0 {
		if byUsername {
			fmt.Fprintf(text, "There is no @%s and no similar usernames were found.\nNext: check the username for a typo, or search for the person by name.\n", needle)
			return
		}
		fmt.Fprintf(text, "Nobody was found for the query %q.\nNext: give an exact username without @, or search for the person by another spelling of the name.\n", needle)
		return
	}

	// The exact username is moved to the head of the list: in a prefix result the
	// one being looked for sits among other people's similar usernames and gets
	// lost. There is one match - a Telegram username is unique - so the order of
	// the rest does not change.
	list := found
	exact := -1
	for i, item := range found {
		if item.Exact {
			exact = i
			break
		}
	}
	if exact > 0 {
		list = make([]Contact, 0, len(found))
		list = append(list, found[exact])
		list = append(list, found[:exact]...)
		list = append(list, found[exact+1:]...)
	}

	fmt.Fprintf(text, "Found for the query %q: %d\n", needle, len(list))
	// This is about the list, not about Telegram: when the limit of similar
	// usernames is full, the exact one was not checked at all, and "there is no
	// such username in Telegram" would be a promise the report does not stand
	// behind.
	if byUsername && exact < 0 {
		fmt.Fprintf(text, "None of the results is the exact username @%s: the usernames below are similar, and they are other people. "+
			"Use tg_send on them only after checking that the username was not taken with a typo.\n", needle)
	}
	for _, item := range list {
		fmt.Fprintf(text, "  %s", personHead(item.Username, item.Name))
		if item.Exact {
			text.WriteString(" - exact username")
		}
		// The line is silent about the dialog on purpose: the service knows only
		// whether the address was seen before, and the plain resolve under tg_read
		// puts the username there too. By promising a dialog, the report would
		// make one plan the reply as a warm one and get a refusal on the window.
		// The address book outranks "address was seen": a person from the contacts
		// is almost always an acquaintance, and the reverse order of branches would
		// never let this line appear - Known is always set for a local find.
		switch {
		case item.IsContact:
			text.WriteString(" - in contacts")
		case item.Known:
			text.WriteString(" - username seen before")
		default:
			text.WriteString(" - unknown to the service")
		}
		if item.Username == "" {
			text.WriteString(noUsernameNote)
		}
		text.WriteString("\n")
	}
	text.WriteString("Next: tg_read shows the dialog, tg_send writes. " +
		"Whether this is a warm conversation or a first touch is decided at sending: a first touch goes out only in the window and counts toward the cap.\n")
}

// personHead is the person at the start of a list line. A username cannot be
// guessed from a name (invariant 5), so a person without a public username is
// printed by name, not with an empty @: an empty @ is taken by the agent for a
// username and substituted into tg_send.
func personHead(username, name string) string {
	switch {
	case username != "" && name != "":
		return "@" + username + " " + name
	case username != "":
		return "@" + username
	case name != "":
		return name
	default:
		return noNameOrUsername
	}
}

// noNameOrUsername stands in for a person who has neither a name nor a username.
const noNameOrUsername = "no name and no username"

// noUsernameNote names the missing username outright: the tg_send guard stops
// such a person anyway, but dissecting the refusal costs more than this line.
const noUsernameNote = "; no public username, tg_send cannot message them"

// writeConversation prints the dialog the way a person reads it: oldest to
// newest, the other side and our side named in words, the text not truncated -
// the volume is held by depth. The header has neither the interlocutor's name
// nor the full message count: the service does not know them, and an extra
// query for a header is not made.
func writeConversation(text *strings.Builder, talk Conversation) {
	fmt.Fprintf(text, "Dialog with @%s", talk.Username)
	if talk.UserID > 0 {
		fmt.Fprintf(text, " (id %d)", talk.UserID)
	}
	if len(talk.Messages) == 0 {
		text.WriteString(": no messages.\n")
	} else {
		fmt.Fprintf(text, ", latest messages (%d):\n", len(talk.Messages))
		writeMessages(text, talk.Messages, false)
	}

	switch {
	case len(talk.Fresh) > 0:
		fmt.Fprintf(text, "New message after %d seconds:\n", int(talk.Waited.Seconds()))
		writeMessages(text, talk.Fresh, false)
	case talk.Waited > 0:
		fmt.Fprintf(text, "Waited %d seconds, no reply came. To wait longer, call tg_read again: "+
			"the wait continues from this point.\n", int(talk.Waited.Seconds()))
	default:
		text.WriteString("Did not wait: without wait_seconds the tool answers at once.\n")
	}
	text.WriteString("Next: tg_send replies into this dialog, tg_read with wait_seconds waits for a reply.\n")
}

// dialogKind names the kind of a row by the meaning of the action, not by the
// way Telegram is built: a supergroup and a plain group read the same - a place
// where people talk to each other - and both are called a group; a broadcast
// channel stays a channel.
func dialogKind(kind telegram.Kind, broadcast bool) string {
	switch {
	case kind == telegram.KindUser:
		return "person"
	case kind == telegram.KindChannel && broadcast:
		return "channel"
	case kind == telegram.KindChannel || kind == telegram.KindChat:
		return "group"
	default:
		return "unknown"
	}
}

func writeDialogs(text *strings.Builder, list []DialogInfo) {
	if len(list) == 0 {
		// Groups and channels are no longer filtered out: the list is empty only
		// when the account has no dialogs at all.
		text.WriteString("No dialogs at all - no private chats, no groups, no channels.\nNext: tg_find finds a person by username, tg_send starts a dialog.\n")
		return
	}
	fmt.Fprintf(text, "Dialogs (%d):\n", len(list))
	for _, item := range list {
		kind, _, err := item.Ref.Split()
		if err != nil {
			// A broken reference does not hide the row: its kind will be named "unknown".
			kind = ""
		}
		person := kind == telegram.KindUser
		head := personHead(item.Username, item.Name)
		// An empty name on a group or channel means the entity was not in the
		// answer (the account was removed from it), not a person without a
		// username: there are no unnamed groups.
		if !person && head == noNameOrUsername {
			head = "title unknown"
		}
		fmt.Fprintf(text, "  %s: %s", dialogKind(kind, item.Broadcast), head)
		if item.Unread > 0 {
			fmt.Fprintf(text, ", %d unread", item.Unread)
		}
		// An empty username on a group or supergroup is how the platform works,
		// not a loss: the warning about tg_send is printed only for a person.
		if person && item.Username == "" {
			text.WriteString(noUsernameNote)
		}
		text.WriteString("\n")
		// A group's top message may be a service one, and then there is none at
		// all: a line "them: [no text]" out of nowhere reads as lost text.
		if item.Last != (ChatMessage{}) {
			writeMessages(text, []ChatMessage{item.Last}, true)
		}
	}
	text.WriteString("Next: tg_read shows the whole dialog. Only a person can be written to - tg_send to a group or channel is refused by a guard.\n")
}

// snippetLen is the cap on a reply in the dialog list: a channel post of twenty
// lines would push out of the screen the very list the tool was called for.
const snippetLen = 100

// writeMessages prints dialog rows: time in MSK, direction in words, an
// attachment as a mark before the text. short collapses a reply into one line -
// that is how the dialog list prints it; a dialog read returns the whole text,
// and the volume is held by depth. The time of a message without a date (the
// dry-run journal) is not printed at all: a substituted date would look real.
func writeMessages(text *strings.Builder, messages []ChatMessage, short bool) {
	for _, msg := range messages {
		text.WriteString("  ")
		if !msg.SentAt.IsZero() {
			fmt.Fprintf(text, "[%s] ", msg.SentAt.In(moscowZone).Format("02.01 15:04"))
		}
		if msg.Outgoing {
			text.WriteString("us: ")
		} else {
			text.WriteString("them: ")
		}
		body := msg.Text
		if short {
			body = shorten(body)
		}
		// A message with no text and no attachment (a service one or a deleted
		// text) reads as a broken line, so it is named in words.
		switch {
		case msg.MediaKind != "" && body != "":
			fmt.Fprintf(text, "[%s] %s", msg.MediaKind, body)
		case msg.MediaKind != "":
			fmt.Fprintf(text, "[%s]", msg.MediaKind)
		case body != "":
			text.WriteString(body)
		default:
			text.WriteString("[no text]")
		}
		text.WriteString("\n")
	}
}

// shorten collapses a reply into one line and cuts it by runes: a cut by bytes
// would tear Cyrillic or an emoji in the middle.
func shorten(body string) string {
	return cutRunes(strings.Join(strings.Fields(body), " "), snippetLen)
}

// templateShown is how many characters of the template load_campaign prints.
// The message is read by eye in full, so the cap is generous; only what would
// push everything else out of the answer is cut.
const templateShown = 500

// cutRunes cuts text by runes and leaves line breaks alone: the template is
// checked as a message, and collapsed into one line it would be no good for that.
func cutRunes(body string, limit int) string {
	runes := []rune(body)
	if len(runes) <= limit {
		return body
	}
	return string(runes[:limit]) + "..."
}

func writeRows(text *strings.Builder, status StatusReport) {
	fmt.Fprintf(text, "Recipients (%d):\n", len(status.Rows))
	for _, row := range status.Rows {
		fmt.Fprintf(text, "  @%s %s - %s", row.Username, row.Name, label(stateLabels, row.State))
		if row.Reason != "" {
			fmt.Fprintf(text, " (%s)", label(reasonLabels, row.Reason))
		}
		text.WriteString("\n")
	}
}
