// SPDX-License-Identifier: MPL-2.0
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  cleanupClientFile,
  cleanupClientFiles,
  readCredentials,
  releaseClient,
  runCleanupCLI,
} from "./client-cleanup.mjs";

function response(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
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
