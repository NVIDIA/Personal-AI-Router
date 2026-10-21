<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-scanner

**The consolidated discovery daemon.** Promoted from a plain node-info browser: it is now the single place this host advertises itself on and discovers the LAN through. Every other service registers its port with this daemon instead of advertising its own mDNS service, and consumers query/subscribe to it instead of browsing themselves.

Concretely, the daemon:

- **Advertises** this node's one `_nvpair-node._tcp` record, built from a registry of the local services its parent (the broker) registers — `ni`/`ol`/`lm`/`er`/`wl`/`cl`/`em`/`ec` ports plus the node identity (`uuid=`, `cluster-uuid=` when clustered, `ip=`). One record per node covers every service.
- **Browses** `_nvpair-node._tcp` and tracks connection candidates by
  `discoveryRecordId`. Claimed UUIDs, names, and addresses cannot select a local
  or verified peer record.
- **Authenticates peers** using exact certificate pins from
  `nvpair-shared/clustertrust`. A successful pinned mTLS node-info read must
  report matching host and certificate UUIDs and complete peer metadata.
  The authenticated name, addresses, ports, and hardware form a distinct
  `pinned` peer record, replacing only that successful discovery. Failed
  candidates stay separate and `unverified`. Public HTTP is never a scanner
  measurement source, and operator CAs cannot substitute for cluster pins.
  Verified record IDs are opaque and change with the local certificate or
  admission and the peer's exact pin or admission. Host UUIDs remain stable.
- **Refreshes models** every 15 seconds from verified endpoints, preserving
  the last successful inventory during outages. A newly authenticated peer
  schedules a model refresh immediately. Remote model lists use pinned
  mTLS at the authenticated engine-manager port. Local models use the owned
  engine-manager over loopback; local hardware comes from private worker IPC.
  Local registry publications use retained models and wake the model sweep,
  so an HTTP read cannot hold the lock needed by owned hardware notifications.
- **Samples scheduling telemetry** from healthy nodes every two seconds with
  stable per-node jitter. Consecutive remote failures back off to 4, 8, 16, then
  30 seconds and reset on success. Local inventory and samples arrive every two
  seconds from the owned node-info worker over private broker/worker IPC.
  Each accepted peer read updates directory metadata and hardware and emits
  compact scheduling telemetry and a full authenticated hardware snapshot.
  Both outputs use the same sample with its elapsed age. Local IPC also updates
  changed hardware inventory.
- **Follows live trust:** a pin addition schedules authentication. Pin removal,
  replacement, or local departure removes the verified row and its scanner
  caches and drops affected idle connections. Requests recheck the current pin
  and serving identities before accepting results, including reused connections
  and reads already in progress.
- **Advertises live membership:** the same reload re-derives this node's own `cluster-uuid=` from the cluster dir and re-advertises when it differs from what is being published, so joining or leaving a cluster takes effect without a restart.
- **Retains verified peers during multicast gaps:** the browser retains its
  twelve missed five-second scans and six empty-scan grace. A promoted peer's
  authenticated inventory from the last 10 seconds, or approved inference
  activity from the last 60 seconds, supplies liveness evidence. Canonical reads
  continue on the sampling loop, without separate synchronous liveness probes.
  An open TCP port or another discovery claiming that UUID cannot keep the peer alive.
  A peer retained after its last discovery expires is still checked during
  the sampling loop. A minute without authenticated inventory or inference
  activity removes its scanner record and caches, preserving membership and pins.
- **A scan that loses everything is treated as our own fault:** when more than one node is known and a scan returns *none* of them, no node is penalized for it — for up to six consecutive scans (~30s), after which they count normally. Independent machines do not leave the network in the same five-second window, but a process too starved to drain its multicast socket within `scanTimeout` hears silence from all of them at once; without this, one saturated machine dropped its entire cluster, idle peers included, in a single reconcile. The suppression needs two or more known nodes to engage: losing a lone peer looks the same whether it left or we stopped listening, so that case falls through to the ordinary threshold.
- **Separates pending authentication from telemetry:** candidates have their own
  bounded workers and retry state. Slow candidates cannot occupy verified-peer
  telemetry workers or replace their polling targets. Authentication and
  telemetry failures use the existing capped backoff. Requests retain deadlines,
  response-size limits, address failover, and one connection per node-info
  origin. Requests never follow redirects to another endpoint or transport.

Queued and in-flight model results are discarded if the peer's authenticated
address list changes, including a secondary address used for failover.

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
- `discovery:local-node-info` `{info}` — hardware received by the broker from its
  own node-info worker. Scanner attaches it only to the matching local registry
  record, updates inventory, and emits compact scheduling telemetry. It never
  polls public node-info for local service data. A browsed UUID or loopback
  address cannot enter this private path. Registration changes retain the owned
  inventory; worker silence leaves scheduling samples aging rather than
  generating fresh zero-usage samples.

### Notifications (daemon → caller)

Emitted as the directory changes. Each carries the affected `DirectoryNode` (identity, advertised service ports, enrichment, `trusted`).

Records also carry `discoveryRecordId` and `identityVerification`. The local registry
record is `local`; browsed records are `unverified`, including when their claimed
principal has a pin. A successful read creates a distinct `peer:<uuid>` record
with `pinned` verification and `trusted: true`. Failed discoveries cannot alter
that record. Discovery record IDs identify candidates independently of UUID and
principal claims; address order does not affect the ID.

- `discovery:node-discovered` — a new node entered the directory
- `discovery:node-updated` — a known node's record or enrichment changed. A re-observation that changes nothing emits no event, so `lastSeen` is a last-*change* stamp rather than a liveness heartbeat
- `discovery:node-removed` — a node aged out (identity + last-known fields)
- `discovery:node-telemetry` — compact maximum-GPU utilization, validity, and
  sample age for broker-internal scheduling

A one-shot `ready` notification is sent on startup carrying the daemon version. The broker replays its service registrations on every scanner spawn, so a restarted daemon is repopulated without needing to compare an epoch.

## Shutdown

The daemon shuts down on:
- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`
- `shutdown` JSON-RPC request
