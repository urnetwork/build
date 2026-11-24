#!/bin/sh
# /usr/local/sbin/bringyour-provider in the bringyour/community-provider image.
#
# The image runs the released provider, the sn miner, kept at
# /usr/local/libexec/bringyour-provider/provider, with the container's
# arguments. The images up to 2026.7.2 ran the old connect provider from this
# path. This wrapper keeps their compose files and scripts working where the
# miner's stricter state handling (miner/PROVIDER-REGISTRATION.md) would stop
# them:
#
# 1. `provide`, `auth-provide` and `start` get --allow-client-registration,
#    unless the arguments already choose: --allow-client-registration,
#    --adopt-legacy-provider-key, an original-capture option, or help. The
#    miner registers a new device only with it, and without it refuses a new
#    state directory and the one the old provider left (a bare .provider.key
#    and no client JWT). The old provider registered a new device at every
#    start. The flag never replaces an identity the state directory keeps, so
#    a restart keeps its device.
#
# 2. The miner keeps its identity in the state directory only when the
#    directory is its own: owned by this user, not writable by group or
#    others, with private files, and used by one miner at a time. Setups the
#    old provider ran in break those rules: a bind mount owned by the host
#    user, a directory made writable by everyone, one directory shared by
#    several containers, a read-only mount. For those, the miner runs on a
#    private state directory inside the container, seeded with the
#    directory's jwt and proxy settings, the way the old provider ran: the
#    device lasts as long as the container, and a new container registers a
#    new one. `auth` writes its jwt into such a directory through a private
#    one. The miner is told which directory it stands in for and why
#    (BRINGYOUR_PROVIDER_STATE_DIR and BRINGYOUR_PROVIDER_STATE_PROBLEM), so
#    its log, provider status and provider doctor name that directory and the
#    volume that fixes it (sn miner/state_persistence.go).
#
# Every other command runs unchanged.

provider="${BRINGYOUR_PROVIDER_BIN:-/usr/local/libexec/bringyour-provider/provider}"
private_root="${BRINGYOUR_PROVIDER_PRIVATE:-/var/lib/bringyour-provider}"
private="$private_root/state"
state="${URNETWORK_STATE_DIR:-${HOME:-/root}/.urnetwork}"
uid=$(id -u)

note() {
    printf 'bringyour-provider: %s\n' "$*" >&2
}

# owned_without PATH MASK: PATH (not followed) is owned by this user and has
# none of the octal MASK's permission bits.
owned_without() {
    meta=$(stat -c '%u %a' -- "$1" 2>/dev/null) || return 1
    [ "${meta%% *}" = "$uid" ] && [ $(( 0${meta#* } & $2 )) -eq 0 ]
}

# Prints why the miner can't keep its identity in directory $1, or nothing.
custody_problem() {
    dir=$1
    if [ -L "$dir" ]; then
        echo "it is a symbolic link"
        return
    fi
    # the miner creates a missing directory as its own
    [ -e "$dir" ] || return
    if [ ! -d "$dir" ]; then
        echo "it is not a directory"
        return
    fi
    if ! owned_without "$dir" 022; then
        # a bind mount of the host user's folder is the common case
        meta=$(stat -c '%u %a' -- "$dir" 2>/dev/null)
        if [ -n "$meta" ] && [ "${meta%% *}" != "$uid" ]; then
            echo "it is owned by uid ${meta%% *}, and this container runs as uid $uid"
        else
            echo "it is writable by group or others (mode ${meta#* })"
        fi
        return
    fi
    if [ ! -w "$dir" ]; then
        echo "it is read-only"
        return
    fi
    for name in jwt .provider.key; do
        if [ -e "$dir/$name" ] || [ -L "$dir/$name" ]; then
            if [ -L "$dir/$name" ] || [ ! -f "$dir/$name" ] || ! owned_without "$dir/$name" 077; then
                echo "its $name is not a private file of this user"
                return
            fi
        fi
    done
    # The miner adopts a bare old key, but not one beside other provider
    # files, which may be another identity's.
    if [ -f "$dir/.provider.key" ] && [ ! -e "$dir/.provider.key.identity" ]; then
        for path in "$dir"/.provider*; do
            case "${path##*/}" in
            .provider.key|.provider.key.registration.lock) ;;
            *)
                if [ -e "$path" ]; then
                    echo "it holds an old provider key beside other provider files"
                    return
                fi
                ;;
            esac
        done
    fi
}

# Holds a lock on directory $1 on descriptor 9, which the miner inherits, so
# a second container that shares the directory runs on its own private state.
lock_state() {
    # (exec's own redirections would stay; the group's are undone)
    { command exec 9<"$1"; } 2>/dev/null || return 1
    flock -n 9
}

# Tells the miner which state directory it stands in for and why ($1).
export_substitution() {
    BRINGYOUR_PROVIDER_STATE_DIR="$state"
    BRINGYOUR_PROVIDER_STATE_PROBLEM="$1"
    export BRINGYOUR_PROVIDER_STATE_DIR BRINGYOUR_PROVIDER_STATE_PROBLEM
}

# Runs the miner on the private state directory, seeded from $state.
use_private() {
    note "the state directory $state can't keep this container's device ($1)," \
        "so the miner runs on a private state directory inside the container, seeded with its jwt and proxy settings." \
        "The device lasts only as long as this container: a new container registers a new device." \
        "Fix: mount a named volume at $state, one for each node (docker volume create urnetwork-provider," \
        "then docker run -v urnetwork-provider:$state ...), or on the host make the directory mounted there" \
        "owned by uid $uid and writable only by its owner (sudo chown -R $uid:$uid <dir>; sudo chmod 700 <dir>)."
    mkdir -p "$private_root" && mkdir -p -m 0700 "$private" && chmod 0700 "$private" || return 1
    if [ ! -e "$private/jwt" ] && [ -r "$state/jwt" ]; then
        install -m 0600 "$state/jwt" "$private/jwt" || return 1
    fi
    if [ -r "$state/proxy" ]; then
        install -m 0600 "$state/proxy" "$private/proxy" || return 1
    else
        rm -f "$private/proxy"
    fi
    # the reason, then the directory, for status and doctor
    printf '%s\n%s\n' "$1" "$state" > "$private_root/private-state" || return 1
    URNETWORK_STATE_DIR="$private"
    export URNETWORK_STATE_DIR
    export_substitution "$1"
}

# Runs `auth` on a private directory seeded with $state's jwt, then puts the
# jwt it writes into $state.
auth_through_private() {
    note "the state directory $state can't hold the miner's sign-in directly ($1); writing its jwt through a private directory."
    shift
    work=$(mktemp -d "${TMPDIR:-/tmp}/bringyour-provider-auth.XXXXXX") || exit 1
    if [ -r "$state/jwt" ]; then
        install -m 0600 "$state/jwt" "$work/jwt" || exit 1
    fi
    URNETWORK_STATE_DIR="$work" "$provider" "$@"
    result=$?
    if [ "$result" -eq 0 ] && [ -f "$work/jwt" ] && ! cmp -s "$work/jwt" "$state/jwt"; then
        if install -m 0600 "$work/jwt" "$state/.jwt.$$" && mv -f "$state/.jwt.$$" "$state/jwt"; then
            note "wrote $state/jwt"
        else
            rm -f "$state/.jwt.$$"
            note "could not write $state/jwt"
            result=1
        fi
    fi
    rm -rf -- "$work"
    exit "$result"
}

for arg in "$@"; do
    case "$arg" in
    -h|--help)
        exec "$provider" "$@"
        ;;
    esac
done

case "${1:-}" in
provide|auth-provide|start)
    register=yes
    for arg in "$@"; do
        case "$arg" in
        --allow-client-registration|--adopt-legacy-provider-key|\
        --whole-work-capture|--whole-work-capture=*|\
        --original-contract-capture|--original-contract-capture=*|\
        --require-whole-work-capture|--require-original-contract-capture|\
        --quarantine-rejected-sign-in)
            register=no
            ;;
        esac
    done
    problem=$(custody_problem "$state")
    if [ -z "$problem" ] && [ -d "$state" ] && ! lock_state "$state"; then
        problem="another provider uses it, or it can't be locked"
    fi
    if [ -z "$problem" ]; then
        rm -f "$private_root/private-state"
    elif ! use_private "$problem"; then
        note "could not set up the private state directory $private; running on $state"
    fi
    if [ "$register" = yes ]; then
        command=$1
        shift
        exec "$provider" "$command" --allow-client-registration "$@"
    fi
    exec "$provider" "$@"
    ;;
auth|login)
    for arg in "$@"; do
        case "$arg" in
        --operator|--operator=*)
            exec "$provider" "$@"
            ;;
        esac
    done
    problem=$(custody_problem "$state")
    if [ -n "$problem" ] && [ -d "$state" ]; then
        auth_through_private "$problem" "$@"
    fi
    exec "$provider" "$@"
    ;;
status|doctor)
    # in a container that runs on the private state directory, with the
    # reason its provide was given
    if [ -f "$private_root/private-state" ] && [ -z "${URNETWORK_STATE_DIR:-}" ]; then
        URNETWORK_STATE_DIR="$private"
        export URNETWORK_STATE_DIR
        given=$(sed -n 2p "$private_root/private-state")
        [ -z "$given" ] || state=$given
        export_substitution "$(sed -n 1p "$private_root/private-state")"
    fi
    exec "$provider" "$@"
    ;;
esac

exec "$provider" "$@"
