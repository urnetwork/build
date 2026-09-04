#!/usr/bin/env zsh

# Deploy one rollout step and preserve Warpctl's status before reporting success.
# Config-updater, lb, and transparent proxy blocks cannot observe their live
# versions through per-block LB status routes, so Warpctl deliberately rejects
# --only-older for those services.
warp_rollout_deploy() {
    local service="$1"
    local percent="$2"
    local -a deploy_args
    local success_suffix=""
    local code

    deploy_args=(
        deploy
        "$BUILD_ENV"
        "$service"
        "$WARP_VERSION"
        "--percent=$percent"
    )
    case "$service" in
        config-updater|lb|proxy)
            ;;
        grafana|taskworker|api|connect|web|app|mcp)
            deploy_args+=(--only-older)
            success_suffix=" (only older)"
            ;;
        *)
            printf 'Unknown rollout service classification: %s\n' "$service" >&2
            return 64
            ;;
    esac

    warpctl "${deploy_args[@]}"
    code=$?
    if (( code != 0 )); then
        return "$code"
    fi

    builder_message "${BUILD_ENV}[${percent}%] ${service} \`${EXTERNAL_WARP_VERSION}\` deployed${success_suffix}"
}

# Sample the rollout boundary and do not report it or advance after a failure.
warp_rollout_sample() {
    local percent="$1"
    local sampled_versions
    local code

    sampled_versions="$(warpctl ls versions "$BUILD_ENV" --sample)"
    code=$?
    if (( code != 0 )); then
        return "$code"
    fi

    builder_message "${BUILD_ENV}[${percent}%] services: \`\`\`${sampled_versions}\`\`\`"
}

# Roll out services in cumulative waves, stopping at the first failed mutation,
# status boundary, success message, or stage wait.
warp_rollout() {
    local -a staged_services
    local percent
    local service

    staged_services=(lb taskworker api connect web app mcp proxy)

    warp_rollout_sample 0 || return $?

    # Request the complete config rollout before any service-image rollout.
    warp_rollout_deploy config-updater 100 || return $?
    warp_rollout_deploy grafana 100 || return $?

    for percent in 25 50 75 100; do
        for service in "${staged_services[@]}"; do
            warp_rollout_deploy "$service" "$percent" || return $?
        done

        warp_rollout_sample "$percent" || return $?
        if (( percent < 100 )); then
            sleep "$STAGE_SECONDS" || return $?
        fi
    done
}
