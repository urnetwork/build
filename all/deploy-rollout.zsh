#!/usr/bin/env zsh

# Deploy one rollout step and preserve Warpctl's status before reporting success.
# Config-updater, lb, transparent proxy, statusless Gossip, and unexposed Alt
# blocks cannot observe their live versions through per-block LB status routes,
# so Warpctl deliberately rejects --only-older for those services.
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
        config-updater|lb|proxy|gossip|alt)
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
    local -a single_pass_services
    local percent
    local service

    staged_services=(lb taskworker api connect web app mcp)
    # Gossip has no status route, while Alt and Proxy intentionally bypass the
    # load balancer. Without --only-older, a cumulative rollout would retag and
    # restart already selected unobservable blocks in every later wave.
    single_pass_services=(gossip alt proxy)

    warp_rollout_sample 0 || return $?

    # Request the complete config rollout before any service-image rollout.
    # run.sh builds this config-updater with --config_restart=no, so landing
    # the config restarts nothing: each block takes it with its own wave
    # below (see build_config_updater in run.sh).
    warp_rollout_deploy config-updater 100 || return $?
    warp_rollout_deploy grafana 100 || return $?

    for percent in 25 50 75 100; do
        for service in "${staged_services[@]}"; do
            warp_rollout_deploy "$service" "$percent" || return $?
        done

        if (( percent == 100 )); then
            for service in "${single_pass_services[@]}"; do
                warp_rollout_deploy "$service" 100 || return $?
            done
        fi

        warp_rollout_sample "$percent" || return $?
        if (( percent < 100 )); then
            sleep "$STAGE_SECONDS" || return $?
        fi
    done
}
