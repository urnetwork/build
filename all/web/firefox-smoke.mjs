// SPDX-License-Identifier: MPL-2.0
import { execFile, spawn } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs";
import net from "node:net";
import { pathToFileURL } from "node:url";
import { promisify } from "node:util";

async function freePort() {
  const socket = net.createServer();
  socket.listen(0, "127.0.0.1");
  await once(socket, "listening");
  const port = socket.address().port;
  await new Promise((resolve) => socket.close(resolve));
  return port;
}

const defaultPlatform = {
  freePort,
  spawn,
  fetch: globalThis.fetch,
  stat: (file) => fs.statSync(file),
  now: () => Date.now(),
  wait: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  flushOutput: () => new Promise((resolve) => setImmediate(resolve)),
  readProcessTable: async () => {
    const { stdout } = await promisify(execFile)("ps", ["-axo", "pid=,ppid=,command="], {
      timeout: 1_000,
      maxBuffer: 8 * 1024 * 1024,
    });
    return stdout;
  },
  signalProcess: (pid, signal) => process.kill(pid, signal),
};

// I/O injection is only for the offline regression. The CLI always uses real
// WebDriver and the same exact-profile owner as the full acceptance runner.
export async function smokeFirefox({ firefoxBin, geckodriverBin, processOwner }, platform = {}) {
  const io = { ...defaultPlatform, ...platform };
  for (const name of [
    "firefoxBinaryIdentity", "firefoxBinaryWasReplaced", "firefoxProfileFromGeckodriverOutput",
    "findAcceptanceFirefox", "terminateAcceptanceFirefox",
  ]) {
    if (typeof processOwner?.[name] !== "function") {
      throw new Error(`Firefox process owner is missing ${name}; update the sibling site checkout`);
    }
  }
  const {
    firefoxBinaryIdentity,
    firefoxBinaryWasReplaced,
    firefoxProfileFromGeckodriverOutput,
    findAcceptanceFirefox,
    terminateAcceptanceFirefox,
  } = processOwner;
  const binaryBefore = firefoxBinaryIdentity(io.stat(firefoxBin));
  const binaryWasReplaced = () => {
    let after = "missing";
    try { after = firefoxBinaryIdentity(io.stat(firefoxBin)); }
    catch { /* A replaced bundle can temporarily disappear. */ }
    return firefoxBinaryWasReplaced(binaryBefore, after);
  };
  const port = await io.freePort();
  const base = `http://127.0.0.1:${port}`;
  const driver = io.spawn(geckodriverBin, ["--port", String(port)], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  let diagnostics = "";
  let profile = "";
  let driverError = null;
  let driverStopped = false;
  driver.once("exit", () => { driverStopped = true; });
  driver.on("error", (error) => {
    driverError = error;
    // A failed spawn has no process to reap and emits no exit event.
    if (!driver.pid) driverStopped = true;
  });
  for (const stream of [driver.stdout, driver.stderr]) {
    stream.on("data", (chunk) => {
      diagnostics = `${diagnostics}${chunk}`.slice(-16 * 1024);
      profile = firefoxProfileFromGeckodriverOutput(diagnostics) || profile;
    });
  }

  const command = async (route, options = {}, timeoutMs = 15_000) => {
    const response = await io.fetch(`${base}${route}`, {
      ...options,
      headers: { "content-type": "application/json", ...(options.headers || {}) },
      signal: AbortSignal.timeout(timeoutMs),
    });
    const result = await response.json().catch(() => ({}));
    if (!response.ok || result.value?.error) {
      throw new Error(result.value?.message || `WebDriver HTTP ${response.status}`);
    }
    return result.value;
  };
  let session = null;
  let sessionId = "";
  let sessionCreationStarted = false;
  let failure = null;
  try {
    const deadline = io.now() + 15_000;
    let ready = false;
    while (io.now() < deadline) {
      if (driverError) throw driverError;
      if (driverStopped || driver.exitCode !== null) {
        throw new Error(`geckodriver exited ${driver.exitCode ?? driver.signalCode}`);
      }
      try {
        const status = await command("/status", {}, Math.min(1_000, deadline - io.now()));
        if (status?.ready === true) { ready = true; break; }
      } catch { /* The loopback listener can lag behind spawn. */ }
      await io.wait(250);
    }
    if (!ready) throw new Error("geckodriver did not become ready");
    sessionCreationStarted = true;
    session = await command("/session", {
      method: "POST",
      body: JSON.stringify({
        capabilities: {
          alwaysMatch: {
            acceptInsecureCerts: true,
            "moz:firefoxOptions": { binary: firefoxBin, args: ["-headless"] },
          },
        },
      }),
    });
    sessionId = session?.sessionId || "";
    if (!sessionId) throw new Error("Firefox WebDriver returned no session ID");
  } catch (error) {
    failure = error;
  }

  const cleanupErrors = [];
  try {
    // HTTP failure and pipe data are independent event-loop sources. Drain
    // already-written output before deciding whether the profile is known.
    await io.flushOutput();
    if (sessionCreationStarted && !profile) {
      const owned = findAcceptanceFirefox(await io.readProcessTable(), driver.pid, firefoxBin);
      profile = owned?.profile || "";
    }
    if (sessionCreationStarted && !profile) {
      throw new Error("Firefox startup cleanup could not identify its temporary profile");
    }
  } catch (error) {
    cleanupErrors.push(error);
  }
  // Discover ownership before DELETE or stopping geckodriver can reparent
  // Firefox. A successful DELETE alone is not proof that the tree is gone.
  if (sessionId) {
    try { await command(`/session/${sessionId}`, { method: "DELETE" }); }
    catch (error) { cleanupErrors.push(error); }
  }
  let replaced = binaryWasReplaced();
  try {
    if (!driverStopped) driver.kill("SIGTERM");
    if (profile) {
      const cleanupDeadline = io.now() + 35_000;
      const cleanup = await terminateAcceptanceFirefox({
        profile,
        readProcessTable: () => {
          // Bound the aggregate too: many individually bounded slow ps calls
          // must not run into setup's outer 120-second termination deadline.
          if (io.now() >= cleanupDeadline) throw new Error("Firefox profile cleanup exceeded 35 seconds");
          return io.readProcessTable();
        },
        signalProcess: io.signalProcess,
        wait: io.wait,
        // A preserved updater failure relaunched Firefox five seconds later.
        // Keep that exact token live for the runner's bounded 15-second drain.
        lateRegistrationChecks: failure || replaced ? 30 : 0,
        graceIntervalMs: failure || replaced ? 500 : 100,
      });
      if (cleanup.remainingProcessIds.length) {
        throw new Error(`Firefox cleanup left ${cleanup.remainingProcessIds.length} process(es) with its acceptance profile`);
      }
    }
  } catch (error) {
    cleanupErrors.push(error);
  } finally {
    // Reap only the driver we spawned, including when profile discovery failed.
    for (const signal of ["SIGTERM", "SIGKILL"]) {
      if (driverStopped) break;
      driver.kill(signal);
      const deadline = io.now() + 2_000;
      while (!driverStopped && io.now() < deadline) await io.wait(50);
    }
    if (!driverStopped) cleanupErrors.push(new Error("geckodriver did not stop"));
  }

  replaced = binaryWasReplaced() || replaced;
  const messages = [];
  if (replaced) {
    messages.push("Firefox binary changed during WebDriver smoke (an application update replaced the tested browser); the run was not retried");
  }
  if (failure) messages.push(failure.message);
  for (const error of cleanupErrors) messages.push(`Firefox cleanup failed: ${error.message}`);
  if (messages.length) throw new Error(`${messages.join("; ")}${diagnostics ? `\n${diagnostics}` : ""}`);
  return { browserVersion: session.capabilities?.browserVersion || "unknown", profile };
}

if (process.argv[1] && pathToFileURL(fs.realpathSync(process.argv[1])).href === import.meta.url) {
  try {
    const [firefoxBin, geckodriverBin, processOwnerFile, ...extra] = process.argv.slice(2);
    if (!firefoxBin || !geckodriverBin || !processOwnerFile || extra.length) {
      throw new Error("usage: firefox-smoke.mjs FIREFOX_BIN GECKODRIVER_BIN FIREFOX_PROCESS_MODULE");
    }
    const processOwner = await import(pathToFileURL(processOwnerFile).href);
    const result = await smokeFirefox({ firefoxBin, geckodriverBin, processOwner });
    console.log(`Firefox WebDriver smoke passed (${result.browserVersion}); owned profile cleanup complete`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
