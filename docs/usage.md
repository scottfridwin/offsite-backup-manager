# CLI & configuration reference

Both binaries print full flag/env var help via `primary help` / `node help`.
This page walks through the common commands; see
[`docs/requirements.md`](requirements.md) for the full design rationale behind
each option.

## Primary: generating keys

```bash
primary keygen -out ./keys -minisign-password <your-passphrase>
```

Generates a fresh `age` identity (`age-identity.txt`) and a `minisign` keypair
(`minisign.key`, `minisign.pub`) and prints `AGE_RECIPIENT=age1...`. Omit
`-minisign-password` (or set `MINISIGN_PASSWORD`/`MINISIGN_PASSWORD_FILE`) for
an unprotected secret key. Move `age-identity.txt` to a password manager and
off the Primary — see [`docs/restore-runbook.md`](restore-runbook.md).

## Primary: building a package manually

```bash
primary package \
  -source /backups \
  -output /out \
  -recipient age1examplexxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx \
  -minisign-key /path/to/minisign.key
```

Equivalent env vars: `BACKUP_SOURCE_DIR`, `PACKAGE_OUTPUT_DIR`, `PACKAGE_WORK_DIR`,
`AGE_RECIPIENT`, `MINISIGN_SECKEY`, `MINISIGN_PASSWORD` (or `MINISIGN_PASSWORD_FILE`
to read it from a file instead, e.g. a Docker secret), `PACKAGE_KEEP_RUNS`. Output is
`run-<ts>.tar.zst.age` plus a signed `run-<ts>.manifest.json` (`.minisig`).

- The source is copied to `PACKAGE_WORK_DIR` first. The copy is only used if
  the source didn't change while it was being made; otherwise it is retried, and
  the run fails if the source never holds still.
- The archive keeps each entry's original owner (numeric UID/GID), mode and
  mtime. Its last entry is `.offsite-backup-manifest.json`: the run's manifest
  without the package checksum, so a decrypted package describes itself.
- Sockets, devices and pipes are not packaged; they are listed in the manifest's
  `contents.skipped` and logged as warnings.
- The package is written as `.run-<ts>.tar.zst.age.partial` and only appears
  under its final name when complete. Leftovers of an interrupted run are
  removed by the next run.
- `PACKAGE_KEEP_RUNS=N` (default 0 = keep all) deletes runs older than the
  newest N from the output directory, but only once every node that should
  hold them has confirmed a run at least that new. Nodes keep the history.

Run this manually for a one-off, or let `primary serve` trigger it on a
schedule internally (see below) — both use the same configuration.

## Primary: serving the node-facing API and self-scheduling

`serve` is the image's default command. It runs the enroll/pull/heartbeat API
continuously and, if `SCHEDULE` is set (a standard 5-field cron expression),
also triggers a packaging run internally on that schedule — no external
cron/orchestrator is required, though one still works fine if you'd rather
trigger `primary package` yourself (leave `SCHEDULE` unset).

```bash
primary serve -output /out -roster /data/roster.json -schedule "0 12 1 * *"
```

Mint a one-time enrollment token for a new node:

```bash
primary enroll-token -roster /data/roster.json -enroll-token-ttl 24h
```

## Node: enrolling and running

`run` is the image's default command.

```bash
node run \
  -backup-host backup.example.com \
  -enrollment-token <token-from-enroll-token> \
  -label pi-garage \
  -store /data/store \
  -minisign-pubkey /path/to/minisign.pub
```

The node enrolls once (persisting its durable pull credential under `-store`),
then on every `-pull-interval` it fetches the run list, verifies each
manifest's minisign signature and each package's checksum before ever writing
it, stores newly-verified runs read-only (never overwriting or re-fetching
existing ones), and reports a heartbeat (free space, last synced run).

- Downloads go to `<store>/.incoming/` and are hard-linked into the store when
  verified, so the store only ever gains entries (it can be made append-only).
- An interrupted download is resumed (HTTP `Range`) on the next cycle.
- A package is not started unless the store has room for it (plus 64 MiB); the
  cycle logs `not enough free space` instead.
- Heartbeats run on their own timer, so a long download doesn't make the node
  look silent.

Equivalent env vars: `BACKUP_HOST`, `ENROLLMENT_TOKEN` (or
`ENROLLMENT_TOKEN_FILE`), `NODE_LABEL`, `PULL_INTERVAL`, `STORE_DIR`,
`MINISIGN_PUBKEY`, `CAPACITY_WARN_PCT`.

## Distribution schemes, node lifecycle, and health

Each `primary package` run is assigned to node(s) per `-distribution-scheme`
(env `DISTRIBUTION_SCHEME`): `replicate-all` (default; every active node may
pull every run) or `round-robin` (each run goes to the next node in rotation,
tracked in `-roster`/`NODE_ROSTER_FILE`). The assignment is recorded in the
run's manifest, and the Primary's `/runs` API only lists a round-robin run to
its assigned node.

```bash
primary nodes -roster /data/roster.json              # list enrolled nodes + status
primary retire-node -roster /data/roster.json <id>   # decommission a node
primary healthcheck -output /out -roster /data/roster.json   # exit 0/1 for Docker HEALTHCHECK
```

`healthcheck` is unhealthy when:

- the last packaging run failed (until a later run succeeds);
- a scheduled run was missed: none published by its due time plus
  `HEALTH_SYNC_GRACE` (without `SCHEDULE`: none within `HEALTH_RUN_INTERVAL`);
- after `HEALTH_SYNC_GRACE` since the latest run, a node required by that run's
  distribution assignment hasn't confirmed pulling it via heartbeat;
- an active node has sent no heartbeat for `HEALTH_NODE_STALE` (default 48h; 0
  disables);
- an active node's reported space use is at/above `NODE_SPACE_CRITICAL_PCT`.

A Primary with no nodes enrolled yet is healthy as long as its runs succeed.
Both published container images run this as their Docker `HEALTHCHECK`.

## Capacity projection

```bash
primary capacity -output /out -roster /data/roster.json
```

Projects average package size and run cadence from already-published runs (no
separate cadence config needed), the resulting growth/year, and each active
node's time-to-full from its last-reported free space.

## Further reading

- [`docs/node-setup.md`](node-setup.md) — provisioning a new node end to end.
- [`docs/restore-runbook.md`](restore-runbook.md) — the manual restore drill.
- [`docs/requirements.md`](requirements.md) — full design, threat model, and
  resolved decisions.
