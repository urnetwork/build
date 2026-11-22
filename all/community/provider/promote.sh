#!/usr/bin/env bash
# Points the tags operators follow at one published version of the community
# provider image, bringyour/community-provider:
#
#   promote.sh <version>        for example promote.sh 2026.11.19-1103608200
#
# The tags are latest, which the docs and most scripts use, and the warp
# block tags beta-latest and g1-latest to g4-latest that the provider docs
# listed. The images up to 2026.7.2 moved them through the community rollout;
# a release now moves them together, once its provider is GitHub's latest
# release.
#
# A tag never moves to an older version. Each image carries its version in
# the label org.opencontainers.image.version, ending in the release's version
# code, and a tag whose image has a higher version code is left where it is.
# The images up to 2026.7.2 have no such label, so they are always replaced.
# Every tag is then checked to name the version's exact manifest.
set -euo pipefail

image="${COMMUNITY_PROVIDER_IMAGE:-bringyour/community-provider}"
tags=(beta-latest g1-latest g2-latest g3-latest g4-latest latest)

fail() {
    echo "promote: $*" >&2
    exit 1
}

[ $# -eq 1 ] || fail "usage: promote.sh <version>"
version=$1
code=${version##*-}
[[ "$version" == *-* && "$code" =~ ^[0-9]+$ ]] || fail "version $version has no version code"

work=$(mktemp -d "${TMPDIR:-/tmp}/community-provider-promote.XXXXXX")
trap 'rm -rf -- "$work"' EXIT

# inspect REF FORMAT: prints the inspection of REF, exit 2 when REF does not
# exist and 1 on any other error (a registry, auth or network failure must
# never read as a missing tag).
inspect() {
    if docker buildx imagetools inspect "$1" --format "$2" > "$work/out" 2> "$work/err"; then
        cat "$work/out"
        return 0
    fi
    if grep -Eqi 'unauthorized|denied|forbidden|credential|authentication|token|transport|tls|certificate|timeout|timed out' "$work/err"; then
        cat "$work/err" >&2
        return 1
    fi
    if grep -Eqi 'manifest unknown|no such manifest|not found' "$work/err"; then
        return 2
    fi
    cat "$work/err" >&2
    return 1
}

# The digest of the manifest (or index) REF names.
digest_of() {
    local json
    json=$(inspect "$1" '{{json .Manifest}}') || return $?
    jq -er '.digest' <<< "$json"
}

# The version label of REF's image: one value for every platform, or nothing.
label_of() {
    local json
    json=$(inspect "$1" '{{json .Image}}') || return $?
    jq -r 'if type == "object" and has("config") then [.] else [.[]] end
        | map(.config.Labels["org.opencontainers.image.version"] // "") | unique
        | if length == 1 then .[0] else error("platforms disagree on the version") end' <<< "$json"
}

# The version code a version label ends in, or nothing for another label.
code_of() {
    if [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+[+-]([0-9]+)$ ]]; then
        echo "${BASH_REMATCH[1]}"
    fi
}

source_digest=$(digest_of "$image:$version") || fail "cannot read $image:$version"
source_label=$(label_of "$image:$version") || fail "cannot read the version of $image:$version"
[ "$(code_of "$source_label")" = "$code" ] ||
    fail "$image:$version is labelled version '$source_label', not version code $code"
echo "promote: $image:$version is $source_digest (version $source_label)"

for tag in "${tags[@]}"; do
    set +e
    current=$(digest_of "$image:$tag")
    result=$?
    set -e
    case $result in
    0)
        if [ "$current" = "$source_digest" ]; then
            echo "promote: $tag already names $version"
            continue
        fi
        current_label=$(label_of "$image:$tag") || fail "cannot read the version of $image:$tag"
        current_code=$(code_of "$current_label")
        if [ -n "$current_code" ] && [ "$current_code" -gt "$code" ]; then
            echo "promote: $tag left at the newer version $current_label"
            continue
        fi
        echo "promote: $tag ${current_label:-(unlabelled)} -> $version"
        ;;
    2)
        echo "promote: $tag (new) -> $version"
        ;;
    *)
        fail "cannot read $image:$tag"
        ;;
    esac
    docker buildx imagetools create -t "$image:$tag" "$image@$source_digest"
    moved=$(digest_of "$image:$tag") || fail "cannot read $image:$tag after moving it"
    [ "$moved" = "$source_digest" ] || fail "$image:$tag names $moved, not $source_digest"
done

echo "promote: done; $image:{$(IFS=,; echo "${tags[*]}")} checked"
