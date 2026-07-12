# URnetwork Linux snap build container.
#
# Based on Canonical's official Snapcraft rock (the supported way to build snaps
# in Docker — no snapd, no LXD, no nested VM). `--destructive-mode` then builds
# directly inside this container. We build one container per target arch:
# arm64 runs native on the Apple-Silicon build host; amd64 runs under Docker's
# qemu emulation. Pre-baking the C++/GTK build deps turns Snapcraft's per-build
# apt installs into cache hits — which matters most for the emulated amd64 pass.
#
# See build.sh (host orchestration) and ../run.sh (Linux snap section).
# SPDX-License-Identifier: MPL-2.0
FROM ghcr.io/canonical/snapcraft:8_core24

# core24 == Ubuntu 24.04; these match snapcraft.yaml's build-packages.
RUN apt-get update && apt-get install -y --no-install-recommends \
        g++ \
        meson \
        ninja-build \
        pkg-config \
        libgtkmm-4.0-dev \
        libadwaita-1-dev \
        libglib2.0-dev \
        nlohmann-json3-dev \
        gettext \
        unzip \
    && rm -rf /var/lib/apt/lists/*

# *important* gettext (msgfmt) is required, not optional: meson's i18n module
# SILENTLY disables translations when msgfmt is missing — the build still
# succeeds and the snap ships english-only, with no error to notice.

# The rock's entrypoint is `snapcraft`; args passed to `docker run` are forwarded
# to it (e.g. `pack --destructive-mode --build-for arm64`).
