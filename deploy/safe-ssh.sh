#!/usr/bin/env bash
# Named operations on the live tg-agent-mcp stack instead of arbitrary SQL.
# The stack runs on this machine in Docker (docker-compose.local.yml), so there
# is no ssh here; the file name keeps the usual convention: safe-ssh.sh is
# an allow-list of actions that read by default and mutate only with "apply".
#
# The touch log and campaign rows are edited ONLY for the owner's accounts:
# they are needed for periodic checks of the mailing, and the "one person, one
# message" guard is lifted for them by hand (owner's decision of 2026-09-06).
# The script rejects any other username - this is how the command is built, not
# an agreement.
#
# The list below is deliberately wider than DEV_ALLOW_LIST. DEV_ALLOW_LIST
# names service recipients for whom the service does not count the window and
# the cap; here are all the owner's accounts whose traces may be forgotten. An
# account that tests the guards themselves must be outside DEV_ALLOW_LIST and
# still be cleaned between runs.
set -euo pipefail

owner_accounts=(smirnov dev_account daniilsukhar manager_one)

compose=(docker compose -f "$(cd "$(dirname "$0")/../src" && pwd)/docker-compose.local.yml")
psql=("${compose[@]}" exec -T postgres psql -U tg_agent_mcp -d tg_agent_mcp -v ON_ERROR_STOP=1 -P pager=off)

action="${1:-}"
arg="${2:-}"
confirm="${3:-}"

usage() {
    cat <<'USAGE'
Usage: deploy/safe-ssh.sh <action> [args...]

Read-only actions:
  registry-owner            touches and campaign rows of the owner's accounts

Mutating actions:
  registry-forget <username> apply   forget the traces of an owner account:
                                manual touches are marked failed, rows of past
                                campaigns are marked cancelled. The import
                                filter and tg_send no longer see them, the
                                history stays. Only usernames from the owner
                                list are accepted.
  account-limit <label> <N> apply   daily cap of an account. The cap lives in
                                the database: ACCOUNT_DAILY_LIMIT sets it only
                                when the row is first created, and a restart
                                with another value does not change it.
USAGE
}

is_owner() {
    local nick
    for nick in "${owner_accounts[@]}"; do
        [[ "$nick" == "$1" ]] && return 0
    done
    return 1
}

owner_list() {
    local nick out=""
    for nick in "${owner_accounts[@]}"; do out+="'$nick',"; done
    echo "${out%,}"
}

case "$action" in
    registry-owner)
        "${psql[@]}" \
            -c "SELECT id, username, state, cold, reason, created_at AT TIME ZONE 'Europe/Moscow' AS created_msk FROM direct_messages WHERE lower(username) IN ($(owner_list)) ORDER BY id;" \
            -c "SELECT r.campaign_id, c.title, r.username, r.state, r.reason FROM recipients r JOIN campaigns c ON c.id = r.campaign_id WHERE lower(r.username) IN ($(owner_list)) ORDER BY r.campaign_id;"
        ;;
    registry-forget)
        arg="${arg#@}"
        arg="$(printf '%s' "$arg" | tr '[:upper:]' '[:lower:]')"
        if [[ -z "$arg" || "$confirm" != "apply" ]]; then
            echo "registry-forget requires: <username> apply" >&2
            exit 2
        fi
        if ! is_owner "$arg"; then
            echo "refusing: @$arg is not an owner account (${owner_accounts[*]})" >&2
            exit 2
        fi
        # Rows are not deleted but marked: the filter and daily-count indexes
        # ignore failed rows, while the touch history stays for analysis. A
        # campaign row is closed as cancelled - the import filter treats a
        # person as having received a message in both sent and replied.
        "${psql[@]}" \
            -c "UPDATE direct_messages SET state = 'failed', reason = 'forgotten: owner account check' WHERE lower(username) = '$arg' AND state <> 'failed';" \
            -c "UPDATE recipients SET state = 'cancelled', reason = 'forgotten: owner account check' WHERE lower(username) = '$arg' AND state IN ('sent', 'replied');"
        ;;
    account-limit)
        limit="${3:-}"
        if [[ -z "$arg" || -z "$limit" || "${4:-}" != "apply" ]]; then
            echo "account-limit requires: <label> <N> apply" >&2
            exit 2
        fi
        if ! [[ "$limit" =~ ^[0-9]+$ ]] || (( limit < 1 || limit > 40 )); then
            echo "refusing: cap outside the sane range 1..40: $limit" >&2
            exit 2
        fi
        "${psql[@]}" \
            -c "UPDATE accounts SET daily_limit = $limit WHERE label = '$arg';" \
            -c "SELECT label, daily_limit, stopped FROM accounts;"
        ;;
    ""|-h|--help)
        usage
        ;;
    *)
        echo "unknown action: $action" >&2
        usage >&2
        exit 2
        ;;
esac
