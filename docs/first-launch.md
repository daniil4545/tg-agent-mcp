# First live launch

The sequence of actions from the moment the account holder hands over the
Telegram account to the first message to a real person. It was completed on
2026-09-03 on the account `@sender_account`; from now on it serves as the
procedure for the next account.

## Where the service lives

On the owner's machine, in Docker (owner's decision of 2026-09-02). The service
has no network address of its own, sending goes from a home IP, and the mailing
lives only while the machine is on.

## 1. What to get from the account holder

Closed on 2026-09-03; the next account needs the same set:

| What | Why |
| --- | --- |
| account phone number | sign-in to the session, `TELEGRAM_PHONE` |
| access to the sign-in code | Telegram sends the code to the app; without it the session cannot be created |
| confirmation that the account is warmed up | a blank number that starts writing to strangers is banned on the first mailing |
| two-factor password, if enabled | otherwise the sign-in stops halfway |

The account must be a working, lived-in one, not freshly bought: the only
observation I have (2026-08-29) is that a
restriction hit at 2-3 messages a day, so pace does not explain it.

`api_id` and `api_hash` are taken at my.telegram.org under the same number and
live in `TELEGRAM_APP_ID` and `TELEGRAM_APP_HASH`. Keys of another account
will work, but the "application to number" binding is what Telegram sees, and a
mismatch there is unnecessary.

## 2. The environment

Settings are in `src/.env`, modeled on `src/.env.example`; the file does not
go into git. The database password and the MCP token are in the Keychain
(`tg-agent-mcp/postgres`, `tg-agent-mcp/mcp`). `ENV` of the environment is
already `prod` (compose default) and does not need changing: `TRANSPORT=live`
outside `ENV=prod` fails the binary at start.

```
cd src
make live-up        # postgres, migrations, app; MCP on 127.0.0.1:8087
make live-logs      # what is happening
make live-down      # stop
```

Claude Code takes the endpoint from the `.mcp.json` of the `campaigns`
project. The token comes from the environment, so in `~/.zshrc`:

```
export TG_AGENT_MCP_TOKEN=$(security find-generic-password -s tg-agent-mcp/mcp -a MCP_TOKEN -w)
```

The project `.mcp.json` requires approval on the first `claude` run in the
`campaigns` directory. This is protection against a foreign repository, not an
error.

## 3. Account sign-in

Sign-in is a one-time manual operation from a terminal: the code arrives in
Telegram, and only a human can enter it.

```
cd src
make live-authorize
```

The command reads neither the database nor the service config, only the
`TELEGRAM_*` variables. It asks for the code, for the password if two-factor is
on, and prints who it signed in as:

```
authorized: Club @sender_account (id <id>, phone <number>)
```

What to know:

- **One process holds the session.** While the sender runs in
  `TRANSPORT=live`, the session file is locked, and sign-in says "storage is
  locked by another process". Sign-in is done with the environment in
  `dry-run`, a mode in which Telegram is not opened at all. After the first
  launch the environment stays in `live`, so before a repeat sign-in set
  `TRANSPORT=dry-run` back and bring the environment up again, or stop `app`.
- **A foreign session in the storage is rejected.** If the volume already holds
  a session of another number, sign-in does not proceed at all rather than
  passing silently: otherwise the mailing would go out from the wrong account.
- The number in the output must match the one the account holder gave. If it
  does not, investigate; do not launch.

## 4. Check before a live send

- `curl -s http://127.0.0.1:8088/health` answers `ok`;
- `status` through MCP answers and shows the account with the label from
  `ACCOUNT_LABEL`;
- the log has `service started`, and the `transport` field matches the
  environment mode.

## 5. Pilot on your own people

Before the first campaign, a few messages to people who know about it. This is
the only way to see a live username resolve, a closed DM and the real MTProto
error codes: until then the transport was checked only by tests.

Completed on 2026-09-03: two messages (own account and a test bot), both
delivered on the first attempt, replies recognized, campaigns closed on their
own. A closed DM and MTProto failures were never encountered; their behavior is
checked only by tests.

Procedure for the next account:

1. in `src/.env`: `DEV_ALLOW_LIST=<usernames of your own people>`. The list
   works in live mode too, despite the variable name: it is the last boundary
   and does not depend on the transport;
2. in the same file `TRANSPORT=live`, then `make live-up` (recreates the
   container);
3. a campaign on these people through MCP: `load_campaign`, `add_recipients`,
   `start`;
4. in the log watch for `message sent` on every send, `recipient replied` on a
   reply, and `sender idle` with the `reason` field when the queue is quiet.

If the account catches a restriction already during the pilot, stop and talk to
the account holder: mailing from such an account is not possible.

## 6. First campaign

Clear `DEV_ALLOW_LIST`, run `make live-up`, then follow the `campaign` skill in
the separate mailing project: the list is assembled there, and a ready batch
arrives here.

Before the campaign starts, run `make live-dump`. The database holds the
knowledge of who has already been written to; it is the only "one person, one
message" boundary between campaigns, and there is no other copy of it.

Observation is `status` in the morning and evening sessions. An account stop
turns the healthcheck red and writes an ERROR to the log; it is lifted only by
`resume_account`, after a human has opened the account and seen what is wrong
with it.

If the account is banned rather than slowed down, `resume_account` is useless:
the procedure for replacing it with another number, together with moving the
unfinished queue, is in `runbook-account.md`. The procedure for changing the
daily cap of a live account is there too.
