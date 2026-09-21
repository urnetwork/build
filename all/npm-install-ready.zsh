#!/usr/bin/env zsh

# Install a dependency tree containing exact package versions that were just
# proven available by npm-package-ready.zsh. npm's normal cache can retain a
# negative packument response from before publication, and a different registry
# edge can briefly lag the one used by the readiness probe. Use a new empty
# cache for every attempt and retry only ETARGET errors naming one of the exact
# packages supplied by the caller. Authentication, transport, unrelated
# dependency, lifecycle, and all other install failures remain immediately
# fatal.

setopt localoptions nounset

if (( $# < 2 || $# % 2 != 0 )); then
    print -u2 -- "usage: $0 <package-name> <exact-version> [<package-name> <exact-version> ...]"
    exit 64
fi

exact_specs=()
while (( $# > 0 )); do
    package_name="$1"
    package_version="$2"
    shift 2

    if [[ -z "$package_name" || -z "$package_version" ]]; then
        print -u2 -- "npm install readiness: package names and exact versions must be nonempty"
        exit 64
    fi
    exact_specs+=("${package_name}@${package_version}")
done

max_attempts="${NPM_INSTALL_READY_MAX_ATTEMPTS:-6}"
retry_delay_seconds="${NPM_INSTALL_READY_RETRY_DELAY_SECONDS:-10}"
for setting in "$max_attempts" "$retry_delay_seconds"; do
    if [[ "$setting" != <-> ]]; then
        print -u2 -- "npm install readiness: retry settings must be non-negative integers"
        exit 64
    fi
done
if (( max_attempts < 1 )); then
    print -u2 -- "npm install readiness: NPM_INSTALL_READY_MAX_ATTEMPTS must be at least 1"
    exit 64
fi

install_temp=$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-npm-install.XXXXXX") || exit $?
install_stdout="$install_temp/stdout"
install_stderr="$install_temp/stderr"
cleanup_npm_install_temp () {
    rm -rf -- "$install_temp"
}
trap cleanup_npm_install_temp EXIT

npm_install_expected_etarget () {
    local error_text="$1"
    local exact_spec

    [[ "$error_text" == *"code ETARGET"* ]] || return 1
    for exact_spec in "${exact_specs[@]}"; do
        if [[ "$error_text" == *"No matching version found for ${exact_spec}."* ]]; then
            return 0
        fi
    done
    return 1
}

integer attempt=1
while (( attempt <= max_attempts )); do
    : > "$install_stdout"
    : > "$install_stderr"
    attempt_cache="$install_temp/cache-$attempt"

    npm --cache "$attempt_cache" install >"$install_stdout" 2>"$install_stderr"
    install_status=$?
    cat "$install_stdout"
    cat "$install_stderr" >&2

    if (( install_status == 0 )); then
        exit 0
    fi

    install_error=$(<"$install_stderr")
    if ! npm_install_expected_etarget "$install_error"; then
        print -u2 -- "npm install readiness: install failed with status $install_status; not retrying a non-propagation error"
        exit $install_status
    fi

    if (( attempt >= max_attempts )); then
        print -u2 -- "npm install readiness: exact release dependency remained unavailable after $attempt attempts"
        exit $install_status
    fi

    print -u2 -- "npm install readiness: exact release dependency not yet resolvable; retrying in ${retry_delay_seconds}s ($attempt/$max_attempts)"
    sleep "$retry_delay_seconds" || exit $?
    attempt=$(( attempt + 1 ))
done

exit 1
