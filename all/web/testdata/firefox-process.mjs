function processRows(processTable) {
  return String(processTable)
    .split("\n")
    .map((line) => line.match(/^\s*(\d+)\s+(\d+)\s+(.+)$/))
    .filter(Boolean)
    .map((match) => ({ pid: Number(match[1]), parentPid: Number(match[2]), command: match[3] }));
}

function profileFromCommand(command) {
  const match = command.match(/(?:^|\s)"?-profile"?\s+(?:"([^"]+)"|(\S+))/);
  return match?.[1] || match?.[2] || "";
}

function isAcceptanceProfile(profile) {
  return /^\/.*\/rust_mozprofile[^/]*$/.test(profile);
}

// Geckodriver prints the unique profile before its POST /session response. It
// is therefore the ownership token when Firefox replaces/relaunches itself and
// the original direct child exits before a WebDriver session can be returned.
export function firefoxProfileFromGeckodriverOutput(output) {
  const profile = profileFromCommand(String(output));
  return isAcceptanceProfile(profile) ? profile : "";
}

// A bundle replacement on macOS swaps the executable inode while geckodriver
// is starting. Keep only stable, non-content identity fields so callers can
// distinguish that external mutation from an ordinary WebDriver failure.
export function firefoxBinaryIdentity(stat) {
  return [stat.dev, stat.ino, stat.size, stat.mtimeMs].map(String).join(":");
}

// Compares snapshots without depending on Firefox's reported version.
export function firefoxBinaryWasReplaced(before, after) {
  return before !== after;
}

// Tracks only the Firefox launched as a direct child of this geckodriver. The
// profile is the durable identity after a wedged browser is reparented to pid 1.
export function findAcceptanceFirefox(processTable, geckodriverPid, firefoxBin) {
  const row = processRows(processTable).find(({ parentPid, command }) =>
    parentPid === geckodriverPid &&
    command.includes(firefoxBin) &&
    command.includes("--marionette") &&
    command.includes("rust_mozprofile"));
  if (!row) return null;
  const profile = profileFromCommand(row.command);
  return isAcceptanceProfile(profile) ? { pid: row.pid, profile } : null;
}

// Every Firefox child carries the same unique temporary profile in its command
// line. This excludes the user's ordinary Firefox profile even after the test
// parent has died and all browser children have been reparented.
export function acceptanceFirefoxProcessIds(processTable, profile) {
  if (!isAcceptanceProfile(profile)) return [];
  return processRows(processTable)
    .filter(({ command }) => profileFromCommand(command) === profile)
    .map(({ pid }) => pid);
}

// Terminates only processes carrying the exact geckodriver-generated profile.
// Process ancestry is deliberately irrelevant: Firefox can reparent itself to
// pid 1 during an application update. The final inventory makes cleanup
// failure observable instead of silently leaving a test-owned browser behind.
export async function terminateAcceptanceFirefox({
  profile,
  readProcessTable,
  signalProcess,
  wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  lateRegistrationChecks = 0,
  graceChecks = 20,
  graceIntervalMs = 100,
}) {
  const ownedProcessIds = async () => acceptanceFirefoxProcessIds(
    await readProcessTable(),
    profile,
  );
  const signalAll = async (processIds, signal) => {
    for (const processId of processIds) {
      try {
        signalProcess(processId, signal);
      } catch {
        // A process may exit between the inventory and signal. The final exact
        // profile inventory, rather than this race, decides cleanup success.
      }
    }
  };

  const termProcessIdSet = new Set();
  for (let attempt = 0; attempt <= lateRegistrationChecks; attempt += 1) {
    const processIds = await ownedProcessIds();
    const newProcessIds = processIds.filter((processId) => !termProcessIdSet.has(processId));
    await signalAll(newProcessIds, "SIGTERM");
    newProcessIds.forEach((processId) => termProcessIdSet.add(processId));
    if (attempt < lateRegistrationChecks) await wait(graceIntervalMs);
  }
  const termProcessIds = [...termProcessIdSet];
  for (let attempt = 0; attempt < graceChecks; attempt += 1) {
    const remainingProcessIds = await ownedProcessIds();
    if (remainingProcessIds.length === 0) {
      return { termProcessIds, killProcessIds: [], remainingProcessIds: [] };
    }
    await wait(graceIntervalMs);
  }

  const killProcessIds = await ownedProcessIds();
  await signalAll(killProcessIds, "SIGKILL");
  await wait(graceIntervalMs);
  const remainingProcessIds = await ownedProcessIds();
  return { termProcessIds, killProcessIds, remainingProcessIds };
}
