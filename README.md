# offsite-backup-manager

A containerized service that propagates a homelab server's on-site backups to one
or more cheap, standalone, **offsite backup nodes** (e.g. a Raspberry Pi plugged in
at a remote site). The orchestrator compresses, encrypts, and signs the latest
backups into a single package per run; nodes **pull** that package over outbound
HTTPS and store it as **write-once (WORM)** data.

> **Status:** design phase. The full requirements & design brief lives in
> [`docs/requirements.md`](docs/requirements.md). No implementation yet.

## Why

Homelab backups are only as safe as their worst single-site failure. This project
gets encrypted copies **off-site** with:

- **Zero-touch nodes** — a node boots, dials home, enrolls itself, and starts
  pulling. No interaction at the remote site, no inbound ports there.
- **No external dependencies** — just the Primary (behind your reverse proxy) and
  the nodes. No mesh/VPN coordinator, no third-party relay.
- **Compromise-resistant** — packages are encrypted to a public key whose private
  identity never lives on the server; nodes only ever hold ciphertext.
- **WORM by construction** — the Primary can never delete or modify node data (nodes
  pull; the Primary has no write path to them).

## Architecture (summary)

```
            (outbound HTTPS only)
  [Backup Node] ──GET manifest + packages──▶ https://backup.<your-domain>  [Primary]
       │  per-node token, read-only pull, append-only store                    │
       └──────────────── never any inbound to the node ◀──────────────────────┘
```

- **Primary orchestrator** — self-schedules runs, snapshots the backup source tree,
  builds one compressed + encrypted + signed package per run, and serves it read-only
  behind your reverse proxy.
- **Node agent** — enrolls once, then periodically pulls and verifies packages into
  an append-only store, and reports a heartbeat (liveness + free space).

See [`docs/requirements.md`](docs/requirements.md) for the detailed design,
threat model, and decisions.

## Repository layout (planned)

This is intended to become a **monorepo** that builds and publishes both sides as
container images to GitHub Container Registry:

- `ghcr.io/scottfridwin/offsite-backup-manager-primary` — the Primary orchestrator.
- `ghcr.io/scottfridwin/offsite-backup-manager-node` — the node agent.

(Image names are provisional and may change before the first release.)

## License

[Apache-2.0](LICENSE).
