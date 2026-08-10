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
    checkRecoverableAccounts({ fetchImpl: response(500) }),
    /HTTP 500/,
  );
});

test("bounds a main API that never responds", async () => {
  const neverResponds = async (_url, { signal }) => new Promise((_, reject) => {
    signal.addEventListener("abort", () => reject(signal.reason), { once: true });
  });
  await assert.rejects(
    checkRecoverableAccounts({
      fetchImpl: neverResponds,
      timeoutMilliseconds: 1,
    }),
    /abort|timeout/i,
  );
});
