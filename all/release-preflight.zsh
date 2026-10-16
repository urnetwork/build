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
    # One deadline bounds the entire read-only pass. GNU timeout owns only this
    # probe subprocess group; its expiry cannot terminate the release parent.
    timeout --kill-after=10 300 zsh -c '
        timeout 100 env BRINGYOURCTL_BINARY="$2" "$1/run-main-lan.sh" release-preflight || exit $?
        source "$BUILD_HOME/all/deploy-rollout.zsh" || exit $?
        builder_message() { print -r -- "$1"; }
        typeset service
        for service in config-updater grafana mmm lb taskworker api connect web app mcp gossip alt proxy; do
            # Each invocation is bounded independently; a stalled status or
            # registry read cannot consume hours before any artifact build.
            warp_rollout_deploy "$service" 100 preflight "$3" || exit $?
        done
    ' release-preflight "$server_dir" "$binary" "$phase"
}
