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
        libwebkitgtk-6.0-dev \
        unzip \
    && rm -rf /var/lib/apt/lists/*

# snapcraft 8.11.1 in this rock is a pip install: its desktop/gnome extension data
# (the command-chain launcher the `extensions: [gnome]` app uses) lives at
# <site-packages>/extensions, but snapcraft resolves it to /usr/share/snapcraft/
# extensions (which doesn't exist) -> "Failed to pull source: unable to determine
# source type of '/usr/share/snapcraft/extensions/desktop/command-chain'". Symlink
# the expected path to the real one so the gnome extension's part pulls.
# NB: /usr/share/snapcraft/extensions already exists as an EMPTY dir in this rock,
# so a plain `ln -sfn target dir` would nest the link INSIDE it. rm -rf it first,
# then symlink, and verify the command-chain resolves (fail the build if not).
RUN target="$(find /usr/lib/python3*/site-packages -maxdepth 1 -type d -name extensions | head -1)"; \
    test -n "$target" || { echo "ERROR: snapcraft extensions dir not found under site-packages"; exit 1; }; \
    rm -rf /usr/share/snapcraft/extensions; mkdir -p /usr/share/snapcraft; \
    ln -s "$target" /usr/share/snapcraft/extensions; \
    ls -d /usr/share/snapcraft/extensions/desktop/command-chain >/dev/null || { echo "ERROR: command-chain still not resolving"; exit 1; }; \
    echo "linked $target -> /usr/share/snapcraft/extensions (command-chain OK)"

# *important* gettext (msgfmt) is required, not optional: meson's i18n module
# SILENTLY disables translations when msgfmt is missing — the build still
# succeeds and the snap ships english-only, with no error to notice.

# *important* libwebkitgtk-6.0-dev is likewise a soft dependency: meson
# (required: false) SILENTLY builds the app without the embedded Stripe
# checkout webview when it is missing — the build still succeeds and upgrade
# quietly falls back to the hosted checkout in the browser, with no error to
# notice.

# The rock's entrypoint is `snapcraft`; args passed to `docker run` are forwarded
# to it (e.g. `pack --destructive-mode --build-for arm64`).
