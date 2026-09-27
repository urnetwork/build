// SPDX-License-Identifier: MPL-2.0
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  cleanupClientFile,
  cleanupClientFiles,
  cleanupFailureReport,
  readCredentials,
  releaseClient,
  runCleanupCLI,
} from "./client-cleanup.mjs";

function response(body, status = 200, headers = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: name => headers[name.toLowerCase()] ?? null },
    json: async () => body,
  };
}

test("releases the exact retained client with a fresh network session", async () => {
  const requests = [];
  await releaseClient({
    clientId: "client-1",
    user: "acceptance@example.com",
    password: "private",
    fetchImpl: async (url, options) => {
      requests.push({ url, options });
      return requests.length === 1
        ? response({ network: { by_jwt: "network-jwt" } })
        : response({});
    },
  });

  assert.equal(requests.length, 2);
  assert.equal(requests[0].url, "https://api.bringyour.com/auth/login-with-password");
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    user_auth: "acceptance@example.com",
    password: "private",
  });
  assert.equal(requests[1].url, "https://api.bringyour.com/network/remove-client");
  assert.equal(requests[1].options.headers.Authorization, "Bearer network-jwt");
  assert.deepEqual(JSON.parse(requests[1].options.body), { client_id: "client-1" });
});

test("removes the retained file only after cleanup succeeds", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-client-cleanup-"));
  const file = path.join(directory, "active-client-id");
  fs.writeFileSync(file, "client-2\n", { mode: 0o600 });
  const environment = { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" };

  await assert.rejects(
    cleanupClientFile(file, environment, async () => response({}, 500)),
    /HTTP 500/,
  );
  assert.equal(fs.readFileSync(file, "utf8"), "client-2\n");

  let request = 0;
  await cleanupClientFile(file, environment, async () => {
    request += 1;
    return request === 1 ? response({ network: { by_jwt: "jwt" } }) : response({});
  });
  assert.equal(fs.existsSync(file), false);
  fs.rmSync(directory, { recursive: true });
});

test("reads the native runners' private two-line credentials file", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-client-credentials-"));
  const file = path.join(directory, "credentials");
  fs.writeFileSync(file, "user\npass\n", { mode: 0o600 });
  assert.deepEqual(readCredentials({ UR_ACCEPT_CREDENTIALS_FILE: file }), {
    user: "user",
    password: "pass",
  });
  fs.rmSync(directory, { recursive: true });
});

test("releases duplicate marker aliases once and removes every alias atomically", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-client-cleanup-batch-"));
  const aliasA = path.join(directory, "active-client-id-1");
  const aliasB = path.join(directory, "active-client-id-2");
  fs.writeFileSync(aliasA, "shared-client\n", { mode: 0o600 });
  fs.writeFileSync(aliasB, "shared-client\n", { mode: 0o644 });
  const requests = [];

  const result = await cleanupClientFiles(
    [aliasA, aliasB],
    { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" },
    async (url, options) => {
      requests.push({ url, body: JSON.parse(options.body) });
      return requests.length === 1
        ? response({ network: { by_jwt: "jwt" } })
        : response({});
    },
  );

  assert.deepEqual(result, { releasedClients: 1, removedMarkers: 2 });
  assert.equal(requests.filter(({ url }) => url.endsWith("/network/remove-client")).length, 1);
  assert.equal(fs.existsSync(aliasA), false);
  assert.equal(fs.existsSync(aliasB), false);
  fs.rmSync(directory, { recursive: true });
});

test("batch cleanup retains a failed group but removes independent successes", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-client-cleanup-partial-"));
  const failedA = path.join(directory, "active-client-id-1");
  const failedB = path.join(directory, "active-client-id-2");
  const succeeded = path.join(directory, "active-client-id-3");
  fs.writeFileSync(failedA, "failed-client\n");
  fs.writeFileSync(failedB, "failed-client\n");
  fs.writeFileSync(succeeded, "successful-client\n");
  const removed = [];

  await assert.rejects(
    cleanupClientFiles(
      [failedA, failedB, succeeded],
      { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" },
      async (url, options) => {
        if (url.endsWith("/auth/login-with-password")) {
          return response({ network: { by_jwt: "jwt" } });
        }
        const clientId = JSON.parse(options.body).client_id;
        removed.push(clientId);
        return clientId === "failed-client" ? response({}, 503) : response({});
      },
    ),
    (error) => error.message.includes("one retained network client group failed cleanup") &&
      error.message.includes("HTTP 503") &&
      !error.message.includes("failed-client"),
  );

  assert.deepEqual(removed, ["failed-client", "successful-client"]);
  assert.equal(fs.existsSync(failedA), true);
  assert.equal(fs.existsSync(failedB), true);
  assert.equal(fs.existsSync(succeeded), false);
  fs.rmSync(directory, { recursive: true });
});

test("batch cleanup rejects non-files and multiline markers before API calls", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-client-cleanup-invalid-"));
  const multiline = path.join(directory, "active-client-id-1");
  fs.writeFileSync(multiline, "client-a\nclient-b\n");
  let requests = 0;
  const fetchImpl = async () => {
    requests += 1;
    return response({});
  };

  await assert.rejects(
    cleanupClientFiles([directory], { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" }, fetchImpl),
    /regular file/,
  );
  await assert.rejects(
    cleanupClientFiles([multiline], { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" }, fetchImpl),
    /exactly one non-empty line/,
  );
  assert.equal(requests, 0);
  fs.rmSync(directory, { recursive: true });
});

test("CLI summary reports only counts and never retained client IDs", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-client-cleanup-cli-"));
  const file = path.join(directory, "active-client-id-1");
  fs.writeFileSync(file, "private-client-id\n");
  let output = "";
  let request = 0;

  await runCleanupCLI(
    [file],
    { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" },
    async () => {
      request += 1;
      return request === 1 ? response({ network: { by_jwt: "jwt" } }) : response({});
    },
    { write: (text) => { output += text; } },
  );

  assert.match(output, /released 1 retained network client from 1 marker/);
  assert.doesNotMatch(output, /private-client-id/);
  fs.rmSync(directory, { recursive: true });
});

test("failed cleanup retains deterministic safe stage, counts and transport cause without private exception text", async t => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-cleanup-receipt-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const files = ["a", "b", "c"].map(name => path.join(directory, name));
  files.forEach((file, index) => fs.writeFileSync(file, `secret-client-${index}\n`));
  let logins = 0;
  let report;
  try {
    await cleanupClientFiles(files, { UR_ACCEPT_USER: "private-user", UR_ACCEPT_PASS: "private-password" }, async url => {
      if (url.endsWith("login-with-password")) {
        logins++;
        if (logins === 1) throw new TypeError("private-user private-password secret-jwt", { cause: Object.assign(new Error("secret-endpoint"), { code: "ECONNRESET" }) });
        return response({ network: { by_jwt: "private-jwt" } });
      }
      if (logins === 2) return response({ error: { message: "private-response" } }, 503);
      return response({});
    });
    assert.fail("cleanup must fail");
  } catch (error) { report = cleanupFailureReport(error); }
  assert.deepEqual(report, { type: "retained-client-cleanup", schemaVersion: 1, eligible: false,
    releasedClients: 1, removedMarkers: 1, failedGroups: 2, remainingMarkers: 2,
    failures: [{ stage: "login", kind: "network", status: null, networkCode: "ECONNRESET" },
      { stage: "remove-client", kind: "http-status", status: 503, networkCode: null }] });
  assert.doesNotMatch(JSON.stringify(report), /private|secret|https|jwt/);
  assert.deepEqual(files.map(file => fs.existsSync(file)), [true, true, false]);
  const forged = Object.assign(new Error("private-password"), { cleanupReport: { leaked: "private-password" },
    route: "/private-url", status: 999, cause: { code: "private-token" } });
  assert.deepEqual(cleanupFailureReport(forged).failures, [{ stage: "unknown", kind: "unclassified", status: null, networkCode: null }]);
  assert.doesNotMatch(JSON.stringify(cleanupFailureReport(forged)), /private/);
});

test("login 429 defers the shared-account batch without another request and preserves every marker", async t => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-cleanup-auth-limit-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const files = ["a", "alias-a", "b", "alias-b", "c", "alias-c"].map(name => path.join(directory, name));
  const contents = ["private-client-a\n", "private-client-a\n", "private-client-b\n", "private-client-b\n", "private-client-c\n", "private-client-c\n"];
  files.forEach((file, index) => fs.writeFileSync(file, contents[index], { mode: 0o600 }));
  const requests = [];
  let failure;
  await assert.rejects(cleanupClientFiles(files,
    { UR_ACCEPT_USER: "private-user", UR_ACCEPT_PASS: "private-password" },
    async url => {
      requests.push(url);
      return response({ error: { message: "private-server-body" } }, 429, { "retry-after": "300" });
    }), error => { failure = error; return true; });

  assert.equal(requests.length, 1, "a shared-account 429 must not trigger a fresh login for another client");
  assert.ok(requests[0].endsWith("/auth/login-with-password"));
  assert.deepEqual(files.map(file => fs.readFileSync(file, "utf8")), contents);
  assert.deepEqual(files.map(file => fs.statSync(file).mode & 0o777), files.map(() => 0o600));
  assert.equal(failure.errors.length, 1, "deferred groups must not be reported as additional HTTP failures");
  assert.match(failure.message, /retry after at least 300 seconds/);
  assert.deepEqual(cleanupFailureReport(failure), {
    type: "retained-client-cleanup", schemaVersion: 1, eligible: false,
    releasedClients: 0, removedMarkers: 0, failedGroups: 1, deferredGroups: 2, remainingMarkers: 6,
    failures: [{ stage: "login", kind: "http-status", status: 429, networkCode: null, retryAfterSeconds: 300 }],
  });
  assert.doesNotMatch(JSON.stringify(cleanupFailureReport(failure)) + failure.message, /private|secret|https|jwt/);
});

test("login 429 preserves earlier cleanup success and stops only the remaining groups", async t => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-cleanup-auth-partial-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const files = ["a", "b", "c"].map(name => path.join(directory, name));
  files.forEach((file, index) => fs.writeFileSync(file, `client-${index}\n`));
  let logins = 0, removals = 0, failure;
  await assert.rejects(cleanupClientFiles(files, { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" },
    async url => {
      if (url.endsWith("/auth/login-with-password")) {
        logins++;
        return logins === 1 ? response({ network: { by_jwt: "private-jwt" } }) : response({}, 429);
      }
      removals++;
      return response({});
    }), error => { failure = error; return true; });
  assert.equal(logins, 2);
  assert.equal(removals, 1);
  assert.deepEqual(files.map(file => fs.existsSync(file)), [false, true, true]);
  const report = cleanupFailureReport(failure);
  assert.deepEqual([report.releasedClients, report.removedMarkers, report.failedGroups, report.deferredGroups, report.remainingMarkers], [1, 1, 1, 1, 2]);
  assert.equal(report.failures[0].retryAfterSeconds, undefined, "no server hint must remain unknown");
});

test("non-auth removal 429 still permits cleanup of an independent client", async t => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ur-cleanup-remove-limit-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const files = ["a", "b"].map(name => path.join(directory, name));
  files.forEach((file, index) => fs.writeFileSync(file, `client-${index}\n`));
  let logins = 0, removals = 0, failure;
  await assert.rejects(cleanupClientFiles(files, { UR_ACCEPT_USER: "user", UR_ACCEPT_PASS: "pass" },
    async url => {
      if (url.endsWith("/auth/login-with-password")) {
        logins++;
        return response({ network: { by_jwt: "private-jwt" } });
      }
      removals++;
      return removals === 1 ? response({}, 429) : response({});
    }), error => { failure = error; return true; });
  assert.equal(logins, 2);
  assert.equal(removals, 2);
  assert.deepEqual(files.map(file => fs.existsSync(file)), [true, false]);
  assert.equal(cleanupFailureReport(failure).deferredGroups, undefined);
  assert.equal(cleanupFailureReport(failure).failures[0].stage, "remove-client");
});

test("rate-limit retry hints are numeric, redacted, and never authorize an automatic retry", async t => {
  t.mock.method(Date, "now", () => Date.parse("2026-09-27T13:07:00Z"));
  for (const [header, expected] of [
    ["300", 300], [" 300 ", 300], ["0", 0],
    ["Sun, 27 Sep 2026 13:12:00 GMT", 300], ["Sun, 27 Sep 2026 13:00:00 GMT", 0],
    ["", undefined], ["private-secret", undefined], ["-1", undefined], ["1.5", undefined],
    ["+300", undefined], ["300 seconds private-secret", undefined],
    ["Sun, 32 Sep 2026 13:12:00 GMT", undefined], ["Mon, 27 Sep 2026 13:12:00 GMT", undefined],
    ["999999999999999999999999999999999999999", undefined],
  ]) {
    let requests = 0, failure;
    await assert.rejects(releaseClient({ clientId: "private-client", user: "private-user", password: "private-password",
      fetchImpl: async () => { requests++; return response({}, 429, { "retry-after": header }); },
    }), error => { failure = error; return true; });
    assert.equal(requests, 1);
    const report = cleanupFailureReport(failure);
    assert.equal(report.failures[0].retryAfterSeconds, expected, `Retry-After ${JSON.stringify(header)}`);
    assert.doesNotMatch(JSON.stringify(report), /private|secret|https|jwt/);
  }
  const forged = Object.assign(new Error("private-body"), { route: "/auth/login-with-password", status: 429, retryAfterSeconds: "private-secret" });
  assert.equal(cleanupFailureReport(forged).failures[0].retryAfterSeconds, undefined);
});
