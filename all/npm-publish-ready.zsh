#!/usr/bin/env zsh

# Publish one exact npm package version and prove that a fresh registry client
# can retrieve its metadata and tarball. npm now acknowledges publication with
# HTTP 202 while processing continues asynchronously. In rare cases an
# accepted job never becomes visible; resubmit it once after the full bounded
# readiness window rather than treating the acknowledgement as completion.
#
# Hard publish/readiness failures are never retried. If a resubmission races
# the original job and npm rejects the duplicate, the readiness check still
# gets one full window in which to prove that the original became available.

setopt localoptions nounset

if (( $# < 3 )); then
    print -u2 -- "usage: $0 <package-name> <exact-version> <publish-command> [args...]"
    exit 64
fi

package_name="$1"
package_version="$2"
shift 2
publish_command=("$@")
exact_spec="${package_name}@${package_version}"
max_publish_attempts="${NPM_PUBLISH_READY_MAX_ATTEMPTS:-2}"
readiness_helper="${0:A:h}/npm-package-ready.zsh"

if [[ -z "$package_name" || -z "$package_version" ]]; then
    print -u2 -- "npm publish readiness: package name and exact version must be nonempty"
    exit 64
fi
if [[ "$max_publish_attempts" != <-> ]] || (( max_publish_attempts < 1 )); then
    print -u2 -- "npm publish readiness for $exact_spec: NPM_PUBLISH_READY_MAX_ATTEMPTS must be a positive integer"
    exit 64
fi
if [[ ! -x "$readiness_helper" ]]; then
    print -u2 -- "npm publish readiness for $exact_spec: missing executable readiness helper: $readiness_helper"
    exit 66
fi

integer publish_attempt=1
while (( publish_attempt <= max_publish_attempts )); do
    "${publish_command[@]}"
    publish_status=$?

    if (( publish_status != 0 && publish_attempt == 1 )); then
        print -u2 -- "npm publish readiness for $exact_spec: initial publish failed with status $publish_status; not retrying"
        exit $publish_status
    fi
    if (( publish_status != 0 )); then
        print -u2 -- "npm publish readiness for $exact_spec: resubmission failed with status $publish_status; checking whether the accepted publish completed"
    fi

    "$readiness_helper" "$package_name" "$package_version"
    readiness_status=$?
    if (( readiness_status == 0 )); then
        exit 0
    fi
    if (( readiness_status != 75 )); then
        print -u2 -- "npm publish readiness for $exact_spec: readiness failed with status $readiness_status; not resubmitting"
        exit $readiness_status
    fi
    if (( publish_status != 0 )); then
        print -u2 -- "npm publish readiness for $exact_spec: accepted publish remained unavailable after failed resubmission"
        exit $publish_status
    fi
    if (( publish_attempt >= max_publish_attempts )); then
        print -u2 -- "npm publish readiness for $exact_spec: unavailable after $publish_attempt accepted publish attempts"
        exit 75
    fi

    print -u2 -- "npm publish readiness for $exact_spec: accepted publish did not complete; resubmitting ($(( publish_attempt + 1 ))/$max_publish_attempts)"
    publish_attempt=$(( publish_attempt + 1 ))
done

exit 75
