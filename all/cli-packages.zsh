#!/usr/bin/env zsh
# SPDX-License-Identifier: MPL-2.0
#
# Package the CLI (the sn miner, which sn_cli_release builds as `provider`)
# for the package managers, as `urnetwork-provider`:
#
#   urnetwork-provider-<version>-<darwin|linux>-<amd64|arm64>.tar.gz
#   urnetwork-provider-<version>-windows-<amd64|arm64>.zip
#       the binary (urnetwork-provider[.exe]) and LICENSE at the top level,
#       for Homebrew, winget and Chocolatey
#   urnetwork-provider_<version>_<amd64|arm64|armhf>.deb
#   urnetwork-provider-<version>.<x86_64|aarch64>.rpm
#       /usr/bin/urnetwork-provider, the license, and
#       /usr/share/urnetwork-provider/managed-by: the command that updates a
#       package-managed binary, which the miner's own `update` defers to
#
# The binaries are the ones in urnetwork-provider-<version>.tar.gz, byte for
# byte; only their name changes (/usr/bin/urnetwork belongs to the daemon
# package). The .deb and .rpm are built with nfpm: $NFPM when set, else nfpm
# v2.47.0 (the version linux/packaging verified) run with `go run`, whose
# module download the Go checksum database verifies. Every file gets the
# same timestamp, derived from the release code, so a resumed nightly
# rebuilds the bytes it already uploaded.
#
#   all/cli-packages.zsh <version> <miner build dir> <LICENSE> <out dir>
#
# <miner build dir> holds <os>/<arch>/provider[.exe]. Exits non-zero on any
# missing input or failed step; run.sh treats that as a warning.
setopt err_exit no_unset pipe_fail

if (( $# != 4 )); then
    print -u2 "usage: $0 <version> <miner build dir> <LICENSE> <out dir>"
    exit 2
fi
version=$1 build=$2 license=$3 out=$4
if [[ ! $version =~ '^[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}-[0-9]+$' ]]; then
    print -u2 "cli-packages: $version is not <yyyy.m.d>-<code>"
    exit 2
fi
upstream=${version%-*}
code=${version##*-}
[[ -f $license ]] || { print -u2 "cli-packages: no license file $license"; exit 1 }

# the code is ten times the seconds since 2023-05-23T00:00:00Z
export SOURCE_DATE_EPOCH=$(( 1684800000 + code / 10 ))
stamp=$(TZ=UTC date -r $SOURCE_DATE_EPOCH +%Y%m%d%H%M.%S 2>/dev/null || TZ=UTC date -d @$SOURCE_DATE_EPOCH +%Y%m%d%H%M.%S)

# outside any workspace or vendor flags: `go run pkg@version` builds the module by itself
nfpm=(${=NFPM:-env GOFLAGS= GOWORK=off go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0})

# no doubled slash: nfpm's glob of contents[].src matches nothing through one
work=$(mktemp -d "${${TMPDIR:-/tmp}%/}/cli-packages.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"
rm -f "$out"/urnetwork-provider[-_]*(N)

# stage <os> <arch>: the renamed binary and LICENSE, fixed modes and times
stage () {
    local os=$1 arch=$2 ext=''
    [[ $os == windows ]] && ext=.exe
    local src="$build/$os/$arch/provider$ext" dir="$work/$os-$arch"
    [[ -f $src ]] || { print -u2 "cli-packages: no $src"; return 1 }
    mkdir -p "$dir"
    cp "$src" "$dir/urnetwork-provider$ext"
    cp "$license" "$dir/LICENSE"
    chmod 0755 "$dir/urnetwork-provider$ext"
    chmod 0644 "$dir/LICENSE"
    TZ=UTC touch -t $stamp "$dir/urnetwork-provider$ext" "$dir/LICENSE"
}

for os arch in darwin arm64 darwin amd64 linux amd64 linux arm64 windows amd64 windows arm64; do
    stage $os $arch
    name="urnetwork-provider-$version-$os-$arch"
    if [[ $os == windows ]]; then
        (cd "$work/$os-$arch" && TZ=UTC zip -X -D -q "$out/$name.zip" urnetwork-provider.exe LICENSE)
    elif tar --version 2>/dev/null | grep -q bsdtar; then
        (cd "$work/$os-$arch" && COPYFILE_DISABLE=1 tar --uid 0 --gid 0 --uname root --gname root --no-xattrs --no-acls --no-fflags \
            -cf - urnetwork-provider LICENSE | gzip -n -9 > "$out/$name.tar.gz")
    else
        (cd "$work/$os-$arch" && tar --owner=0 --group=0 --numeric-owner --mtime=@$SOURCE_DATE_EPOCH \
            -cf - urnetwork-provider LICENSE | gzip -n -9 > "$out/$name.tar.gz")
    fi
done
stage linux arm

# nfpm_config <arch dir> <nfpm arch> <license path>; the arch dir holds managed-by
nfpm_config () {
    print -r -- "name: urnetwork-provider
arch: $2
platform: linux
version: \"$upstream\"
release: \"$code\"
version_schema: none
maintainer: \"URnetwork <support@ur.io>\"
vendor: \"URnetwork\"
homepage: \"https://ur.io\"
license: \"MPL-2.0\"
section: net
priority: optional
description: |-
  URnetwork provider
  Shares this computer's internet connection with the URnetwork
  decentralized VPN and earns rewards for the traffic it carries.
recommends:
  - ca-certificates
contents:
  - src: $work/$1/urnetwork-provider
    dst: /usr/bin/urnetwork-provider
    file_info:
      mode: 0755
  - src: $work/$1/managed-by
    dst: /usr/share/urnetwork-provider/managed-by
    file_info:
      mode: 0644
  - src: $work/$1/LICENSE
    dst: $3
    file_info:
      mode: 0644"
}

# deb: <go arch> <nfpm arch> <deb arch>
for goarch nfpmarch debarch in amd64 amd64 amd64 arm64 arm64 arm64 arm arm7 armhf; do
    print -r -- "sudo apt-get install --only-upgrade urnetwork-provider" > "$work/linux-$goarch/managed-by"
    TZ=UTC touch -t $stamp "$work/linux-$goarch/managed-by"
    nfpm_config linux-$goarch $nfpmarch /usr/share/doc/urnetwork-provider/copyright > "$work/deb-$goarch.yaml"
    $nfpm package --config "$work/deb-$goarch.yaml" --packager deb --target "$out/urnetwork-provider_${version}_$debarch.deb" > /dev/null
done
# rpm: <go arch> <nfpm arch> <rpm arch>
for goarch nfpmarch rpmarch in amd64 amd64 x86_64 arm64 arm64 aarch64; do
    print -r -- "sudo dnf upgrade urnetwork-provider" > "$work/linux-$goarch/managed-by"
    TZ=UTC touch -t $stamp "$work/linux-$goarch/managed-by"
    nfpm_config linux-$goarch $nfpmarch /usr/share/licenses/urnetwork-provider/LICENSE > "$work/rpm-$goarch.yaml"
    $nfpm package --config "$work/rpm-$goarch.yaml" --packager rpm --target "$out/urnetwork-provider-$version.$rpmarch.rpm" > /dev/null
done

print -rl -- "$out"/urnetwork-provider[-_]*(N.:t)
