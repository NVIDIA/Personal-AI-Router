<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Workload Manager

> **Implementation note (cluster membership is live):** the inter-node mTLS
> described below is unconditional — there is no plain-HTTP personality on the
> `wl` port — and membership is evaluated continuously rather than at startup.
> The port is bound once and its server certificate is resolved per handshake, so
> a node that belongs to no cluster presents no leaf and serves nothing, while a
> node that joins mid-session starts serving pinned peers on the same listener
> with no rebind and no restart. Outbound, the per-peer pinned client is resolved
> at the start of every broadcast round, and a non-member broadcasts nothing.

## 1. Purpose
Tracks inference workloads cluster-wide as they are queued, executed, and retired, giving the customer the data to render cluster state and follow workloads across nodes.

## 2. Scope
**In scope**
- Broadcast local workload events to peer nodes; relay peer events to the local Broker.
- Maintain the broadcast target set from the consolidated discovery directory — subscribe for `wl` nodes through the Broker's discovery relay and rebuild the target set from each snapshot.

**Out of scope**
- Persistence and long-term memory — the Broker persists and tracks workload state; this service forwards transitions but stores none across sessions.

## 3. Key Use Cases
- **Broadcast a local event**: the Broker emits a `workload:*` lifecycle event (`workload:submitted`, `workload:started`, `workload:completed`, `workload:errored`) or `workloads:remove` on `stdin`; the Workload Manager broadcasts it to every discovered peer via `POST /v1/workloads/events` over mTLS.
- **Relay a remote event**: on receiving a peer's `workload:*`, validate and deduplicate (by `(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)`), then emit `workloads:upsert` on `stdout` so the Broker updates its catalog. A peer `workloads:remove` (deduplicated by `(nodeId, workloadId)`) is relayed as `workloads:remove` on `stdout`.
- **Discover peers**: the Broker registers this node's `wl` port with the `nvpair-node-scanner` discovery daemon, which carries it on this node's single consolidated record; the Workload Manager subscribes for `wl` nodes and rebuilds the target set from each `discovery:nodes` snapshot the Broker relays, so a node joins the set when it appears in a snapshot and leaves when it is absent from the next one.
- **Edge case — duplicate / re-broadcast events**: retries or re-broadcasts arriving more than once are deduplicated (`(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)` for lifecycle, `(nodeId, workloadId)` for removals) so the Broker is updated at most once.
- **Edge case — peer unreachable**: a target that is down, slow, or partitioned (crash, network split, laptop suspend) does not block the local Broker or delivery of the current frame to other peers. Fan-out runs concurrently with per-peer timeouts and bounded retries, but the ordered worker waits for the round to finish before sending the next frame.

## 4. Open Questions / Risks
- **`initializing` state (closed)**: removed. It had no `workload:*` method and nothing ever produced it, so it was an unreachable member of a closed enum. The proxy has no distinct pre-dispatch moment to represent — admission and the first dispatch are effectively simultaneous — so the value was deleted rather than given a method.
- **`EngineType` values (open — needs third-party feedback)**: the valid `engine` set is undefined. Pending the inference-engine team's list, `engine` is treated as an opaque pass-through string (must be present and non-empty, value not validated).
- **Risk — late joiners / partial backfill**: newly discovered peers receive a re-assertion of this node's active and recently terminal workloads. Retired workloads and removals are not replayed, so backfill is not a complete event history.
- **Risk — best-effort fan-out**: an unreachable peer misses events; later lifecycle re-assertions can repair tracked state when delivered, but a missed removal has no heartbeat or backfill repair path.
- **Risk — snapshot staleness**: the target set is only as current as the last `discovery:nodes` snapshot, so a departed node lingers as a target and a new node appears slowly, bounded by the discovery daemon's own liveness handling rather than by anything this service controls.
- **Risk — dedup granularity**: the key is `(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)`, so a re-broadcast carrying the same key with updated metadata (e.g. a corrected `error`) is dropped, not merged. `seq` is the producer's event counter and is what makes the key exact, because this index is a permanent set: any key derived only from a workload's current shape collides as soon as the workload revisits a shape it already had, which a retry does routinely (queued on A, placement cleared between attempts, queued on A again). Without it peers dropped that third event, kept the interim unplaced record, and stopped counting an active job against the node running it.

## 5. Requirements

**Functional**
- Accept local `workload:*` and `workloads:remove` notifications from the Broker over `stdin` (or a named pipe) and broadcast each to all discovered peers via `POST /v1/workloads/events`.
- Preserve the enqueue order of local Broker notifications when sending frames to each peer. If both are delivered, a removal must not overtake a lifecycle event queued before it; this is an ordering guarantee, not a delivery guarantee.
- For each validated, deduplicated inter-node `workload:*`, emit `workloads:upsert` to the Broker (translated, not forwarded unchanged); for each inter-node `workloads:remove`, emit `workloads:remove` to the Broker (not re-broadcast).
- Record an inter-node dedup key only after the corresponding notification is successfully written to the Broker. If the write fails, return `500` and leave the key available for a retry; concurrent requests for the same key must not both emit successfully.
- Subscribe to the Broker's discovery relay with `discovery:subscribe` filtered to the `wl` service, and rebuild the broadcast target set from every `discovery:nodes` snapshot: take each peer's dialable address and `wl` port from its directory entry, skip entries advertising no `wl` port or no address, and exclude this node's own entry by `hostUuid` rather than by hostname. A snapshot carries the full filtered set and replaces the target set wholesale, so there are no per-node deltas to apply and a peer absent from a snapshot simply stops being a target.
- Deduplicate inbound lifecycle events by `(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)` and removals by `(nodeId, workloadId)`, using a configurable bounded LRU index (default ~10,000 entries, sized for session-scoped volume at ~dozen-node scale). `nodeId` is part of the key because `Workload.id` is only unique per node (§11) — keying on `id` alone would collide across nodes and silently drop a legitimate peer's event. `engine` and `runId` are there for the same reason one level down: `Workload.id` is a per-process counter, both engine proxies count from 1, and the counter resets on restart, so without them a concurrent Ollama and LM Studio job both holding id `"1"` would collapse into one. The Broker keys a workload by `(originatedFrom, engine, runId, id)`; this key adds `state`, `scheduledOn` and `seq` so that it names one event rather than one workload.
- Validate inbound payloads; reject malformed envelopes or unknown `method` values (`400 Bad Request`).

**Non-functional**
- Authenticate all inter-node traffic with mTLS, validating client and server certificates against the trusted node store; reject untrusted clients (`403 Forbidden`).
- Stay stateless re: workload history — the Broker is the source of truth.
- A failed broadcast to one peer must not block delivery of the current frame to other peers or the local Broker. A slow peer may delay later frames while the ordered worker finishes the current round.
- Serialize `stdout` writes so frames never interleave; the Broker identifies a workload by `(originatedFrom, engine, runId, id)` and applies its events in the origin's `seq` order, and by state for events without one.

## 6. Inputs and Outputs

**Inputs** — from the Broker over `stdin` / named pipe, or from peer nodes via HTTP REST. JSON-RPC 2.0 notification envelopes (`jsonrpc`, `method`, `params`); see §7 for transport details. Lifecycle methods carry `params.workloadInfo` (a `Workload`); `workloads:remove` carries `params.workloadId` (string) and an optional `params.originatedFrom` (string) that disambiguates which node's workload to remove (see §7.3 — additive field; absent on legacy senders, in which case dedup degrades to id-only keying).

`Workload` object:
```json
{
  id: string
  model: string
  engine: EngineType
  state: WorkloadState
  runId?: string          // Producing process's nonce, minted at proxy startup. Optional/additive (§7.3). Part of the dedup key: `id` is a per-process counter, both engine proxies count from 1, and it resets on restart, so runId is what keeps a reused id from colliding with an older workload. Absent from a producer that does not stamp it, in which case dedup degrades to a coarser key.
  seq?: number            // Producer's event counter for this workload, from 1, in emission order. Optional/additive (§7.3). Part of the dedup key, and the component that makes it exact: the index is a permanent set, so any key derived only from a workload's current shape collides when the workload revisits a shape it already had (a retry re-dispatching to a node it already tried). Absent from a producer that does not stamp it, in which case dedup degrades to shape-only and such a repeat is dropped as a duplicate.
  originatedFrom: string  // Owner/origin node of the workload (part of the (originatedFrom, engine, runId, id) global key). Distinct from scheduledOn, the node it was routed to.
  scheduledOn?: string  // Node the workload was routed to / scheduled on (where it actually ran), as opposed to originatedFrom (where it came from). Supplied by the Broker/proxy and passed through opaquely. Optional/additive (§7.3) — absent until a target is chosen (e.g. a still-queued workload:submitted).
  createdAt: number
  startedAt: number | null
  completedAt: number | null
  error: string | null
  requesterId: string | null
}
```

`WorkloadState` (enum): `"queued" | "running" | "completed" | "failed" | "cancelled"`
- Method → `state`: `workload:submitted` → `queued`, `workload:started` → `running`, `workload:completed` → `completed`, `workload:errored` → `failed` or `cancelled`.
- `queued` covers every wait, for a node or for a slot in the engine of the node the workload was sent to. `running` means that engine has started it: it holds one of the engine's slots, or the engine has produced output for it. A failover sends `workload:submitted` after `workload:started`, returning the workload to `queued` on its next node, so `running` is not final. The proxy stamps every event with `seq`, and the Broker applies a workload's events in `seq` order, so that return is applied rather than rejected as a lower state.
- `cancelled` is terminal and means the requester stopped waiting — a disconnected client, a dead client whose write deadline tripped, or an in-flight request cancelled by the producer's own shutdown. It rides `workload:errored` rather than having a method of its own, because state is not validated against the method that carried it and consumers read `state` from the payload. Keeping it distinct from `failed` is what lets a consumer's failed bucket mean "an outcome someone might act on" instead of also collecting every time a user pressed stop.

Validation: check every inbound envelope before processing. A `Workload` must
carry `id`, `model`, `engine`, `state`, and `originatedFrom`; `runId`, `seq`,
and `scheduledOn` are optional/additive, and the timestamp and nullable fields
are passed through opaquely. Note the asymmetry with the dedup key: `runId` and
`seq` are part of that key but are not required, so a producer that omits them
is accepted and simply gets coarser deduplication rather than a rejection.
Inter-node and local interfaces both accept the four `workload:*` methods (with
`params.workloadInfo`) and `workloads:remove` (with `params.workloadId`). Reject malformed/unknown-`method` payloads on the inter-node interface (`400`); drop-and-log on the local interface (never broadcast). Deduplicate inter-node lifecycle by `(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)` and removals by `(nodeId, workloadId)`. Assume the Broker supplies well-formed notifications with epoch-millisecond timestamps.

**Outputs** — to the Broker over `stdout` / named pipe, or to peers via HTTP REST. `workloads:upsert` on `stdout` after a remote `workload:*`; `workloads:remove` on `stdout` after a remote inter-node `workloads:remove`. Inter-node broadcast posts the same JSON-RPC notification to `POST /v1/workloads/events` (`200 OK` on success; see §7.2). No response on the local interface (notifications only); `stdout` writes are serialized so frames never interleave; inter-node delivery is best-effort (concurrent, per-peer timeouts, bounded retries).

## 7. API / Interface Contract

Two interfaces: a **local interface** toward the same-node UI Broker, and an **inter-node interface** toward Workload Manager instances on other nodes.

### 7.0 Methods and notifications

All traffic is JSON-RPC 2.0. The local interface uses `stdin`/`stdout` (or a named pipe); inter-node uses notifications on `POST /v1/workloads/events` (see §7.2). The Broker owns durable state — `workloads:get-initial` is a Broker concern, **not** part of this contract.

| Name | Local (`stdin`) | Inter-node (`POST /v1/workloads/events`) | WM → Broker (`stdout`) |
|------|------------|----------------------------------------|--------------------------------------|
| `workload:submitted` | Broker → WM; broadcast | Peer → WM; → `workloads:upsert` | — |
| `workload:started` | Broker → WM; broadcast | Peer → WM; → `workloads:upsert` | — |
| `workload:completed` | Broker → WM; broadcast | Peer → WM; → `workloads:upsert` | — |
| `workload:errored` | Broker → WM; broadcast | Peer → WM; → `workloads:upsert` | — |
| `workloads:upsert` | — | — | After validated remote `workload:*` |
| `workloads:remove` | Broker → WM; broadcast | Peer → WM; → `workloads:remove` | After validated remote `workloads:remove` |

- **Inbound `workload:*` (Broker → WM)**: local-origin transitions to broadcast cluster-wide; `params.workloadInfo` is a full `Workload`. No response.
- **`workloads:upsert` (WM → Broker)**: emitted for remote-origin lifecycle updates — each validated inter-node `workload:*` is translated (not forwarded) into an upsert. Not used on inter-node HTTP or Broker `stdin`. Params: `workloadInfo` copied unchanged from the peer event. Semantics: upsert into the Broker catalog by `(originatedFrom, engine, runId, id)` (create if absent, otherwise merged in `seq` order) — the latest metadata/state, not a replay of the method name.
- **`workloads:remove` (WM → Broker)**: emitted on `stdout` after a validated inter-node removal (same `(nodeId, workloadId)`), deduplicated so retries emit at most once; not re-posted to peers. Local-origin removals arrive on `stdin` and are broadcast over HTTP unchanged. Semantics: remove the workload by `(nodeId, workloadId)` from the Broker's view if present, else no-op. Inter-node handler returns `200 OK` once accepted/relayed (or deduplicated).

Example `workloads:upsert` (`stdout`):
```json
{
  "jsonrpc": "2.0",
  "method": "workloads:upsert",
  "params": {
    "workloadInfo": {
      "id": "wl-1a2b3c",
      "model": "llama-3-70b",
      "engine": "trt-llm",
      "state": "running",
      "runId": "9f3c1a7b",
      "seq": 3,
      "originatedFrom": "node-B",
      "scheduledOn": "node-C",
      "createdAt": 1716998400000,
      "startedAt": 1716998401000,
      "completedAt": null,
      "error": null,
      "requesterId": "req-42"
    }
  }
}
```

Example `workloads:remove` (identical body inter-node and on `stdout`; `originatedFrom` is the optional disambiguator):
```json
{
  "jsonrpc": "2.0",
  "method": "workloads:remove",
  "params": {
    "workloadId": "wl-1a2b3c",
    "originatedFrom": "node-B"
  }
}
```

### 7.1 Local Interface (UI Broker ↔ Workload Manager)
- Transport: `stdin` / `stdout` (or a named pipe); JSON-RPC 2.0 notifications only (no request/response).
- **Inbound** (`stdin`): local `workload:*` lifecycle notifications and `workloads:remove`, for broadcast.
  - `workload:submitted` — queued: admitted on this node, or waiting for a node or an engine slot, including after a failover
  - `workload:started` — running: holds an engine slot, or the engine has produced output for it
  - `workload:completed` — finished successfully
  - `workload:errored` — failed (may retry on another node)
- **Outbound** (`stdout`): `workloads:upsert` after each validated remote `workload:*`; `workloads:remove` after each validated remote removal (see §7.0).
- Params: lifecycle uses `params.workloadInfo`; `workloads:remove` uses `params.workloadId`.

Example notification:
```json
{
  "jsonrpc": "2.0",
  "method": "workload:submitted",
  "params": {
    "workloadInfo": {
      "id": "wl-1a2b3c",
      "model": "llama-3-70b",
      "engine": "trt-llm",
      "state": "queued",
      "runId": "9f3c1a7b",
      "seq": 1,
      "originatedFrom": "node-A",
      "createdAt": 1716998400000,
      "startedAt": null,
      "completedAt": null,
      "error": null,
      "requesterId": "req-42"
    }
  }
}
```

### 7.2 Inter-node Interface (Workload Manager ↔ Workload Manager)
- Endpoint: `POST /v1/workloads/events`; port `14320` (TCP), the default this node listens on and registers as its `wl` port. Every peer is reached on the `wl` port carried in its own directory entry, which is `14320` unless that node was started with an override.
- Protocol: HTTPS REST over mTLS; client and server certificates validated against the trusted node store.
- Connections are **keep-alive and pooled per peer**, not per event. A sender holds a bounded set of long-lived connections per peer and reaps one after `clustertrust.PeerIdleTimeout`; the listener sets a strictly longer `IdleTimeout` (`clustertrust.PeerListenerIdleTimeout`) so the sender is always the side that discards a doubtful connection. This is normative, not an optimization: a handshake per event both starves the receiver and, without an idle bound on either side, pins a descriptor per connection for the life of the process.
- A node broadcasts each local `workload:*` and `workloads:remove` to all discovered peers.
- Body: JSON-RPC 2.0 notification — `workload:*` with `params.workloadInfo`, or `workloads:remove` with `params.workloadId` (and optional `params.originatedFrom`).
- Responses: `200 OK` (written to the local Broker or already deduplicated); `400 Bad Request` (malformed / unknown `method`); `403 Forbidden` (client cert absent or untrusted); `500 Internal Server Error` (Broker write failed, so the sender may retry). A successful write does not acknowledge that the Broker applied the event.
- Idempotency: lifecycle by `(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)`; removal by `(nodeId, workloadId)`. Record a key after a successful Broker write, and serialize concurrent requests for the same key through that write. Tagged lifecycle re-sync frames bypass dedup so they can re-assert state.

Example request (`POST /v1/workloads/events`):
```json
{
  "jsonrpc": "2.0",
  "method": "workload:errored",
  "params": {
    "workloadInfo": {
      "id": "wl-1a2b3c",
      "model": "llama-3-70b",
      "engine": "trt-llm",
      "state": "failed",
      "originatedFrom": "node-A",
      "scheduledOn": "node-A",
      "createdAt": 1716998400000,
      "startedAt": 1716998401000,
      "completedAt": 1716998402500,
      "error": "CUDA out of memory",
      "requesterId": "req-42"
    }
  }
}
```
Response: `200 OK`.

### 7.3 Versioning
- URL path prefix (`/v1`) on the inter-node interface.
- JSON-RPC `method` names are namespaced and additive — new event types are new methods, never changes to existing ones.
- `Workload` schema changes are backward compatible (new fields optional, unknown fields ignored); breaking changes require a new version (`/v2`).

## 8. Dependencies
- **Upstream**: UI Broker — the parent process that launches the Workload Manager as a child and feeds it local events over `stdin`.
- **Downstream**: local UI Broker (consumes `workloads:upsert` / `workloads:remove` on `stdout`); peer Workload Manager instances (receive broadcasts via `POST /v1/workloads/events`).
- **External**:
  - **mTLS trusted node store** — backs inter-node authentication; supplies certificates and trust anchors and is used to reject untrusted clients (`403`).
  - **`nvpair-node-scanner` discovery daemon, via the Broker's relay** — the sole source of broadcast targets. It advertises this node's `wl` port on the node's single consolidated record and supplies the `discovery:nodes` snapshots the target set is rebuilt from (§5). The daemon's mDNS service name and TXT layout are its own internal contract; this service reads only the directory entries it is handed and never parses a record itself.
  - **Multicast-capable network** — required by the discovery daemon's browse, not by this service directly. Where multicast is unavailable the daemon reports no peers, the snapshots are empty, and broadcast has no targets.

## 9. Data Ownership
- **Owned**: none durable — only transient in-memory state (the bounded dedup index and the discovered/target peer set). `Workload` entities are not owned.
- **Source of truth**: no — the Broker is authoritative; this service is a stateless relay.
- **Storage**: none — in-memory state is discarded on exit.

## 10. Design Constraints
- **Performance**: low event volume (lifecycle transitions, not telemetry); no end-to-end latency SLA, delivery is best-effort. Local forwarding writes serialized frames to `stdout`; inter-node broadcast is concurrent with per-peer timeouts.
- **Scalability**: ~dozen-node cluster; each local event fans out to all peers. Transient in-memory state includes the bounded dedup index, target set, active/recently-terminal re-sync set, and bounded outbound queue.
- **Reliability**: best-effort fan-out, no SLA. Peers receive each frame concurrently, with timeouts and bounded exponential-backoff retries; the ordered worker finishes that round before sending the next frame. Drop after max attempts (metric/alert; parameters implementation-defined), and drop newly enqueued frames with a warning if the queue is full so the local read loop remains unblocked. Serialized `stdout`; the Broker applies each workload's events in `seq` order, and by state for events without one. Heartbeat and discovery backfill re-assert tracked lifecycle state, but neither replays removals or full event history.
- **Re-sync cadence is a cross-process contract.** The anti-entropy heartbeat enqueues a re-assertion of every *active* local-origin workload on every interval, indefinitely, and tags each re-assertion so the receiver's state dedup does not swallow it. Delivery can still be delayed or dropped by the outbound queue. A consumer is entitled to treat prolonged silence about a workload it believes active as evidence that the workload is finished or the origin is gone — `nvpair-ui-broker` does exactly that in its staleness sweep, keyed to a multiple of this interval. Lengthening the interval beyond that consumer's budget, or dropping the re-assertion tag, therefore causes peers to retire live work; change both sides together.
- **Security**: all inter-node traffic mTLS-authenticated against the trusted node store; reject and log untrusted callers (`403`) with the presented identity; monitor cert expiry; keep the trust store hot-reloadable. Payloads carry workload metadata only (model, state, node IDs, errors) — no request/response data or PII. `requesterId`, `nodeId`, and `Workload.id` are opaque system-generated identifiers (not PII).
- **Compliance**: workload information must contain no PII.

## 11. Assumptions
- The Broker is always the parent process: it spawns the Workload Manager, writes `workload:*` / `workloads:remove` to `stdin`, and consumes `workloads:upsert` / `workloads:remove` from `stdout`. If the Broker exits, the manager shuts down cleanly — no reconnect or buffering.
- The Broker is the source of truth; local notifications are well-formed JSON-RPC with epoch-ms timestamps.
- Small cluster (~dozen nodes), low event volume; peers mutually authenticated via mTLS. New nodes receive current active and recently terminal workload re-assertions after discovery, but no historical event or removal replay.
- Each engine proxy assigns `Workload.id` from a per-process counter that starts at 1, so ids repeat across nodes, engines and runs. The Broker identifies a workload by `(originatedFrom, engine, runId, id)` and orders its events by the origin's `seq`.

## 12. Failure Modes and Mitigations
- **Peer unreachable/slow during broadcast** (partition, crash, suspend, GC pause): that peer's Broker misses the event, causing temporary inconsistency. → Concurrent fan-out lets other peers receive the current frame and keeps the local Broker unblocked; bounded exponential-backoff retries drop after max attempts with a warning. The ordered broadcast worker waits for all peers before taking the next frame, so a slow peer can delay later frames to healthy peers and build a queue. Revisit independent per-peer queues if this delay becomes a problem.
- **Outbound queue full**: a slow broadcast round can fill the bounded queue; the read loop drops each new frame with a warning rather than blocking. A dropped active or recently terminal lifecycle event may be repaired by a later heartbeat or discovery backfill if that re-assertion is delivered. A removal is deleted from the re-sync set before broadcast and is not replayed, so dropping its frame can leave a stale workload on a peer with no removal repair path.
- **Connection exhaustion under burst** (observed in the field): a per-event connection makes every lifecycle event pay a full mTLS handshake, and an unreaped idle connection pins a descriptor for the process lifetime. At inference-burst rates this starves the sender and the receiving listener until handshakes fail outright, and the events lost are disproportionately terminal ones — which the ~two-interval terminal retention cannot recover once the window passes. → Pool connections per peer with a bound on both total and idle connections per host, reap idle connections on both ends (§7.2), and drain response bodies so a connection is actually reusable. Fan-out remains concurrent across peers for one frame, while the ordered worker sends frames sequentially.
- **mTLS handshake failure / untrusted or expired cert**: that peer is isolated from broadcasts (`403`). → Reject and log with the presented identity; monitor and alert ahead of cert expiry; keep the trust store hot-reloadable so rotations apply without restart.
- **Duplicate / re-broadcast events**: the Broker could see the same transition repeatedly, corrupting counts/state. → Deduplicate by `(nodeId, engine, runId, Workload.id, state, scheduledOn, seq)` (removals by `(nodeId, workloadId)`) and serialize concurrent requests for one key. Record the key only after a successful Broker write; on failure, return `500` and let a retry attempt the write. Re-sync lifecycle frames bypass this index so they can correct the Broker's view.
- **Malformed payload / unknown `method`**: could crash the parser or propagate garbage. → Validate every envelope; reject (`400`) inter-node or drop-and-log locally; never forward unvalidated payloads.
- **Local interface severed** (Broker exited → `stdin` EOF / `stdout` `EPIPE` / `ERROR_BROKEN_PIPE`): an orphaned manager would accept peer events with nowhere to forward them. → Treat EOF/`EPIPE` as the shutdown signal: stop the listener and exit cleanly. No reconnect/buffering — a new Broker spawns a new (stateless) manager. The small load lets the OS pipe buffer absorb serialized `stdout` writes.
- **Out-of-order delivery for the same workload (local)**: the Broker could see an inconsistent progression (e.g. `completed` before `started`). → Serialize `stdout` writes; the Broker applies each workload's events in `seq` order and never replaces a terminal its origin reported.
- **Stale discovery snapshot** (departed node still targeted, or new node not yet in a snapshot): wasted attempts to dead nodes, or new nodes missing events. → Rebuild the target set from every `discovery:nodes` snapshot, so a peer absent from the next snapshot stops being a target without any expiry logic here; liveness is the discovery daemon's, which probes a node before evicting it so a transient miss does not flap the set. Newly discovered peers receive active and recently terminal state backfill, but not removals or event history.

## 13. Observability
- **Logging**: mTLS/cert rejections with presented identity (`403`) and malformed/unknown-`method` rejections (`400`); per-peer broadcast failures including final drop; deduplicated/dropped inbound events; target-set changes applied from a `discovery:nodes` snapshot (peers added, peers removed, resulting total); startup and clean shutdown on `stdin` EOF / `stdout` `EPIPE`.
- **Metrics**: inbound rate (local and inter-node) and outbound broadcast rate; per-peer success/failure counts, latency, retry and final-drop counts; dedup hit rate; discovered/active peer count; mTLS handshake failure count.
- **Alerts**: sustained broadcast failures to a peer; elevated `400` / `403` rate (misconfiguration or untrusted caller).

## 14. Sample usage
A request on node A is routed to its local inference proxy, which emits a `workload:submitted` notification (`params.workloadInfo` = a `Workload`). The Broker forwards it to the Workload Manager, which broadcasts it to all discovered peers over HTTPS/mTLS using trusted-store certificates.

Each recipient (nodes K1…KN) validates and deduplicates the broadcast, then emits `workloads:upsert` on `stdout` so its local Broker updates its cluster-wide catalog.

Having seen both originating workloads and remote `workloads:upsert` / `workloads:remove`, each Broker tracks all workloads and states. When a workload is retired, the Broker sends `workloads:remove` on `stdin`; the Workload Manager broadcasts it, and each recipient emits `workloads:remove` on `stdout` after validating the inter-node signal.
