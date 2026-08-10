#!/usr/bin/env node
// SPDX-License-Identifier: MPL-2.0
import { pathToFileURL } from "node:url";

const defaultApiURL = "https://api.bringyour.com";

export async function checkRecoverableAccounts({
  apiURL = defaultApiURL,
  fetchImpl = fetch,
  timeoutMilliseconds = 20_000,
} = {}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMilliseconds);
  try {
    const response = await fetchImpl(`${apiURL}/auth/generate-seedphrase`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
      signal: controller.signal,
    });
    if (response.status === 401 || response.status === 403) {
      return;
    }
    if (response.status === 404) {
      throw new Error(
        "main does not yet expose recoverable secret-key accounts " +
          "(/auth/generate-seedphrase is 404); deploy the current server before running app acceptance",
      );
    }
    throw new Error(
      `unexpected main capability response: HTTP ${response.status}`,
    );
  } finally {
    clearTimeout(timeout);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    await checkRecoverableAccounts({
      apiURL: process.env.UR_ACCEPT_API_URL || defaultApiURL,
    });
    console.log("acceptance: main supports recoverable secret-key accounts");
  } catch (error) {
    console.error(`acceptance: ${error.message}`);
    process.exitCode = 1;
  }
}
