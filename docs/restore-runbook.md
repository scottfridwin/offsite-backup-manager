# Restore drill runbook (manual, §6 / §13.6)

This is the **manual, documented restore drill** referenced in
[`docs/requirements.md`](requirements.md) §6 and §13 (resolved decision 6): there
is no automated scheduled restore verification in v1. Run this drill periodically
(e.g. quarterly) and whenever you need to actually recover data, on a trusted
recovery host — **never on the Primary or a node**.

## Prerequisites

- The **age private identity** (`AGE-SECRET-KEY-...`), retrieved from the
  password manager (plus the independent offline copy, if you need to verify it
  still exists/works).
- The **minisign public key** (`minisign.pub`) used to verify manifests — this is
  not secret and can live in the repo/config, same as nodes use.
- The `age`, `minisign`, and `zstd` CLIs (already present in the dev container;
  install via your package manager elsewhere).
- A copy of the package + manifest + signature you want to restore, either from
  a node's append-only store (`STORE_DIR/run-<ts>.*`) or the Primary's output
  directory (`PACKAGE_OUTPUT_DIR/run-<ts>.*`).

You need all three files for a given run:

```
run-<ts>.tar.zst.age           # the encrypted package
run-<ts>.manifest.json         # plaintext metadata + checksum
run-<ts>.manifest.json.minisig # detached signature over the manifest
```

## 1. Verify the manifest signature

```bash
minisign -V -p minisign.pub -m run-<ts>.manifest.json
```

This reads `run-<ts>.manifest.json.minisig` automatically (minisign's default
naming convention, which matches what the Primary writes). **Stop here if this
fails** — do not proceed to decrypt a package whose manifest didn't verify.

## 2. Verify the package checksum

Compare the package's SHA-256 against the `package.sha256` field in the
(now-verified) manifest:

```bash
sha256sum run-<ts>.tar.zst.age
jq -r '.package.sha256' run-<ts>.manifest.json
```

The two values must match exactly. If they don't, the package is corrupt or
tampered — do not proceed.

## 3. Decrypt

```bash
age -d -i age-identity.txt -o run-<ts>.tar.zst run-<ts>.tar.zst.age
```

`age-identity.txt` is the private identity from the password manager. This step
is the only place the private identity is ever used, and should only ever run on
a trusted, offline-capable recovery host.

## 4. Decompress and extract

```bash
zstd -d run-<ts>.tar.zst -o run-<ts>.tar
mkdir -p restored-<ts>
tar -xf run-<ts>.tar -C restored-<ts>
```

`restored-<ts>/` now contains a full copy of the backup source tree
(`BACKUP_SOURCE_DIR`) exactly as it was captured for that run — every service's
backup output, including full + incremental chains, since each run is a
whole-tree snapshot (§3.1).

## 5. Spot-check

- Confirm the top-level directory names under `restored-<ts>/` match what you
  expect from `BACKUP_SOURCE_DIR` (the manifest's `contents.top_level` lists
  each one with its file count and size — compare against what you see).
- Pick at least one service's backup and confirm it opens/restores correctly
  with that service's own tooling (this project only guarantees the bytes are
  intact and authentic; it doesn't understand any service-specific backup
  format).
- Record the drill (date, run ID, outcome) wherever you track this — there's no
  automated tracking in v1.

## 6. Clean up

Securely delete the decrypted/decompressed intermediates
(`run-<ts>.tar.zst`, `run-<ts>.tar`, `restored-<ts>/`) and the identity file copy
if it was staged on disk for this drill. Only the password manager (plus its
offline backup copy) should retain the private identity long-term.
