# Setup guide

From a clean machine to the first campaign: install the stack, sign in to a
Telegram account, connect Claude Code, check the chain on yourself and send.
The guide assumes macOS and Docker Desktop; on Linux only the Keychain commands
in section 6 differ.

Every step ends with a command and what it must print. If the output does not
match, stop and find the cause before going on: the service writes to strangers
from a live account, and the price of a mistake is the account.

## Contents

1. [What you get and the ground rules](#1-what-you-get-and-the-ground-rules)
2. [What to prepare](#2-what-to-prepare)
3. [Install and configure](#3-install-and-configure)
4. [Start the stack](#4-start-the-stack)
5. [Sign in to the account](#5-sign-in-to-the-account)
6. [Connect Claude Code](#6-connect-claude-code)
7. [Check the chain before a real send](#7-check-the-chain-before-a-real-send)
8. [The first campaign](#8-the-first-campaign)
9. [Daily observation](#9-daily-observation)
10. [When something is wrong](#10-when-something-is-wrong)
11. [Adding a second account](#11-adding-a-second-account)

## 1. What you get and the ground rules

At the end the machine runs its own stack: PostgreSQL with the campaign
database, the application with the MCP server on `127.0.0.1:8087`, and a Claude
Code session with ten mailing and dialog tools.

Four rules hold for the whole guide:

1. **One account, exactly one stack.** Two processes on one number mean two
   independent queues and two databases that do not know about each other. The
   "one person, one message" rule is held only by the database, so two stacks
   can write twice to the same person.
2. **Never reuse a session file from another machine.** Sign in anew with
   `make live-authorize`, after the old stack has been stopped.
3. **Do not move a campaign database by hand.** The database holds the record
   of who has already been written to. Moving a dump between machines is a
   deliberate step, not a convenience.
4. **Stay in `TRANSPORT=dry-run` until the account is signed in.** In that mode
   messages go to the log, not to Telegram.

## 2. What to prepare

Sign-in needs all four `TELEGRAM_*` values; it fails on the first missing one.

| What | Why | Where to get it |
| --- | --- | --- |
| Docker Desktop, running | the whole stack lives in containers | docker.com |
| The phone number of the Telegram account | `TELEGRAM_PHONE`, session sign-in | the account holder |
| `api_id` and `api_hash` | `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`; the client cannot be built without them | create the pair at my.telegram.org, signed in under the same number |
| Access to the confirmation code | Telegram sends the code to an app that holds a session of this account; only a human can enter it | a phone with an active session |
| The two-factor password, if enabled | without it sign-in stops halfway | the account holder |

The pair `api_id` and `api_hash` belongs to the number, not to the machine. It
cannot be derived and cannot be restored without access to the number.

Use a lived-in account, not a freshly registered one: a blank number that
starts writing to strangers is restricted quickly.

## 3. Install and configure

```
git clone https://github.com/daniil4545/tg-agent-mcp.git ~/tg-agent-mcp
docker compose version
```

Expected: `Cloning into ...` and a line like `Docker Compose version v5.3.1`.
`Cannot connect to the Docker daemon` means Docker Desktop is not running.

Prepare the settings:

```
cd ~/tg-agent-mcp/src
cp .env.example .env
```

`.env` does not go into git. Fill in:

| Variable | What it is | What to fill in |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | password of the local campaign database | any long random string; it does not change after the database is created |
| `MCP_TOKEN` | bearer token of the MCP endpoint; without it the server answers 401 | a random string, for example the output of `openssl rand -hex 32` |
| `ACCOUNT_LABEL` | account label; the row in `accounts` is created under it | any short label, for example `main` |
| `ACCOUNT_DAILY_LIMIT` | daily cap of a new account (default 20) | keep the default for a new account |
| `TRANSPORT` | `dry-run` writes to the log, `live` sends to Telegram | `dry-run` until sign-in |
| `ENV` | `dev` or `prod`; `live` works only in `prod` | `dev` while checking, `prod` for live |
| `DEV_ALLOW_LIST` | service recipients, usually your own usernames | your usernames, comma separated, for the duration of checks |
| `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`, `TELEGRAM_PHONE` | account credentials | from section 2; empty until sign-in |
| `TELEGRAM_PROXY_URL` | proxy for MTProto, optional | empty unless your network needs it |
| `DIGEST_ACCOUNT`, `DIGEST_TO` | account that sends the evening digest, and its recipients | empty with a single sender; recipients must be inside `DEV_ALLOW_LIST` |
| `MCP_PORT`, `HEALTH_PORT` | loopback ports | keep 8087 and 8088 unless something else holds them |

`ACCOUNT_LABEL` matters more than it looks. A queue row once taken by an account
stays pinned to it. If you restore a database dump under another label, the
application creates a second account row and the unfinished queue rows stay out
of reach of the new account.

`ACCOUNT_DAILY_LIMIT` is applied only when the account row is created. After
that the cap lives in the database; to change it, see
[runbook-account.md](runbook-account.md), section 1.

## 4. Start the stack

```
cd ~/tg-agent-mcp/src
make live-up
```

The first start builds the image and takes several minutes. At the end the
command prints:

```
MCP: http://127.0.0.1:8087/mcp
```

Check both containers:

```
docker compose -f docker-compose.local.yml ps --format 'table {{.Service}}\t{{.Status}}'
```

Expected:

```
SERVICE    STATUS
app        Up 5 seconds (healthy)
postgres   Up 20 seconds (healthy)
```

The `healthy` mark on `app` may take up to 30 seconds: the healthcheck starts 15
seconds after launch and runs every 15 seconds.

Check health and the log:

```
curl -s http://127.0.0.1:8088/health; echo
make live-logs
```

Expected: `ok`, and JSON lines in the log, among them (the `time`, `service` and
`env` fields are left out here):

```
{"level":"INFO","msg":"service started","account_label":"main","transport":"dry-run","mcp_enabled":true,"health_addr":":8081"}
{"level":"INFO","msg":"sender idle","account":"main","reason":"no_active_campaign"}
```

The `transport` field must match `.env`. `sender idle` with the reason
`no_active_campaign` is normal: there is no campaign yet. Leave the log with
Ctrl+C; the stack keeps running. A line `daily limit differs from env` is not an
error: the cap in the database differs from the variable, and the database wins.

## 5. Sign in to the account

Sign-in is a one-time manual step: the code arrives in Telegram and only a human
can enter it. Enter `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH` and `TELEGRAM_PHONE`
in `src/.env`, make sure `TRANSPORT=dry-run`, then run `make live-up` again so
the container sees them. In live mode the running sender locks the session file,
and sign-in would refuse.

```
cd ~/tg-agent-mcp/src
make live-authorize
```

The command reads only the `TELEGRAM_*` variables. It asks for the code, for the
password if two-factor is on, and prints who it signed in as:

```
authorized: <name> @<username> (id <id>, phone <number>)
```

The number must be the one you intended. If it is not, stop and find out why.

| Refusal text | What happened | What to do |
| --- | --- | --- |
| `telegram storage ... is locked by another process` | the stack runs in `TRANSPORT=live` and holds the session file | set `TRANSPORT=dry-run`, `make live-up`, repeat sign-in |
| `telegram storage holds a session of another account (phone ..., expected ...)` | the volume holds a session of another number | this is an account replacement, not a first sign-in: see [runbook-account.md](runbook-account.md), section 2 |
| `TELEGRAM_APP_ID is required` (and the same for `APP_HASH`, `PHONE`) | the variable is empty in `.env` | fill it in and run `make live-up` again |

A foreign session in the storage is rejected on purpose: otherwise the mailing
could go out from the wrong account.

Go live only after the checks in section 7: in `src/.env` set `TRANSPORT=live`
and `ENV=prod`, run `make live-up`, and look for `service started` with
`"transport":"live"` in the log.

## 6. Connect Claude Code

Claude Code reaches the endpoint with the bearer token. Keep the token out of
git and out of the session settings. On macOS, put it into the Keychain (the
same value as `MCP_TOKEN` in `src/.env`):

```
security add-generic-password -s tg-agent-mcp/mcp -a MCP_TOKEN -w
```

The command asks for the value and prints nothing on success. If an entry with
that name exists, `security` refuses; keep the old value or remove it with
`security delete-generic-password -s tg-agent-mcp/mcp` and create it again.

Create a working directory to launch Claude Code from. Keep it apart from the
clone so that `git pull` never argues with local files.

```
mkdir -p ~/tg-agent/scripts
```

`~/tg-agent/.mcp.json`:

```json
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

`~/tg-agent/scripts/mcp-headers.sh`:

```bash
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

Expected: `401`. Without a token the server answers nobody; that is a boundary,
not a failure.

```
cd ~/tg-agent
claude mcp list
```

On the first run the server is listed as pending approval: a project
`.mcp.json` needs a human to approve it. Run `claude` in this directory, approve
the server, then repeat `claude mcp list`.

Inside a session the tool list must hold ten names: `load_campaign`,
`add_recipients`, `start`, `stop`, `resume_account`, `status`, `tg_find`,
`tg_send`, `tg_read`, `tg_dialogs`. If they are missing, the token in the
Keychain and `MCP_TOKEN` in `src/.env` differ.

## 7. Check the chain before a real send

Two passes: first without Telegram at all, then to your own usernames.

**First pass, dry-run.** In `src/.env`: `ENV=dev`, `TRANSPORT=dry-run`,
`DEV_ALLOW_LIST=<your usernames>`, then `make live-up`. Nothing goes outside:
there is no live Telegram, and in `dev` the list is also the last boundary of
the transport.

Through Claude Code:

1. `load_campaign` with a title and a template, for example
   `Привет, {имя}! Это проверка контура.` (Russian: "Hi, {имя}! This is a check
   of the setup.");
2. `add_recipients` with one entry: any `contact_id`, a `name` and your own
   `username`;
3. `start`.

Expected: the `add_recipients` reply starts with `Accepted: 1 of 1.`, and within
a minute or two the log shows

```
{"level":"INFO","msg":"dry-run send","username":"<your username>","random_id":...,"text":"Привет, ..."}
```

The chain is assembled: campaign, queue, guards, sender. Nothing went to
Telegram. A username listed in `DEV_ALLOW_LIST` is a service recipient: the send
window, the daily cap and the one-message rule do not apply to it.

**Second pass, live.** After sign-in: `TRANSPORT=live`, `ENV=prod`,
`DEV_ALLOW_LIST` with your own usernames, `make live-up`. Run the same campaign
to your own username. The message must arrive in Telegram, and the log must
show

```
{"level":"INFO","msg":"message sent","username":"<your username>",...}
```

Be exact about the boundary: **in `prod`, `DEV_ALLOW_LIST` does not forbid
writing to outsiders.** In `prod` it only lifts the window, the daily cap and
the one-message rule for the listed usernames, so checks do not spend the quota.
The only thing that limits the addressees of a live campaign is the list you
load with `add_recipients`. To check on yourself, put only your own usernames
into the campaign.

Go on when the message arrived, the log has `message sent`, and `status` shows
the campaign and an account line with the cap from the database. Before a real
campaign clear `DEV_ALLOW_LIST` and run `make live-up`.

A live trial is also the only way to see a username resolve, a closed DM and real
MTProto error codes; until then the transport is covered by tests only.

## 8. The first campaign

Take a database dump before the start. It holds the only copy of the record of
who has already been written to:

```
cd ~/tg-agent-mcp/src
make live-dump
```

Expected: the last line lists a new `campaigns-YYYYMMDD-HHMM.sql` in `backups/`.
The directory is ignored by git: it holds names and usernames of real people.

Then three tools in order:

1. **`load_campaign`**: title and template. `{имя}` is replaced with the
   recipient's name; there are no other substitutions, and any other curly brace
   is rejected. The previous draft is cancelled.
2. **`add_recipients`**: up to 50 people per call; each needs `contact_id`,
   `name`, `username`. For more than 50, call several times.
3. **`start`**: the draft goes into work. The reply names the campaign id and the
   send window.

The `add_recipients` report starts with `Accepted: N of M.`; if someone was
filtered out, the reasons follow with counts.

| Reason in the report | What it means |
| --- | --- |
| `no usable Telegram username` | no username, or a malformed one: 5-32 characters, Latin letters, digits and underscore |
| `no contact id` | the entry has no `contact_id` |
| `duplicate: already in the campaign or twice in the batch` | this contact is already in the campaign |
| `stop list` | nothing is ever sent to this person, neither by campaign nor by hand |
| `already received a message` | this or an earlier campaign wrote to them |
| `already messaged by hand through tg_send` | a touch from the manual send journal |
| `messaged by hand, delivery unknown` | a manual touch whose outcome Telegram did not confirm |
| `the name contains a curly brace, which would end up in the message` | the name would spoil the message text |
| `the message with their name is longer than the Telegram limit` | the template together with the name does not fit |

Filtering is a boundary: every dropped row is a person who would have received a
second message.

After that the campaign runs by itself. Two guards set the pace: the weekday
window (10:00-19:00 Moscow time) and the account daily cap. The pause between
sender cycles is random, 10-30 minutes by default, so the first message goes out
within half an hour of `start`, and 20 messages stretch over about six hours.
What does not fit into the day moves to the next weekday; the queue is not lost.

## 9. Daily observation

The `status` tool in the morning and evening is the whole routine. It prints the
campaign with counters, one line per account with cap spending and, if the queue
stands, the idle reason in words and numbers:

```
Campaign: Spring invitation (id 1), status active.
Rows total: 40, sent 12
Accounts:
  main: today 12 of 20 (cap from the database, counting rows in work)
```

With several accounts each has a line, and a reason for silence stands at the
account it concerns. A common reason (a closed window, no campaign, an empty
queue) stays one line for everyone.

| Reason in the report | What it means | Action |
| --- | --- | --- |
| the send window is closed, next opening ... MSK. | night, weekend or outside 10:00-19:00 | nothing, it opens by itself |
| the account's daily cap is used up (N of M), sending resumes tomorrow in the window. | the day's portion is taken | nothing |
| the account is stopped, only resume_account clears the stop, after a human check. | Telegram restricted the account | section 10 |
| no campaign is running - launch one with start. | draft or pause | `start` |
| the campaign is closed; load_campaign creates a new one. | nothing to work on | a new campaign |
| the template of the running campaign could not be parsed, nothing will be sent for it. | a stray curly brace in the text | rebuild the campaign |
| no rows left in work. | everything is sent | nothing |
| the remaining rows are pinned to another account, only it will take them. | the account that took them will send them | nothing if it is alive; investigate if the report says it is not sending now |
| a guard removes all the remaining rows (N) - stop list, already messaged, or an unusable name. | the guards filtered everything left | investigate with `status` and `full` |
| rows are available, the sender takes them one at a time with pauses. | normal course | nothing |

Stop and investigate in three cases: the account is stopped; `status` says the
account holding the rows is not sending now; the log has level `ERROR`.

## 10. When something is wrong

| Symptom | Cause | Action |
| --- | --- | --- |
| `app` without `healthy`, `curl` to `:8088/health` answers `account stopped: ...` | Telegram restricted the account: `PEER_FLOOD`, a long `FLOOD_WAIT` or a revoked session | sending is stopped entirely and does not return by timer. Open the account by hand, see what is wrong, then lift the stop with `resume_account` and the account label. A revoked session is fixed by a repeat `make live-authorize` instead |
| `curl` to `:8088/health` answers `database: ...` | the database container did not come up | `docker compose -f docker-compose.local.yml ps`, then `make live-up` |
| Sign-in prints `telegram storage ... is locked by another process` | the sender runs in `live` and holds the session file | `TRANSPORT=dry-run` in `.env`, `make live-up`, repeat sign-in |
| `make live-up` fails with `port is already allocated` | another service holds the port | change `MCP_PORT` and `HEALTH_PORT` in `.env`, fix `url` in `.mcp.json`, `make live-up` |
| The log has `daily limit differs from env` | `ACCOUNT_DAILY_LIMIT` differs from the cap in the database | normal: the variable seeds the cap only when the account is created. To change the cap see [runbook-account.md](runbook-account.md), section 1 |
| `app` restarts, the log has `load config` with an error | a broken environment, for example `TRANSPORT=live is refused outside prod (ENV="dev")` | fix `.env` by the table in section 3, `make live-up` |
| Claude Code has no tools, `curl` to `/mcp` answers `401` | the Keychain entry and `MCP_TOKEN` in `src/.env` differ | make them one value, `make live-up` |

If the account is banned rather than slowed down, `resume_account` does not
help: replace the account together with its unfinished queue, as described in
[runbook-account.md](runbook-account.md).

## 11. Adding a second account

One account at a cap of 20 messages a day gives about a hundred a week. A second
account joins the same campaign: the queue stays shared, each account takes its
own recipients, the cap and the stop are counted per account, and the
one-message rule applies to the whole campaign. The running campaign need not be
stopped.

From `src/` (details in [runbook-account.md](runbook-account.md), section 3):

1. copy `accounts/account.env.example` to `accounts/<label>.env` and fill in the
   label, the phone of the second account and the keys from my.telegram.org
   under that number;
2. `make account-up label=<label>`, then `make account-authorize label=<label>`
   and enter the code from Telegram;
3. `make account-live label=<label>`;
4. check: `status` shows a second account line, `make accounts` lists the
   running senders.

Control and dialogs stay with the main account: the added one has neither MCP
nor dialog tools. A stop caused by a Telegram restriction stops only the account
it touched.

## Where to go next

- [runbook-account.md](runbook-account.md): raise the daily cap, replace a banned
  account with a queue transfer, add a sender;
- [architecture.md](architecture.md): design, invariants and decisions; read it
  before any settings change.

Not to be fixed ad hoc: an account stop after a Telegram restriction, moving the
campaign database between machines, replacing the account, manual edits in the
database, and any `ERROR` in the log that is not in the tables above.
