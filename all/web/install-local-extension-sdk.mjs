#!/usr/bin/env node
// SPDX-License-Identifier: MPL-2.0
// Standalone acceptance uses the canonical checked sibling SDK artifact.
// Keep tracked manifests untouched; npm ci verifies a private, explicit local
// SDK substitution while every other locked package remains unchanged.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { isDeepStrictEqual } from "node:util";
import { fileURLToPath } from "node:url";

const SDK = "@urnetwork/sdk";
const ENTRY = `node_modules/${SDK}`;
const MARKER = ".urnetwork-local-sdk.json";
const hash = (value, algorithm = "sha256", encoding = "hex") => createHash(algorithm).update(value).digest(encoding);
const readJSON = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
const requireCondition = (ok, message) => { if (!ok) throw new Error(message); };
const privateJSON = (file, value) => fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });

function sourceIdentity(sdkDirectory) {
  const root = fs.realpathSync(path.dirname(sdkDirectory));
  const git = (...args) => execFileSync("git", ["-C", root, ...args], { encoding: "utf8", maxBuffer: 16 * 1024 * 1024 });
  requireCondition(fs.realpathSync(git("rev-parse", "--show-toplevel").trim()) === root, "SDK must be its own source checkout");
  const revision = git("rev-parse", "HEAD").trim();
  const files = [...new Set(git("ls-files", "--cached", "--others", "--exclude-standard", "-z").split("\0").filter(Boolean))].sort();
  const digest = createHash("sha256");
  for (const name of files) {
    digest.update(`${name}\0`);
    const file = path.join(root, name);
    if (!fs.existsSync(file)) { digest.update("deleted\0"); continue; }
    const stat = fs.lstatSync(file);
    requireCondition(!stat.isDirectory(), `SDK source contains an unsupported nested checkout: ${name}`);
    digest.update(`${stat.mode & 0o777}\0`);
    digest.update(stat.isSymbolicLink() ? fs.readlinkSync(file) : fs.readFileSync(file));
    digest.update("\0");
  }
  return { revision, sha256: digest.digest("hex") };
}

// npm bins normally use relative links inside node_modules. An install script
// must not leave an absolute link into the disposable staging directory.
function verifyRelocatable(directory, stage) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const file = path.join(directory, entry.name);
    if (entry.isSymbolicLink()) {
      const target = fs.readlinkSync(file);
      const resolved = fs.realpathSync(file); // fail closed for dangling links
      const inStage = resolved === stage || resolved.startsWith(`${stage}${path.sep}`);
      const inModules = resolved.startsWith(`${path.join(stage, "node_modules")}${path.sep}`);
      requireCondition(!(path.isAbsolute(target) && inStage) && (!inStage || inModules), `non-relocatable staging symlink: ${file}`);
    } else if (entry.isDirectory()) {
      verifyRelocatable(file, stage);
    }
  }
}

function install(extensionDirectory, sdkDirectory) {
  const extension = fs.realpathSync(extensionDirectory);
  const sdk = fs.realpathSync(sdkDirectory);
  const originalPackage = fs.readFileSync(path.join(extension, "package.json"));
  const originalLock = fs.readFileSync(path.join(extension, "package-lock.json"));
  const pkg = JSON.parse(originalPackage);
  const lock = JSON.parse(originalLock);
  requireCondition(lock.lockfileVersion === 3, "local SDK installation requires package-lock v3");
  const locked = lock.packages?.[ENTRY];
  const sourcePackage = readJSON(path.join(sdk, "package.json"));
  requireCondition(sourcePackage.name === SDK && sourcePackage.version === locked?.version, "SDK version does not match the exact extension lock");
  requireCondition(pkg.dependencies?.[SDK] === lock.packages?.[""]?.dependencies?.[SDK], "extension SDK manifest and lock disagree");
  requireCondition(typeof pkg.dependencies?.[SDK] === "string", "extension must directly depend on the SDK");
  for (const name of ["preinstall", "install", "postinstall", "prepublish", "preprepare", "prepare", "postprepare"]) {
    requireCondition(!pkg.scripts?.[name], `extension root lifecycle ${name} cannot run in a dependency-only staging tree`);
  }
  // Relative dependencies elsewhere would change meaning in a staging tree.
  for (const [name, entry] of Object.entries(lock.packages)) {
    if (name !== ENTRY) requireCondition(!entry.link && !entry.resolved?.startsWith("file:"), `other local dependency is not supported in staged install: ${name}`);
  }
  const manifestBytes = fs.readFileSync(path.join(sdk, "release/manifest.json"));
  const manifest = JSON.parse(manifestBytes);
  const checked = readJSON(path.join(sdk, "release/checked.json"));
  requireCondition(checked.manifest_sha256 === hash(manifestBytes), "SDK package checks do not attest the current manifest");
  requireCondition(manifest.version === locked.version, "SDK version in package inventory does not match the extension lock");
  const filename = `urnetwork-sdk-${locked.version}.tgz`;
  const artifacts = manifest.artifacts.filter((entry) => entry.name === filename);
  requireCondition(artifacts.length === 1, "canonical SDK artifact missing or duplicated in inventory");
  const tarball = path.join(sdk, "release/artifacts", filename);
  const tarballBytes = fs.readFileSync(tarball);
  requireCondition(tarballBytes.length === artifacts[0].size && hash(tarballBytes) === artifacts[0].sha256, "SDK artifact hash or size mismatch");
  const localPackage = JSON.parse(execFileSync("tar", ["-xOf", tarball, "package/package.json"], { encoding: "utf8" }));
  requireCondition(localPackage.name === SDK && localPackage.version === locked.version, "SDK version or package name inside artifact disagrees with lock");
  // A local source change may not silently change the dependency graph under
  // a hand-edited SDK lock entry. Such a change needs an ordinary lock update.
  for (const name of ["dependencies", "optionalDependencies", "peerDependencies", "peerDependenciesMeta", "engines", "os", "cpu", "bin"]) {
    requireCondition(isDeepStrictEqual(localPackage[name], locked[name]), `SDK ${name} differs from the extension lock; update its dependency contract explicitly`);
  }
  requireCondition(!localPackage.scripts, "canonical SDK artifact unexpectedly contains lifecycle scripts");
  const sdkIntegrity = `sha512-${hash(tarballBytes, "sha512", "base64")}`;
  const receipt = {
    schema: 1,
    installerSha256: hash(fs.readFileSync(fileURLToPath(import.meta.url))),
    packageSha256: hash(originalPackage), lockSha256: hash(originalLock),
    sdk: { version: locked.version, integrity: sdkIntegrity, manifestSha256: hash(manifestBytes), source: sourceIdentity(sdk) },
  };
  const modules = path.join(extension, "node_modules");
  const marker = path.join(modules, MARKER);
  if (fs.existsSync(marker) && isDeepStrictEqual(readJSON(marker), receipt) &&
      fs.existsSync(path.join(modules, SDK, "package.json"))) {
    console.log(">>> local SDK locked dependencies already installed in " + extension);
    return;
  }

  const stage = fs.mkdtempSync(path.join(extension, ".urnetwork-local-sdk-"));
  fs.chmodSync(stage, 0o700);
  const previous = path.join(stage, "previous-node_modules");
  let oldMoved = false;
  let installed = false;
  let preserveStage = false;
  try {
    // Snapshot the attested bytes so a concurrent repackage cannot change
    // what this npm ci installs after the hash was checked.
    const stagedTarball = path.join(stage, "sdk.tgz");
    fs.writeFileSync(stagedTarball, tarballBytes, { mode: 0o600 });
    const spec = "file:./sdk.tgz";
    pkg.dependencies[SDK] = spec;
    lock.packages[""].dependencies[SDK] = spec;
    locked.resolved = spec;
    locked.integrity = sdkIntegrity;
    privateJSON(path.join(stage, "package.json"), pkg);
    privateJSON(path.join(stage, "package-lock.json"), lock);
    const npmrc = path.join(extension, ".npmrc");
    if (fs.existsSync(npmrc)) fs.copyFileSync(npmrc, path.join(stage, ".npmrc"));
    console.log(">>> installing extension dependencies with checked local SDK " + sdkIntegrity);
    execFileSync("npm", ["ci", "--no-audit", "--no-fund"], { cwd: stage, stdio: "inherit" });
    requireCondition(fs.readFileSync(path.join(extension, "package.json")).equals(originalPackage) &&
      fs.readFileSync(path.join(extension, "package-lock.json")).equals(originalLock), "extension manifests changed during local SDK installation");
    requireCondition(isDeepStrictEqual(sourceIdentity(sdk), receipt.sdk.source), "SDK source changed during local SDK installation");
    const stagedModules = path.join(stage, "node_modules");
    verifyRelocatable(stagedModules, fs.realpathSync(stage));
    // Keep a private auditable staged lock in the installed tree; its SDK
    // integrity is explicit and every other entry is the original lock.
    privateJSON(path.join(stagedModules, ".urnetwork-local-sdk-lock.json"), lock);
    privateJSON(path.join(stagedModules, MARKER), receipt);
    if (fs.existsSync(modules)) { fs.renameSync(modules, previous); oldMoved = true; }
    fs.renameSync(stagedModules, modules);
    installed = true;
  } catch (error) {
    if (oldMoved && !installed) {
      try { fs.renameSync(previous, modules); }
      catch (restoreError) {
        preserveStage = true;
        throw new Error(`installation failed; previous dependencies preserved at ${previous}: ${restoreError.message}`, { cause: error });
      }
    }
    throw error;
  } finally {
    if (!preserveStage) fs.rmSync(stage, { recursive: true, force: true });
  }
}

try {
  requireCondition(process.argv.length === 4, "usage: install-local-extension-sdk.mjs EXTENSION SDK_JS");
  install(process.argv[2], process.argv[3]);
} catch (error) {
  console.error(`[local-extension-sdk] ${error.message}`);
  process.exitCode = 1;
}
