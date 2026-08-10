#!/usr/bin/env node
// Delete a production instant-account fixture after a successful native-app
// acceptance campaign.  The secret is read from a private file and is never
// printed or placed on a process command line.
import fs from "node:fs";

const [, , command, fixturePath] = process.argv;
if (command !== "delete" || !fixturePath) {
  console.error("usage: node fixture.mjs delete <secret-key-file>");
  process.exit(2);
}

const source = fs.readFileSync(fixturePath, "utf8").trim();
let secretKey = source;
if (source.startsWith("{")) {
  const fixture = JSON.parse(source);
  secretKey = fixture.seedphrase;
}
if (typeof secretKey !== "string" || secretKey.trim().split(/\s+/).length !== 24) {
  throw new Error("fixture does not contain a valid secret key");
}

async function post(path, body, jwt = "") {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 30_000);
  try {
    const response = await fetch(`https://api.bringyour.com${path}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Client-Version": "1.0.0-native-acceptance",
        ...(jwt ? { Authorization: `Bearer ${jwt}` } : {}),
      },
      body: body === null ? undefined : JSON.stringify(body),
      signal: controller.signal,
    });
    const parsed = await response.json().catch(() => ({}));
    if (!response.ok || parsed?.error) {
      throw new Error(parsed?.error?.message || `request failed (${response.status})`);
    }
    return parsed;
  } finally {
    clearTimeout(timeout);
  }
}

const login = await post("/auth/login", { seedphrase: secretKey });
const jwt = login?.network?.by_jwt;
if (!jwt) throw new Error("fixture secret key no longer logs in");
await post("/auth/network-delete", null, jwt);
console.log("acceptance: deleted the instant-account fixture");
