# Remote (SSH / headless) builds and macOS keychain unlocking

Why codesigning fails over SSH on the builder, why it happened, what we run
today, and the verified options for doing it better. Findings from a deep,
source-verified research pass (2026-07-21, macOS 26.x era, Apple Silicon;
20 confirmed claims, 5 refuted, sources at the bottom).

## The failure

`xcodebuild archive` dies at its first `CodeSign` step with
`errSecInternalComponent`, the ios/macos `.ipa`/`.pkg` are never produced, and
the release pipeline then hard-fails uploading the missing artifact. A minimal
repro is signing any file from a non-GUI session:

```sh
echo x > /tmp/t && codesign -f -s "Apple Development: Product Builder (F7RQ5ZZ798)" /tmp/t
# -> errSecInternalComponent  == keychain not usable from THIS session
```

## Root cause (verified, Apple-primary sources)

- Keychain unlock state lives in the **per-login security context** — the
  kernel audit session tracked by securityd — not in per-user global state
  (TN2083; DTS thread 712005, revised 2026-07-06). An unlock performed in one
  SSH session is **invisible to sibling SSH sessions and to detached
  (nohup/daemon) process trees**.
- SSH logins never auto-unlock the login keychain: only GUI login (loginwindow)
  does that. When codesign then needs the key, the unlock dialog cannot be
  presented over SSH → `errSecInternalComponent` (DTS 712005 — Apple's
  actively-maintained diagnosis of exactly this failure).
- This is long-standing behavior, not a macOS 26 regression. The notion that
  `security unlock-keychain` is user-global was **refuted 0-3** in
  verification; cross-invocation non-persistence was reproduced on pre-Tahoe
  macOS (Nov 2024) and the per-session context taxonomy dates back to TN2083.
  (Historical "it seemed global" memories likely came from long-lived/multiplexed
  SSH sessions reusing one audit session.)

Practical law: **the unlock must run inside the same login session (process
tree) as the signing.** Unlocking "for" the build from any other SSH session
can never work.

## What we run today (works; Apple-DTS-endorsed pattern)

`~/urnetwork/build.sh` on the builder (server-only file, not in this repo):

1. Refuses to start unless `~/.keychain-pw` exists (chmod 600; created from
   `by-pass builder` on an ops machine — never store the password in a repo).
2. `security unlock-keychain -p "$(cat ~/.keychain-pw)" login.keychain-db`
   **inside the build's own session**, before invoking `build/all/run.sh`.
3. A keep-alive subshell re-runs the unlock every 240 s until the script exits
   (belt-and-suspenders: unlock persistence semantics across time/sessions are
   undocumented).
4. One-time per key, non-interactive key access was granted with:
   `security set-key-partition-list -S apple-tool:,apple: -s -k PW login.keychain-db`
   (the `codesign:` partition id sometimes seen in recipes is harmless but
   unnecessary — GitHub's official recipe and Apple DTS use `apple-tool:,apple:`).

See also the header note in `all/run.sh`.

## Ranked options (verified against macOS 26-era sources)

1. **Best: dedicated per-run build keychain, created inside the build's own
   process tree** — the officially documented GitHub Actions macOS pattern;
   never touches login.keychain-db at all. **Implemented (2026-07-21): a header
   block in `all/run.sh`, gated on `BUILD_APPLE_IDENTITY` (set it in the
   builder's `~/urnetwork/build.sh`, which can then drop its unlock +
   keep-alive).** One-time: export the signing
   identity to `identity.p12` (chmod 600) and create an App Store Connect API
   key (`.p8` + Key ID + Issuer ID). Two gotchas hit doing the export on the
   builder (`~/.identity.p12` + `~/.p12-pw`):
   - `security export` of a private key is gated by an ACL operation that the
     partition list does NOT cover — headless it fails with `User interaction
     is not allowed`, so run the export from a GUI session; and create the
     password file **before** exporting, since `-P "$(cat ~/.p12-pw)"` with a
     missing file silently encrypts the p12 with an *empty* password.
   - A botched p12 can be re-encrypted headless without touching the login
     keychain: `security import` it (with its current password) into a
     throwaway keychain with `-A`, then `security export` from there with the
     right password. (`/usr/bin/openssl` is LibreSSL and cannot parse the
     AES/PBES2 p12 that macOS writes.)

   Then, as the first steps of the build:

   ```sh
   KC="$HOME/Library/Keychains/build.keychain-db"
   security create-keychain -p "$PW" "$KC"
   security set-keychain-settings -lut 21600 "$KC"        # 6h auto-lock
   security unlock-keychain -p "$PW" "$KC"
   security import identity.p12 -P "$P12PW" -f pkcs12 \
       -T /usr/bin/codesign -T /usr/bin/security -k "$KC"  # -T is tighter than GitHub's -A
   security set-key-partition-list -S apple-tool:,apple: -s -k "$PW" "$KC"
   security list-keychains -d user -s "$KC" login.keychain-db
   # ... build ...
   security delete-keychain "$KC"                          # on exit; restore search list
   ```

   macOS 26 gotchas: always reference the `*-db` path, and verify the key
   actually landed in `$KC` before `set-key-partition-list` — it **silently
   no-ops** when the key isn't in the targeted keychain.

2. **Current setup (keep as fallback):** in-session unlock of the login
   keychain + one-time partition list + keep-alive loop, as above. Endorsed by
   DTS; equivalent password-on-disk exposure to option 1, but the identity
   stays in the login keychain rather than a scoped throwaway.

3. **System keychain import** (`sudo security import identity.p12 -k
   /Library/Keychains/System.keychain -T /usr/bin/codesign`): worked in
   Catalina-era reports (System keychain is the default search list for true
   daemon contexts, TN3137), but evidence is 2020-vintage and **unverified on
   macOS 26**; grants machine-wide, root-installed access to the key. Medium
   confidence only.

4. **Custom launchd-managed sshd with `SessionCreate` — NOT viable.** This is
   the mechanism behind the "entitlement added to a launchd plist" memory, and
   it does the opposite of what's needed: `SessionCreate` spawns the job into a
   **new** audit/security session (launchd.plist(5), verified live on macOS
   26.5.2); there is no launchd mechanism to attach a job to an existing
   unlocked session. On macOS 26 the stock ssh.plist doesn't even set it —
   `/usr/libexec/sshd-session` assigns every login a fresh audit session itself
   (`setaudit_addr`, confirmed in the shipped binary). Apple forum tests show
   LaunchDaemons fail codesign with SessionCreate both true and false. Also:
   no `com.apple.security.*` entitlement, keychain-access-groups, or TCC grant
   unlocks **file-based** keychains for third-party CLI tools — those govern
   the Data Protection keychain / app sandbox / privacy resources. And the
   Data Protection (SEP-backed) keychain is unusable from daemon contexts
   entirely (TN3137), so file-based keychains are the only game for headless
   signing.

5. **Auto-login GUI user / VNC session:** works (loginwindow unlocks the
   keychain at login) but enlarges the attack surface and is unnecessary given
   options 1–2. `launchctl asuser`/`bsexec` UID-switching without establishing
   the security context is exactly the "mixed execution context" anti-pattern
   DTS warns about.

## xcodebuild -allowProvisioningUpdates extras

Beyond codesign's keychain needs, `-allowProvisioningUpdates` talks to the
Developer portal (downloads profiles for manual signing; may create/update
profiles, App IDs, and certs for automatic signing). It needs credentials from
either Xcode's Accounts pane (GUI legacy) or — the headless-friendly path,
Xcode 13+ — an **App Store Connect API key**:

```sh
xcodebuild ... -allowProvisioningUpdates \
    -authenticationKeyPath /path/AuthKey_XXXX.p8 \
    -authenticationKeyID XXXX -authenticationKeyIssuerID YYYY
```

With that, no Apple ID credentials live in any keychain on the box. Xcode 13+
cloud signing can additionally keep distribution private keys on Apple's
servers (signing happens in the cloud; ASC key needs Admin/App Manager or
cloud-managed-cert permission) — though a local Apple Development identity is
still needed for the archive step, so this reduces but does not eliminate the
local-keychain requirement.

## Security trade-offs (single-purpose build box)

- Options 1 and 2 both keep a keychain password on disk chmod 600 — equivalent
  exposure; option 1 additionally scopes the identity to a throwaway keychain
  and named tools (`-T`), so a leaked build.keychain-db costs less than a
  leaked login keychain.
- Option 3 trades password-on-disk for a root-installed machine-wide key.
- Never `security import -A` if avoidable ("insecure, not recommended" per
  security(1)); prefer `-T /usr/bin/codesign`.
- ASC API key on disk replaces Apple ID + app-specific passwords entirely and
  is revocable server-side.

## Open questions (not settled by any source)

- Does an unlocked custom keychain (`-lut 21600`) stay usable from a *different*
  later audit session on macOS 26? (If yes, the keep-alive loop is partially
  redundant; we keep it anyway.)
- Does the System-keychain pattern still work on macOS 26 + Xcode 17?
- Was SSH keychain unlock ever genuinely cross-session on very old macOS, or
  was that always session-reuse artifacts?

## Key sources

- Apple DTS, "Resolving errSecInternalComponent errors during code signing" —
  developer.apple.com/forums/thread/712005 (revised 2026-07-06)
- Apple TN2083 "Daemons and Agents" (execution/security contexts) —
  developer.apple.com/library/archive/technotes/tn2083
- Apple TN3137 "On Mac keychains" (file-based vs Data Protection, daemon rules)
- GitHub Docs, "Sign Xcode applications" (per-run build keychain recipe) —
  docs.github.com/en/actions/how-tos/deploy/deploy-to-third-party-platforms/sign-xcode-applications
- WWDC21 session 10204 "Distribute apps in Xcode with cloud signing" (ASC API
  key flags, cloud signing)
- xcodebuild(1) man page (Xcode 26.6); developer.apple.com/forums threads
  768354, 685967, 666107; fastlane#19369; jmmv.dev 2020 codesign-and-ssh
