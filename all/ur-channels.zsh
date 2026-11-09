#!/usr/bin/env zsh
# Renders ur's v1 package channels from a promoted release (DESIGN.md §4.18's v1 line, §4.10's
# "winget later"; REPO-M6-01): a Homebrew cask of the notarized bundle for macOS and a formula of the
# .tar.gz for Linux, laid out as a tap's Casks/ and Formula/, and winget's three manifests for the
# portable zip, laid out as microsoft/winget-pkgs' manifests/.
#
#   ur-channels.zsh <version>
#
# A channel only promotes a signed release. The release's SHA256SUMS and SHA256SUMS.sig come from
# where ur-promote.zsh staged them on mmm, the signature is verified against RELEASE-KEY.pub before
# anything is read from them, and every checksum a channel carries is taken from those sums, never
# from a download: nothing here fetches anything. apt and yum are not v1 channels: the owner dropped
# them (plan-REPO-v1-channels, QD-023), and the .deb and .rpm stay direct downloads that the same
# signed sums cover.
#
# UR_RELEASE_KEY names the published key and UR_PROMOTE_MMM the mmm checkout, as for ur-promote.zsh
# (default: the sidecar checkout's RELEASE-KEY.pub and mmm, beside this build checkout).
# UR_CHANNELS_OUT is where the channels go (default: ${BUILD_OUT:-<build>/out}/ur-channels/<version>),
# replaced whole when they render and left as it was on a refusal. Publishing them is the owner's
# (EXT-8): this pushes nothing and opens nothing.
#
# exit: 0 rendered; 1 refused; 64 usage
#
# SPDX-License-Identifier: MPL-2.0

emulate -R zsh
setopt pipe_fail no_unset

ur_channels_die () {
    print -u2 -- "ur-channels: $*"
    exit 1
}

(( $# == 1 )) || { print -u2 -- "usage: ur-channels.zsh <version>"; exit 64 }
version="$1"
here="${0:A:h}"
script_root="${0:A:h:h:h}"
release_key="${UR_RELEASE_KEY:-$script_root/sidecar/RELEASE-KEY.pub}"
mmm="${UR_PROMOTE_MMM:-$script_root/mmm}"
out="${UR_CHANNELS_OUT:-${BUILD_OUT:-${here:h}/out}/ur-channels/$version}"

[[ "$version" =~ '^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+$' ]] || ur_channels_die "$version is not a release version yyyy.m.d-code"
promoted="$mmm/ur.io/astro/public/recurd/ur/$version"
[[ -s "$promoted/SHA256SUMS" ]] ||
    ur_channels_die "$promoted holds no SHA256SUMS: only a promoted release has channels (ur-promote.zsh)"
[[ -s "$promoted/SHA256SUMS.sig" ]] ||
    ur_channels_die "$promoted holds no SHA256SUMS.sig: only a signed release has channels"

# the published key: one Ed25519 line, or no release is signed yet
[[ -s "$release_key" ]] ||
    ur_channels_die "$release_key does not exist: the release key ceremony (EXT-3) has not run, so no release is signed"
key_lines=$(grep -c . "$release_key")
key_type=$(awk 'NF { print $1; exit }' "$release_key")
[[ "$key_lines" == 1 && "$key_type" == ssh-ed25519 ]] || ur_channels_die "$release_key is not one ssh-ed25519 key"

work=$(mktemp -d) || exit 1
staging="$out.rendering.$$"
trap 'rm -rf "$work" "$staging"' EXIT

# the copies verified are the copies read
cp "$promoted/SHA256SUMS" "$work/SHA256SUMS" && cp "$promoted/SHA256SUMS.sig" "$work/SHA256SUMS.sig" || exit 1
print -r -- "ur-release namespaces=\"ur-release\" $(awk 'NF { print $1, $2; exit }' "$release_key")" > "$work/allowed_signers"
ssh-keygen -Y verify -f "$work/allowed_signers" -I ur-release -n ur-release -s "$work/SHA256SUMS.sig" < "$work/SHA256SUMS" > "$work/verify.log" 2>&1 || {
    cat "$work/verify.log" >&2
    ur_channels_die "$promoted/SHA256SUMS.sig does not verify against $release_key"
}

# the one well-formed line the signed sums hold for a name
ur_channels_sum () {
    local sum
    sum=$(awk -v name="$1" '
        $2 == name { count++; if (NF == 2 && length($1) == 64 && $1 !~ /[^0-9a-f]/) sum = $1 }
        END { if (count == 1 && sum != "") print sum }' "$work/SHA256SUMS")
    [[ -n "$sum" ]] || ur_channels_die "ur $version's signed SHA256SUMS lists no one $1"
    print -r -- "$sum"
}

typeset -A values
values[VERSION]="$version"
for os arch in darwin amd64 darwin arm64 linux amd64 linux arm64; do
    values[SHA256_${(U)os}_${(U)arch}]=$(ur_channels_sum "ur-$version-$os-$arch.tar.gz") || exit 1
done
# winget-pkgs writes a hash in capitals
for arch in amd64 arm64; do
    sum=$(ur_channels_sum "ur-$version-windows-$arch.zip") || exit 1
    values[SHA256_WINDOWS_${(U)arch}]=${(U)sum}
done

# a template with its @NAME@ placeholders filled, refusing one this script does not fill
ur_channels_render () {
    local template="$1" output="$2" text name
    text=$(<"$template") || exit 1
    for name in ${(k)values}; do
        text=${text//"@$name@"/$values[$name]}
    done
    print -r -- "$text" | grep -q '@[A-Z0-9_][A-Z0-9_]*@' && ur_channels_die "$template holds a placeholder nothing fills"
    mkdir -p "${output:h}" && print -r -- "$text" > "$output" || exit 1
}

winget="winget/manifests/u/URnetwork/ur/$version"
rm -rf "$staging"
ur_channels_render "$here/ur/homebrew/ur.cask.rb.tmpl" "$staging/homebrew/Casks/ur.rb"
ur_channels_render "$here/ur/homebrew/ur.formula.rb.tmpl" "$staging/homebrew/Formula/ur.rb"
ur_channels_render "$here/ur/winget/ur.installer.yaml.tmpl" "$staging/$winget/URnetwork.ur.installer.yaml"
ur_channels_render "$here/ur/winget/ur.locale.yaml.tmpl" "$staging/$winget/URnetwork.ur.locale.en-US.yaml"
ur_channels_render "$here/ur/winget/ur.version.yaml.tmpl" "$staging/$winget/URnetwork.ur.yaml"

rm -rf "$out" && mv "$staging" "$out" || ur_channels_die "cannot move the channels into $out"
print -r -- "rendered ur $version's channels from its signed SHA256SUMS into $out:"
for file in "$out"/**/*(.N); do
    print -r -- "  ${file#$out/}"
done
print -r -- "owner: commit homebrew/ to the tap repository and submit $winget to microsoft/winget-pkgs (EXT-8)"
