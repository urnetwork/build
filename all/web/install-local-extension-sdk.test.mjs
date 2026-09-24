// SPDX-License-Identifier: MPL-2.0
// Real npm, tiny local tarballs, and an SDK registry endpoint that always 404s.
// Browser provisioning stops at a sentinel; no browser or live workspace runs.
import assert from "node:assert/strict";
import { execFile as execFileCallback } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import test from "node:test";

const execFile = promisify(execFileCallback);
const here = path.dirname(fileURLToPath(import.meta.url));
const helper = path.join(here, "install-local-extension-sdk.mjs");
const sha256 = (data) => createHash("sha256").update(data).digest("hex");
const integrity = (data) => `sha512-${createHash("sha512").update(data).digest("base64")}`;
const json = (file, value) => fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
const script = (file, body) => fs.writeFileSync(file, `#!/bin/sh\nset -eu\n${body}\n`, { mode: 0o700 });

async function command(program, args, options = {}) {
  try {
    const result = await execFile(program, args, { timeout: 30_000, maxBuffer: 8 * 1024 * 1024, ...options });
    return { ...result, code: 0 };
  } catch (error) {
    return { code: error.code, stdout: error.stdout || "", stderr: error.stderr || "" };
  }
}

async function fixture(t, { brokenLink = false } = {}) {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), "ur-local-sdk-test-"));
  const root = path.join(temporary, "workspace with spaces");
  const extension = path.join(root, "extension");
  const sdk = path.join(root, "sdk/js");
  const bin = path.join(root, "bin");
  for (const dir of [extension, sdk, bin, path.join(root, "tests"), path.join(root, "mmm/ur.io/react"), path.join(root, "mmm/ur.io/astro"), path.join(root, "localizations")]) {
    fs.mkdirSync(dir, { recursive: true });
  }
  t.after(() => fs.rmSync(temporary, { recursive: true, force: true }));
  const env = { ...process.env, PATH: `${bin}${path.delimiter}${process.env.PATH}`, URNETWORK_ROOT: root,
    URNETWORK_NETWORK_TEST_LOCK_HELD: "1", UR_FIREFOX_BIN: path.join(bin, "firefox"),
    npm_config_cache: path.join(temporary, "cache"), npm_config_userconfig: path.join(temporary, "npmrc"),
    npm_config_fetch_retries: "0" };
  fs.writeFileSync(env.npm_config_userconfig, "");
  script(path.join(root, "tests/network-intensive-suite-lock.sh"), 'test "$1" = --verify-held');
  script(path.join(bin, "firefox"), 'echo DEPENDENCIES_COMPLETE\nexit 86');
  script(path.join(bin, "make"), 'printf "make:%s\\n" "$*" >> "$URNETWORK_ROOT/events"\nexit "${FIXTURE_BUILD_FAILURE:-0}"');
  const npm = (await execFile("sh", ["-c", "command -v npm"])).stdout.trim();
  script(path.join(bin, "npm"), `printf 'npm:%s\\n' "$PWD" >> "$URNETWORK_ROOT/events"\nexec '${npm.replaceAll("'", "'\\''")}' "$@"`);

  const tar = async (name, packageJson, files) => {
    const directory = path.join(temporary, name);
    fs.mkdirSync(path.join(directory, "package"), { recursive: true });
    json(path.join(directory, "package/package.json"), packageJson);
    for (const [name, content] of Object.entries(files)) {
      const dest = path.join(directory, "package", name);
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      fs.writeFileSync(dest, content, { mode: name === "bin.js" ? 0o755 : 0o644 });
    }
    const file = path.join(temporary, `${name}.tgz`);
    await execFile("tar", ["-czf", file, "-C", directory, "package"]);
    return { file, bytes: fs.readFileSync(file) };
  };
  const sdkPackage = { name: "@urnetwork/sdk", version: "0.0.1-beta.7", type: "module", main: "./dist/index.js" };
  json(path.join(sdk, "package.json"), sdkPackage);
  json(path.join(sdk, "package-lock.json"), { name: sdkPackage.name, version: sdkPackage.version, lockfileVersion: 3, packages: { "": sdkPackage } });
  fs.writeFileSync(path.join(sdk, "source.ts"), "export const source = 1;\n");
  fs.writeFileSync(path.join(root, "sdk/.gitignore"), "js/release/\n");
  await execFile("git", ["init", "-q"], { cwd: path.join(root, "sdk") });
  await execFile("git", ["add", "."], { cwd: path.join(root, "sdk") });
  await execFile("git", ["-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], { cwd: path.join(root, "sdk") });
  const local = await tar("sdk", sdkPackage, {
    "dist/index.js": 'export const source = "canonical-local-sdk";\n',
    "dist/index.d.ts": "export declare const source: string;\n",
    "wasm/sdk.wasm": Buffer.from([0, 97, 115, 109, 1, 0, 0, 0]),
    "wasm/wasm_exec.js": "// fixture toolchain glue\n",
  });
  const artifacts = path.join(sdk, "release/artifacts");
  fs.mkdirSync(artifacts, { recursive: true });
  const name = "urnetwork-sdk-0.0.1-beta.7.tgz";
  fs.copyFileSync(local.file, path.join(artifacts, name));
  json(path.join(sdk, "release/manifest.json"), { version: sdkPackage.version, artifacts: [{ name, size: local.bytes.length, sha256: sha256(local.bytes) }] });
  json(path.join(sdk, "release/checked.json"), { manifest_sha256: sha256(fs.readFileSync(path.join(sdk, "release/manifest.json"))) });

  const normal = await tar("normal", { name: "fixture-normal", version: "1.0.0", bin: { "fixture-normal": "bin.js" }, scripts: { postinstall: "node install.js" } }, {
    "bin.js": '#!/usr/bin/env node\nconsole.log(require("node:fs").readFileSync(require("node:path").join(__dirname,"built.txt"),"utf8"));\n',
    "install.js": 'require("node:fs").writeFileSync("built.txt", "script-and-bin-relocated");\n' + (brokenLink ? 'require("node:fs").symlinkSync(process.cwd(), "absolute-install-path");\n' : ""),
  });
  const requests = [];
  const server = http.createServer((request, response) => {
    requests.push(request.url);
    if (request.url === "/normal.tgz") {
      response.writeHead(200, { "Content-Type": "application/octet-stream" });
      response.end(normal.bytes);
    } else {
      response.writeHead(404, { "Content-Type": "application/json" });
      response.end('{"error":"unpublished SDK fixture"}');
    }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const registry = `http://127.0.0.1:${server.address().port}`;
  env.npm_config_registry = registry;
  const pkg = { name: "fixture-extension", version: "1.0.0", private: true, allowScripts: { "fixture-normal": true },
    dependencies: { "@urnetwork/sdk": "^0.0.1-beta.7", "fixture-normal": "1.0.0" } };
  const lock = { name: pkg.name, version: pkg.version, lockfileVersion: 3, requires: true, packages: {
    "": { name: pkg.name, version: pkg.version, dependencies: pkg.dependencies },
    "node_modules/@urnetwork/sdk": { version: sdkPackage.version, resolved: `${registry}/sdk-404.tgz`, integrity: integrity(Buffer.from("unavailable registry bytes")) },
    "node_modules/fixture-normal": { version: "1.0.0", resolved: `${registry}/normal.tgz`, integrity: integrity(normal.bytes), hasInstallScript: true, bin: { "fixture-normal": "bin.js" } },
  } };
  json(path.join(extension, "package.json"), pkg);
  json(path.join(extension, "package-lock.json"), lock);
  for (const relative of ["mmm/ur.io/react", "mmm/ur.io/astro", "localizations"]) {
    const name = relative.split("/").at(-1);
    const dependencies = { "fixture-normal": "1.0.0" };
    json(path.join(root, relative, "package.json"), { name, version: "1.0.0", private: true, dependencies, allowScripts: { "fixture-normal": true } });
    json(path.join(root, relative, "package-lock.json"), { name, version: "1.0.0", lockfileVersion: 3, packages: {
      "": { name, version: "1.0.0", dependencies },
      "node_modules/fixture-normal": lock.packages["node_modules/fixture-normal"],
    } });
  }
  const originals = ["package.json", "package-lock.json"].map((name) => fs.readFileSync(path.join(extension, name), "utf8"));
  return { root, extension, sdk, env, lock, requests, local, artifacts, originals,
    repackage: async () => {
      const rebuilt = await tar("sdk-rebuilt", sdkPackage, { "dist/index.js": 'export const source = "rebuilt-local-sdk";\n' });
      fs.copyFileSync(rebuilt.file, path.join(artifacts, name));
      json(path.join(sdk, "release/manifest.json"), { version: sdkPackage.version, artifacts: [{ name, size: rebuilt.bytes.length, sha256: sha256(rebuilt.bytes) }] });
      json(path.join(sdk, "release/checked.json"), { manifest_sha256: sha256(fs.readFileSync(path.join(sdk, "release/manifest.json"))) });
      return rebuilt;
    },
    setup: (extra = {}) => command("bash", [path.join(here, "setup.sh"), "--no-sudo"], { env: { ...env, ...extra } }),
    install: () => command(process.execPath, [helper, extension, sdk], { env }),
    unchanged: () => ["package.json", "package-lock.json"].forEach((name, index) => assert.equal(fs.readFileSync(path.join(extension, name), "utf8"), originals[index])),
  };
}

test("setup installs local SDK with real npm ci when registry SDK is 404, without weakening extension coverage", async (t) => {
  const f = await fixture(t);
  const result = await f.setup();
  assert.equal(result.code, 86, `${result.stdout}${result.stderr}`);
  assert.match(result.stdout, /DEPENDENCIES_COMPLETE/);
  f.unchanged();
  assert.equal(f.requests.some((url) => url.includes("sdk-404")), false, "local setup still fetched registry SDK");
  assert.equal(fs.readFileSync(path.join(f.extension, "node_modules/@urnetwork/sdk/dist/index.js"), "utf8"), 'export const source = "canonical-local-sdk";\n');
  const bin = await command(path.join(f.extension, "node_modules/.bin/fixture-normal"), [], { env: f.env });
  assert.equal(bin.code, 0, bin.stderr);
  assert.match(bin.stdout, /script-and-bin-relocated/);
  const markerPath = path.join(f.extension, "node_modules/.urnetwork-local-sdk.json");
  const first = JSON.parse(fs.readFileSync(markerPath, "utf8"));
  assert.equal(first.sdk.integrity, integrity(f.local.bytes));
  assert.equal(first.sdk.version, "0.0.1-beta.7");
  assert.match(first.sdk.source.revision, /^[a-f0-9]{40}$/);
  assert.equal(fs.statSync(markerPath).mode & 0o777, 0o600);
  const auditedLockPath = path.join(f.extension, "node_modules/.urnetwork-local-sdk-lock.json");
  assert.equal(fs.statSync(auditedLockPath).mode & 0o777, 0o600);
  const auditedLock = JSON.parse(fs.readFileSync(auditedLockPath));
  for (const [name, entry] of Object.entries(f.lock.packages)) {
    if (name !== "" && name !== "node_modules/@urnetwork/sdk") assert.deepEqual(auditedLock.packages[name], entry);
  }
  fs.writeFileSync(path.join(f.extension, "node_modules/reuse-sentinel"), "old");
  assert.equal((await f.setup()).code, 86);
  assert.ok(fs.existsSync(path.join(f.extension, "node_modules/reuse-sentinel")), "identical locked install was not reused");
  fs.appendFileSync(path.join(f.sdk, "source.ts"), "// changed source, same fixture tarball\n");
  assert.equal((await f.setup()).code, 86);
  assert.equal(fs.existsSync(path.join(f.extension, "node_modules/reuse-sentinel")), false, "SDK source change did not invalidate marker");
  const second = JSON.parse(fs.readFileSync(markerPath, "utf8"));
  assert.notEqual(second.sdk.source.sha256, first.sdk.source.sha256);
  await f.repackage();
  assert.equal((await f.setup()).code, 86);
  const third = JSON.parse(fs.readFileSync(markerPath, "utf8"));
  assert.notEqual(third.sdk.integrity, second.sdk.integrity, "rebuilt SDK did not invalidate marker");
  assert.deepEqual(third.sdk.source, second.sdk.source, "fixture changed source instead of only the build artifact");
  assert.equal((await f.setup({ FIXTURE_BUILD_FAILURE: "37" })).code, 37, "failed SDK build silently reused old install");
  f.unchanged();
});

test("local package attestation rejects a tampered SDK tarball before installation", async (t) => {
  const f = await fixture(t);
  fs.appendFileSync(path.join(f.artifacts, "urnetwork-sdk-0.0.1-beta.7.tgz"), "tampered");
  const result = await f.install();
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /SDK artifact.*(?:hash|size|integrity)/i);
  assert.equal(f.requests.length, 0);
  f.unchanged();
});

test("local SDK version must agree with the exact extension lock", async (t) => {
  const f = await fixture(t);
  const pkg = JSON.parse(fs.readFileSync(path.join(f.sdk, "package.json")));
  pkg.version = "0.0.1-beta.8";
  json(path.join(f.sdk, "package.json"), pkg);
  const result = await f.install();
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /SDK version/i);
  assert.equal(f.requests.length, 0);
});

test("npm ci still rejects another package's bad integrity and preserves the previous node_modules", async (t) => {
  const f = await fixture(t);
  f.lock.packages["node_modules/fixture-normal"].integrity = integrity(Buffer.from("wrong normal bytes"));
  json(path.join(f.extension, "package-lock.json"), f.lock);
  fs.mkdirSync(path.join(f.extension, "node_modules"));
  fs.writeFileSync(path.join(f.extension, "node_modules/previous-install"), "preserved");
  const result = await f.install();
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /EINTEGRITY/);
  assert.equal(fs.readFileSync(path.join(f.extension, "node_modules/previous-install"), "utf8"), "preserved");
  assert.equal(fs.existsSync(path.join(f.extension, "node_modules/.urnetwork-local-sdk.json")), false);
});

test("installation refuses a script-created absolute staging symlink that relocation would break", async (t) => {
  const f = await fixture(t, { brokenLink: true });
  const result = await f.install();
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /non-relocatable|staging symlink/i);
  assert.equal(fs.existsSync(path.join(f.extension, "node_modules")), false);
  f.unchanged();
});

test("SDK dependency metadata cannot silently expand the original locked graph", async (t) => {
  const f = await fixture(t);
  f.lock.packages["node_modules/@urnetwork/sdk"].dependencies = { unexpected: "1.0.0" };
  json(path.join(f.extension, "package-lock.json"), f.lock);
  const result = await f.install();
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /SDK dependencies differs/);
  assert.equal(f.requests.length, 0);
});

test("an unchecked canonical SDK artifact cannot be installed", async (t) => {
  const f = await fixture(t);
  json(path.join(f.sdk, "release/checked.json"), { manifest_sha256: "stale-check" });
  const result = await f.install();
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /SDK package checks do not attest/);
  assert.equal(f.requests.length, 0);
});
