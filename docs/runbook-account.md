# Operating a mailing account

Three operations on a live environment: raise the daily cap, replace an account
after a ban, and add one more account to the mailing. All are done by hand;
none has an MCP tool. First launch and sign-in to the main account are in
`setup.md`; this document does not repeat them.

## The account lives in three places

| Place | What is there | What happens to it on replacement |
| --- | --- | --- |
| row in `accounts` (label from `ACCOUNT_LABEL`) | daily cap, stop and its reason | a new one is created, the old one stays forever |
| session file in the process volume | MTProto session, peer cache, update state | erased: a foreign session in the storage blocks sign-in |
| `recipients.account_id` and `direct_messages.account_id` | who wrote to whom | not touched; this is the journal |

The session volume depends on whose account it is: for the main one it is
`tg-agent-mcp-live_telegram_data`, for an added sender it is
`tg-agent-mcp-acc-<label>_telegram_data`.

One MTProto session lives in exactly one process, so each account is its own
process. The main one is in `docker-compose.local.yml` and holds the MCP; the
others are brought up by section 3 and carry no control.

## 1. Raise the daily cap of a live account

`ACCOUNT_DAILY_LIMIT` sets the cap only when the account row is created:
on a label conflict `EnsureAccount` does not overwrite the row, and a mismatch
between the variable and the DB is reported at start by the warning `daily
limit differs from env`. So the cap of a live account is changed in the DB.

The standard way is a repository command, from the project root:

```
deploy/safe-ssh.sh account-limit main 20 apply
```

Without the word `apply` the command refuses to run (`account-limit requires:
<label> <N> apply`), and a value outside the range 1..40 is rejected by the
wrapper itself. A bare `UPDATE accounts SET daily_limit = ...` does the same
thing but bypasses these checks, so it is needed only when the wrapper is
unavailable.

What matters:

- no restart of the environment is needed: the cap is read from the DB in every
  selection transaction, and `status` shows the new number at once;
- the change survives a rebuild of the environment (`make live-up --build`) and
  a machine restart: it is in the volume `tg-agent-mcp-live_pgdata`, not in the
  image;
- it does not survive the loss of the database volume. So that an environment
  brought up from scratch does not return to the old number, the same value
  stands in `ACCOUNT_DAILY_LIMIT` in `src/.env`, in `.env.example` and as the
  default in `docker-compose.local.yml`;
- the cap must be greater than zero (`accounts_daily_limit_check`), otherwise
  the database rejects the query.

Check: `status` prints the account line "today N of 20 (cap from the database, counting rows in work)".

## 2. Replacing an account after a ban

The procedure is assembled from existing means. It needs no new code, but two
steps are a database query, not a tool.

### What to decide before starting

The replacement is done **under a new** `ACCOUNT_LABEL`, not by signing in
another number under the old label. The reason is the boundary: a queue row
once taken by an account stays with it even after returning to the queue,
because delivery retry rests on `random_id`, and Telegram deduplicates it only
within one account. Signing in another number under the old label would leave
the rows pinned to the label and silently give a person a second message. Under
a new label the same risk remains, but it is reduced to one row and is cleared
by hand in step 5.

Cost of the decision: the old account row stays in `accounts` forever (the
journal references it), and `status` prints both accounts.

### Steps

1. **Record the situation.** `status` through MCP: campaign id, how many were
   sent, what the account line says. Then from `src/`: `make live-dump`. The
   database holds the only copy of the knowledge of who has been written to.

2. **Stop the campaign.** The `stop` tool without `cancel`: this is a pause, the
   queue stays. With the pause, the sender will not take new rows while the
   replacement goes on. The campaign continues with the same `start` at the
   end.

3. **Prepare the environment for sign-in.** In `src/.env`:
   `TRANSPORT=dry-run` (in live mode the session file is locked by the
   process), `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH`, `TELEGRAM_PHONE` of the
   new account, the new `ACCOUNT_LABEL`, the needed `ACCOUNT_DAILY_LIMIT`.
   Application keys are taken at my.telegram.org under the same number as the
   account.

4. **Erase the old account's session.** From `src/`:

   ```
   make live-down
   docker volume rm tg-agent-mcp-live_telegram_data
   make live-up
   ```

   `make live-down` does not delete named volumes, and the campaign database
   stays in place: only the session volume is removed. Without this step
   sign-in passes silently on the old session, and the number check rejects it
   with the error `storage holds a session of another account`.

   Then sign in: `make live-authorize`. The printed number and username must
   match the new account; if they do not, investigate rather than continue.

5. **Release the unfinished queue.** Rows taken by the old account are
   unavailable to the new one: the selection takes only `account_id IS NULL` or
   its own. First look at what is left (substitute the old account's label):

   ```
   docker compose -f docker-compose.local.yml exec -T postgres \
     psql -U tg_agent_mcp -d tg_agent_mcp -c "
     SELECT r.id, r.username, r.claimed_at, r.reason
       FROM recipients r JOIN accounts a ON a.id = r.account_id
      WHERE r.state = 'planned' AND a.label = 'OLD_LABEL'
      ORDER BY r.claimed_at NULLS FIRST;"
   ```

   Rows with an empty `claimed_at` are handed to the new account without
   questions: no message went out for them.

   ```
   docker compose -f docker-compose.local.yml exec -T postgres \
     psql -U tg_agent_mcp -d tg_agent_mcp -c "
     UPDATE recipients r SET account_id = NULL
       FROM accounts a
      WHERE a.id = r.account_id AND a.label = 'OLD_LABEL'
        AND r.state = 'planned' AND r.claimed_at IS NULL;"
   ```

   A row with a non-empty `claimed_at` is one on which the ban caught a send:
   whether the message went out, the database does not know. For each such row
   a human decides, looking at `make live-logs` (`message sent` with this
   username) and at the old account's dialog in Telegram. If it went out, close
   it as sent:

   ```
   UPDATE recipients SET state = 'sent', sent_at = now(), claimed_at = NULL
    WHERE id = <id>;
   ```

   If it did not go out, reset `claimed_at` and `account_id` with the same query
   as for the rest. If you could not tell, leave it as is: the person stays
   closed to the mailing, and that is cheaper than a second message.

6. **Return to live mode.** In `src/.env`: `TRANSPORT=live`, then `make
   live-up`. Check: `curl -s http://127.0.0.1:8088/health` answers `ok`, the log
   has `service started` with `transport=live`, and `status` shows two
   accounts, the old one and the new one with zero spending today.

7. **Continue the campaign.** The `start` tool with the same `campaign_id`. The
   first message will not go out instantly but within `SEND_PAUSE_MAX`: the
   pause is the whole sender cycle.

### What the procedure does not do

- **The old account is not resurrected.** Its row stays `stopped`, and there is
  no need to call `resume_account` on it: the number is banned, not slowed
  down. The row must not be deleted, because the journal references it.
- **Daily spending is not carried over.** The new account starts the day from
  zero, even if the old one managed to send its portion today. This is
  deliberate: the cap exists for the number, and the number is different. Watch
  this by hand on replacement day.
- **Replacement is not addition.** Here the old account leaves the environment;
  to have several accounts run the mailing at once, see section 3.
- **There is no MCP tool for replacement.** Steps 4 and 5 are done by hand. If
  accounts start dropping more often than once a week, this is the first
  candidate for automation.

## 3. Add an account to the mailing

There is one campaign and several accounts: the queue is shared, each takes its
own recipients from it, the daily cap and the stop are counted per account, and
the "one person, one message" rule stays common to the whole campaign. Adding
happens on the fly: the running campaign does not need to be stopped.

The sender is brought up as its own project on top of the network and database
of the main environment; all commands are from `src/`.

1. **Account file.** Copy `accounts/account.env.example` to
   `accounts/<label>.env`, fill in `ACCOUNT_LABEL` (equal to the file name),
   `TELEGRAM_PHONE` and the keys `TELEGRAM_APP_ID`, `TELEGRAM_APP_HASH` from
   my.telegram.org under this same number: they are not inherited from the main
   environment. Each account has its own label, and it cannot change after the
   first start: spending, stop and taken queue rows are bound to it. `make`
   rejects a label that matches the main environment.
2. **Bring it up in dry-run:** `make account-up label=<label>`. In this mode
   sending goes to the log, not to Telegram: the session file is still empty.
3. **Sign in to the account:** `make account-authorize label=<label>`. Telegram
   sends a code to the number, and only a human can enter it.
4. **Switch to live mode:** `make account-live label=<label>`; the target
   rebuilds the image, otherwise the sender would stay on the old code. The
   container is recreated on the same session.
5. **Check:** `status` shows a second account line with spending and cap;
   `make accounts` lists the files and the running senders.

What matters:

- the cap of the new account is set by `ACCOUNT_DAILY_LIMIT` in its file and
  only at the first start; after that it lives in the DB and is changed by
  section 1;
- the send window, pauses, flood threshold and the list of service addressees
  come from the shared `src/.env` and must match across all processes: they
  take part in the spending count and in the boundaries;
- the sender has no network address of its own: no MCP and no ports outside.
  Control and dialog tools stay with the main account, and replies to the
  second account are read by its owner;
- the accounts write from one address unless `TELEGRAM_PROXY_URL` is set in the
  account file;
- to stop one sender, use `make account-down label=<label>`; the session volume
  stays, and bringing it up again resumes work without a repeat sign-in. Its
  log is `make account-logs label=<label>`.

An account stop caused by a Telegram restriction stops only that account:
neighboring accounts continue the campaign, and `status` names the stopped one
and prints the reason for silence for each account separately.
