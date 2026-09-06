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

function readClientMarker(file) {
  const stat = fs.lstatSync(file);
  if (!stat.isFile()) {
    throw new Error("acceptance client marker must be a regular file");
  }
  const lines = fs.readFileSync(file, "utf8")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
  if (lines.length !== 1) {
    throw new Error("acceptance client marker must contain exactly one non-empty line");
  }
  return lines[0];
}

export async function cleanupClientFiles(files, environment = process.env, fetchImpl = fetch) {
  if (!Array.isArray(files) || files.length === 0) {
    throw new Error("at least one acceptance client marker is required");
  }

  // Read and validate the whole ledger before mutating either API or disk.
  // Multiple runners can retain aliases for the same client; group them so the
  // destructive API operation happens exactly once.
  const markerGroups = new Map();
  for (const file of files) {
    const clientId = readClientMarker(file);
    const aliases = markerGroups.get(clientId) ?? [];
    aliases.push(file);
    markerGroups.set(clientId, aliases);
  }

  const { user, password } = readCredentials(environment);
  let releasedClients = 0;
  let removedMarkers = 0;
  const failures = [];
  for (const [clientId, aliases] of markerGroups) {
    try {
      await releaseClient({ clientId, user, password, fetchImpl });
      for (const file of aliases) {
        fs.rmSync(file, { force: true });
        removedMarkers += 1;
      }
      releasedClients += 1;
    } catch (error) {
      // Continue with independent client groups, but retain every alias for
      // this group so a later cleanup can retry it explicitly.
      failures.push(error);
    }
  }

  if (failures.length > 0) {
    throw new AggregateError(
      failures,
      `${failures.length === 1 ? "one" : failures.length} retained network client group${failures.length === 1 ? "" : "s"} failed cleanup`,
    );
  }
  return { releasedClients, removedMarkers };
}

export async function cleanupClientFile(file, environment = process.env, fetchImpl = fetch) {
  const clientId = readClientMarker(file);
  try {
    await cleanupClientFiles([file], environment, fetchImpl);
  } catch (error) {
    // Preserve the original single-file API's concrete failure for existing
    // Linux/Windows callers while the multi-file CLI reports grouped failures.
    if (error instanceof AggregateError && error.errors.length === 1) {
      throw error.errors[0];
    }
    throw error;
  }
  return clientId;
}

export async function runCleanupCLI(
  files,
  environment = process.env,
  fetchImpl = fetch,
  stdout = process.stdout,
) {
  const result = await cleanupClientFiles(files, environment, fetchImpl);
  const clientWord = result.releasedClients === 1 ? "client" : "clients";
  const markerWord = result.removedMarkers === 1 ? "marker" : "markers";
  stdout.write(
    `acceptance: released ${result.releasedClients} retained network ${clientWord} from ${result.removedMarkers} ${markerWord}\n`,
  );
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const files = process.argv.slice(2);
  if (files.length === 0) {
    console.error("usage: node client-cleanup.mjs <active-client-id-file> [...]");
    process.exit(2);
  }
  await runCleanupCLI(files);
}
