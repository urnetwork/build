#!/usr/bin/env node
// SPDX-License-Identifier: MPL-2.0
import { pathToFileURL } from "node:url";

const defaultApiURL = "https://api.bringyour.com";

function sleep(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function retryableStatus(status) {
  return status === 408 || status === 429 || status >= 500;
}

async function fetchCapability({ apiURL, fetchImpl, timeoutMilliseconds }) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMilliseconds);
  try {
    return await fetchImpl(`${apiURL}/auth/generate-seedphrase`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
      signal: controller.signal,
    });
  } finally {
    clearTimeout(timeout);
  }
}

export async function checkRecoverableAccounts({
  apiURL = defaultApiURL,
  fetchImpl = fetch,
  timeoutMilliseconds = 20_000,
  attempts = 5,
  retryDelayMilliseconds = 1_000,
  sleepImpl = sleep,
} = {}) {
  if (!Number.isInteger(attempts) || attempts < 1) {
    throw new Error("capability check attempts must be a positive integer");
  }
  for (let attempt = 1; attempt <= attempts; attempt += 1) {
    let response;
    try {
      response = await fetchCapability({ apiURL, fetchImpl, timeoutMilliseconds });
    } catch (error) {
      if (attempt === attempts) throw error;
      await sleepImpl(retryDelayMilliseconds * (2 ** (attempt - 1)));
      continue;
    }
    if (response.status === 401 || response.status === 403) {
      return;
    }
    if (response.status === 404) {
      throw new Error(
        "main does not yet expose recoverable secret-key accounts " +
          "(/auth/generate-seedphrase is 404); deploy the current server before running app acceptance",
      );
    }
    if (retryableStatus(response.status) && attempt < attempts) {
      await sleepImpl(retryDelayMilliseconds * (2 ** (attempt - 1)));
      continue;
    }
    throw new Error(
      `unexpected main capability response: HTTP ${response.status}`,
    );
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
