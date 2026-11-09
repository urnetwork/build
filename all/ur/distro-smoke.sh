#!/usr/bin/env bash
# ur's packages on their distributions (REPO-M1-03): each amd64 package installs with no network on
# ubuntu 22.04 and 24.04, fedora and archlinux, `ur --version` runs, git-remote-ur resolves to
# /usr/bin/ur, and a removal leaves nothing behind. --network none, so a package that needs a fetch
# fails here rather than on a person's machine. Each container is named sidecar-test-ur-smoke-<distro>
# and removed after.
#
#   distro-smoke.sh <out-dir> <EXTERNAL_WARP_VERSION>
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: distro-smoke.sh <out-dir> <version>" >&2; exit 64; }
out_dir="$1"
version="$2"

smoke() {
  local distro="$1" image="$2" package="$3" install="$4" remove="$5"
  [ -s "$out_dir/$package" ] || { echo "ERROR: no $package in $out_dir" >&2; exit 1; }
  echo ">>> $package on $distro"
  docker run --rm --network none --platform linux/amd64 --name "sidecar-test-ur-smoke-$distro" \
    -v "$out_dir/$package:/pkg/$package:ro" "$image" sh -euc "
      $install /pkg/$package
      ur --version
      test \"\$(readlink -f \"\$(command -v git-remote-ur)\")\" = /usr/bin/ur
      $remove
      for path in /usr/bin/ur /usr/bin/git-remote-ur /usr/share/doc/ur; do
        if [ -e \"\$path\" ] || [ -L \"\$path\" ]; then echo \"\$path is left after the removal\" >&2; exit 1; fi
      done
    "
}

smoke ubuntu-22.04 ubuntu:22.04 "ur_${version}_amd64.deb" "dpkg -i" "dpkg --purge ur"
smoke ubuntu-24.04 ubuntu:24.04 "ur_${version}_amd64.deb" "dpkg -i" "dpkg --purge ur"
smoke fedora fedora:latest "ur-${version}.x86_64.rpm" "rpm -i" "rpm -e ur"
smoke archlinux archlinux:latest "ur-${version}-x86_64.pkg.tar.zst" "pacman -U --noconfirm" "pacman -R --noconfirm ur"
echo ">>> ur's packages install, run and remove cleanly on four distributions"
