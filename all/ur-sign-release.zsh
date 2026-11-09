#!/usr/bin/env zsh
# The signing ceremony's helper (DESIGN.md §4.10, §2.6; REPO-M3-01; plan-REPO-release-signature-flow,
# decided 2026-11-06): ur's releases are promoted, not each signed. When both holders of the offline
# two-person Ed25519 release key pick a release, they sign its SHA256SUMS with this, on the offline
# machine, and ur-promote.zsh publishes the signature.
#
#   ur-sign-release.zsh fetch <version> <directory>
#       on a holder's online machine: the release's SHA256SUMS and ur-release.json from its GitHub
#       release, into <directory>, to carry to the offline machine
#   ur-sign-release.zsh sign <version> <key> <directory>
#       on the offline machine: checks that ur-release.json names <version> and is the file the sums
#       list, prints the sums and their own SHA-256 for both holders to compare with what each fetched,
#       asks for the version to be typed back, signs with ssh-keygen -Y sign under namespace ur-release,
#       verifies its own signature against the key's public half, and only then writes SHA256SUMS.sig
#       beside the sums
#
# UR_RELEASE_KEY, when set, names the published RELEASE-KEY.pub, and the signing key must be its private
# half. No private key touches the build host, the repository or vault: <key> is the offline machine's,
# and nothing here copies it. Namespace and identity are both ur-release (plan-REPO-small-readings #10).
#
# exit: 0 done; 1 refused; 64 usage
#
# SPDX-License-Identifier: MPL-2.0

emulate -R zsh
setopt pipe_fail no_unset

ur_sign_die () {
    print -u2 -- "ur-sign-release: $*"
    exit 1
}

ur_sign_usage () {
    print -u2 -- "usage: ur-sign-release.zsh fetch <version> <directory> | sign <version> <key> <directory>"
    exit 64
}

(( $# >= 1 )) || ur_sign_usage
command="$1"
shift
version="${1:-}"
[[ "$version" =~ '^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+$' ]] || ur_sign_die "$version is not a release version yyyy.m.d-code"

case "$command" in
    fetch)
        (( $# == 2 )) || ur_sign_usage
        directory="$2"
        mkdir -p "$directory" || exit 1
        for name in SHA256SUMS ur-release.json; do
            curl -s -S -L --fail-with-body -o "$directory/$name" \
                "https://github.com/urnetwork/build/releases/download/v$version/$name" ||
                ur_sign_die "cannot fetch $name of v$version"
        done
        print -r -- "fetched v$version's SHA256SUMS and ur-release.json into $directory"
        print -r -- "SHA-256 of SHA256SUMS: $(shasum -a 256 "$directory/SHA256SUMS" | awk '{ print $1 }')"
        exit 0
        ;;
    sign)
        (( $# == 3 )) || ur_sign_usage
        key="$2"
        directory="$3"
        ;;
    *)
        ur_sign_usage
        ;;
esac

sums="$directory/SHA256SUMS"
release="$directory/ur-release.json"
[[ -s "$sums" && -s "$release" ]] || ur_sign_die "$directory holds no SHA256SUMS and ur-release.json"
[[ -r "$key" ]] || ur_sign_die "cannot read the key $key"
[[ ! -e "$directory/SHA256SUMS.sig" ]] || ur_sign_die "$directory/SHA256SUMS.sig exists already"

# ur-release.json names this version, and is the very file the sums list
grep -qxF "  \"version\": \"$version\"," "$release" || ur_sign_die "ur-release.json does not name $version"
listed=$(awk '$2 == "ur-release.json" { print $1 }' "$sums")
[[ -n "$listed" && "$listed" == "$(shasum -a 256 "$release" | awk '{ print $1 }')" ]] ||
    ur_sign_die "SHA256SUMS does not list this ur-release.json"
awk 'NF != 2 || length($1) != 64 || $2 !~ /^[A-Za-z0-9][A-Za-z0-9._+-]*$/ { bad = 1 } END { exit bad }' "$sums" ||
    ur_sign_die "SHA256SUMS holds a line that is not shasum -a 256's"

public_key="$key.pub"
if [[ ! -r "$public_key" ]]; then
    public_key=$(mktemp) || exit 1
    ssh-keygen -y -f "$key" > "$public_key" || ur_sign_die "cannot read the public half of $key"
fi
if [[ -n "${UR_RELEASE_KEY:-}" ]]; then
    [[ "$(awk '{ print $1, $2 }' "$public_key")" == "$(awk '{ print $1, $2 }' "$UR_RELEASE_KEY")" ]] ||
        ur_sign_die "$key is not the private half of $UR_RELEASE_KEY"
fi

print -r -- "Release v$version. Both holders compare these sums with what each fetched:"
print -r -- ""
cat "$sums"
print -r -- ""
print -r -- "SHA-256 of SHA256SUMS: $(shasum -a 256 "$sums" | awk '{ print $1 }')"
print -n -- "Type the version both holders compared, to sign it: "
read -r typed || typed=""
[[ "$typed" == "$version" ]] || ur_sign_die "the holders typed $typed, not $version; nothing is signed"

work=$(mktemp -d) || exit 1
trap 'rm -rf "$work"' EXIT
cp "$sums" "$work/SHA256SUMS" || exit 1
ssh-keygen -Y sign -f "$key" -n ur-release "$work/SHA256SUMS" || ur_sign_die "ssh-keygen -Y sign failed"
print -r -- "ur-release namespaces=\"ur-release\" $(awk '{ print $1, $2 }' "$public_key")" > "$work/allowed_signers"
ssh-keygen -Y verify -f "$work/allowed_signers" -I ur-release -n ur-release -s "$work/SHA256SUMS.sig" < "$work/SHA256SUMS" ||
    ur_sign_die "the signature does not verify against the key's own public half; nothing is written"
cp "$work/SHA256SUMS.sig" "$directory/SHA256SUMS.sig" || exit 1
print -r -- "signed: $directory/SHA256SUMS.sig. Carry it back and run ur-promote.zsh $version <sig>."
