<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-scanner

**The consolidated discovery daemon.** Promoted from a plain node-info browser: it is now the single place this host advertises itself on and discovers the LAN through. Every other service registers its port with this daemon instead of advertising its own mDNS service, and consumers query/subscribe to it instead of browsing themselves.

Concretely, the daemon:

- **Advertises** this node's one `_nvpair-node._tcp` record, built from a registry of the local services its parent (the broker) registers — `ni`/`ol`/`lm`/`er`/`wl`/`cl`/`em`/`ec` ports plus the node identity (`uuid=`, `cluster-uuid=` when clustered, `ip=`). One record per node covers every service.
- **Browses** `_nvpair-node._tcp` for every node on the LAN (including itself) and maintains a queryable directory keyed by host UUID.
- **Enriches** each discovered node: GPU/CPU/memory inventory from its `ni` (node-info) port over plain HTTP, and the model list from its `em` (engine-manager) port (`GET /v1/models`) over cluster mTLS — a peer's model inventory is cluster data, so it is fetched only when this node holds a pin for the principal that peer advertises, while this node's own list is read over loopback in plaintext — the flat union, the per-engine breakdown (`modelsByEngine`), and the per-engine set of models loaded in memory (`loadedByEngine`), all carried onto the node's directory entry so remote cards can show loaded state. Each enrichment has a last-good cache so a transient fetch miss doesn't blank the node's card. Node-info enrichment can optionally be moved onto HTTPS (see the TLS flags below) — off by default and gated only on operator flags, never inferred per-node.
- **Samples scheduling telemetry** from healthy nodes every two seconds with
  stable per-node jitter. Consecutive remote failures back off to 4, 8, 16, then
  30 seconds and reset on success. Local inventory and samples arrive every two
  seconds from the owned node-info worker over private broker/worker IPC.
  It emits only host UUID, maximum GPU utilization, validity, and
  source-reported sample age. Remote telemetry reads do not mutate the public
  directory; local IPC also updates changed hardware inventory.
- **Annotates trust:** with `--cluster-dir`, a browsed peer whose `cluster-uuid=` this node holds a pin for is marked `trusted` (via `nvpair-shared/clustertrust`). The annotation is re-derived on `discovery:reload-trust`, which the broker sends when `nvpair-cluster-manager` reports a pin-set change, as well as when a peer's mDNS record changes. Both triggers are needed: a peer that was already advertising when this node joined never moves its record again, so the browse path alone would leave it annotated from before this node's own pins existed. A change emits one `discovery:node-updated`; a reload that finds nothing to change is silent.
- **Advertises live membership:** the same reload re-derives this node's own `cluster-uuid=` from the cluster dir and re-advertises when it differs from what is being published, so joining or leaving a cluster takes effect without a restart.
- **Anti-flap:** a node has to be absent from every scan for the browser's full miss threshold (twelve scans at 5s, a full minute) before it is evicted, and from ~15s in it is TCP-probed on its advertised service ports on *every* scan — any answer resets the counter and recovers it outright. So neither a momentary multicast gap nor a sustained one drops a healthy node, and eviction rests on a run of ~10 consecutive failures rather than one dial on the deadline. The window is sized deliberately: a node saturated by its own inference load starves its control plane, so it stops answering mDNS *and* stops accepting probe connections for as long as the load runs, while still serving requests. Two signals can also vouch for a node without any probe — a node-info enrichment in the last 10s, and inference response bytes from it in the last 60s (`discovery:node-activity`, relayed by the broker from whichever proxy saw the bytes). The second is the only liveness evidence that gets stronger the busier a node is.
- **A scan that loses everything is treated as our own fault:** when more than one node is known and a scan returns *none* of them, no node is penalized for it — for up to six consecutive scans (~30s), after which they count normally. Independent machines do not leave the network in the same five-second window, but a process too starved to drain its multicast socket within `scanTimeout` hears silence from all of them at once; without this, one saturated machine dropped its entire cluster, idle peers included, in a single reconcile. The suppression needs two or more known nodes to engage: losing a lone peer looks the same whether it left or we stopped listening, so that case falls through to the ordinary threshold.
- **Converges without mDNS:** a 15s sweep currently re-reads each peer over public HTTP and folds changes into the directory, independent of browse events. This covers missed announcements and facts that change without moving the mDNS record. `clusterUuid` remains available on public node-info, including when its separate peer node inventory handler is enabled. Membership is refreshed before models, because model reads use pinned mTLS keyed on that principal. Failed or absent membership reports leave the annotation unchanged. Migrating these remote inventory reads to authenticated promotion is the next implementation step in [the spec](spec.md).

## Communication

Bidirectional newline-delimited JSON-RPC 2.0. Stdio by default; `--ipc <path>` switches to a Unix domain socket or Windows named pipe.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`). Gates four things: a browsed peer holding a pin we trust is annotated `trusted`; this node advertises `cluster-uuid=` while it is actually a member; a peer's `em` model list is fetched over cluster mTLS; and the directory above it is the base this node's identity is resolved from |
| `--ca-bundle <path>` | _(none)_ | PEM CA bundle to trust for verifying node-info server certs (additive to the system trust store). Setting any TLS flag moves node-info enrichment onto HTTPS |
| `--client-cert <path>` | _(none)_ | PEM client certificate to present when fetching `/v1/node-info` from TLS-enabled nodes (mTLS; requires `--client-key`) |
| `--client-key <path>` | _(none)_ | PEM client private key matching `--client-cert` |
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
| `discovery:reload-trust` | — | `{ok:true}` — re-derive `trusted` on every directory entry from the current pin set |
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
principal has a pin. `pinned` denotes authenticated identity in the shared
contract. Discovery record IDs identify connection candidates independently of UUID
and principal claims. Their address order does not affect the ID. The existing
`trusted` pin annotation alone does not prove endpoint ownership.

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
