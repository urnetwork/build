#!/usr/bin/env zsh

# Purge the Cloudflare edge cache for the static sites after a rollout.
#
# Cloudflare edge-caches HTML on ur.io and ur.xyz for 5 minutes (cache rule
# "Edge-cache HTML"); browsers revalidate every visit (Browser Cache TTL:
# respect existing headers). Without a purge, visitors can see the previous
# release's pages for up to 5 minutes after the new blocks serve.
#
# The rollout promotes with --timeout 0, so it returns before blocks converge
# (an mmm deploy takes about 15 minutes). Purging at that moment would just
# refill the edge with the old pages, so this waits CACHE_PURGE_DELAY_SECONDS
# first. A purge failure is reported and never fails the release: the cache
# expires on its own within 5 minutes.
#
# The token is a Cloudflare API token with only Zone → Cache Purge → Purge on
# these two zones, kept in vault. A purge-only token cannot look zones up by
# name, so the zone ids are fixed here.

CACHE_PURGE_TOKEN_FILE="${CACHE_PURGE_TOKEN_FILE:-$WARP_HOME/vault/cf-cache-purge}"
CACHE_PURGE_DELAY_SECONDS="${CACHE_PURGE_DELAY_SECONDS:-1200}"
typeset -gA CACHE_PURGE_ZONES
CACHE_PURGE_ZONES=(
    ur.io 9ee8c03ca7df0dbe6ff8eef56a2fb5cf
    ur.xyz e5fb0c3111fe44c9249c478dd40e10d3
)

# Purge one zone; prints nothing secret. Returns non-zero on any failure.
cf_cache_purge_zone() {
    local zone_id="$1"
    local token="$2"
    local response

    # The token goes to curl on stdin (a config line), never on the command
    # line, so it does not appear in the process list or the build log.
    response="$(printf 'header = "Authorization: Bearer %s"\n' "$token" | curl -sS --max-time 30 \
        --config - \
        -X POST "https://api.cloudflare.com/client/v4/zones/${zone_id}/purge_cache" \
        -H 'Content-Type: application/json' \
        --data '{"purge_everything":true}')" || return $?
    [[ "$response" == *'"success":true'* ]]
}

# Wait for convergence, then purge every zone. Always returns 0.
cf_cache_purge() {
    local token
    local zone
    local -a purged failed

    if [[ ! -r "$CACHE_PURGE_TOKEN_FILE" ]]; then
        builder_message "${BUILD_ENV} cache purge skipped: no token at vault/cf-cache-purge"
        return 0
    fi
    token="$(tr -d '[:space:]' < "$CACHE_PURGE_TOKEN_FILE")"
    if [[ -z "$token" ]]; then
        builder_message "${BUILD_ENV} cache purge skipped: vault/cf-cache-purge is empty"
        return 0
    fi

    sleep "$CACHE_PURGE_DELAY_SECONDS"

    for zone in ${(ko)CACHE_PURGE_ZONES}; do
        if cf_cache_purge_zone "${CACHE_PURGE_ZONES[$zone]}" "$token"; then
            purged+=("$zone")
        else
            failed+=("$zone")
        fi
    done

    if (( ${#failed} )); then
        builder_message "${BUILD_ENV} cache purge failed for ${(j:, :)failed} (purged: ${(j:, :)purged:-none}); pages refresh on their own within 5 minutes"
    else
        builder_message "${BUILD_ENV} cache purged: ${(j:, :)purged}"
    fi
    return 0
}
