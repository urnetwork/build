// SPDX-License-Identifier: MPL-2.0
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { cleanupClientFile, readCredentials, releaseClient } from "./client-cleanup.mjs";

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
