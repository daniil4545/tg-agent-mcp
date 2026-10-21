# tg-agent-mcp

An MCP server that gives a Claude Code agent hands in Telegram, plus a daemon
that does the long-running work. The agent can find a person, write to them,
read a dialog, and run a campaign over a ready recipient list. The service keeps
pacing, the send window, the daily cap and the account stop in PostgreSQL, so a
restart or a retry cannot bypass them.

It is Go, built on the official `modelcontextprotocol/go-sdk` v1 and an MTProto
client (a real user account, not a bot).

User-facing texts are in Russian: the template placeholder is `{имя}`, and test
fixtures exercise Russian names and messages. Code, docs and tool descriptions
are in English.

## What it does

Two groups of tools share one live Telegram session:

- **Campaign delivery.** The caller passes a ready recipient list and a message
  template. The service paces the sends, respects the send window and the daily
  cap, and stops the account on a flood limit, until everything is delivered or
  a limit is hit.
- **Dialogs.** Find a person, write to them by hand, read the conversation.
  Private chats and text only. The dialog list is wider than the conversations
  and also shows groups and channels of the account.

The service does not decide whom to write to. The caller builds the list and
passes it through a tool; where the list came from is unknown to the service.

A **campaign** is one outreach run: a text template and a recipient list loaded
with `load_campaign` and `add_recipients`. From there the service owns it:
queue, pacing, send window, skipping people who were already written to, and
stopping on an account limit. There is one active campaign in the system and
accounts share its queue. States are draft, running, paused, cancelled and
finished; `status` reports the current one.

## Usage

The agent talks to the server over MCP (streamable HTTP, bearer token). A typical
campaign from the agent side:

1. `load_campaign` with a template such as `Здравствуйте, {имя}! ...` creates a
   draft and rejects a template with an unresolved brace.
2. `add_recipients` adds up to 50 people per call and returns a report of who
   was filtered out and why, with a message preview.
3. `start` moves the draft to running.
4. `status` shows campaigns, accounts and, if the queue is idle, the reason with
   numbers (window closed and opening hour, cap used up, account stopped).

Manual dialog work: `tg_find` to resolve a person, `tg_read` to read the
conversation (optionally waiting for a reply), `tg_send` to write.

## Tools

| Tool | What it does |
| --- | --- |
| `load_campaign` | validates the template and creates a draft campaign, cancelling the previous draft |
| `add_recipients` | adds recipients in batches of up to 50 and returns a filtering report |
| `start` | moves a draft to running; also resumes a paused campaign |
| `stop` | pauses the campaign, or cancels it together with its queue when `cancel` is set |
| `resume_account` | lifts an account stop; meant to be called by a human only |
| `status` | state of campaigns and accounts, and the reason the queue is idle |
| `tg_find` | finds a person: an exact username is resolved through Telegram, a name is searched among contacts and own dialogs |
| `tg_send` | sends a private message by hand and records the touch in the log |
| `tg_read` | shows a dialog and, on request, waits for the other side's reply |
| `tg_dialogs` | lists dialogs with unread counts; each row names its kind: person, group or channel |

Transport is streamable HTTP behind a bearer token. A second token,
`MCP_AGENT_TOKEN`, gives agent access: only `tg_send` and `tg_read` are visible,
and only for usernames in `AGENT_ALLOW_LIST`; the service rejects any other
username. I use it so that agents can test bots without the owner confirming
every message.

## Guarantees

These are the reason the service exists separately from a send script:

- One message per person: within a campaign and across campaigns, including
  messages written by hand with `tg_send`, restarts and repeated starts.
- A recipient without a usable Telegram username is never queued. Under a
  template with `{имя}`, a recipient whose name contains a curly brace is also
  filtered, because the brace would end up in the message. An empty name is not
  a reason to filter: the message goes out without the greeting and without a
  stray comma.
- A message with an unresolved brace is never sent. The template is rejected on
  input; for a campaign built by an older version the queue halts, the campaign
  itself is left alone (it is rebuilt), and `status` and an ERROR log say why.
- The window (10:00-19:00 on weekdays, Moscow time) and the daily cap apply to
  campaigns and to the first touch of a stranger. A reply inside a running
  dialog is deliberately not bound by them: refusing to answer at 21:30 would
  make the account look less like a person, not more.
- An account limit (`PEER_FLOOD`, a long `FLOOD_WAIT`) stops that whole account
  and is lifted only by a human, never by a timer.
- A stop of one account does not touch the others and does not break the
  campaign.
- Counters, pauses, stops and the cap itself live in the database, not in
  memory. Otherwise a restart loop would bypass any cap.
- A silent queue is observable: `status` names the reason with numbers, instead
  of leaving it to a log line.

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

## Run and test

```bash
cd src
docker compose up -d          # postgres:16 on port 5433
make ci-check                 # migrations, tests, vet, fmt, lint, build
make test-fast                # fast tier, no database
```

The full run needs `TEST_DATABASE_URL` separate from `DATABASE_URL`, a guard
against migrating the wrong database. Without a database the integration tier is
skipped by `go test`; `make test` fails instead of skipping.

Configuration: `DATABASE_URL` and `ACCOUNT_LABEL` are required. `MCP_ENABLED`
turns the control server on (off by default); when on, `MCP_TOKEN` is required
and the address is `MCP_ADDR` (`:8080`). `ACCOUNT_DAILY_LIMIT` sets the daily
cap only when the account row is created; after that the cap lives in the
database, `status` prints the effective number, and a mismatch with the variable
is logged as a warning at startup. The sender can run with MCP disabled, so
control and delivery can be split across instances.

## Status and limitations

- Working code with unit and integration tests. Live sending needs a real
  Telegram account, a proxy where required, and `TRANSPORT=live` with
  `ENV=prod`. There are no tagged releases.
- Not seen live: a closed private chat, a refused resolve and an account limit.
  Their handling is covered by tests only.
- A touch-log row stuck in `pending` blocks that person permanently. It is
  visible as a counter in `status` and an ERROR at startup; the fix is manual
  SQL.
- Groups and channels appear in the dialog list only: history cannot be read,
  `tg_find` does not search them and sending to them is not possible.
  Attachments are neither downloaded nor sent.
- A reply closes the recipient row as `replied`, but nothing notifies anyone;
  a human reads the chat.
- The send window and weekday rule are fixed to Moscow time.
- One active campaign at a time.
- Telegram's anti-spam behavior is not documented and changes. The pacing and
  caps are conservative defaults, not a guarantee that an account will not be
  limited.

## Documents

The documentation map is [docs/README.md](docs/README.md); it says which file
to open for what. The closest ones:
[docs/customer-setup.md](docs/customer-setup.md) (set up from scratch on your
own machine) and [docs/architecture.md](docs/architecture.md) (structure and
decisions).

## License

MIT, see [LICENSE](LICENSE).
