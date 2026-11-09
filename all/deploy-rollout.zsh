#!/usr/bin/env zsh

# Promote one rollout step without waiting for version convergence, and preserve
# Warpctl's status before reporting success.
# Config-updater, lb, transparent proxy, statusless Gossip, and unexposed Alt
# blocks cannot observe their live versions through per-block LB status routes,
# so Warpctl deliberately rejects --only-older for those services. smtpedge is
# classed with them, to deploy in one pass.
#
# Sidecar's services are classed here ahead of their images, as §19.11 of its
# design has them, with mxedge-platform, mxedge's platform listener, staged beside
# it (QD-243). dnsedge runs as two services, dnsedge-1 and dnsedge-2, one host
# each (warp-w5w6-dnsedge-one-host), so a release never takes both NS at once:
# each deploys in one pass, and dnsedge-2 only once dns_rollout_stop_check says the
# first serves clean (Q-OPS-5). The bare role name dnsedge stays unknown.

# Q-OPS-5's stop rule before the second NS: a clean canary signature and equal NS
# serials. DNS_ROLLOUT_CHECK names a command that exits 0 only when both hold; with
# none set nothing can read them, which stops the step, as a burn state the monitor
# cannot read does (plan-OPS-small-readings #11). An operator who has checked by
# hand overrides by naming the service in ROLLOUT_STOP_OVERRIDE.
dns_rollout_stop_check() {
    local service="$1"
    if [[ " ${ROLLOUT_STOP_OVERRIDE:-} " == *" $service "* ]]; then
        printf '%s: the stop rule is overridden by name\n' "$service" >&2
        return 0
    fi
    if [[ -z "${DNS_ROLLOUT_CHECK:-}" ]]; then
        printf '%s waits: no DNS_ROLLOUT_CHECK reads the canary signature and NS serials\n' "$service" >&2
        return 1
    fi
    ${(z)DNS_ROLLOUT_CHECK}
}
# The sidecar's services, whose SLO burn gates every rollout wave they are in
# (D-OPS-9, §19.8; OPS-M3-02).
sidecar_burn_gated_services=(evs gkm sockets dns tlsedge mxedge mxedge-platform smtpedge)

# D-OPS-9's stop before a wave: SLO_BURN_CHECK names a command that takes
# --service=<svc> and exits 0 only while the service's SLOs read clear, which is
# server/cli/monitor's `monitor check-burn` with the monitor's settings. With none
# set nothing reads the burn, which stops the step, as a burn the monitor cannot
# read does (plan-OPS-small-readings #11); so does a service whose SLOs cannot be
# read yet, such as one under 30 days old. An operator who has checked by hand
# overrides by naming the service in ROLLOUT_STOP_OVERRIDE, and the override is
# posted with the rollout's messages.
warp_rollout_gate() {
    local service="$1"
    if [[ " ${ROLLOUT_STOP_OVERRIDE:-} " == *" $service "* ]]; then
        printf '%s: the SLO burn stop is overridden by name\n' "$service" >&2
        builder_message "${BUILD_ENV} ${service}: the SLO burn stop is overridden by name"
        return 0
    fi
    if [[ -z "${SLO_BURN_CHECK:-}" ]]; then
        printf '%s waits: no SLO_BURN_CHECK reads its SLO burn\n' "$service" >&2
        return 1
    fi
    ${(z)SLO_BURN_CHECK} --service="$service"
}

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
        --timeout 0
    )
    case "$service" in
        config-updater|lb|proxy|gossip|alt|smtpedge|dnsedge-1)
            ;;
        dnsedge-2)
            # a preflight checks the class alone; the stop rule reads the first NS at deploy time
            if [[ "${3:-}" != preflight ]] && ! dns_rollout_stop_check "$service"; then
                return 65
            fi
            ;;
        mmm)
            # New MMM blocks have no selector or old-LB status route. The
            # opt-in initializes only absent records, then recognizes this
            # release's completed initialization in later cumulative waves.
            deploy_args+=(--only-older --initialize-unset)
            success_suffix=" (only older; initialize unset)"
            ;;
        grafana|taskworker|api|connect|web|app|mcp|evs|gkm|sockets|dns|tlsedge|mxedge|mxedge-platform)
            deploy_args+=(--only-older)
            success_suffix=" (only older)"
            ;;
        *)
            printf 'Unknown rollout service classification: %s\n' "$service" >&2
            return 64
            ;;
    esac

    # Preflight uses precisely the same classification and guard arguments as
    # promotion. Warpctl exits before all external writes/announcements.
    if [[ "${3:-}" = preflight ]]; then
        deploy_args+=(--preflight)
        if [[ "${4:-}" = future ]]; then deploy_args+=(--allow-missing-image); fi
    fi

    warpctl "${deploy_args[@]}"
    code=$?
    if (( code != 0 )); then
        return "$code"
    fi

    if [[ "${3:-}" = preflight ]]; then return 0; fi

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

    # mmm goes first in every wave. The load balancer routes the mmm sites'
    # hostnames to mmm (vault services.yml v26), so each wave starts mmm's
    # blocks before it brings any lb onto that routing (mmm/mmm/PLAN.md).
    # A service joins a list only with its image in run.sh: without one, every
    # release's rollout would fail at that service. A sidecar service also needs
    # its block, a fragment in warp/services/sidecar-blocks/ that the owner applies
    # to the vault before these branches merge to main, since warpctl deploy stops
    # on a service with no blocks. The sidecar's take §19.11's order, mmm lb
    # taskworker api connect web app mcp dns gkm evs sockets tlsedge mxedge, as each
    # joins: evs now, gkm with server/cli/gkm's image (plan-OPS-small-readings #12).
    staged_services=(mmm lb taskworker api connect web app mcp evs)
    # Gossip has no status route, while Alt and Proxy intentionally bypass the
    # load balancer. Without --only-older, a cumulative rollout would retag and
    # restart already selected unobservable blocks in every later wave.
    single_pass_services=(gossip alt proxy)

    warp_rollout_sample 0 || return $?

    # Request the complete config rollout before any service-image rollout.
    # run.sh builds this config-updater with --config_restart=no, so landing
    # the config restarts nothing: each block takes it with its own wave
    # below, and a block the waves skip (--only-older) takes it at its next
    # deploy (see build_config_updater in run.sh).
    warp_rollout_deploy config-updater 100 || return $?
    warp_rollout_deploy grafana 100 || return $?

    for percent in 25 50 75 100; do
        # D-OPS-9: a sidecar service burning its budget, or whose burn cannot
        # be read, stops this wave before any of it deploys; the waves before
        # it stand
        for service in "${staged_services[@]}"; do
            if (( ${sidecar_burn_gated_services[(Ie)$service]} )); then
                if ! warp_rollout_gate "$service"; then
                    printf 'the %s%% wave stops: %s\n' "$percent" "$service" >&2
                    return 65
                fi
            fi
        done
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
