// SPDX-License-Identifier: MPL-2.0
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { EventEmitter, once } from "node:events";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

import * as processOwner from "./testdata/firefox-process.mjs";

const FIREFOX = "/Applications/Firefox.app/Contents/MacOS/firefox";
const PROFILE = "/tmp/rust_mozprofileSetupSmoke";
const ORIGINAL_FAILURE = "Process (pid=451) unexpectedly closed with status 0";
const baseline = process.env.UR_FIREFOX_SMOKE_BASELINE_COMMIT;
const setupSource = baseline
  ? execFileSync("git", ["show", `${baseline}:all/web/setup.sh`], {
    cwd: new URL("../..", import.meta.url), encoding: "utf8",
  })
  : fs.readFileSync(new URL("./setup.sh", import.meta.url), "utf8");

// Before the fix, execute the exact inline setup source with virtual I/O.
// After extraction, exercise the wired production module with the same I/O.
// Neither path starts a browser, reads the host process table, or sends signals.
async function runSetupSmoke(fixture) {
  const inline = setupSource.match(/timeout 120 node -e '([\s\S]*?)\n'\n\necho ">>> installing locked Playwright/);
  if (inline) {
    let reportedError = "";
    const sandbox = {
      require: (name) => {
        if (name === "node:events") return { once };
        if (name === "node:child_process") return { spawn: fixture.platform.spawn };
        if (name === "node:net") return {
          createServer: () => {
            const socket = new EventEmitter();
            socket.listen = () => queueMicrotask(() => socket.emit("listening"));
            socket.address = () => ({ port: 4444 });
            socket.close = (callback) => callback();
            return socket;
          },
        };
        throw new Error(`unexpected require ${name}`);
      },
      fetch: fixture.platform.fetch,
      AbortSignal,
      Date: { now: fixture.platform.now },
      setTimeout: (callback, ms) => queueMicrotask(() => {
        fixture.advance(ms);
        callback();
      }),
      console: { error: (message) => { reportedError = String(message); } },
      process: {
        env: { UR_SETUP_FIREFOX_BIN: FIREFOX, UR_SETUP_GECKODRIVER_BIN: "/test/geckodriver" },
        exit: () => {},
      },
    };
    await vm.runInNewContext(inline[1], sandbox, { filename: "setup-inline-firefox-smoke.js" });
    return reportedError ? new Error(reportedError) : null;
  }
  assert.match(setupSource, /timeout 120 node "\$here\/firefox-smoke\.mjs"/);
  assert.match(setupSource, /"\$react\/tests\/firefox-process\.mjs"/);
  const { smokeFirefox } = await import("./firefox-smoke.mjs");
  try {
    await smokeFirefox({
      firefoxBin: FIREFOX,
      geckodriverBin: "/test/geckodriver",
      processOwner,
    }, fixture.platform);
    return null;
  } catch (error) {
    return error;
  }
}

function virtualUpdater({
  succeeds = false,
  replacesBinary = !succeeds,
  lateRelaunch = !succeeds,
  output = "immediate",
  ownedAtFailure = false,
  resistsSignals = false,
  missingSessionId = false,
  deleteFails = false,
  spawnFails = false,
  neverReady = false,
} = {}) {
  let time = 0;
  let changed = false;
  let sessionStarted = false;
  let lateRegistered = false;
  let pendingOutput = "";
  const calls = [];
  const signals = [];
  const driverSignals = [];
  const live = new Map([
    [100, { parent: 1, command: `${FIREFOX} -profile /Users/test/default-release` }],
    [101, { parent: 1, command: `${FIREFOX} --marionette -profile ${PROFILE}-other` }],
  ]);
  const addOwned = (parent = 1) => {
    live.set(546, { parent, command: `${FIREFOX} --marionette -headless -profile ${PROFILE}` });
    live.set(547, { parent: 546, command: `/Applications/Firefox.app/Contents/MacOS/plugin-container -profile ${PROFILE}` });
  };
  const advance = (ms) => {
    time += ms;
    if (lateRelaunch && sessionStarted && !lateRegistered && time >= 5_000) {
      lateRegistered = true;
      addOwned();
    }
  };
  const driver = new EventEmitter();
  driver.pid = 400;
  driver.exitCode = null;
  driver.signalCode = null;
  driver.stdout = new EventEmitter();
  driver.stderr = new EventEmitter();
  driver.kill = (signal) => {
    driverSignals.push(signal);
    for (const row of live.values()) if (row.parent === driver.pid) row.parent = 1;
    if (driver.exitCode === null) {
      driver.exitCode = 0;
      driver.signalCode = signal;
      queueMicrotask(() => driver.emit("exit", 0, signal));
    }
    return true;
  };
  const emitOutput = (message) => {
    // Profile tokens may be split across pipe chunks.
    driver.stderr.emit("data", message.slice(0, message.length - 9));
    driver.stderr.emit("data", message.slice(-9));
  };
  const response = (value, ok = true) => ({ ok, status: ok ? 200 : 500, json: async () => ({ value }) });
  const platform = {
    now: () => time,
    wait: async (ms) => advance(ms),
    freePort: async () => 4444,
    stat: () => ({ dev: 1, ino: changed ? 21 : 20, size: 174592, mtimeMs: 40 }),
    spawn: () => {
      if (spawnFails) {
        driver.pid = undefined;
        queueMicrotask(() => driver.emit("error", new Error("spawn /test/geckodriver ENOENT")));
      }
      return driver;
    },
    flushOutput: async () => {
      if (pendingOutput) emitOutput(pendingOutput);
      pendingOutput = "";
    },
    readProcessTable: async () => {
      calls.push("process-table");
      return [...live].map(([pid, row]) => `${pid} ${row.parent} ${row.command}`).join("\n");
    },
    signalProcess: (pid, signal) => {
      signals.push([pid, signal]);
      if (!resistsSignals) live.delete(pid);
    },
    fetch: async (url, options = {}) => {
      const route = new URL(url).pathname;
      const method = options.method || "GET";
      calls.push(`${method} ${route}`);
      if (method === "GET" && route === "/status") {
        if (spawnFails) throw new Error("connection refused");
        return response({ ready: !neverReady });
      }
      if (method === "POST" && route === "/session") {
        const capabilities = JSON.parse(options.body).capabilities.alwaysMatch;
        assert.equal(capabilities["moz:firefoxOptions"].binary, FIREFOX);
        assert.deepEqual(capabilities["moz:firefoxOptions"].args, ["-headless"]);
        sessionStarted = true;
        changed = replacesBinary;
        const command = `mozrunner::runner INFO Running command: "${FIREFOX}" "--marionette" "-headless" "-profile" "${PROFILE}"\n`;
        if (output === "immediate") emitOutput(command);
        if (output === "delayed") pendingOutput = command;
        if (succeeds || ownedAtFailure) addOwned(driver.pid);
        if (!succeeds) return response({ error: "unknown error", message: ORIGINAL_FAILURE }, false);
        return response({ sessionId: missingSessionId ? "" : "session-1", capabilities: { browserName: "firefox", browserVersion: "154.0.1" } });
      }
      if (method === "DELETE" && route === "/session/session-1") {
        if (deleteFails) return response({ error: "unknown error", message: "delete failed" }, false);
        // Deliberately leave a reparented browser tree: the exact owner must
        // inventory after DELETE instead of treating HTTP success as cleanup.
        for (const row of live.values()) if (row.parent === driver.pid) row.parent = 1;
        return response(null);
      }
      throw new Error(`unexpected WebDriver command ${method} ${route}`);
    },
  };
  return { platform, advance, calls, signals, driverSignals, live, elapsed: () => time };
}

test("updater status-0 failure drains its late profile, reports replacement, and never retries", async () => {
  const fixture = virtualUpdater();
  const error = await runSetupSmoke(fixture);
  const cleanupDuration = fixture.elapsed();
  fixture.advance(20_000);
  assert.deepEqual({
    classified: /Firefox binary changed.*not retried/s.test(error?.message || ""),
    preservedFailure: error?.message.includes(ORIGINAL_FAILURE),
    postCount: fixture.calls.filter((call) => call === "POST /session").length,
    lateDrainCompleted: cleanupDuration >= 15_000,
    ownedRemaining: [...fixture.live.keys()].filter((pid) => pid >= 500),
    unrelatedRemaining: [...fixture.live.keys()].filter((pid) => pid < 500),
    signals: fixture.signals,
  }, {
    classified: true,
    preservedFailure: true,
    postCount: 1,
    lateDrainCompleted: true,
    ownedRemaining: [],
    unrelatedRemaining: [100, 101],
    signals: [[546, "SIGTERM"], [547, "SIGTERM"]],
  }, error?.message);
});

test("drains already-written profile output after the failed session response", async () => {
  const fixture = virtualUpdater({ output: "delayed" });
  const error = await runSetupSmoke(fixture);
  fixture.advance(20_000);
  assert.match(error?.message || "", /Firefox binary changed/);
  assert.deepEqual([...fixture.live.keys()], [100, 101]);
});

test("unchanged Firefox startup error remains an error and uses direct-child ownership before driver exit", async () => {
  const fixture = virtualUpdater({ replacesBinary: false, lateRelaunch: false, output: "none", ownedAtFailure: true });
  const error = await runSetupSmoke(fixture);
  assert.match(error?.message || "", /unexpectedly closed with status 0/);
  assert.doesNotMatch(error?.message || "", /binary changed/);
  assert.deepEqual([...fixture.live.keys()], [100, 101]);
  assert.deepEqual(fixture.signals, [[546, "SIGTERM"], [547, "SIGTERM"]]);
});

test("healthy Firefox still needs a session and successful exact-profile cleanup", async () => {
  for (const output of ["immediate", "none"]) {
    const fixture = virtualUpdater({ succeeds: true, output });
    const error = await runSetupSmoke(fixture);
    assert.equal(error, null);
    assert.equal(fixture.calls.filter((call) => call === "POST /session").length, 1);
    assert.ok(fixture.calls.includes("DELETE /session/session-1"));
    assert.deepEqual([...fixture.live.keys()], [100, 101]);
    assert.ok(fixture.elapsed() < 15_000, "healthy startup acquired the updater-only wait");
  }
});

test("unidentified profile and surviving exact-profile processes fail closed", async () => {
  const unknown = virtualUpdater({ lateRelaunch: false, replacesBinary: false, output: "none" });
  assert.match((await runSetupSmoke(unknown))?.message || "", /could not identify.*temporary profile/);
  assert.deepEqual(unknown.signals, []);
  const stuck = virtualUpdater({ succeeds: true, resistsSignals: true });
  assert.match((await runSetupSmoke(stuck))?.message || "", /cleanup left 2 process/);
  assert.deepEqual(stuck.signals, [[546, "SIGTERM"], [547, "SIGTERM"], [546, "SIGKILL"], [547, "SIGKILL"]]);
  assert.ok(stuck.live.has(100) && stuck.live.has(101));
});

test("a binary replacement cannot pass just because WebDriver returned a session", async () => {
  const fixture = virtualUpdater({ succeeds: true, replacesBinary: true, lateRelaunch: true });
  assert.match((await runSetupSmoke(fixture))?.message || "", /Firefox binary changed.*not retried/s);
  fixture.advance(20_000);
  assert.deepEqual([...fixture.live.keys()], [100, 101]);
});

test("missing session and failed session deletion do not weaken the browser gate", async () => {
  const missing = virtualUpdater({ succeeds: true, missingSessionId: true });
  assert.match((await runSetupSmoke(missing))?.message || "", /no session ID/);
  assert.deepEqual([...missing.live.keys()], [100, 101]);
  const failedDelete = virtualUpdater({ succeeds: true, deleteFails: true });
  assert.match((await runSetupSmoke(failedDelete))?.message || "", /delete failed/);
  assert.deepEqual([...failedDelete.live.keys()], [100, 101]);
});

test("a missing or unready geckodriver cannot launch a session or bypass failure", async (t) => {
  // The original inline source has no ChildProcess error listener; do not
  // emit an uncaught process event into the test runner in baseline mode.
  if (!setupSource.includes('node "$here/firefox-smoke.mjs"')) {
    t.skip("original inline smoke has no spawn-error listener; its lifecycle regressions are tested above");
    return;
  }
  for (const options of [{ spawnFails: true }, { neverReady: true }]) {
    const fixture = virtualUpdater(options);
    const error = await runSetupSmoke(fixture);
    assert.match(error?.message || "", /ENOENT|did not become ready/);
    assert.ok(!fixture.calls.includes("POST /session"));
    assert.deepEqual(fixture.signals, []);
  }
});

test("process inventory errors stop cleanup explicitly without signalling unrelated browsers", async () => {
  const fixture = virtualUpdater({ output: "none", lateRelaunch: false, replacesBinary: false, ownedAtFailure: true });
  fixture.platform.readProcessTable = async () => { throw new Error("process inventory timed out"); };
  const error = await runSetupSmoke(fixture);
  assert.match(error?.message || "", /cleanup failed: process inventory timed out/);
  assert.deepEqual(fixture.signals, []);
  assert.ok(fixture.driverSignals.includes("SIGTERM"));
});
