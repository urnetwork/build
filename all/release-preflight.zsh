# Dynamic checks are repeated early, on resume, and immediately before every
# production mutation group. They do not reserve health for a later hour.
release_early_preflight() {
    [[ -z "${WARP_SKIP_DEPLOY:-}" ]] || return 0
    local server_dir="$1" phase="$2" binary_dir
    binary_dir=$(mktemp -d "$WARP_HOME/.release-checkpoints/.preflight.XXXXXXXX") || return $?
    (cd "$server_dir" && timeout --kill-after=10 300 go build -o "$binary_dir/bringyourctl" .) || return $?
    release_preflight "$server_dir" "$binary_dir/bringyourctl" "$phase"
    local result=$?
    rm -f "$binary_dir/bringyourctl"
    rmdir "$binary_dir"
    return "$result"
}

release_preflight() {
    [[ -z "${WARP_SKIP_DEPLOY:-}" ]] || return 0
    local server_dir="$1" binary="$2" phase="$3"
    local WARP_VERSION="${WARP_VERSION:-9999.0.0+0}"
    export WARP_VERSION
    builder_message "read-only deployment preflight ($phase)"
    # One unchanged deadline includes the native gate and the service pool.
    # The coordinator owns/cancels each probe group; timeout is its backstop,
    # not permission to leave separately grouped descendants running.
    timeout --kill-after=10 300 python3 -B "$BUILD_HOME/all/release-preflight.py" \
        "$server_dir" "$binary" "$phase"
}
