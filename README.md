# tg-agent-mcp

An MCP server that gives a Claude Code agent hands in Telegram: find people, write to them, read dialogs and run campaigns.

[![ci](https://github.com/daniil4545/tg-agent-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/daniil4545/tg-agent-mcp/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/daniil4545/tg-agent-mcp)](https://github.com/daniil4545/tg-agent-mcp/releases)
[![license](https://img.shields.io/github/license/daniil4545/tg-agent-mcp)](LICENSE)

The agent talks to the server over MCP. The server drives a real Telegram user
account through an MTProto client (not a bot) and keeps pacing, the send window,
the daily cap and the account stop in PostgreSQL, so a restart or a retry cannot
bypass them. Go, built on the official `modelcontextprotocol/go-sdk` v1.

Status: version 0.1. Live sending needs a real Telegram account; everything else
runs and is tested without one (see [Limitations](#limitations)).

## Demo

Client configuration (Claude Code `.mcp.json`; the token comes from the
environment):

```json
{
  "mcpServers": {
    "tg-agent": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer ${TG_AGENT_MCP_TOKEN}" }
    }
  }
}
```

A campaign from the agent side. The calls and answers below are the output of
`TestCampaignFlowOverMCP`, which drives the server through an MCP client session
against PostgreSQL.

`add_recipients`, with one person who has no Telegram username:

```json
{"name": "add_recipients", "arguments": {"people": [
  {"contact_id": 101, "name": "Anna",  "username": "anna_k"},
  {"contact_id": 102, "name": "Boris", "username": ""}
]}}
```

```
Accepted: 1 of 2.
Screened out:
  no usable Telegram username: 1
Campaign: Spring invitation (id 1), status draft.
Rows total: 1, queued 1
Message preview (1):
  @anna_k: Здравствуйте, Anna! Приглашаем вас на встречу в четверг.
```

`start`, then `status`:

```
Campaign started (id 1). Sending runs in the window 10:00-19:00 on weekdays, within the account's daily cap.
Campaign: Spring invitation (id 1), status active.
Rows total: 1, queued 1
Queue is moving: rows are available, the sender takes them one at a time with pauses.
Accounts:
  test: today 0 of 10 (cap from the database, counting rows in work)
```

## Features

| What | In one line |
| --- | --- |
| Campaigns | load a template and a recipient list; the service owns queue, pacing, window and cap |
| One message per person | enforced across campaigns, repeated starts, restarts and manual `tg_send` |
| Account stop | `PEER_FLOOD` or a long `FLOOD_WAIT` stops that account until a human lifts it |
| Dialogs | find a person, send by hand, read a dialog and optionally wait for the reply |
| Idle reasons | `status` names why the queue is silent, with numbers |
| Several accounts | one sender process per account over one shared queue |
| Two tokens | the agent token sees only `tg_send` and `tg_read`, only for allow-listed usernames |
| Dry-run transport | the default; messages go to the log, `live` refuses to start outside `ENV=prod` |

The service does not decide whom to write to. The caller builds the list and
passes it through `add_recipients`.

## Install

Needs Go 1.25.7 or newer, Docker, `make` and `golangci-lint` for the checks.

```sh
git clone https://github.com/daniil4545/tg-agent-mcp.git
cd tg-agent-mcp/src
```

There is no prebuilt binary: the service is meant to run as the Docker stack
described in [docs/setup.md](docs/setup.md).

## Quick start

A local run in dry-run mode, with no Telegram account.

1. Start PostgreSQL and apply the migrations:

   ```sh
   docker compose up -d
   export DATABASE_URL='postgres://tg_agent_mcp:tg_agent_mcp@localhost:5433/tg_agent_mcp?sslmode=disable'
   make migrate-up
   ```

   Expected: `goose: successfully migrated database to version: 5`.

2. Run the server:

   ```sh
   ACCOUNT_LABEL=main MCP_ENABLED=true MCP_TOKEN=change-me go run ./cmd/tg-agent-mcp
   ```

   Expected, among JSON log lines: `"msg":"service started"` with
   `"transport":"dry-run"`, then `"msg":"sender idle"` with
   `"reason":"no_active_campaign"`.

3. In another terminal, check health and the token boundary:

   ```sh
   curl -s http://127.0.0.1:8081/health; echo
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/mcp
   ```

   Expected: `ok`, then `401`: the endpoint answers nobody without the token.

4. Export `TG_AGENT_MCP_TOKEN=change-me`, put the configuration from the demo
   into `.mcp.json` and start `claude` in that directory. Approve the server
   once; the ten tools appear in the session.

For a live account, sign-in and the first campaign, follow
[docs/setup.md](docs/setup.md).

## Tools

| Tool | What it does |
| --- | --- |
| `load_campaign` | validates the template and creates a draft campaign, cancelling the previous draft |
| `add_recipients` | adds recipients in batches of up to 50 and returns a screening report |
| `start` | moves a draft to running; also resumes a paused campaign |
| `stop` | pauses the campaign, or cancels it together with its queue when `cancel` is set |
| `resume_account` | lifts an account stop; meant to be called by a human only |
| `status` | state of campaigns and accounts, and the reason the queue is idle |
| `tg_find` | an exact username is resolved through Telegram, a name is searched among contacts and own dialogs |
| `tg_send` | sends a private message by hand and records the touch in the log |
| `tg_read` | shows a dialog and, on request, waits for the other side's reply |
| `tg_dialogs` | lists dialogs with unread counts; each row names its kind: person, group or channel |

A campaign is one outreach run: a text template with the placeholder `{имя}` and
a recipient list. States are draft, running, paused, cancelled and finished.
There is one active campaign in the system, and accounts share its queue.
The template placeholder and the test fixtures are Russian; code, documentation
and tool descriptions are English.

## Configuration

Secrets are given by name only; values live in `src/.env` (not in git) or in your
secret store.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | none, required | PostgreSQL connection string |
| `ACCOUNT_LABEL` | none, required | label of the account this process sends from |
| `MCP_ENABLED` | `false` | turns the control server on; then `MCP_TOKEN` is required |
| `MCP_TOKEN` | none | bearer token of the MCP endpoint |
| `MCP_ADDR`, `HEALTH_ADDR` | `:8080`, `:8081` | listen addresses |
| `TRANSPORT` | `dry-run` | `dry-run` logs messages, `live` sends through MTProto |
| `ENV` | `dev` | `live` works only with `ENV=prod` |
| `ACCOUNT_DAILY_LIMIT` | `20` | daily cap, applied only when the account row is created |
| `SEND_PAUSE_MIN`, `SEND_PAUSE_MAX` | `10m`, `30m` | random pause between sender cycles |
| `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`, `TELEGRAM_PHONE` | none | account credentials, needed for sign-in |
| `MCP_AGENT_TOKEN`, `AGENT_ALLOW_LIST` | none | agent-level token and the usernames it may reach |
| `DEV_ALLOW_LIST` | none | service recipients: the window, the cap and the one-message rule do not apply to them |

After the first start the cap lives in the database: `status` prints the
effective number, and a mismatch with the variable is logged as a warning. The
sender can run with MCP disabled, so control and delivery can be split across
instances. Every variable of the stack is covered in
[docs/setup.md](docs/setup.md).

## How it works

```mermaid
flowchart LR
    A[Claude Code agent] -->|MCP, bearer token| B[MCP server]
    B -->|campaigns, caps, stops| C[(PostgreSQL)]
    C -->|rows to send| D[Sender loop]
    D -->|window, cap, one message, stop list| E[MTProto client]
    B -->|dialog tools| E
    E <--> F[Telegram]
```

The agent loads a campaign through MCP; the rows, counters, pauses and stops are
written to PostgreSQL. The sender loop of each account takes rows one at a time,
checks the guards at send time and hands the message to the MTProto client. A
reply from the person closes their row. Design, states and invariants:
[docs/architecture.md](docs/architecture.md).

## Delivery guarantees

The service exists separately from a send script because of these promises.

| Promise | Checked by |
| --- | --- |
| One message per person, across campaigns and manual sends, restarts and repeated starts | `TestNoSecondMessageAcrossCampaigns`, `TestDirectSendClosesPersonForCampaigns`, `TestNoResendAfterRestart` |
| A recipient without a usable username is never queued; a name with a curly brace is filtered | `TestBadNamesNeverLeaveTheQueue`, `TestBadNamesSkippedAtImport` |
| A message with an unresolved brace is never sent; a broken template halts the queue | `TestLoadCampaignChecksTemplate`, `TestBrokenTemplateStopsTheQueue` |
| The window (10:00-19:00 on weekdays, Moscow time) and the daily cap bind campaigns and the first touch of a stranger | `TestNoSendOutsideWindow`, `TestNoSendOverDailyLimit` |
| A reply inside a running dialog is not bound by the window or the cap | `TestWarmReplyIgnoresWindowAndCeiling` |
| An account limit stops that whole account, other accounts keep going, and only a human lifts it | `TestPeerFloodStopsAccount`, `TestNoSendWhenAccountStopped` |
| The stop list is checked at send time | `TestStopListCheckedAtSendTime` |
| Counters, pauses, stops and the cap live in the database, not in memory | `TestRestartContinuesCampaign` |
| The touch log row is written before the network call | `TestDirectSendRecordedBeforeDelivery` |
| `live` does not start outside production | `TestLiveRefusedOutsideProd` |
| The endpoint needs the token; the agent token reaches only allow-listed usernames | `TestTokenPicksTools`, `TestAgentReachesOnlyTestAccounts` |

Data and tokens: names and usernames of recipients stay in your PostgreSQL. The
service calls no CRM or other external system; its only outbound connection is
the MTProto session to Telegram, and its one HTTP client probes its own health
endpoint. `.gitignore` excludes `.env` files, session files, recipient lists and
database dumps. The compose files publish their ports on `127.0.0.1` only, and the live stack
publishes no database port.

## Engineering decisions

- **State in PostgreSQL, not in memory.** The cap, the pacing and the account
  stop must survive a restart; with in-memory counters a crash loop resets them.
- **A flood limit is lifted by a person.** A timer would retry into the same
  limit and risk the account.
- **A separate service, not a feature of the caller.** A flood stop must not
  silence other work of the system. Several accounts mean several instances over
  one shared queue, because one MTProto session belongs to one process.
- **Template plus `{имя}`, no free rewriting.** The text is fixed by the caller;
  the service only substitutes the name. Rejected: generating a variant per
  recipient inside the service.
- **The service does not pick recipients.** Segment selection stays outside; the
  service only enforces delivery rules, so the guarantees can be tested without
  knowing why a message is sent.
- **Control through MCP, no Telegram bot.** The agent is the operator, so there
  is no second control surface to secure. Resolving a person by phone number is
  off by default.
- **One account for campaigns and dialogs; the guard is the touch log.** The
  one-message rule is enforced by a touch log, not by assigning roles to
  accounts, and it applies at the cold contact, not at the tool. The log row is
  written before the network call. Its key is the username, which can change in
  Telegram; I accepted that limit.
- **Waiting for a reply polls the history.** Rejected: subscribing to updates in
  the Telegram library, which adds a long-lived stream to a request/response
  tool.
- **Two tokens.** The agent token sees only `tg_send` and `tg_read`, and only
  for allow-listed usernames, so a test agent cannot become a general sender.
- **A fixed UTC+3 offset for the send window.** Moscow has had no clock change
  since 2014, and the container may lack tzdata, in which case the window would
  silently shift to UTC. The cost is that the window cannot follow another
  region.
- **Reject at the boundary and again at send time.** A broken template is
  refused when loaded; the queue also refuses to send an unresolved brace, for
  campaigns created before that check existed.
- **The live transport refuses to start outside production.** `TRANSPORT=live`
  with `ENV` other than `prod` aborts at startup. The default transport is
  `dry-run`.

## Tests

```sh
cd src
docker compose up -d
make ci-check       # migrations, tests, vet, gofmt, golangci-lint, build
make test-fast      # fast tier, no database
```

The integration tier needs `TEST_DATABASE_URL`, separate from `DATABASE_URL`, a
guard against migrating the wrong database; the Makefile defaults it to the
compose database. Without a database `go test` skips the integration tier, and
`make test` fails instead of skipping. CI runs the same `make ci-check` against
a PostgreSQL service container.

## Limitations

- Live sending needs a real Telegram account, a proxy where required, and
  `TRANSPORT=live` with `ENV=prod`. A closed private chat, a refused resolve
  and an account limit have not been observed against live Telegram; their
  handling is covered by tests only.
- A touch-log row stuck in `pending` blocks that person permanently. It is
  visible as a counter in `status` and an ERROR at startup; the fix is manual
  SQL.
- Groups and channels appear in the dialog list only: history cannot be read,
  `tg_find` does not search them and sending to them is not possible.
  Attachments are neither downloaded nor sent. Private chats and text only.
- A reply closes the recipient row as `replied`, but nothing notifies anyone; a
  human reads the chat.
- The send window and weekday rule are fixed to Moscow time.
- One active campaign at a time.
- Telegram's anti-spam behavior is not documented and changes. The pacing and
  caps are conservative defaults, not a guarantee that an account will not be
  limited.

## Documents

[docs/README.md](docs/README.md) is the map. The closest ones:
[docs/setup.md](docs/setup.md) (install, sign-in, first campaign),
[docs/architecture.md](docs/architecture.md) (structure and decisions),
[docs/runbook-account.md](docs/runbook-account.md) (raise the cap, replace an
account) and [CHANGELOG.md](CHANGELOG.md).

## License

MIT, see [LICENSE](LICENSE).
