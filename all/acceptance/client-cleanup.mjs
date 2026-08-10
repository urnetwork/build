#!/usr/bin/env node
// SPDX-License-Identifier: MPL-2.0
// Releases a retained acceptance client after its app process is terminated.
import fs from "node:fs";
import { pathToFileURL } from "node:url";

const defaultApiURL = "https://api.bringyour.com";

async function post(apiURL, route, body, jwt, fetchImpl) {
  const response = await fetchImpl(`${apiURL}${route}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Client-Version": "1.0.0-acceptance-cleanup",
      ...(jwt ? { Authorization: `Bearer ${jwt}` } : {}),
    },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(`main API ${route} returned HTTP ${response.status}`);
  return result;
}

export async function releaseClient({
  clientId,
  user,
  password,
  apiURL = defaultApiURL,
  fetchImpl = fetch,
}) {
  if (!clientId || !user || !password) throw new Error("client ID, user, and password are required");
  const login = await post(apiURL, "/auth/login-with-password", { user_auth: user, password }, "", fetchImpl);
  if (login.error) throw new Error(`cleanup login failed: ${login.error.message}`);
  const jwt = login.network?.by_jwt;
  if (!jwt) throw new Error("cleanup login returned no network session");

  const removed = await post(apiURL, "/network/remove-client", { client_id: clientId }, jwt, fetchImpl);
  if (removed.error && removed.error.message !== "Client does not exist.") {
    throw new Error(`network-client cleanup failed: ${removed.error.message}`);
  }
}

export function readCredentials(environment = process.env) {
  if (environment.UR_ACCEPT_CREDENTIALS_FILE) {
    const lines = fs.readFileSync(environment.UR_ACCEPT_CREDENTIALS_FILE, "utf8")
      .trim()
      .split("\n")
      .map((line) => line.trim());
    if (lines.length !== 2 || lines.some((line) => !line)) {
      throw new Error("acceptance credentials file must have exactly two non-empty lines");
    }
    return { user: lines[0], password: lines[1] };
  }
  return {
    user: environment.UR_ACCEPT_USER,
    password: environment.UR_ACCEPT_PASS,
  };
}

export async function cleanupClientFile(file, environment = process.env, fetchImpl = fetch) {
  const clientId = fs.readFileSync(file, "utf8").trim();
  const { user, password } = readCredentials(environment);
  await releaseClient({ clientId, user, password, fetchImpl });
  fs.rmSync(file, { force: true });
  return clientId;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const file = process.argv[2];
  if (!file) {
    console.error("usage: node client-cleanup.mjs <active-client-id-file>");
    process.exit(2);
  }
  const clientId = await cleanupClientFile(file);
  console.log(`acceptance: released retained network client ${clientId}`);
}
