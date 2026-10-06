# Offsite Backup System — Requirements & Design Brief

Status: **Draft for review**
Date: 2026-10-06

---

## 1. Purpose & scope

Design a containerized **backup orchestration service** that runs on the Primary
server and ships the on-site backups to one or more **offsite backup nodes**
(cheap, standalone, zero-touch devices such as a Raspberry Pi).

This system's job is **offsite propagation of already-produced backups** — it does
**not** produce the per-service backups themselves. It consumes the existing
on-Primary backup directory structure (the output of each service's own backup
job) and distributes it.

### In scope
- Compressing + encrypting a single combined **backup package** per run from the
  specified backup source directory.
- Transporting that package offsite to one or more backup nodes.
- Zero-touch backup-node enrollment, registration, and lifecycle.
- Distribution schemes (replicate-all vs. round-robin) across nodes.
- WORM (write-once) guarantees at the node.
- Scheduling, monitoring, and capacity alerting.
- Documented restore procedure.

### Out of scope (explicitly)
- Creating per-service backups on Primary.
- Multi-directory backup — one top-level backup directory assumed.
- Orchestrated restore

---

## 2. Definitions

| Term | Meaning |
|------|---------|
| **Primary** | The source server that owns the authoritative backup tree and runs the orchestrator. |
| **Backup node** (node) | A standalone offsite device that stores encrypted backup packages. Zero-touch, no user interaction. |
| **Source tree** | The Primary's entire backup source directory, e.g. `/backups`. |
| **Run** | One scheduled execution of the orchestrator that produces exactly one package. |
| **Backup package** | A single compressed + encrypted artifact per run, containing a full copy of the source tree. |
| **Manifest** | Signed metadata describing a run: package name, size, checksum, per-service contents, timestamp, and node assignment. |
| **WORM** | Write-Once-Read-Many. Once a package lands on a node it can never be modified or deleted by the Primary. |

---

## 3. Source selection & package format

### 3.1 Selection rule — **whole-tree snapshot** (confirmed)
Each run captures the **entire backup source directory as-is** (`BACKUP_SOURCE_DIR`,
e.g. `/backups`) — every service, every file, including full + incremental backup
chains. No per-file selection logic.

Rationale: some services produce **incremental** backups that are only restorable
together with the full backup they depend on. Cherry-picking within a backup would
break those chains. Copying the whole tree is simpler, chain-safe, and
self-contained — at the cost of faster storage growth, which is accepted given a
typically small source tree (§7.3).

Consistency: the source directory may live on any filesystem, including a network
mount (e.g. NFS) with **no copy-on-write snapshot** available, and its contents are
written by **heterogeneous third-party backup tools** that cannot be assumed to
write atomically. A naive whole-tree capture could therefore race a producer that is
mid-write. Two common properties make this tractable without touching the producers:

- Many producers write each backup into a **new timestamped file/directory**, so
  only the single in-progress newest entry can ever be partial; all prior entries
  are complete and immutable.
- Backup jobs typically run on their own schedule within a predictable window (often
  overnight), leaving long quiet periods.

So the design does **not** require producer changes. Instead:

1. **Quiet-window scheduling (primary measure):** schedule the run in a window when
   no producer is active, so capture never races a write.
2. **Stage-and-quiesce (safety net):** before packaging, the orchestrator stages the
   source directory to a local work dir (rsync) and confirms stability — no
   in-progress/temp markers and a second rsync pass that transfers nothing
   (sizes/mtimes unchanged) — then packages the **staged copy**, never the live tree.
   If the tree won't quiesce it retries, then aborts (reporting unhealthy, §9.2).

Together these give per-file completeness with **zero changes to producers**.
Cross-file atomicity across an entire run isn't guaranteed, but for an infrequent
backup-of-backups a mixed-but-complete snapshot is the acceptable worst case.

### 3.2 Package unit — **single whole-tree package per run**
Each run bundles the full `/backups` tree into **one** package:

```
run-YYYYMMDDTHHMMSSZ.tar.zst.age
```

- **Bundle**: tar of the entire source tree plus a generated `manifest.json`.
- **Compress**: `zstd` (best-effort; many service backups may already be compressed/
  incompressible).
- **Encrypt**: `age` (see §6). The encrypted package is the only thing written offsite.
- A detached **manifest** (plaintext metadata + checksum, signed) is also published
  so nodes and operators can verify/track without decrypting.

> **Capacity note:** a full-tree package per run + "never prune" (§7) means storage
> grows by one full-tree package every run, so **run cadence** is the primary lever on
> node storage lifetime. See §7.3.

---

## 4. Architecture & transport

### 4.1 The core constraint
A node lives behind a friend's home router: **no inbound ports, NAT'd, dynamic IP.**
The node can reach the Primary (outbound) over the public Internet via a
configurable DNS name; the Primary cannot reach into the node's LAN.

### 4.2 Decision: node-pull over authenticated public HTTPS — **no VPN** *(chosen)*

```
            (outbound HTTPS only — traverses any home NAT unaided)
  [Backup Node] ──GET manifest + packages──▶ https://${BACKUP_HOST}  [Primary]
       │  node authenticates with its per-node token, pulls read-only        │
       └────────────────── (never any inbound to the node) ◀─────────────────┘
```

1. The node is the **only** party that initiates connections. It makes ordinary
   **outbound HTTPS** to the Primary's public endpoint (served through the Primary's
   existing reverse proxy). Outbound HTTPS traverses any home router with **zero**
   configuration, so the node needs no inbound ports, no static IP, and **no VPN**.
2. The node **pulls** from a **read-only** (GET/HEAD) endpoint, authenticating with a
   per-node credential issued at enrollment. The Primary never connects to the node.

#### Why this option (and why not a mesh)
- **No external dependencies**: everything is the Primary's own reverse proxy + the
  node's HTTPS client. No coordinator, no relay service, no third party — which is a
  hard requirement.
- **NAT traversal is free**: because the node only ever dials *out*, there is nothing
  to punch through; a mesh/VPN (WireGuard/Headscale/Tailscale) existed only to give
  the Primary a route *into* the node, which the pull model doesn't need.
- **Scales to any node count trivially**: each node is just another HTTPS client with
  its own token; nothing to peer or coordinate (fits “up to ~3 now, N later”).
- **WORM by interface**: the node runs **no** inbound service at all — it exposes
  nothing writable/deletable and keeps an append-only store (§7.1). The Primary holds
  no route or credential to mutate node storage.
- **Defense in depth**: the public endpoint serves only **already-encrypted**
  packages (§6) behind per-node auth + TLS + rate limiting, so exposure of the
  endpoint never exposes plaintext.

Rejected alternatives:
- *Self-hosted Headscale / WireGuard mesh* — removed. It added a control-plane (and,
  for Tailscale, DERP relays) that isn't needed once the node only pulls; its sole
  remaining benefit was the Komodo nice-to-have (§10), not worth a mesh.
- *Primary pushes* — requires the Primary to reach into the node and hold write
  access, breaking the no-inbound and WORM constraints. Rejected.

### 4.3 Pull mechanism
- Node runs a periodic **append-only pull**: authenticated HTTPS GET of the
  **manifest** first, then of any packages it is responsible for (per distribution
  scheme, §8) that it does not already hold.
- Node **verifies the minisign-signed manifest and per-package checksum** before and
  after download; already-present packages are never re-fetched or overwritten.
- The endpoint is read-only (GET/HEAD only); node auth is a per-node bearer token
  (high-entropy) over TLS issued at enrollment. Optional hardening: mutual TLS
  (per-node client cert) if desired — not required since payloads are encrypted.

### 4.4 Deployment & hosting on the Primary (reverse proxy)
The orchestrator is a standard container, deployed however the operator runs their
other services. Its node-facing endpoint is published through the operator's
**reverse proxy**.

- **Routing**: a router on a host such as `backup.<your-domain>`. The host is
  **not hardcoded** — it comes from the operator's existing domain/proxy
  configuration at deploy time (see §11).
- **No SSO on node routes**: the node-facing routes must **bypass any browser SSO /
  forward-auth** the proxy applies to human-facing apps. Nodes are headless and
  authenticate with a per-node bearer token enforced **inside** the orchestrator.
  Apply rate limiting to blunt scraping/abuse.
  - *Future option (not v1):* instead of bypassing SSO, issue each node a
    **service-account credential** from the operator's IdP (OAuth2 client-credentials
    grant, or a machine API token / mTLS client cert) and let the proxy's
    forward-auth validate it. This folds node auth into existing SSO without an
    interactive login; the in-app per-node token remains the v1 baseline.
- **Mounts**: bind-mount the backup source directory **read-only** and a scratch dir
  for staging/packaging.
- **Read-only exposure**: only GET/HEAD routes (`/enroll`, `/manifest`, `/packages/*`,
  heartbeat) are served; nothing allows writing/deleting node-side data.

---

## 5. Backup-node lifecycle (zero-touch)

### 5.1 Provisioning (documented, one script)
Deliverable: a documented image/setup script so building a node is "flash + boot."
The flashed node carries only:
- The Primary's backup URL, supplied as an **environment variable** on the node
  (e.g. `BACKUP_HOST=backup.example.com`) — this is the node's only pointer home and
  is nothing more than runtime config (no value is baked into the image).
- A one-time **enrollment token** (single-use, expiring) — the only bootstrap secret.
- The node agent (container) + its auto-start unit.

No per-node manual configuration beyond writing those two values before first boot.

### 5.2 Enrollment & registration flow (no user interaction on node)
1. On first boot the node calls `POST https://${BACKUP_HOST}/enroll` (outbound HTTPS)
   with the one-time enrollment token and a self-chosen hostname/label.
2. The Primary validates and **burns** the token, generates a durable **per-node pull
   token** (and optional client cert), records the node in the **roster**, and
   returns the pull credential.
3. Orchestrator marks the node **active**.
4. Node stores its credential locally and begins the heartbeat + append-only pull
   loop against `https://${BACKUP_HOST}`.
5. Node is included in the selected distribution scheme on the next run.

### 5.3 Heartbeat & health
- Node periodically reports liveness, free space, and last-synced run via an
  authenticated outbound POST. Primary records last-seen, capacity, and sync lag per
  node, which feeds the container healthcheck (§9.2).

### 5.4 Decommission
- An operator action revokes the node's pull token (and roster entry) and marks it
  retired. (The node's existing data remains WORM on the node's own media; the
  Primary cannot and does not wipe it.)

---

## 6. Encryption & key custody — **asymmetric (`age`)**  *(recommended)*

- The Primary encrypts every package to a **public recipient** (`age` X25519, or
  age-ssh recipient). The **private identity is never present on the Primary** in
  normal operation.
- The private identity is held **offline in the password manager** (plus one
  independent offline copy on separate media), for restore only. Decryption happens
  on a trusted recovery host during DR, never on a node and never routinely on the
  Primary.

#### Why
- A **compromised Primary cannot read historical backups** — it only ever holds the
  public recipient. This is the key advantage over a symmetric key that must be
  present at backup time.
- A **curious or compromised node/friend** sees only ciphertext; nodes never receive
  a decryption key.
- **Integrity**: the manifest is additionally signed with **minisign** (small,
  dedicated, well-audited Ed25519 signing) so nodes and operators can verify
  authenticity without decrypting. The minisign public key is distributed to nodes;
  the signing secret key lives on the Primary (compromise of it only allows forging
  manifests, never decrypting data).

#### Key management requirements
- Recipient public key is configuration (committed/declared); losing it is harmless.
- The private identity's loss = **total loss of recoverability** → it must be
  escrowed in at least two independent offline locations and its existence verified
  on a schedule.
- Key rotation: support adding a new recipient going forward without needing to
  re-encrypt historical packages (old packages stay readable by the old identity).

---

## 7. Retention, capacity & WORM

### 7.1 WORM guarantee (hard requirement)
- **The orchestrator has no deletion or modification capability over node data — by
  construction (pull model, §4.2), not merely by policy.**
- On the node, the package store is **append-only**: the pulling identity can create
  new files but cannot unlink or overwrite existing ones (enforced via filesystem
  permissions / append-only semantics). Goal: contamination of the Primary cannot
  propagate destructively offsite.

### 7.2 Retention — **never auto-prune; size to hold everything** (confirmed)
- No automated pruning by the Primary or the node agent. Nodes are provisioned with
  enough media to hold the full intended history.
- Any eventual reclamation is a **manual, local, operator-initiated** action on the
  node — never driven by the Primary.

### 7.3 Capacity planning (required because of 7.2)
Each run stores a full-tree package and nothing is pruned, so storage grows linearly
and unbounded. Sizing inputs are operator-specific; the orchestrator derives them
from live measurements:

- Package size ≈ source tree size (service backups are often already compressed, so
  budget on raw size rather than expecting much zstd shrink).
- Growth per year = package size × runs per year.

Indicative node lifetime (replicate-all). The example assumes a ~20 GB package to
illustrate the cadence/media tradeoff — substitute your own measured size:

| Cadence | Runs/yr | ~Growth/yr (20 GB/run) | 256 GB card | 1 TB card |
|--------|---------|-----------|-------------|-----------|
| Daily | 365 | ~7.3 TB | ~12 days | ~50 days |
| Weekly | 52 | ~1.0 TB | ~12 weeks | ~1 yr |
| Monthly | 12 | ~240 GB | ~1 yr | ~4 yr |

Therefore:
- The system must **compute and publish projected growth** (avg package size × runs
  per horizon) and each node's **time-to-full** from live free-space telemetry.
- **Run cadence is the primary capacity lever** and must be easily configurable;
  the table shows frequent runs are impractical on small SD media — less frequent
  cadences or larger media are the realistic choices for a small node.
- Nodes must **alert well before full** (§9) and **refuse/queue** rather than
  corrupt state when space is exhausted.

---

## 8. Distribution schemes (multi-node)

Selectable via configuration, applied per run and encoded in the manifest:

- **Replicate-all**: every active node is responsible for every run's package.
  Maximum durability; storage cost = N × history.
- **Round-robin**: each run is assigned to the next node in rotation; a node only
  pulls runs assigned to it. Maximum total capacity; each package exists on one node
  (lower durability per package).

Requirements:
- Scheme is a single config switch; changing it affects **future** runs only.
- Assignment is recorded in the manifest so each pulling node deterministically
  knows its responsibilities.
- Node roster changes (enroll/retire) are handled gracefully: round-robin rotation
  accounts for the current active set at run time.
- (Optional future) `replication_factor = k` for round-robin (each run to *k* of
  *N* nodes) as a middle ground — noted, not required for v1.

---

## 9. Scheduling, monitoring & alerting

### 9.1 Scheduling — **self-contained, orchestration-agnostic** (chosen)
- The orchestrator does its **own** scheduling internally (e.g. a `SCHEDULE` cron
  expression it reads and acts on), so it works under **any** orchestration system
  (plain Compose, systemd, Kubernetes, a container orchestrator, etc.). It does
  **not** depend on any external scheduler.
- **Run trigger is Primary-local and decoupled from nodes.** The scheduler fires
  inside the Primary orchestrator and builds + publishes the package entirely on the
  Primary (stage → tar → zstd → encrypt → sign); this step never contacts a node and
  does not depend on any node being reachable. Delivery is the separate,
  node-initiated pull (§4.2–§4.3) on each node's own interval. The outbound-only
  nodes therefore constrain only *delivery*, never the *trigger*.
- Default: a run in a **quiet window** when no source producer is active (§3.1), at a
  configurable cadence. Cadence is chosen to fit node media (§7.3).
- Each node independently polls/syncs on its own interval; a node offline during a
  run simply catches up on its next poll (eventual consistency).
- Both intervals are configuration.

### 9.2 Monitoring / alerting — **container healthcheck (primary), node agent alert (optional)** (chosen)
The orchestrator is **orchestration-agnostic**, so it surfaces problems through a
standard **Docker `HEALTHCHECK`** rather than calling any specific alerting API.
Whatever runs the container (Compose, a container orchestrator's container-unhealthy
alerting, Kubernetes liveness, etc.) then surfaces and routes the alert.

- **Healthy** means: the most recent scheduled run **built + published** successfully
  **and**, within a configurable grace window, the **required** set of nodes has
  **confirmed pulling + verifying** that run (all nodes for `replicate-all`; the
  assigned node for `round-robin`, per §8) via their heartbeat (§5.3).
- **Unhealthy** (the container reports failing) when: the build/package step failed,
  **or** no run has succeeded within the expected interval, **or** after the grace
  window **no eligible node** has confirmed the latest run — i.e. "unable to get this
  backup to any node." This is the signal the operator's existing health management
  turns into an alert.
- Rationale: this needs **nothing on the node** and no coupling to a specific
  alerting system, so it works on day one regardless of orchestrator.

**Node-side drive-space alert (optional, needs a monitoring agent on the node):** a
direct per-node capacity warning — "this backup node's disk is filling" — is best
delivered by the operator's **existing monitoring/orchestration agent running on the
backup node**, reporting through their existing alerting. That requires the
agent-on-node nice-to-have (§10) and so is **not guaranteed in v1**. Until then, a
node's free space is still reported in its heartbeat and folds into the Primary
healthcheck above (the Primary can go unhealthy if a node reports critically low
space), but a dedicated per-node disk alert awaits an agent on the node.

---

## 10. Monitoring-agent-on-node integration (nice-to-have)

- **Primary motivation**: a **per-node drive-space alert**. Running the operator's
  existing monitoring/orchestration agent on each backup node lets the node's
  disk-usage warning flow through their **existing alerting**, giving a direct "this
  node is filling up" alert instead of only inferring it from the Primary healthcheck
  (§9.2). Centralized node management (updates, restarts) is a secondary benefit.
- This requires the Primary side to reach **into** each node, i.e. reverse
  connectivity the base design deliberately avoids (no inbound at the node, no VPN).
  It is therefore **deferred / out of v1 scope**. If wanted later it can be added
  without changing the backup path by having the node open its **own** outbound
  connection to a Primary-side control channel (e.g. a node-initiated reverse
  tunnel) — still no inbound at the remote site.
- **WORM caveat**: any such control path lets a compromised Primary reach node
  storage, weakening the WORM guarantee of §7.1. Nodes that must stay strictly WORM
  should run **only** the outbound pull client and expose nothing mutable.

---

## 11. Configuration model

All configuration via **environment variables and/or flat text files** (no UI, no
database of record beyond the node roster file). Indicative set:

### Primary (orchestrator)
On the Primary the public hostname is **not** an app env var — it's part of the
reverse-proxy routing configuration (§4.4), derived from the operator's existing
domain setup. Only the *node* needs a pointer-home value (`BACKUP_HOST`).

| Key | Purpose |
|-----|---------|
| `BACKUP_SOURCE_DIR` | Root of the backup source tree to snapshot in full (e.g. `/backups`). |
| `PACKAGE_WORK_DIR` | Scratch for staging + building packages (regenerable → `/work`). |
| `AGE_RECIPIENT` | Public recipient(s) the package is encrypted to. |
| `MINISIGN_SECKEY` | Primary's manifest-signing secret key (sign only). |
| `DISTRIBUTION_SCHEME` | `replicate-all` \| `round-robin`. |
| `SCHEDULE` | Cron expression for run cadence (default monthly, midday). |
| `ENROLL_TOKEN_TTL` | Expiry for single-use node enrollment tokens. |
| `NODE_ROSTER_FILE` | Flat file listing enrolled nodes + assignments. |
| `HEALTH_RUN_INTERVAL` | Max age of the last successful run before the container reports unhealthy (§9.2). |
| `HEALTH_SYNC_GRACE` | Grace window for required nodes to confirm the latest run before unhealthy (§9.2). |
| `NODE_SPACE_CRITICAL_PCT` | Node free-space level that (via heartbeat) drives the Primary unhealthy (§9.2). |

### Node (agent)
| Key | Purpose |
|-----|---------|
| `BACKUP_HOST` | Primary endpoint the node pulls from (e.g. `backup.example.com`). The node's only pointer home; runtime env var, not baked into the image. |
| `ENROLLMENT_TOKEN` | One-time bootstrap secret (consumed on enroll). |
| `NODE_LABEL` | Human-friendly node name. |
| `PULL_INTERVAL` | How often the node polls/syncs. |
| `STORE_DIR` | Append-only package store (WORM). |
| `CAPACITY_WARN_PCT` | Local free-space warning threshold. |

---

## 12. Security & threat model (summary)

| Threat | Mitigation |
|--------|-----------|
| Primary compromised → wants to destroy offsite copies | Pull-only model + node exposes **no inbound service at all** + append-only store (§4.2, §7.1). |
| Primary compromised → wants to read backups | Asymmetric encryption; Primary holds only the public recipient + a sign-only minisign key (§6). |
| Node media inspected or stolen (e.g. by whoever hosts the node) | Node stores **ciphertext only**; no decryption key present (§6). |
| Node impersonation / rogue enrollment | Single-use, expiring enrollment tokens; durable per-node pull token (optional mTLS); roster is operator-governed (§5.2). |
| Public pull endpoint attacked / scraped | Per-node token + TLS + rate limiting; endpoint is read-only and serves only encrypted packages (§4.2, §6). |
| Tampering with packages in transit or at rest | minisign-signed manifest + per-package checksums verified on pull (§6, §9.2). |
| Node storage exhaustion → silent data loss | Never-prune + capacity projection + pre-full alerting + refuse-rather-than-corrupt (§7.3). |
| Inbound exposure at the remote site | None required; node is outbound-only (§4). |

---

## 13. Resolved decisions & remaining open items

Resolved:
1. **Transport**: **no VPN/mesh** — node-pull over authenticated **public HTTPS**
   (node dials out only), served by the Primary's existing reverse proxy (§4.2).
2. **Pull protocol**: authenticated **HTTPS GET**, per-node bearer token (§4.3).
3. **Manifest signing**: **minisign** (Ed25519); decryption identity (`age`) lives in
   the password manager (§6).
4. **Cadence / media**: configurable cadence chosen to fit node media; node sized to
   hold the full retained history (§7.3).
5. **Whole-tree package**: each run is a full, self-contained source-tree snapshot
   (chain-safe for incrementals); a corrupt package loses only that run and the
   series self-heals next run (§3).
6. **Restore verification**: **manual, documented** restore drill run ad hoc — no
   automated scheduled verification in v1 (§6, §14).
7. **No hardcoding**: the public hostname comes from the operator's reverse-proxy/
   domain configuration; the node's only pointer home is its `BACKUP_HOST` runtime
   env var. Nothing is baked in (§4.4, §11).
8. **Consistency** (source may be on a snapshot-less network mount; heterogeneous
   third-party producers): **quiet-window scheduling** + orchestrator
   **stage-and-quiesce**, with **no producer changes** (§3.1).
9. **Hosting**: deployed as a container behind the operator's **reverse proxy**, with
   node routes bypassing browser SSO and rate-limited (bearer token enforced in-app)
   (§4.4). The container is **orchestration-agnostic** — no dependency on any specific
   orchestrator.
10. **Scheduling**: the orchestrator schedules its **own** runs internally
    (`SCHEDULE` env), independent of any external scheduler (§9.1).
11. **Alerting**: primary signal is a standard **Docker `HEALTHCHECK`** — the
    container reports **unhealthy** when it can't build a run or can't get the latest
    run confirmed to any eligible node, so **any** orchestrator's health management
    surfaces it (§9.2). A dedicated **per-node drive-space alert via a monitoring
    agent on the node** is the nice-to-have (§10), not guaranteed in v1.

Remaining open items:
- **Hostname**: pick the node-facing hostname and wire it into the reverse proxy.
- **Node heartbeat transport**: confirm nodes report health (incl. free space) by a
  periodic authenticated outbound POST that the orchestrator records for its
  healthcheck (no inbound to node).

---

## 14. Phased roadmap (proposed)

1. **Phase 0 — spec sign-off** (this document).
2. **Phase 1 — Primary-side packaging**: whole-tree bundle, zstd, age-encrypt,
   minisign-signed manifest, local output. No nodes yet. Verifiable by decrypting
   locally with the password-manager identity.
3. **Phase 2 — single node**: enrollment over public HTTPS + append-only HTTPS pull +
   heartbeat, replicate-all with N=1. Prove end-to-end + a manual test restore.
4. **Phase 3 — multi-node + schemes**: roster management, round-robin, and the
   healthcheck gating on required-node confirmation (§9.2).
5. **Phase 4 (optional, deferred) — monitoring-agent-on-node**: node-initiated reverse
   control channel so each node's drive-space alert flows through the operator's
   existing alerting (§10), only if wanted (with the WORM caveat).
6. **Phase 5 — hardening**: capacity projection, documented restore-drill runbook,
   docs for building a node.