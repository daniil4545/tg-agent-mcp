# tg-agent-mcp architecture

This document describes the design of the built service. One binary, two
long-lived flows: the MCP management server (streamable HTTP) and the MTProto
sender on a live account. The operator is an agent: it brings a ready list and
a text, and the service runs the campaign from there.

Two groups of tools sit on top of one live session: six campaign tools and
four dialog tools (`tg_find`, `tg_send`, `tg_read`, `tg_dialogs`). The account
is shared, so the cap and the stop are shared; there is no separate "test"
account. The "one message" boundary is held by touch accounting, not by
hardware isolation.

The service does not call any external list source: selecting recipients is
the caller's job and lives in a separate project (`campaigns`).

The whole campaign state is in PostgreSQL, so a restart loses neither the
place in the list nor the boundaries. There can be several accounts: each is
its own sender instance with its own session and stop, and the queue is shared.

## Blocks and files

| Block | Responsibility |
| --- | --- |
| MCP server | ten tools (six campaign, four dialog) |
| Campaign | list import, filters, per-person state |
| Sender | day layout (window, pauses, cap), username resolve, MTProto send, failure parsing, stop, inbound messages |
| Dialog | manual correspondence and reading: `tg_send` boundaries, touch journal, history polling (`chat.go`) |
| Transport | send/read interface; MTProto and dry-run implementations |
| Report | campaign summary and per-row result |

Files in `src/internal/app/` are flat; a file name equals its responsibility.

## Data

The source of truth about people is the list from the caller; about the
campaign and delivery it is the service's own database.

| Entity | Key constraints |
| --- | --- |
| Campaign (list, text, state) | one active in the system; accounts share the queue |
| Recipient (external id, username, state, failure reason) | the campaign-contact pair is unique, which protects against a second message |
| Account (daily cap, active stop) | cap and stop are in the DB, not in memory or env; the counter covers sent rows, a live claim and cold manual touches |
| Stop list | checked before every send, not only on import |
| `tg_send` touch journal (username, account, text, `random_id`, coldness) | key is the username; the row is written before the network call; `sent` and `pending` close the person for campaigns |

The MTProto session and peer cache are in bbolt on a volume, separate from the
DB: only the session file is bound to the account, so changing the account
does not touch campaign data.

## States and flows

Recipient states: `planned`, then `sent`, then `replied`; dead ends are
`undelivered`, `skipped`, `cancelled`. Four are terminal; `sent` is
intermediate (it moves to `replied`).

Flow: `load_campaign` (template check, draft), then `add_recipients` (filters,
report), then `start`, then the sender takes a row within the window, pause and
cap, resolves the username, sends, and the row reaches a terminal state. An
inbound message moves the addressee to `replied`; a person who wrote first
(before our send) is taken off the queue with the reason `wrote_first`. That is
not a reply to our message and does not count in the metric.

**Template check** (`load_campaign`, repeated on the way to sending): (1)
braces: after `{имя}` is parsed, no brace may remain, otherwise the call is
refused with the first three positions and the count; (2) length: a 64-character
margin below the Telegram limit. The service does not repair a broken campaign:
`ClaimNext` returns no row, `broken_template` is logged at ERROR (not INFO) and
is visible in `status`; the campaign is rebuilt, and an unusable one is
cancelled with `stop --cancel`.

**Name boundary.** `name_braces` is a CRM name with a brace in it (only when
`{имя}` is in the template). An empty name is not a boundary: `RenderMessage`
removes the substitution together with the comma (`no_name` has not been set
since 2026-09-07). The name boundary and `text_too_long` are checked again on
the row taken from the queue, not only on import. Length is not expressible in
SQL (UTF-16: a character outside the BMP takes two code units).

The filter order is part of the boundary: stop list, "already received" and
manual touch come before the brace in the name and the message length.

Manual touch journal: `pending`, then `sent` or `failed`. `sent` and `pending`
close a person for future campaigns. `failed` (`Failure.NotSent`) returns the
row to the queue; `pending` is a temporary failure (connection drop, timeout,
usually 5xx). While a `pending` row is younger than the `claim` expiry, a
repeated `tg_send` is rejected (`ErrDirectPending`).

Cold means a person who was not written to and has no inbound message. Daily
spending is one sum over both channels (`accountUsedSQL`), under a single cap.

## Observability

The log is a full trace, not only failures. INFO: `message sent`, MCP control
actions (`campaign loaded`, `recipients added`, `campaign started`,
`campaign stopped`, `account resumed`), `recipient replied`,
`recipient wrote first`, `sender idle` (method `IdleReason`, once per
appearance of a reason). `broken_template` is ERROR, not INFO: other idle
states clear on their own, this one only after the campaign is rebuilt.

Nine idle reasons (`status` and `IdleReason`): `IdleWindowClosed`,
`IdleAccountStop`, `IdleDailyLimit`, `IdleNoCampaign`, `IdleBrokenTemplate`,
`IdleQueueEmpty`, `IdleQueueBlocked`, `IdleQueueDisqualified`,
`IdleClaimRaceLost` (the last is a normal queue event). `Idle` carries numbers
("7 of 7", the hour the window opens).

Manual channel: INFO `direct message sent` (`cold`, `random_id`, `message_id`),
INFO `inbound message in manual dialog` (without it, WARN `inbound message not
matched`, a real defect). ERROR requires a human: an account restriction,
inability to authorize, an undelivered database write, `direct message
stuck pending` at start. An ordinary per-addressee failure is a WARN and a
report line.

`status`: cap spending as a number (`usedTodaySQL`), a breakdown "of which
first touches by hand: N", warm touches separately (not limited by the cap);
the cap is taken from the DB (a mismatch with `ACCOUNT_DAILY_LIMIT` gives a
WARN at start); two counters: stuck touches (`StuckDirect`) and rows with an
expired `claim`.

An active stop is observable in three ways: an ERROR in the log, a red
healthcheck (503 on `/health`), and `status` in the morning and evening.

## Side effects and idempotency

There is one outward action: sending to Telegram, by campaign or by hand.

The queue is in the database with a claim model: a transaction under a lock on
the account row checks the boundaries and takes a row (`FOR UPDATE SKIP
LOCKED`), the network call happens strictly between transactions, and
finalization is a separate transaction; an expired claim returns to the
selection. Idempotency is `RandomID`: a retry of a row does not create a second
message (`RANDOM_ID_DUPLICATE` counts as success). A temporary failure returns
the row to the queue, with a budget of 5 attempts. An account-restriction
failure is not retried; it raises the stop.

A manual touch follows the same order, but `random_id` is fresh on every call
(a live dialog is full of identical replies, and a stable id would swallow them
as a duplicate); a repeated call is guarded by the `pending` row check, not by
`random_id`.

A manual send takes the same account-row lock as the queue claim. Race guard:
a message does not go to a person whose row a campaign took into work
(`account_id`, set only by the claim); campaigns in `active`, `stopped` and
`draft` are counted, and a live claim blocks in any status.

## External boundaries

**Telegram MTProto.** Sending text, username resolve, finding a person (an exact
username is a separate request), dialog history, dialog list.

`messages.getDialogs` is the only place where the service sees more than
people: a person, a group and a channel in one reply, with the kind named by
what you can do (person: you can write; group: read the correspondence;
channel: a stream of posts; a supergroup counts as a group for the reader).
The send boundaries are untouched: `ResolveUsername` rejects everything except
`PeerUser`. A channel or supergroup from the list is stored in the addressee
storage with its access hash (groundwork for reading group history, M7 slice 2).

A failed resolve of an exact username is a WARN and is not passed off as an
answer (when fuzzy search is empty, `tg_find` returns the failure reason, not
"nobody found").

Failures are a closed list of ten kinds (`internal/telegram/errors.go`): client
not ready, a request to wait, restriction on messages to strangers, a ban on
writing to the addressee, username taken by a group or channel, a rejected
attachment, lost authorization, a malformed request, a failure of the own
addressee storage, other temporary. Next to the kind is the MTProto error code
(`FLOOD_WAIT`, `MSG_ID_INVALID`). A restriction belongs to the account, not to
the call: `PEER_FLOOD`, a long `FLOOD_WAIT` and a revoked session stop the
whole account. The stop is not lifted by a timer: `resume_account` is for a
Telegram restriction, `authorize` (`docs/first-launch.md`) is for a revoked
session. Only a short `FLOOD_WAIT` is waited out.

**MCP.** `modelcontextprotocol/go-sdk` (v1): streamable HTTP,
`auth.RequireBearerToken`. Arguments stay under ~8 KB (Claude Code bugs with
large tool arguments), hence the recipient batches. Waiting for a reply in
`tg_read` is 45 seconds (60+ timed out on the client). Mutations are confirmed
by Claude's standard permission mechanism. The endpoint is one instance
(`MCP_ENABLED`), and the process is bound to an account (`ACCOUNT_LABEL`).

**List sources.** The service calls none at all (no CRM, no files, no foreign
databases); the only input is `add_recipients`. Knowledge of the data layout
lives in `campaigns`.

## Environments and configuration

There is one environment, and it is the production one, on the owner's machine.
Transport: `dry-run` writes to the log and does not go outside; `live` goes
through MTProto and does not start outside `ENV=prod`.

`DEV_ALLOW_LIST` is the last boundary before going outside, only with
`ENV=dev`; it does not start in `prod`. It restricts only sending, not reading
(a decision, not an omission): the resolve for reading (`Lookup`) is separate
from the resolve for sending (`Resolve`), otherwise with a non-empty allow-list
you could not read a dialog with someone you can no longer write to.

Set in configuration, not in code: the account daily cap, pause bounds, send
window, FLOOD_WAIT threshold, transport mode (there are no verified pace
figures). The cap is only seeded by configuration; the effective number is in
`accounts` and is changed in the DB (`runbook-account.md`).

The pause between sender cycles (default 10-30 minutes) is the whole sender
tick: 20 messages = 19 pauses, ~6h20m against a 9-hour window; the extreme case
(all at 30 minutes) does not fit, and the remainder goes to the next weekday.

## Invariants

| Invariant | Where it is enforced |
| --- | --- |
| One person, one message, including past campaigns and manual touches | uniqueness of the campaign-contact pair; touch journal in the filter and claim; retry on send |
| The window and the cap hold both the campaign and a first touch to a stranger | check in the send transaction, counted from the DB; a warm reply is deliberately exempt |
| An account restriction stops only that account | stop at the account level |
| The stop is not lifted by a timer | lifted only by a tool, on a human command |
| The stop list is closed for manual sending too | check by username and by amoCRM contact |
| Without a usable username, and under `{имя}` with a brace, a person does not go to sending | import filter plus a repeat on the row taken from the queue; length is checked on the taken row |
| A message with an unparsed brace does not go out | check in `load_campaign`/`AddRecipients`/`StartCampaign`; `ClaimNext` returns no row |
| The service does not call external list sources | there is no client for them in the service (reviewed, passed twice) |

## Decisions

Main decisions:

A separate service (not a function of the lead-handling system, so that a flood stop does
not silence inbound leads); several accounts mean several instances with a
shared queue (one MTProto session per process); the text is a template plus
`{имя}`, without free rewriting; segment selection outside the service
(`campaigns`); no Telegram bot, management is MCP; resolve by phone number is
off by default; one account for campaigns and dialogs, and the boundary is the
touch journal, not the account role; the boundary is on a cold contact, not on
a tool; the journal is written before the network call; the journal key is the
username (mutable, an accepted limitation); waiting for a reply is history
polling, not a subscription to gotd updates.

## Limitations and open items

- Not encountered live: a closed DM, a resolve failure, an account
  restriction; behavior rests on tests.
- A `planned` row in a cancelled campaign, if the process dies within the claim
  expiry window, waits for a manual sweep.
- A journal row stuck in `pending` closes the person forever (the queue claim
  has a second chance, the journal does not). It is visible as a counter in
  `status` and an ERROR at start; the fix is SQL by hand.
- Groups and channels are visible only through the dialog list: the tools do
  not read their history, `tg_find` does not search for them, and sending is
  impossible. Reading history is the next slice (M7 slice 2). Attachments are
  neither downloaded nor sent.
- A person's reply closes the row (`replied`), but there are no notifications
  about replies; a human reads the DM. With two or more accounts the question
  returns.
