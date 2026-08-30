// SPDX-License-Identifier: MPL-2.0
import assert from "node:assert/strict";
import test from "node:test";

import { checkRecoverableAccounts } from "./preflight-main.mjs";

function response(status) {
  return async () => ({ status });
}

test("accepts an authentication challenge from the seedphrase route", async () => {
  let request;
  await checkRecoverableAccounts({
    apiURL: "https://main.example",
    fetchImpl: async (url, options) => {
      request = { url, options };
      return { status: 401 };
    },
  });
  assert.equal(request.url, "https://main.example/auth/generate-seedphrase");
  assert.equal(request.options.method, "POST");
  assert.equal(request.options.body, "{}");
  await checkRecoverableAccounts({ fetchImpl: response(403) });
});

test("reports a main deployment that predates recoverable accounts", async () => {
  await assert.rejects(
    checkRecoverableAccounts({ fetchImpl: response(404) }),
    /deploy the current server/,
  );
});

test("rejects an ambiguous capability response", async () => {
  await assert.rejects(
    checkRecoverableAccounts({ fetchImpl: response(500), attempts: 1 }),
    /HTTP 500/,
  );
});

test("retries transient fetch failures before accepting the capability", async () => {
  let requests = 0;
  const delays = [];
  await checkRecoverableAccounts({
    attempts: 3,
    retryDelayMilliseconds: 7,
    sleepImpl: async (milliseconds) => delays.push(milliseconds),
    fetchImpl: async () => {
      requests += 1;
      if (requests < 3) throw new TypeError("fetch failed");
      return { status: 401 };
    },
  });
  assert.equal(requests, 3);
  assert.deepEqual(delays, [7, 14]);
});

test("retries a transient server response before accepting the capability", async () => {
  const statuses = [503, 403];
  const delays = [];
  await checkRecoverableAccounts({
    retryDelayMilliseconds: 11,
    sleepImpl: async (milliseconds) => delays.push(milliseconds),
    fetchImpl: async () => ({ status: statuses.shift() }),
  });
  assert.deepEqual(statuses, []);
  assert.deepEqual(delays, [11]);
});

test("bounds a main API that never responds", async () => {
  const neverResponds = async (_url, { signal }) => new Promise((_, reject) => {
    signal.addEventListener("abort", () => reject(signal.reason), { once: true });
  });
  await assert.rejects(
    checkRecoverableAccounts({
      fetchImpl: neverResponds,
      timeoutMilliseconds: 1,
      attempts: 1,
    }),
    /abort|timeout/i,
  );
});
