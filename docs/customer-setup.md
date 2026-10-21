# Guide: the tg-agent-mcp stack on the operator's machine

The sequence from scratch to the first campaign on the operator's machine
(macOS, Docker Desktop). Every step ends with a command and what it must print.
If the output does not match, that is a stop and a question to the owner, not a
reason to go on: the service writes to strangers from a live account, and the
price of a mistake here is the account.

## 1. Scope and result

At the end, the operator's machine runs its own stack: Postgres with the
campaign database, the application with the MCP server on `127.0.0.1:8087`, and
Claude Code with ten mailing and dialog tools in its session.

What this is not:

- **it is not taking over a mailing in progress.** The invitation pilot until
  19 September is run by the owner from the owner's own stack. Until the pilot
  is closed, the operator's stack runs without live sending: it comes up, is
  checked on its own addressees and waits for the account handover;
- **it is not a second channel of the same account.** The account is handed over
  whole and once: first the owner stops the owner's stack, only then does the
  operator sign in to the account on the operator's side. The order is section
  4;
- **it is not a copy of someone else's database.** The campaign database holds
  the knowledge of who has already been written to. Until it is handed over,
  the operator's stack does not have this knowledge.

Rules that are not up for discussion:

1. **One account, exactly one stack.** Two processes on one number mean two
   independent queues and two databases that do not know about each other,
   while the "one person, one message" boundary is held only by the database.
   The result is two messages to one person and a complaint from that person,
   not from Telegram.
2. **Do not bring up someone else's live session.** The session file is not
   copied from another machine. Sign-in is done anew with `make live-authorize`
   after the owner has stopped the owner's stack.
3. **Do not copy the owner's database on your own.** Moving a dump is the
   owner's decision and a separate handover step, not an action of the
   operator.
4. **Do not enable live sending before the account handover.** Until then the
   stack lives in `TRANSPORT=dry-run`, where messages are written to the log,
   not to Telegram.

## 2. What to prepare in advance

Without any row of the table, account sign-in will not pass: `authorize`
requires all four `TELEGRAM_*` variables and fails on the first missing one.

| What is needed | Why | Where to get it |
| --- | --- | --- |
| Access to the repository `daniil4545/tg-agent-mcp` | the stack code; updates are fetched with `git pull` | the repository is public; no access grant is needed |
| Docker Desktop for macOS, running | the whole stack lives in containers | docker.com, ordinary installation |
| The phone number of the mailing Telegram account | `TELEGRAM_PHONE`, session sign-in | the owner, at the account handover |
| `api_id` and `api_hash` of this account | `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`; the client cannot be built without them | the owner passes the owner's pair, or the operator creates a new one at my.telegram.org, signing in under the same number |
| Access to the confirmation code | Telegram sends the code to the app on this number; only a human can enter it | a phone with an active session of the account |
| The two-factor password, if enabled | without it sign-in stops halfway | the owner, at the account handover |

About `api_id` and `api_hash`: the pair belongs to the number, not to the
machine. It cannot be derived from anywhere and cannot be restored without
access to the number. So it either comes from the owner together with the
account, or it is created at my.telegram.org after the operator already has
access to the number.

## 3. Installation and configuration

Clone the repository:

```
cd ~
git clone git@github.com:daniil4545/tg-agent-mcp.git
cd tg-agent-mcp
```

Expected: `Cloning into 'tg-agent-mcp'...` and at the end the line
`Resolving deltas: 100% (...), done.`. A `Permission denied (publickey)`
refusal means the SSH key is not set up for GitHub; clone over HTTPS
(`https://github.com/daniil4545/tg-agent-mcp.git`) instead.

Check the tools:

```
docker compose version
```

Expected: a line like `Docker Compose version v5.3.1`. Command not found or
`Cannot connect to the Docker daemon` means Docker Desktop is not installed or
not running.

Prepare the settings:

```
cd ~/tg-agent-mcp/src
cp .env.example .env
```

The `.env` file does not go into git. Fill in:

| Variable | What it is | What to fill in |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | password of the local campaign database | any long random string; does not change after the database is created |
| `MCP_TOKEN` | password of the MCP endpoint; without it the server answers 401 | your own random string, for example the output of `openssl rand -hex 32` |
| `ACCOUNT_LABEL` | account label; the row in `accounts` is created under it | `main` if the owner hands over the campaign database together with the account; otherwise your own label |
| `ACCOUNT_DAILY_LIMIT` | daily cap of the new account | 20, the figure of the invitation pilot |
| `TRANSPORT` | `dry-run` writes to the log, `live` sends to Telegram | `dry-run` until the account handover |
| `ENV` | `dev` or `prod`; `live` works only in `prod` | the line `ENV=dev` before the handover, `prod` after |
| `DEV_ALLOW_LIST` | service addressees of the stack operator | your own usernames, comma separated, for the duration of checks |
| `DIGEST_ACCOUNT`, `DIGEST_TO` | label of the account that sends the evening digest, and its recipients | empty until the second sender; recipients must be inside `DEV_ALLOW_LIST` |
| `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`, `TELEGRAM_PHONE` | account credentials | from the table in section 2; empty until the account handover |
| `MCP_PORT`, `HEALTH_PORT` | loopback ports | leave 8087 and 8088 unless a neighbor occupies them |

`ACCOUNT_LABEL` matters more than it looks. A queue row once taken by an
account stays pinned to it forever. If the owner hands over a database dump and
the label in `.env` is different, the application creates a second account row,
and the unfinished queue rows from the dump are unavailable to the new account.

The value of `ACCOUNT_DAILY_LIMIT` is applied only when the account row is
created. For an existing account the cap lives in the database and is changed
by a query: `runbook-account.md`, section 1.

## 4. First start of the stack and account sign-in

Bring the stack up:

```
cd ~/tg-agent-mcp/src
make live-up
```

The first start builds the image and takes several minutes. At the end the
command prints:

```
MCP: http://127.0.0.1:8087/mcp
```

Check that both containers are up:

```
docker compose -f docker-compose.local.yml ps --format 'table {{.Service}}\t{{.Status}}'
```

Expected:

```
SERVICE    STATUS
app        Up 5 seconds (healthy)
postgres   Up 20 seconds (healthy)
```

The `healthy` mark on `app` may not appear at once: the healthcheck runs every
15 seconds and starts 15 seconds after launch.

Check health and the log:

```
curl -s http://127.0.0.1:8088/health; echo
make live-logs
```

Expected: `ok` from `curl` and JSON lines in the log, among them:

```
{"level":"INFO","msg":"service started","account_label":"main","transport":"dry-run","mcp_enabled":true,"health_addr":":8081"}
{"level":"INFO","msg":"http listening","addr":":8080"}
{"level":"INFO","msg":"sender started","account":"main"}
{"level":"INFO","msg":"sender idle","account":"main","reason":"no_active_campaign"}
```

The `transport` field must match what is in `.env`. A `sender idle` line with
the reason `no_active_campaign` means normal operation: there is no campaign
yet. Leave the log view with Ctrl+C; the stack keeps running.

If the log has `daily limit differs from env`, this is not an error: the cap in
the database differs from the variable, and the one in the database applies. The
number from the database is named by `status`.

### Account handover from the owner

While the pilot is not closed, signing in to the account is not allowed, and it
is not about a prohibition. One MTProto session lives in exactly one process,
and the "one person, one message" boundary is held by the campaign database.
Two stacks on one number mean two databases that do not see each other's
touches, that is, a second message to the same person.

Handover order; steps 1 and 2 are done by the owner:

1. The owner stops the owner's stack: `make live-down` on the owner's machine.
2. The owner passes the number, `api_id`, `api_hash`, the two-factor password
   and the decision on the campaign database: whether a dump is moved or not.
3. The operator enters `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`, `TELEGRAM_PHONE`
   in `src/.env` and brings the stack up again: `make live-up`.
4. The operator signs in to the account (command below). The confirmation code
   arrives in Telegram on the handed-over number, and a human must enter it.

During sign-in `TRANSPORT` stays `dry-run`: in live mode the session file is
locked by the running sender, and sign-in will refuse.

Sign-in:

```
cd ~/tg-agent-mcp/src
make live-authorize
```

The command asks for the code, for the password if two-factor is on, and prints
who it signed in as:

```
authorized: Club @sender_account (id <id>, phone <number>)
```

The number and username must match the handed-over account. If they do not,
stop and sort it out with the owner.

Refusals and their meaning:

| Refusal text | What happened | What to do |
| --- | --- | --- |
| `telegram storage ... is locked by another process` | the stack runs in `TRANSPORT=live` and holds the session file | set `TRANSPORT=dry-run`, `make live-up`, repeat sign-in |
| `telegram storage holds a session of another account (phone ..., expected ...)` | the volume holds a session of another number | this is an account replacement, not a first sign-in: procedure in `runbook-account.md`, section 2 |
| `TELEGRAM_APP_ID is required` (and the like for `APP_HASH`, `PHONE`) | the variable is not filled in `.env` | fill it in and repeat; the container is recreated by `make live-up` |

After a successful sign-in, live mode:

1. in `src/.env`: `TRANSPORT=live`, `ENV=prod` (or remove the `ENV` line; the
   stack default is already `prod`);
2. `make live-up`;
3. the log has `service started` with `"transport":"live"`.

## 5. Connecting Claude Code to MCP

Claude Code goes to the endpoint with a token. The token is put neither in git
nor in the session settings: it is in the Keychain, and Claude Code gets it
through a headers script before every connection.

Put the token into the Keychain (the same as `MCP_TOKEN` in `src/.env`):

```
security add-generic-password -s tg-agent-mcp/mcp -a MCP_TOKEN -w
```

The command asks for the value and prints nothing on success. If an entry with
that name already exists, `security` refuses with a message about an existing
keychain item: then either keep the old value, or delete the entry with
`security delete-generic-password -s tg-agent-mcp/mcp` and create it again.

Create a working directory from which Claude Code will be launched. It is
separate from the clone on purpose: `git pull` must not argue with local files.

```
mkdir -p ~/tg-agent/scripts
cd ~/tg-agent
```

File `~/tg-agent/.mcp.json`:

```
{
  "mcpServers": {
    "tg-agent": {
      "type": "http",
      "url": "http://127.0.0.1:8087/mcp",
      "headersHelper": "./scripts/mcp-headers.sh"
    }
  }
}
```

File `~/tg-agent/scripts/mcp-headers.sh`:

```
#!/usr/bin/env bash
set -euo pipefail

token="$(security find-generic-password -s tg-agent-mcp/mcp -a MCP_TOKEN -w 2>/dev/null || true)"

if [[ -z "$token" ]]; then
    echo "no tg-agent-mcp/mcp entry in Keychain" >&2
    exit 1
fi

printf '{"Authorization": "Bearer %s"}\n' "$token"
```

Make it executable and check that the endpoint is closed without a token:

```
chmod +x ~/tg-agent/scripts/mcp-headers.sh
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8087/mcp
```

Expected: `401`. Without a token the server answers nobody, and this is a
boundary, not a failure.

Check the connection:

```
cd ~/tg-agent
claude mcp list
```

Expected: the line `tg-agent: http://127.0.0.1:8087/mcp (HTTP)` with the mark
`Pending approval (run claude to approve)` on the first run. The project
`.mcp.json` requires approval by a human; this is protection against a foreign
repository, not an error. Run `claude` in this directory, approve the server,
then repeat `claude mcp list`: the mark changes to connected.

Inside a Claude Code session there is one check: the tool list must contain ten
names: `load_campaign`, `add_recipients`, `start`, `stop`, `resume_account`,
`status`, `tg_find`, `tg_send`, `tg_read`, `tg_dialogs`. If the tools are
missing, the token in the Keychain and `MCP_TOKEN` in `src/.env` differ.

## 6. Self-check before the first live message

The check goes in two passes: first without Telegram at all, then on your own
usernames.

**First pass, before the account handover.** In `src/.env`: `ENV=dev`,
`TRANSPORT=dry-run`, `DEV_ALLOW_LIST=<your usernames, comma separated>`, then
`make live-up`. With this combination nothing goes outside: there is no live
Telegram, and in `dev` the list works as the last boundary of the transport.

Through Claude Code:

1. `load_campaign` with a title and a template, for example `Привет, {имя}! Это
   проверка контура.` (Russian: "Hi, {имя}! This is a check of the setup.");
2. `add_recipients` with one entry: your own `contact_id` (any number), `name`
   and `username` of your own username;
3. `start`.

Expected in the `add_recipients` reply, the first line `Accepted: 1 of 1.`, and
in the log within a minute or two:

```
{"level":"INFO","msg":"dry-run send","username":"<your username>","random_id":...,"text":"Привет, ..."}
```

The `dry-run send` line means the whole chain is assembled: campaign, queue,
boundaries, sender. Nothing went to Telegram.

**Second pass, after the account handover.** `TRANSPORT=live`, `ENV=prod`,
`DEV_ALLOW_LIST` with your own usernames, `make live-up`. The same campaign to
your own username, and the message must arrive in Telegram, and the log must
show:

```
{"level":"INFO","msg":"message sent","username":"<your username>",...}
```

Understand the boundary honestly here: **in `prod`, `DEV_ALLOW_LIST` does not
forbid writing to outsiders.** In `prod` it does exactly one thing: it lifts the
window, the daily cap and the "one message" rule for the listed usernames, so
that checks do not spend the quota. The only thing that limits the addressees of
a live campaign is the list loaded into `add_recipients`. Checking on yourself
means putting only your own usernames into the campaign.

The criterion for "you may go on": the message arrived in Telegram, the log has
`message sent`, and `status` shows the campaign and the account line with the
cap from the database. Before a real campaign, `DEV_ALLOW_LIST` is cleared and
`make live-up` is run.

## 7. The first real campaign

Before the start, a database dump. It holds the only copy of the knowledge of
who has already been written to:

```
cd ~/tg-agent-mcp/src
make live-dump
```

Expected: the last line like
`-rw-r--r-- 1 user staff 45K ... campaigns-20260908-1030.sql`. The `backups/`
directory does not go into git: it holds names and usernames of real people.

Then three tools in order.

1. **`load_campaign`**: title and template. `{имя}` is replaced with the
   recipient's name; there are no other substitutions, and any other curly
   brace in the text is rejected. The previous draft is cancelled by this.
2. **`add_recipients`**: a batch of up to 50 people per call; each needs
   `contact_id`, `name`, `username`. For more than 50, several calls in a row.
3. **`start`**: the draft goes into work. The reply names the campaign id and
   the send window, for example: `Campaign started (id 25). Sending runs in the
   window 10:00-19:00 on weekdays, within the account's daily cap.`

The `add_recipients` report starts with the line `Accepted: N of M.`, and if
someone was filtered out, the reasons follow with counts. The reasons and their
meaning:

| Reason in the report | What it means |
| --- | --- |
| `no usable Telegram username` | there is no username or it is malformed: 5-32 characters, Latin letters, digits and underscore |
| `no amoCRM contact id` | the entry has no `contact_id` |
| `duplicate: already in the campaign or twice in the batch` | this contact is already in the campaign |
| `stop list` | nothing is ever sent to this person, neither by campaign nor by hand |
| `already received a message` | this or a past campaign wrote to them |
| `already messaged by hand through tg_send` | a touch from the manual send journal |
| `messaged by hand, delivery unknown` | a manual touch whose outcome Telegram did not confirm |
| `the name contains a curly brace, which would end up in the message` | the name would spoil the message text |
| `the message with their name is longer than the Telegram limit` | the template together with the name does not fit |

Filtering is a boundary, not nitpicking: every dropped row means a person to
whom the message would have gone second.

After that the campaign runs by itself. The pace is set by two boundaries: the
10:00-19:00 weekday window and the account daily cap. The pause between sender
cycles is random, 10-30 minutes, so the first message goes out not in the second
of `start` but within half an hour, and 20 messages stretch over about 6 hours
20 minutes. A portion that does not fit into the day goes to the next weekday:
the queue is not lost.

## 8. Daily observation

The `status` tool in the morning and evening is the whole routine. It prints the
campaign with counters, the account line with cap spending and, if the queue
stands, the idle reason in words and numbers. An example of a live reply:

```
Campaign: Invitation run (id 25), status active.
Rows total: 40, sent 12
Accounts:
  main: today 12 of 20 (cap from the database, counting rows in work)
```

There can be several accounts (section 10). Then each has a line, and the reason
for silence stands in the same place, at the account it concerns:

```
Accounts:
  main: today 20 of 20 (cap from the database, counting rows in work), silent: the account's daily cap is used up (20 of 20), sending resumes tomorrow in the window.
  second: today 6 of 20 (cap from the database, counting rows in work)
```

A common reason (a closed window, no campaign, an empty queue) stays one line
for everyone: it is the same for all accounts.

Nine idle reasons and what to do about them:

| Reason in the report | What it means | Action |
| --- | --- | --- |
| the send window is closed, next opening ... MSK. | night, weekend or outside 10:00-19:00 | nothing, it opens by itself |
| the account's daily cap is used up (N of M), sending resumes tomorrow in the window. | the day's portion is taken | nothing, continues tomorrow |
| the account is stopped, only resume_account clears the stop, after a human check. | Telegram restricted the account | stop, section 9 |
| no campaign is running - launch one with start. | draft or pause | `start` |
| the campaign is closed; load_campaign creates a new one. | nothing to work on | a new campaign |
| the template of the running campaign could not be parsed, nothing will be sent for it. | a stray curly brace in the text | rebuild the campaign |
| no rows left in work. | everything is sent | nothing |
| the remaining rows are pinned to another account, only it will take them. | the account that took them will send them | nothing if it is alive; a question to the owner if the report says "and it is not sending now" |
| a guard removes all the remaining rows (N) - stop list, already messaged, or an unusable name. | stop list, already written to, or an unusable name | investigate through `status` with `full` |
| rows are available, the sender takes them one at a time with pauses. | normal course | nothing |

Call the owner without investigating on your own in three cases: the account is
stopped; `status` says the account holding the rows is not sending now; the log
has level `ERROR`.

## 9. When something is wrong

| Symptom | Cause | Action |
| --- | --- | --- |
| `docker compose ps` shows `app` without `healthy`, `curl` to `:8088/health` answers `account stopped: ...` | Telegram restricted the account: `PEER_FLOOD`, a long `FLOOD_WAIT` or a revoked session | sending is stopped entirely and will not return by timer. Open the account by hand, see what is wrong with it, and only then lift the stop with the `resume_account` tool with the account label. A revoked session is lifted not by it but by a repeat `make live-authorize` |
| `curl` to `:8088/health` answers `database: ...` | the database container did not come up | `docker compose -f docker-compose.local.yml ps`, then `make live-up` |
| Sign-in prints `telegram storage ... is locked by another process` | the sender runs in `live` and holds the session file | `TRANSPORT=dry-run` in `.env`, `make live-up`, repeat sign-in |
| `make live-up` fails with `Bind for 127.0.0.1:8087 failed: port is already allocated` | the port is taken by a neighboring service | change `MCP_PORT` and `HEALTH_PORT` in `.env`, fix `url` in `.mcp.json`, `make live-up` |
| The log has `daily limit differs from env` | `ACCOUNT_DAILY_LIMIT` in `.env` is not equal to the cap in the database | this is normal: the variable seeds the cap only when the account is created, and the number from the database applies. To change the cap, see `runbook-account.md`, section 1 |
| The `app` container restarts, the log has `load config` with an error | a broken environment, for example `TRANSPORT=live is refused outside prod (ENV="dev")` | fix `.env` according to the table in section 3 and `make live-up` |
| Claude Code has no mailing tools, `curl` to `/mcp` answers `401` | the `tg-agent-mcp/mcp` entry in the Keychain and `MCP_TOKEN` in `src/.env` differ | make them one value and `make live-up` |

## 10. When one account is not enough

One account at a cap of 20 messages a day gives about a hundred a week. If more
is needed, another account is added to the mailing: the queue stays shared, each
account takes its own recipients from it, the cap and the stop are counted per
account, and the "one person, one message" rule applies to the whole campaign.
The running campaign does not need to be stopped.

Procedure from `src/`, in detail in `docs/runbook-account.md`, section 3:

1. copy `accounts/account.env.example` to `accounts/<label>.env`, enter the
   label, the phone of the second account and the keys from my.telegram.org
   under that number;
2. `make account-up label=<label>`, then `make account-authorize label=<label>`
   and enter the code from Telegram;
3. `make account-live label=<label>`;
4. check: `status` shows a second account line, `make accounts` shows the
   running senders.

Control and correspondence stay with the main account: the added one has
neither MCP nor dialog tools, and replies in it are read by the person who owns
it. A stop caused by a Telegram restriction stops only the account it touched.

## Where to go next

- `docs/first-launch.md`: the procedure of the first live launch and what is
  needed from the account holder;
- `docs/runbook-account.md`: raise the daily cap, replace a banned account with
  a queue transfer;
- `docs/architecture.md`: design, invariants and decisions; read before any
  settings change.

Not fixed on your own and passed to the owner: an account stop after a Telegram
restriction, moving the campaign database between machines, replacing the
account, any manual edits in the database, and any `ERROR` in the log that is
not in the table above.
