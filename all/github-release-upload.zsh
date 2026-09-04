#!/usr/bin/env zsh

# Upload one asset to an already-created GitHub release. HTTP failures are
# inspected without printing request headers, so the bearer token can never
# enter the build log. Only explicit primary/secondary rate-limit responses are
# retried: GitHub documents those responses as definite rejections, while a
# transport failure or 5xx can be ambiguous after a request body was sent.

setopt localoptions nounset

if (( $# != 3 )); then
    print -u2 -- "usage: $0 <release-upload-url> <asset-name> <asset-path>"
    exit 64
fi

upload_url="$1"
asset_name="$2"
asset_path="$3"
max_attempts="${GITHUB_UPLOAD_MAX_ATTEMPTS:-4}"
max_wait_seconds="${GITHUB_UPLOAD_MAX_WAIT_SECONDS:-3700}"
fallback_delay_seconds="${GITHUB_UPLOAD_RETRY_DELAY_SECONDS:-60}"

for setting in "$max_attempts" "$max_wait_seconds" "$fallback_delay_seconds"; do
    if [[ "$setting" != <-> ]]; then
        print -u2 -- "github release upload $asset_name: retry settings must be non-negative integers"
        exit 64
    fi
done
if (( max_attempts < 1 )); then
    print -u2 -- "github release upload $asset_name: GITHUB_UPLOAD_MAX_ATTEMPTS must be at least 1"
    exit 64
fi
if [[ ! -f "$asset_path" ]]; then
    print -u2 -- "github release upload $asset_name: asset is not a regular file"
    exit 66
fi

upload_temp=$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-github-upload.XXXXXX") || exit $?
response_body="$upload_temp/body"
response_headers="$upload_temp/headers"
cleanup_upload_temp () {
    rm -f -- "$response_body" "$response_headers"
    rmdir -- "$upload_temp" 2>/dev/null
}
trap cleanup_upload_temp EXIT

github_upload_header () {
    awk -v wanted="$1" '
        BEGIN { wanted = tolower(wanted) ":" }
        {
            sub(/\r$/, "")
            if (index(tolower($0), wanted) == 1) {
                value = $0
                sub(/^[^:]*:[[:space:]]*/, "", value)
            }
        }
        END { if (value != "") print value }
    ' "$response_headers"
}

github_upload_message () {
    local message
    message=$(jq -r 'if type == "object" and (.message | type == "string") then .message else empty end' "$response_body" 2>/dev/null)
    if [[ -z "$message" ]]; then
        message="response did not include a JSON message"
    fi
    message="${message//$'\r'/ }"
    message="${message//$'\n'/ }"
    if (( ${#message} > 300 )); then
        message="${message[1,300]}..."
    fi
    print -r -- "$message"
}

integer attempt=1
integer total_wait=0
while (( attempt <= max_attempts )); do
    : > "$response_body"
    : > "$response_headers"
    http_status=$(curl -s -S -L \
        --output "$response_body" \
        --dump-header "$response_headers" \
        --write-out '%{http_code}' \
        -X POST \
        -H 'Accept: application/vnd.github+json' \
        -H 'X-GitHub-Api-Version: 2022-11-28' \
        -H 'Content-Type: application/octet-stream' \
        -H "Authorization: Bearer ${GITHUB_API_KEY:-}" \
        "$upload_url?name=$asset_name" \
        --data-binary "@$asset_path")
    curl_status=$?
    if (( curl_status != 0 )); then
        print -u2 -- "github release upload $asset_name: curl transport error $curl_status; not retrying an ambiguous POST"
        exit $curl_status
    fi
    if [[ "$http_status" != <-> ]] || (( ${#http_status} != 3 )); then
        print -u2 -- "github release upload $asset_name: curl returned invalid HTTP status '$http_status'"
        exit 22
    fi

    integer http_code=$http_status
    if (( http_code >= 200 && http_code < 300 )); then
        cat "$response_body"
        exit 0
    fi

    message=$(github_upload_message)
    retry_after=$(github_upload_header retry-after)
    rate_remaining=$(github_upload_header x-ratelimit-remaining)
    rate_reset=$(github_upload_header x-ratelimit-reset)
    rate_resource=$(github_upload_header x-ratelimit-resource)
    request_id=$(github_upload_header x-github-request-id)
    message_lower="${message:l}"
    rate_limited=0
    if (( http_code == 429 )); then
        rate_limited=1
    elif (( http_code == 403 )) && \
        { [[ "$rate_remaining" == 0 ]] || [[ "$retry_after" == <-> ]] || \
          [[ "$message_lower" == *"rate limit"* ]] || [[ "$message_lower" == *"temporarily blocked"* ]]; }; then
        rate_limited=1
    fi

    detail="HTTP $http_code: $message; request-id=${request_id:-unknown}; rate-resource=${rate_resource:-unknown}; rate-remaining=${rate_remaining:-unknown}; rate-reset=${rate_reset:-unknown}; retry-after=${retry_after:-unknown}"
    if (( ! rate_limited )); then
        print -u2 -- "github release upload $asset_name: $detail; not retrying a non-rate-limit response"
        exit 22
    fi
    if (( attempt >= max_attempts )); then
        print -u2 -- "github release upload $asset_name: $detail; retry limit reached ($attempt/$max_attempts)"
        exit 22
    fi

    integer delay_seconds
    if [[ "$retry_after" == <-> ]]; then
        delay_seconds=$retry_after
    elif [[ "$rate_remaining" == 0 && "$rate_reset" == <-> ]]; then
        now_epoch=$(date +%s)
        delay_seconds=$(( rate_reset - now_epoch + 1 ))
        if (( delay_seconds < 1 )); then
            delay_seconds=1
        fi
    else
        delay_seconds=$(( fallback_delay_seconds * (1 << (attempt - 1)) ))
    fi
    if (( total_wait + delay_seconds > max_wait_seconds )); then
        print -u2 -- "github release upload $asset_name: $detail; required wait ${delay_seconds}s exceeds remaining retry budget $(( max_wait_seconds - total_wait ))s"
        exit 22
    fi

    print -u2 -- "github release upload $asset_name: $detail; retrying in ${delay_seconds}s ($attempt/$max_attempts)"
    sleep "$delay_seconds" || exit $?
    total_wait=$(( total_wait + delay_seconds ))
    attempt=$(( attempt + 1 ))
done

exit 22
