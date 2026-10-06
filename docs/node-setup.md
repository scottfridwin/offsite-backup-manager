# Building and provisioning a backup node (§5.1)

This covers the "flash + boot" deliverable from
[`docs/requirements.md`](requirements.md) §5.1: a zero-touch node that needs no
interaction beyond writing its configuration before first boot.

## What a node needs, once, before first boot

| Value | Source | Secret? |
|-------|--------|---------|
| `BACKUP_HOST` | The Primary's public hostname, e.g. `backup.example.com` | No |
| `ENROLLMENT_TOKEN` | A fresh, single-use token from the Primary operator | **Yes, one-time** |
| `MINISIGN_PUBKEY` | The Primary's minisign public key file (not secret, commit/distribute freely) | No |
| `NODE_LABEL` | A name you choose for this node, e.g. `pi-garage` | No |
| `STORE_DIR` | Where the node writes its append-only package store (persistent media) | No |

Nothing else is node-specific; the container image itself carries no baked-in
secrets or per-node values (§4.4, §11).

## 1. Get an enrollment token (operator, on the Primary)

```bash
primary enroll-token -roster /data/roster.json -enroll-token-ttl 24h
```

This prints a single-use token (and its expiry) to stdout. Treat it as a
bootstrap secret: it's burned on first use and expires on its own, but anyone
who has it before that can enroll a node under your Primary. Hand it to
whoever is provisioning the node through a reasonably secure channel (it's
short-lived, so a password manager share or even a messaged link is normally
fine — just don't commit it anywhere).

## 2. Build or pull the node image

```bash
make docker-node   # builds ghcr.io/scottfridwin/offsite-backup-manager-node:<version> locally
```

For Raspberry Pi (`arm64`/`armv7`), cross-build with Docker buildx, or pull the
published image once this project publishes to GHCR (see the main
[README](../README.md) — image names are provisional pre-release).

## 3. Prepare the node's persistent storage

The append-only store (`STORE_DIR`) must be on durable, persistent media (the
SD card or an attached disk) — this is where every verified run accumulates
forever (§7.2, never pruned). Size it for your intended retention per the
capacity guidance below.

## 4. First boot: environment + auto-start unit

Example `docker-compose.yml` (adjust paths/image for your setup):

```yaml
services:
  node:
    image: ghcr.io/scottfridwin/offsite-backup-manager-node:latest
    restart: unless-stopped
    environment:
      BACKUP_HOST: backup.example.com
      ENROLLMENT_TOKEN: "<one-time token from step 1>"
      NODE_LABEL: pi-garage
      STORE_DIR: /store
      MINISIGN_PUBKEY: /config/minisign.pub
      PULL_INTERVAL: 1h
      CAPACITY_WARN_PCT: "90"
    volumes:
      - /srv/backup-node/store:/store
      - /srv/backup-node/minisign.pub:/config/minisign.pub:ro
    command: ["run"]
```

Equivalent plain `docker run`:

```bash
docker run -d --name backup-node --restart unless-stopped \
  -e BACKUP_HOST=backup.example.com \
  -e ENROLLMENT_TOKEN=<one-time-token> \
  -e NODE_LABEL=pi-garage \
  -e STORE_DIR=/store \
  -e MINISIGN_PUBKEY=/config/minisign.pub \
  -v /srv/backup-node/store:/store \
  -v /srv/backup-node/minisign.pub:/config/minisign.pub:ro \
  ghcr.io/scottfridwin/offsite-backup-manager-node:latest run
```

On first start, the agent redeems `ENROLLMENT_TOKEN` against `BACKUP_HOST`,
persists its durable pull credential under `STORE_DIR` (so it is never needed
again — subsequent restarts reuse it and ignore `ENROLLMENT_TOKEN` if still
set), then begins the pull + heartbeat loop. No further interaction is needed:
restart the container/reboot the device and it resumes automatically.

## 5. Confirm enrollment (operator, on the Primary)

```bash
primary nodes -roster /data/roster.json
```

The new node should appear `active`; `last_seen` and `last_run` populate after
its first heartbeat (within one `PULL_INTERVAL`).

## 6. Capacity planning

Before — or shortly after — bringing a node online, check projected growth and
how long its media will last:

```bash
primary capacity -output /out -roster /data/roster.json
```

See §7.3 for the cadence-vs-media tradeoff; run cadence (how often `primary
package` runs) is the main lever if a node's projected time-to-full is too
short for its media.

## 7. Decommissioning a node

```bash
primary retire-node -roster /data/roster.json <node-id>
```

This revokes the node's pull token — it can no longer authenticate or be
assigned future runs. Its existing data remains on its own media; the Primary
has no path to reach or wipe it (§5.4, §7.1). Physically retire/wipe the
device's media yourself when ready.
