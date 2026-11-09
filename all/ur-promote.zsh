#!/usr/bin/env zsh
# Promotes a signed ur release to ur.io (DESIGN.md §4.10; REPO-M3-01; plan-REPO-release-signature-flow,
# decided 2026-11-06): once both holders have signed a release's SHA256SUMS (ur-sign-release.zsh), this
# verifies the signature again against the published RELEASE-KEY.pub, then stages
#
#   mmm/ur.io/astro/public/recurd/ur/<version>/SHA256SUMS
#   mmm/ur.io/astro/public/recurd/ur/<version>/SHA256SUMS.sig
#   mmm/ur.io/astro/public/recurd/ur/latest.json, a copy of the release's ur-release.json
#
# as one commit on the mmm checkout for the owner to push and deploy. install.sh reads latest.json,
# verifies the sums' signature and latest.json's own line in them, then fetches the artifact from the
# GitHub release. ur's artifacts are not copied here: GitHub serves them, and the signed sums bind them.
#
#   ur-promote.zsh <version> <sig>
#
# <sig> is SHA256SUMS.sig, with the release's SHA256SUMS and ur-release.json beside it.
# UR_RELEASE_KEY names the published key (default: the sidecar checkout's RELEASE-KEY.pub beside this
# build checkout), and UR_PROMOTE_MMM the mmm checkout (default: beside it too).
#
# It refuses a signature that does not verify, sums that do not list this ur-release.json, an
# ur-release.json of another version, a latest.json it would move backward and a dirty mmm, and a
# refusal changes nothing. RELEASE-KEY.pub does not exist until the key ceremony (EXT-3), and its
# absence is a refusal: never commit a placeholder. Publishing waits on D-TRUST-3: no sidecar ships
# before stage 1 and 7-day tokens. Nothing here pushes.
#
# exit: 0 promoted; 1 refused; 64 usage
#
# SPDX-License-Identifier: MPL-2.0

emulate -R zsh
setopt pipe_fail no_unset

ur_promote_die () {
    print -u2 -- "ur-promote: $*"
    exit 1
}

(( $# == 2 )) || { print -u2 -- "usage: ur-promote.zsh <version> <SHA256SUMS.sig>"; exit 64 }
version="$1"
signature="$2"
script_root="${0:A:h:h:h}"
release_key="${UR_RELEASE_KEY:-$script_root/sidecar/RELEASE-KEY.pub}"
mmm="${UR_PROMOTE_MMM:-$script_root/mmm}"
public="ur.io/astro/public/recurd/ur"

[[ "$version" =~ '^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+$' ]] || ur_promote_die "$version is not a release version yyyy.m.d-code"
directory="${signature:h}"
sums="$directory/SHA256SUMS"
release="$directory/ur-release.json"
[[ -s "$signature" && -s "$sums" && -s "$release" ]] ||
    ur_promote_die "$directory must hold SHA256SUMS, SHA256SUMS.sig and ur-release.json"

# the published key: one Ed25519 line, or nothing to promote with until the ceremony
[[ -s "$release_key" ]] || ur_promote_die "$release_key does not exist: the release key ceremony (EXT-3) has not run, so nothing can be promoted"
key_lines=$(grep -c . "$release_key")
key_type=$(awk 'NF { print $1; exit }' "$release_key")
[[ "$key_lines" == 1 && "$key_type" == ssh-ed25519 ]] || ur_promote_die "$release_key is not one ssh-ed25519 key"

work=$(mktemp -d) || exit 1
trap 'rm -rf "$work"' EXIT
print -r -- "ur-release namespaces=\"ur-release\" $(awk 'NF { print $1, $2; exit }' "$release_key")" > "$work/allowed_signers"
ssh-keygen -Y verify -f "$work/allowed_signers" -I ur-release -n ur-release -s "$signature" < "$sums" > "$work/verify.log" 2>&1 || {
    cat "$work/verify.log" >&2
    ur_promote_die "SHA256SUMS.sig does not verify against $release_key"
}
awk 'NF != 2 || length($1) != 64 || $2 !~ /^[A-Za-z0-9][A-Za-z0-9._+-]*$/ { bad = 1 } END { exit bad }' "$sums" ||
    ur_promote_die "SHA256SUMS holds a line that is not shasum -a 256's"
listed=$(awk '$2 == "ur-release.json" { print $1 }' "$sums")
[[ -n "$listed" && "$listed" == "$(shasum -a 256 "$release" | awk '{ print $1 }')" ]] ||
    ur_promote_die "the signed SHA256SUMS does not list this ur-release.json"
grep -qxF "  \"version\": \"$version\"," "$release" || ur_promote_die "ur-release.json is not version $version's"

# a release version as numbers: yyyy, m, d and the code, compared in that order
ur_promote_older () {
    local -a a b
    local index
    a=(${(s:.:)${1/-/.}})
    b=(${(s:.:)${2/-/.}})
    for index in 1 2 3 4; do
        (( a[index] < b[index] )) && return 0
        (( a[index] > b[index] )) && return 1
    done
    return 1
}
latest="$mmm/$public/latest.json"
if [[ -e "$latest" ]]; then
    current=$(sed -n 's/^  "version": "\([0-9.]*-[0-9]*\)",$/\1/p' "$latest")
    [[ -n "$current" ]] || ur_promote_die "$latest names no version"
    ur_promote_older "$version" "$current" && ur_promote_die "latest.json is at $current, and $version would move it backward"
fi

[[ -d "$mmm/.git" || -f "$mmm/.git" ]] || ur_promote_die "$mmm is not a git checkout"
[[ -z "$(git -C "$mmm" status --porcelain --untracked-files=all)" ]] || ur_promote_die "$mmm has changes outside a commit"
head=$(git -C "$mmm" rev-parse HEAD) || exit 1

# every check has passed; now the files and one commit, undone if the commit fails
mkdir -p "$mmm/$public/$version" || exit 1
cp "$sums" "$mmm/$public/$version/SHA256SUMS" &&
    cp "$signature" "$mmm/$public/$version/SHA256SUMS.sig" &&
    cp "$release" "$mmm/$public/latest.json" &&
    git -C "$mmm" add -- "$public/$version/SHA256SUMS" "$public/$version/SHA256SUMS.sig" "$public/latest.json" &&
    git -C "$mmm" commit -q -m "Promote ur $version: its signed SHA256SUMS and latest.json on ur.io" || {
        git -C "$mmm" reset -q --hard "$head"
        git -C "$mmm" clean -q -fd -- "$public"
        ur_promote_die "the promotion commit failed; mmm is back at $head"
    }
print -r -- "promoted ur $version in $mmm"
print -r -- "owner: git -C $mmm push origin $(git -C "$mmm" symbolic-ref --quiet --short HEAD || print -r -- HEAD)"
