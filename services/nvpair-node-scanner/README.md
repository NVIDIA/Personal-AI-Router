<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-scanner

**The consolidated discovery daemon.** Promoted from a plain node-info browser: it is now the single place this host advertises itself on and discovers the LAN through. Every other service registers its port with this daemon instead of advertising its own mDNS service, and consumers query/subscribe to it instead of browsing themselves.

Concretely, the daemon:

- **Advertises** this node's one `_nvpair-node._tcp` record, built from a registry of the local services its parent (the broker) registers — `ni`/`ol`/`lm`/`vl`/`er`/`wl`/`cl`/`em`/`ec` ports plus the node identity (`uuid=`, `cluster-uuid=` when clustered, `ip=`). One record per node covers every service.
- **Browses** `_nvpair-node._tcp` for every node on the LAN (including itself) and maintains a queryable directory keyed by host UUID.
- **Enriches** each discovered node: GPU/CPU/memory inventory from its `ni` (node-info) port over plain HTTP, and the model list from its `em` (engine-manager) port (`GET /v1/models`) over cluster mTLS — a peer's model inventory is cluster data, so it is fetched only when this node holds a pin for the principal that peer advertises, while this node's own list is read over loopback in plaintext — the flat union, the per-engine breakdown (`modelsByEngine`), and the per-engine set of models loaded in memory (`loadedByEngine`), all carried onto the node's directory entry so remote cards can show loaded state. Each enrichment has a last-good cache so a transient fetch miss doesn't blank the node's card. Node-info enrichment can optionally be moved onto HTTPS (see the TLS flags below) — off by default and gated only on operator flags, never inferred per-node.
- **Samples scheduling telemetry** from healthy nodes every two seconds with
  stable per-node jitter. Consecutive remote failures back off to 4, 8, 16, then
  30 seconds and reset on success; local loopback sampling stays at the healthy
  cadence. It emits only host UUID, maximum GPU utilization, validity, and
  source-reported sample age; it does not mutate the public directory for each
  telemetry sample.
- **Annotates trust:** with `--cluster-dir`, a browsed peer whose `cluster-uuid=` this node holds a pin for is marked `trusted` (via `nvpair-shared/clustertrust`). The annotation is re-derived on `discovery:reload-trust`, which the broker sends when `nvpair-cluster-manager` reports a pin-set change, as well as when a peer's mDNS record changes. Both triggers are needed: a peer that was already advertising when this node joined never moves its record again, so the browse path alone would leave it annotated from before this node's own pins existed. A change emits one `discovery:node-updated`; a reload that finds nothing to change is silent.
- **Advertises live membership:** the same reload re-derives this node's own `cluster-uuid=` from the cluster dir and re-advertises when it differs from what is being published, so joining or leaving a cluster takes effect without a restart.
- **Anti-flap:** a node has to be absent from every scan for the browser's full miss threshold (twelve scans at 5s, a full minute) before it is evicted, and from ~15s in it is TCP-probed on its advertised service ports on *every* scan — any answer resets the counter and recovers it outright. So neither a momentary multicast gap nor a sustained one drops a healthy node, and eviction rests on a run of ~10 consecutive failures rather than one dial on the deadline. The window is sized deliberately: a node saturated by its own inference load starves its control plane, so it stops answering mDNS *and* stops accepting probe connections for as long as the load runs, while still serving requests. Two signals can also vouch for a node without any probe — a node-info enrichment in the last 10s, and inference response bytes from it in the last 60s (`discovery:node-activity`, relayed by the broker from whichever proxy saw the bytes). The second is the only liveness evidence that gets stronger the busier a node is.
- **A scan that loses everything is treated as our own fault:** when more than one node is known and a scan returns *none* of them, no node is penalized for it — for up to six consecutive scans (~30s), after which they count normally. Independent machines do not leave the network in the same five-second window, but a process too starved to drain its multicast socket within `scanTimeout` hears silence from all of them at once; without this, one saturated machine dropped its entire cluster, idle peers included, in a single reconcile. The suppression needs two or more known nodes to engage: losing a lone peer looks the same whether it left or we stopped listening, so that case falls through to the ordinary threshold.
- **Converges without mDNS:** a 15s sweep re-reads each peer over HTTP and folds any change into the directory, independent of browse events. It covers facts that change without moving the mDNS record (a model pull/delete, a late engine start) and facts that did move it but whose announcement was never received. The second case is why a peer's cluster membership is re-read from its `ni` port (`clusterUuid` on `/v1/node-info`, which under the broker is served plain — so it answers whether or not the two nodes share a cluster; a node-info run standalone with `--cluster-dir` gates the endpoint while clustered and will not answer a non-pinned peer, which the sweep treats as no answer): membership otherwise arrives solely as the `cluster-uuid=` TXT key, read once per record change, and the anti-flap probe above deliberately keeps a still-reachable peer's entry alive — so a missed re-advertisement would leave a departed peer's principal in place for the life of the process, suppressing the invite that would bring it back. Membership is refreshed before models on each tick, since the model fetch is pinned mTLS keyed on that principal. Only a peer's explicit report is acted on: a failed fetch, or a peer too old to report the field, leaves the annotation untouched rather than inferring "unclustered" from silence.

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

Two notifications are accepted rather than requests, because the daemon has
nothing to answer and the caller must not wait:

- `discovery:node-activity` `{hostUuid, msSince}` — a peer's engine returned
  inference response bytes `msSince` ms ago. Counts as proof of life and cancels
  that node's eviction. The broker sends this while inference is streaming, so an
  id-bearing call would cost it a pending entry and a blocked goroutine per
  report in exchange for an ack nobody reads.
- `log/set-level` (above).

### Notifications (daemon → caller)

Emitted as the directory changes. Each carries the affected `DirectoryNode` (identity, advertised service ports, enrichment, `trusted`).

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
