# changelogs/ — the generated release notes, one set per release

Every release run writes this directory. `all/changelog.py` walks the submodule
pins between the previous release tag and this one, groups the commits by
component, and renders one file per audience:

| file | goes to | limit |
| --- | --- | --- |
| `<versionCode>_Full.md` | the GitHub release body links to it | none |
| `<versionCode>_Simple.txt` | inlined in the release body; no storefront | 500 |
| `<versionCode>_Android.txt` | Play Console → release notes | 500 |
| `<versionCode>_Apple.txt` | App Store Connect → What's New in This Version (iOS **and** macOS) | 4000 |
| `<versionCode>_Windows.txt` | Partner Center → Store listings → What's new in this version | 1500 |
| `<versionCode>_Linux.xml` | `urnetwork/linux` metainfo `<release><description>` | none (AppStream XML) |

The version code prefix is the same key `metadata/en-US/changelogs/<versionCode>.txt`
already uses, so the directory is a per-release archive that sorts chronologically
and never overwrites an older release's notes.

## What a publisher has to do

Open the file for your store, paste it, and **delete the shared tail if your
reviewers do not need it.** Each store note is that app's own changes first, then
the changes shared with every other URnetwork app (`sdk`, `connect`, and the two
small libraries) after a boundary marker. One motion removes the tail:

- `_Apple.txt` and `_Windows.txt` — delete from the `Under the hood` line down.
- `_Android.txt` — delete from the blank line down. It has no heading because the
  note is 486 of its 500 characters and a heading costs 14 of them.
- `_Linux.xml` — delete between the two XML comments, which say so inline.

That is the whole point of this directory: a developer publishing to a store is
handed the actual changes instead of typing "Fixes and improvements" again.

## Android has two copies, on purpose

F-Droid reads `metadata/en-US/changelogs/<versionCode>.txt` off this repository
with no human in between, and that path cannot move. `all/run.sh` writes it there
(once per `FDROID_VERSION_CODE_OFFSETS` entry, plus `default.txt`) from the same
render that produces `_Android.txt` here, so the two cannot drift. The copy here
is the per-release record a human opens when writing the Play note.

## Nothing here may move under `metadata/`

`fdroidserver` globs `build/[A-Za-z]*/metadata/[a-z][a-z]*` and then `os.walk()`s
the result, matching changelog files by bare filename at any depth. A directory
under `metadata/` would fabricate a locale in the F-Droid index. `changelogs/` is
a sibling of `metadata/` and is invisible to that glob.

## Regenerating by hand

```
GITHUB_API_KEY=$(gh auth token) python3 all/changelog.py \
    --from v2026.8.15-1020621320 --to v2026.8.21-1025763520 \
    --notes-dir changelogs
```

Run it without a token and it still works, but the path check that decides which
commits are shippable degrades to subject matching — it warns loudly when it does.
