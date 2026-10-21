# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-10-21

First public release.

### Added

- MCP server over streamable HTTP with a bearer token and ten tools:
  `load_campaign`, `add_recipients`, `start`, `stop`, `resume_account`,
  `status`, `tg_find`, `tg_send`, `tg_read`, `tg_dialogs`.
- Campaign delivery over a ready recipient list and a message template with the
  `{имя}` placeholder: queue, random pauses, a weekday send window, a daily cap
  per account and a screening report for every batch.
- Delivery guards held in PostgreSQL: one message per person across campaigns
  and manual sends, a stop list, an account stop on `PEER_FLOOD` or a long
  `FLOOD_WAIT` that only a human lifts, and a touch-log row written before the
  network call.
- Dialog tools on the same Telegram session: find a person, send by hand, read a
  dialog and optionally wait for a reply, list dialogs with unread counts.
- MTProto client for a real user account with a `dry-run` transport that writes
  to the log; `TRANSPORT=live` refuses to start outside `ENV=prod`.
- A second token, `MCP_AGENT_TOKEN`, that exposes only `tg_send` and `tg_read`
  and only for usernames in `AGENT_ALLOW_LIST`.
- Several accounts over one shared queue, one sender process per account.
- `status` names the reason a queue is idle, with numbers.
- Docker Compose stacks for local, sender-only and Coolify deployment, goose
  migrations, a Makefile with `ci-check`, and `deploy/safe-ssh.sh` for named
  operations on the live database.
- GitHub Actions workflow that runs migrations, the integration tests against a
  PostgreSQL service container, `go vet`, `gofmt`, `golangci-lint` and a build.
