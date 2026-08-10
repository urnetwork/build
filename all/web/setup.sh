#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
#
# Provision and smoke-test the browser environment used by
# mmm/ur.io/test-main.sh.
#
# Usage:
#   ./setup.sh
#   ./setup.sh --no-sudo   do not require the host-mapping sudo path
set -euo pipefail
umask 077

here="$(cd "$(dirname "$0")" && pwd)"
root="${URNETWORK_ROOT:-$(cd "$here/../../.." && pwd)}"
site="$root/mmm/ur.io"
react="$site/react"
astro="$site/astro"
extension="$root/extension"
no_sudo=0
temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-web-setup.XXXXXX")"
cleanup() { rm -rf "$temporary_root"; }
trap cleanup EXIT

for arg in "$@"; do
  case "$arg" in
    --no-sudo) no_sudo=1 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

die() { echo "ERROR: $*" >&2; exit 1; }

for command_name in timeout node npm go openssl curl tar zip lsof shasum install; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done
node -e '
  const [major, minor] = process.versions.node.split(".").map(Number);
  if (major < 22 || (major === 22 && minor < 12)) process.exit(1);
' || die "Node 22.12 or newer is required"

for directory in "$react" "$astro" "$extension"; do
  [ -f "$directory/package.json" ] || die "missing package at $directory"
  [ -f "$directory/package-lock.json" ] || die "missing package lock at $directory"
done

install_dependencies() {
  local directory="$1" expected marker current
  marker="$directory/node_modules/.urnetwork-acceptance-lock-sha256"
  expected="$(shasum -a 256 "$directory/package.json" "$directory/package-lock.json" | shasum -a 256 | awk '{print $1}')"
  current=""
  [ -f "$marker" ] && current="$(tr -d '\r\n' <"$marker")"
  if [ ! -d "$directory/node_modules" ] || [ "$current" != "$expected" ]; then
    echo ">>> installing locked dependencies in $directory"
    (cd "$directory" && timeout 1200 npm ci --no-audit --no-fund)
    printf '%s\n' "$expected" >"$marker"
    chmod 600 "$marker"
  else
    echo ">>> locked dependencies already installed in $directory"
  fi
}

install_dependencies "$react"
install_dependencies "$astro"
install_dependencies "$extension"

case "$(uname -s)" in
  Darwin) default_firefox=/Applications/Firefox.app/Contents/MacOS/firefox ;;
  *) default_firefox="$(command -v firefox || true)" ;;
esac
firefox_bin="${UR_FIREFOX_BIN:-$default_firefox}"
if [ ! -x "$firefox_bin" ]; then
  if command -v brew >/dev/null 2>&1 && [ "$(uname -s)" = Darwin ]; then
    echo ">>> installing Firefox with Homebrew"
    timeout 1800 brew install --cask firefox
    firefox_bin=/Applications/Firefox.app/Contents/MacOS/firefox
  else
    die "Firefox is missing; install it or set UR_FIREFOX_BIN"
  fi
fi
"$firefox_bin" --version

geckodriver_version=v0.37.1
geckodriver_dir="$react/tests/.geckodriver"
geckodriver_bin="$geckodriver_dir/geckodriver"
if [ ! -x "$geckodriver_bin" ] || ! "$geckodriver_bin" --version 2>/dev/null | grep -Fq '0.37.1'; then
  case "$(uname -s)-$(uname -m)" in
    Darwin-arm64) geckodriver_archive=macos-aarch64 ;;
    Darwin-*) geckodriver_archive=macos ;;
    Linux-aarch64|Linux-arm64) geckodriver_archive=linux-aarch64 ;;
    Linux-*) geckodriver_archive=linux64 ;;
    *) die "unsupported geckodriver host: $(uname -s)-$(uname -m)" ;;
  esac
  temporary_dir="$temporary_root/geckodriver"
  mkdir -p "$temporary_dir"
  echo ">>> installing geckodriver $geckodriver_version ($geckodriver_archive)"
  timeout 300 curl -fsSL --connect-timeout 15 \
    "https://github.com/mozilla/geckodriver/releases/download/$geckodriver_version/geckodriver-$geckodriver_version-$geckodriver_archive.tar.gz" \
    -o "$temporary_dir/geckodriver.tar.gz"
  tar -xzf "$temporary_dir/geckodriver.tar.gz" -C "$temporary_dir"
  mkdir -p "$geckodriver_dir"
  install -m 755 "$temporary_dir/geckodriver" "$geckodriver_bin"
fi
"$geckodriver_bin" --version | sed -n '1p'

echo ">>> smoke-testing Firefox through geckodriver"
UR_SETUP_FIREFOX_BIN="$firefox_bin" \
UR_SETUP_GECKODRIVER_BIN="$geckodriver_bin" \
timeout 120 node -e '
  const net = require("node:net");
  const { once } = require("node:events");
  const { spawn } = require("node:child_process");

  async function freePort() {
    const socket = net.createServer();
    socket.listen(0, "127.0.0.1");
    await once(socket, "listening");
    const port = socket.address().port;
    await new Promise((resolve) => socket.close(resolve));
    return port;
  }

  async function command(base, path, options = {}) {
    const response = await fetch(`${base}${path}`, {
      ...options,
      headers: { "content-type": "application/json", ...(options.headers || {}) },
      signal: AbortSignal.timeout(15_000),
    });
    const result = await response.json().catch(() => ({}));
    if (!response.ok || result.value?.error) {
      throw new Error(result.value?.message || `WebDriver HTTP ${response.status}`);
    }
    return result.value;
  }

  (async () => {
    const port = await freePort();
    const base = `http://127.0.0.1:${port}`;
    const driver = spawn(process.env.UR_SETUP_GECKODRIVER_BIN, ["--port", String(port)], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    let diagnostics = "";
    for (const stream of [driver.stdout, driver.stderr]) {
      stream.on("data", (chunk) => {
        diagnostics = (diagnostics + chunk.toString()).slice(-8_000);
      });
    }

    let sessionId = "";
    try {
      const deadline = Date.now() + 15_000;
      while (Date.now() < deadline) {
        if (driver.exitCode !== null) throw new Error(`geckodriver exited ${driver.exitCode}`);
        try {
          const status = await command(base, "/status");
          if (status?.ready !== undefined) break;
        } catch {}
        await new Promise((resolve) => setTimeout(resolve, 250));
      }
      if (Date.now() >= deadline) throw new Error("geckodriver did not become ready");

      const session = await command(base, "/session", {
        method: "POST",
        body: JSON.stringify({
          capabilities: {
            alwaysMatch: {
              acceptInsecureCerts: true,
              "moz:firefoxOptions": {
                binary: process.env.UR_SETUP_FIREFOX_BIN,
                args: ["-headless"],
              },
            },
          },
        }),
      });
      sessionId = session?.sessionId || "";
      if (!sessionId) throw new Error("Firefox WebDriver returned no session ID");
    } catch (error) {
      throw new Error(`${error.message}${diagnostics ? `\n${diagnostics}` : ""}`);
    } finally {
      if (sessionId) {
        await command(base, `/session/${sessionId}`, { method: "DELETE" }).catch(() => {});
      }
      driver.kill("SIGTERM");
      await Promise.race([
        once(driver, "exit"),
        new Promise((resolve) => setTimeout(resolve, 2_000)),
      ]).catch(() => {});
      if (driver.exitCode === null) driver.kill("SIGKILL");
    }
  })().catch((error) => {
    console.error(error.message);
    process.exit(1);
  });
'

echo ">>> installing Playwright Chromium"
(cd "$react" && timeout 1200 npx playwright install chromium)
echo ">>> smoke-testing Playwright Chromium"
(cd "$react" && timeout 120 node -e '
  const { chromium } = require("playwright");
  (async () => {
    const browser = await chromium.launch({ headless: true });
    const page = await browser.newPage();
    await page.setContent("<title>URnetwork acceptance setup</title>");
    if (await page.title() !== "URnetwork acceptance setup") process.exitCode = 1;
    await browser.close();
  })().catch((error) => { console.error(error); process.exit(1); });
')

tls_dir="$temporary_root/tls"
mkdir -p "$tls_dir"
timeout 30 openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
  -keyout "$tls_dir/ur.io.key" -out "$tls_dir/ur.io.crt" \
  -subj /CN=ur.io -addext 'subjectAltName=DNS:ur.io,DNS:*.ur.io' >/dev/null 2>&1

if lsof -nP -iTCP:443 -sTCP:LISTEN >/dev/null 2>&1; then
  die "port 443 is already in use"
fi
echo ">>> smoke-testing the acceptance server bind on port 443"
timeout 15 node -e '
  const net = require("node:net");
  const timer = setTimeout(() => {
    console.error("port 443 bind did not complete");
    process.exit(1);
  }, 10_000);
  const server = net.createServer();
  server.once("error", (error) => {
    clearTimeout(timer);
    console.error(error.message);
    process.exit(1);
  });
  server.listen(443, "0.0.0.0", () => {
    server.close((error) => {
      clearTimeout(timer);
      if (error) throw error;
    });
  });
' || die "cannot bind the local acceptance server to port 443"
if [ "$no_sudo" != 1 ]; then
  command -v sudo >/dev/null 2>&1 || die "sudo is required without --no-sudo"
fi

echo ">>> SMOKE TEST PASSED — the ur.io browser environment is ready"
