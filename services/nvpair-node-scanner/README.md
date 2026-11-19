<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-scanner

**The node discovery daemon.** It advertises this host on the LAN, discovers
connection candidates, and maintains the node directory. The broker registers
local service ports with its registry, and consumers query or subscribe to its
directory through the broker.

Concretely, the daemon:

- **Advertises** this node's one `_nvpair-node._tcp` record, built from a registry of the local services its parent (the broker) registers — `ni`/`ol`/`lm`/`er`/`wl`/`cl`/`em`/`ec` ports plus the node identity (`uuid=`, `cluster-uuid=` when clustered, `ip=`). One record per node covers every service.
- **Browses** `_nvpair-node._tcp` and tracks connection candidates by
  `discoveryRecordId`. Announcements require a nonempty `uuid`. An optional
  `cluster-uuid` remains an unverified claim even when it differs from `uuid`;
  those discoveries can receive display-only hardware previews. Missing host
  UUIDs and claims to the local host remove only the cached discovery observation
  and its previous peer association. Claimed UUIDs, names, and addresses cannot
  select a local or verified peer record.
- **Authenticates peers** using exact certificate pins from
  `nvpair-shared/clustertrust`. A successful pinned mTLS node-info read must
  report `hostUuid` and `clusterUuid` matching the serving certificate's node UUID,
  along with complete peer metadata.
  The authenticated name, addresses, ports, and hardware form a distinct
  `pinned` peer record, replacing only that successful discovery. Failed
  candidates stay separate and `unverified`. Public HTTP previews never supply
  verified inventory or scheduling measurements, and operator CAs cannot
  substitute for cluster pins.
  Node-info responses are limited to 64 KiB.
  Verified record IDs are opaque and change with the local certificate or
  admission and the peer's exact pin or admission. Host UUIDs remain stable.
  Further announcements from that observation stay hidden while its
  authenticated association is current and its host UUID and advertised cluster
  identity are unchanged. Changing either UUID claim, including adding or clearing
  the cluster identity, clears the observation's previous association.
  Changed claims require authentication before updating the verified record.
- **Refreshes models** every 15 seconds from verified endpoints, preserving
  the last successful inventory during outages while its authenticated proof,
  addresses, and engine-manager port remain unchanged. A changed source clears
  all model inventories and schedules an immediate refresh when engine-manager
  is present. A newly authenticated peer also schedules a refresh. Remote model
  lists use pinned mTLS at the authenticated engine-manager port. Local models use the owned
  engine-manager over loopback; local hardware comes from private worker IPC.
  Local registry publications use retained models and wake the model sweep,
  so an HTTP read cannot hold the lock needed by owned hardware notifications.
  Model inventory decoding reads at most 16 MiB. A JSON value that extends past
  that limit fails enrichment and leaves the last successful inventory intact.
  `downloadedByEngine` retains downloaded models for engines such as vLLM even
  while they are stopped; `modelsByEngine` records the models they serve.
  Queued and in-flight model results are discarded if the peer's authenticated
  address list changes, including a secondary address used for failover.
- **Samples scheduling telemetry** from healthy nodes every two seconds with
  stable per-node jitter. Consecutive remote failures back off to 4, 8, 16, then
  30 seconds and reset on success. Local inventory and samples arrive every two
  seconds from the owned node-info worker over private broker/worker IPC.
  Each accepted peer read updates directory metadata and hardware and emits
  compact scheduling telemetry and a full authenticated hardware snapshot.
  Both outputs use the same sample with its elapsed age. Local IPC also updates
  changed hardware inventory.
- **Collects unpaired hardware previews** on a separate two-second loop with
  at most four public HTTP requests in flight across all nodes and addresses,
  and failure backoff capped at 30 seconds. Public HTTP reads have
  a 1.5-second total deadline, a 64 KiB body limit, no redirects, and address
  failover. Sanitized hardware is emitted as `discovery:node-info` under the
  current unverified discovery record ID. It never updates directory identity,
  verified hardware, liveness, models, or scheduling. The broker delivers
  previews to desktop and TUI through node snapshots and hardware updates.
- **Follows live trust:** a pin addition schedules authentication. Pin removal,
  replacement, or local departure removes the verified row and its scanner
  caches, restores its known observations as unverified pairing candidates,
  and drops affected idle connections. Requests recheck the current pin
  and serving identities before accepting results, including reused connections
  and reads already in progress.
- **Advertises live membership:** the same reload re-derives this node's own `cluster-uuid=` from the cluster dir and re-advertises when it differs from what is being published, so joining or leaving a cluster takes effect without a restart.
- **Expires observations independently of peer availability:** twelve missed
  five-second scans remove an observation and its candidate caches. Announcement
  changes and removals cannot directly remove a verified peer, change its saved
  pins, or close its connections. The canonical sampling loop continues reading
  the peer independently of multicast. Once no discovery remains associated with
  the peer, a failed pinned mTLS read removes its record and caches only after a
  minute without authenticated inventory or approved inference activity.
  Membership and pins remain. An open TCP port or another discovery claiming
  that UUID cannot keep the peer alive.
- **Allows grace for empty scans:** with multiple known nodes, up to six
  consecutive empty scans receive a shared grace period of about 30 seconds.
  Later empty scans count toward each record's miss threshold. This allows for
  local multicast receive delays under load. A single known peer follows the
  ordinary miss threshold.
- **Separates pending authentication from telemetry:** candidates have their own
  bounded workers and retry state. Slow candidates cannot occupy verified-peer
  telemetry workers or replace their polling targets. Authentication and
  telemetry failures use capped backoff. Requests have deadlines,
  response-size limits, address failover, and one connection per node-info
  origin. Requests never follow redirects to another endpoint or transport.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0. Stdio by default; `--ipc <path>` switches to a Unix domain socket or Windows named pipe.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`) for pinned peer authentication and local identity |
| `--log-level <level>` | `info` | `error` / `warn` / `info` / `debug` |
| `--version` | | Print version and exit |

## JSON-RPC surface

### Requests (caller → daemon)

| Method | Params | Result |
|---|---|---|
| `discovery:register` | `{service, port, txt?}` | `{ok:true}` — record a local service so it's folded into this node's `_nvpair-node` record |
| `discovery:unregister` | `{service}` | `{ok:true}` — drop a local service |
| `discovery:update-txt` | `{service, port, txt?}` | `{ok:true}` — same as register with a new TXT |
| `discovery:get-nodes` | `{service?}` | `{nodes:[DirectoryNode]}` — the directory, optionally filtered to nodes advertising one service |
| `discovery:reload-identity` | — | `{ok:true}` — re-resolve and re-advertise this node's identity |
| `discovery:reload-trust` | — | `{ok:true}` — invalidate stale peer proofs and schedule authentication against current pins |
| `discovery:set-observed-addresses` | `{addresses}` | `{ok:true}` — the local addresses peers have actually reached this node on, so address selection can rank a peer-proven address above an inferred one |
| `shutdown` | — | `null` |

`log/set-level` is also accepted.

These notifications are accepted rather than requests, because the daemon has
nothing to answer and the caller must not wait:

- `discovery:node-activity` `{hostUuid, msSince}` — a peer's engine returned
  inference response bytes `msSince` ms ago. Counts as proof of life and cancels
  that node's eviction. The broker sends this while inference is streaming, so an
  id-bearing call would cost it a pending entry and a blocked goroutine per
  report in exchange for an ack nobody reads.
- `log/set-level` (above).
- `discovery:local-node-info` `{info, capturedAt}` — hardware received by the broker from its
  own node-info worker. Scanner attaches it only to the matching local registry
  record, updates inventory, and emits compact scheduling telemetry. Owned
  subprocess IPC establishes the measurement source. A browsed UUID or loopback
  address cannot enter this private path. Registration changes retain the owned
  inventory; samples retain their elapsed age during worker silence. The private
  capture time uses Unix milliseconds on the local host clock, so blocked pipe
  delivery counts toward GPU age. Missing or future capture times cannot supply
  fresh GPU telemetry.

### Notifications (daemon → caller)

Emitted as the directory changes. Each carries the affected `DirectoryNode` (identity, advertised service ports, enrichment, `trusted`).

Records also carry `discoveryRecordId` and `identityVerification`. The local registry
record is `local`; browsed records are `unverified`, including when their claimed
principal has a pin. A successful read creates a distinct opaque peer record
with `pinned` verification and `trusted: true`. Failed discoveries cannot alter
that record. Discovery record IDs identify candidates independently of UUID and
principal claims; address order does not affect the ID.

- `discovery:node-discovered` — a new node entered the directory
- `discovery:node-updated` — a known node's record or enrichment changed. A re-observation that changes nothing emits no event, so `lastSeen` is a last-*change* stamp rather than a liveness heartbeat
- `discovery:node-removed` — a directory record was removed (identity + last-known fields)
- `discovery:node-telemetry` — compact maximum-GPU utilization, validity, and
  sample age for broker-internal scheduling
- `discovery:node-info` — a full local or pinned hardware snapshot, or an
  unverified display-only preview, with its discovery record ID and verification.
  The broker retains the latest source observation for `AvailableNode.info`
  snapshots and forwards live updates. Preview identity and registry claims are
  stripped and cannot change verified service state.

A one-shot `ready` notification carries the daemon version at startup. The
broker repopulates the service registry on every scanner spawn.

## Shutdown

The daemon shuts down on:
- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`
- `shutdown` JSON-RPC request
