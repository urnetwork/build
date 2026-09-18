#!/usr/bin/env zsh

# Wait until one exact npm package version is usable by a fresh registry
# client. `npm publish` can exit while npmjs is still processing the version;
# metadata visibility alone is insufficient because the tarball can propagate
# separately. A dry-run pack with a new empty cache for every attempt fetches
# and inspects both without retaining an earlier ETARGET/E404 response.
# Only an exact-version ETARGET or an exact-version/tarball E404 is considered
# an expected propagation miss. Authentication, transport, server, and all
# other failures are fatal immediately. Exhausting the bounded propagation
# window exits with EX_TEMPFAIL (75), so a publisher can distinguish a dropped
# asynchronous publish job from a hard registry failure.

setopt localoptions nounset

if (( $# != 2 )); then
    print -u2 -- "usage: $0 <package-name> <exact-version>"
    exit 64
fi

package_name="$1"
package_version="$2"
exact_spec="${package_name}@${package_version}"
max_attempts="${NPM_PACKAGE_READY_MAX_ATTEMPTS:-60}"
retry_delay_seconds="${NPM_PACKAGE_READY_RETRY_DELAY_SECONDS:-10}"

if [[ -z "$package_name" || -z "$package_version" ]]; then
    print -u2 -- "npm package readiness: package name and exact version must be nonempty"
    exit 64
fi
for setting in "$max_attempts" "$retry_delay_seconds"; do
    if [[ "$setting" != <-> ]]; then
        print -u2 -- "npm package readiness for $exact_spec: retry settings must be non-negative integers"
        exit 64
    fi
done
if (( max_attempts < 1 )); then
    print -u2 -- "npm package readiness for $exact_spec: NPM_PACKAGE_READY_MAX_ATTEMPTS must be at least 1"
    exit 64
fi

probe_temp=$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-npm-ready.XXXXXX") || exit $?
probe_stdout="$probe_temp/stdout"
probe_stderr="$probe_temp/stderr"
cleanup_npm_probe_temp () {
    rm -rf -- "$probe_temp"
}
trap cleanup_npm_probe_temp EXIT

npm_expected_propagation_miss () {
    local error_text="$1"

    # This is the form npm emits while the newly published version is absent
    # from package metadata (including the production failure this guards).
    if [[ "$error_text" == *"code ETARGET"* && "$error_text" == *"$exact_spec"* ]]; then
        return 0
    fi

    # Once metadata is present, the package tarball can still return 404. Keep
    # this narrow: require E404 plus this exact version and either the exact
    # spec or npm's tarball URL shape. A generic E404 is not retried.
    if [[ "$error_text" == *"code E404"* && "$error_text" == *"$package_version"* ]]; then
        if [[ "$error_text" == *"$exact_spec"* ]] || \
            { [[ "$error_text" == *"/-/"* ]] && [[ "$error_text" == *".tgz"* ]]; }; then
            return 0
        fi
    fi

    return 1
}

integer attempt=1
while (( attempt <= max_attempts )); do
    : > "$probe_stdout"
    : > "$probe_stderr"
    attempt_cache="$probe_temp/cache-$attempt"

    # A distinct empty cache on every attempt prevents npm from replaying a
    # negative packument response after the version has reached the registry.
    # It also prevents a tarball left by publication or another build from
    # proving readiness. npm pack must retrieve the exact version's registry
    # metadata and tarball, while --dry-run leaves no package behind.
    npm --cache "$attempt_cache" pack "$exact_spec" \
        --dry-run --json --ignore-scripts \
        >"$probe_stdout" 2>"$probe_stderr"
    probe_status=$?

    if (( probe_status == 0 )); then
        if ! jq -e --arg package "$package_name" --arg version "$package_version" '
            type == "array" and length == 1 and
            .[0].name == $package and .[0].version == $version and
            (.[0].filename | type == "string" and length > 0)
        ' "$probe_stdout" >/dev/null 2>&1; then
            print -u2 -- "npm package readiness for $exact_spec: npm pack returned success without the requested package and version"
            exit 65
        fi
        print -- "npm package available: $exact_spec (metadata and tarball verified)"
        exit 0
    fi

    probe_error=$(<"$probe_stderr")
    if ! npm_expected_propagation_miss "$probe_error"; then
        print -u2 -- "npm package readiness for $exact_spec: npm pack failed with status $probe_status; not retrying a non-propagation error"
        cat "$probe_stderr" >&2
        exit $probe_status
    fi

    if (( attempt >= max_attempts )); then
        print -u2 -- "npm package readiness for $exact_spec: still unavailable after $attempt attempts"
        cat "$probe_stderr" >&2
        exit 75
    fi

    print -u2 -- "npm package readiness for $exact_spec: registry propagation incomplete; retrying in ${retry_delay_seconds}s ($attempt/$max_attempts)"
    sleep "$retry_delay_seconds" || exit $?
    attempt=$(( attempt + 1 ))
done

exit 1
