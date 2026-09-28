#!/usr/bin/env bash
# Именованные операции над боевым контуром tg-agent-mcp вместо произвольного
# SQL. Контур стоит на этой машине в Docker (docker-compose.local.yml), ssh
# здесь нет; имя файла держит конвенцию портфеля: safe-ssh.sh - разрешённый
# список действий, читающих по умолчанию и мутирующих только с «apply».
#
# Журнал касаний и строки кампаний правятся ТОЛЬКО для аккаунтов владельца:
# они нужны для периодических проверок рассылки, и рубеж «один человек - одно
# сообщение» для них снимается руками (решение владельца 06.09.2026). Любой
# другой ник скрипт отвергает - это устройство команды, а не договорённость.
#
# Список ниже шире DEV_ALLOW_LIST намеренно. DEV_ALLOW_LIST называет служебных
# адресатов, которым сервис не считает окно и потолок; здесь перечислены все
# аккаунты владельца, следы которых можно забывать. Аккаунт, проверяющий сами
# рубежи, обязан быть вне DEV_ALLOW_LIST и при этом чиститься между прогонами.
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
  registry-owner            касания и строки кампаний по аккаунтам владельца

Mutating actions:
  registry-forget <ник> apply   забыть следы аккаунта владельца: ручные касания
                                помечаются failed, строки прошлых кампаний -
                                cancelled. Отсев импорта и tg_send их больше не
                                видят, история остаётся. Ник только из списка
                                владельца.
  account-limit <метка> <N> apply   дневной потолок аккаунта. Потолок живёт в
                                базе: ACCOUNT_DAILY_LIMIT задаёт его только при
                                первом заведении строки, и перезапуск с другой
                                переменной его не меняет.
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
            echo "registry-forget requires: <ник> apply" >&2
            exit 2
        fi
        if ! is_owner "$arg"; then
            echo "refusing: @$arg is not an owner account (${owner_accounts[*]})" >&2
            exit 2
        fi
        # Строки не удаляются, а помечаются: индексы отсева и суточного счёта
        # failed не видят, а история касаний остаётся для разбора. Строка
        # кампании закрывается cancelled - отсев импорта считает человека
        # получившим письмо и по sent, и по replied.
        "${psql[@]}" \
            -c "UPDATE direct_messages SET state = 'failed', reason = 'forgotten: owner account check' WHERE lower(username) = '$arg' AND state <> 'failed';" \
            -c "UPDATE recipients SET state = 'cancelled', reason = 'forgotten: owner account check' WHERE lower(username) = '$arg' AND state IN ('sent', 'replied');"
        ;;
    account-limit)
        limit="${3:-}"
        if [[ -z "$arg" || -z "$limit" || "${4:-}" != "apply" ]]; then
            echo "account-limit requires: <метка> <N> apply" >&2
            exit 2
        fi
        if ! [[ "$limit" =~ ^[0-9]+$ ]] || (( limit < 1 || limit > 40 )); then
            echo "refusing: потолок вне разумных пределов 1..40: $limit" >&2
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
