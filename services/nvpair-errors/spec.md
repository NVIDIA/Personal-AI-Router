<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Service Error Store (`nvpair-errors`)

## 1. Purpose

`nvpair-errors` owns the node's single list of surfaced operational errors. Every producer in the process tree (each `nvpair-proxy` engine facade, `nvpair-engine-manager`, `nvpair-manual-nodes`, `nvpair-job-scheduler`, the broker's own supervisor, and the desktop client) emits `errors:report` / `errors:clear` on its existing stdio, and `nvpair-ui-broker` forwards each frame here. This service stores the entry, resolves conflicts by highest `timestamp`, and pushes the complete merged list back as an `errors:update` notification, which the broker relays verbatim to its client. It is a passive datastore: it does not decide what an error means, infer `severity`, escalate, retry, or interpret `action` — the producer is the authority on all of that.

It is also the cluster's error fan-out. With `--peer-sync` (which the broker always passes) the same in-process store serves a cross-node HTTPS surface on `:14319`, learns its peer set from the broker's discovery relay, and pushes its own local-origin snapshot to every pinned cluster peer, so each node's UI renders the whole cluster's errors from one list. The store keys by the composite `(nodeId, id)` because ids are unique only within a node, and a node propagates only entries whose `nodeId` equals its own — the single rule that makes push fan-out loop-free. Both peer directions are pin-based cluster mTLS with no plaintext personality: a node that belongs to no cluster exchanges nothing there and remains a purely local datastore.

## 2. Scope

**In scope**

- Upsert an `errors.ServiceError` by `(nodeId, id)` with highest-`timestamp`-wins resolution; delete by `(localNodeID, id)` on clear.
- Serve `errors:get-initial` snapshots and push `errors:update` on every committed change, always as the full, deterministically sorted list.
- Serve `POST /v1/errors` (ingest a peer's `SyncEnvelope` and reconcile replace-by-origin) and `GET /v1/errors` (this node's local-origin snapshot).
- Push the local-origin snapshot to pinned peers on local change, on peer discovery, on a 30 s heartbeat, and on a cluster membership flip.
- Evict every stored entry originating on a peer that leaves the discovery set.
- Gate both HTTPS directions on live cluster membership and per-peer certificate pins read from `--cluster-dir`.

**Out of scope**

- Detecting failures and choosing `id`, `severity`, `action`, `engineType`, `operation`, `modelName` — each producer owns its own.
- Stamping `nodeId`, `timestamp`, and `clearedBy` on producer frames — `nvpair-ui-broker` (§7.6).
- mDNS advertisement and browsing — the `nvpair-node-scanner` daemon; the peer set arrives as `discovery:nodes` snapshots from the broker's relay.
- Cluster identity, certificate minting, PIN pairing, and the `trusted/` pin store — `nvpair-cluster-manager`.
- Relaying errors to the desktop client or `nvpair-tui` — `nvpair-ui-broker`.
- Cross-restart durability — nothing persists the list; producers re-emit (§9).
- Workload lifecycle — `nvpair-workload-manager`. Inference routing and node ranking — `nvpair-proxy` and `nvpair-job-scheduler`.

## 3. Key Use Cases

- **Report a local error**: a producer emits `errors:report` with a `ServiceError`; the broker stamps `nodeId` / `timestamp` and forwards it. The store upserts under `(nodeId, id)`, answers `null` if the frame was a request, then emits `errors:update` with the full list and fires the local-change hook so `PeerSync` pushes to peers.
- **Clear (acknowledge) an error**: the broker forwards `errors:clear` with `clearedBy` stamped. The entry at `(localNodeID, id)` is deleted, `errors:update` carries the reduced list, and the next peer push simply omits the entry, so peers drop it too.
- **Seed a new consumer**: the broker relays a client's `errors:get-initial` as an id-bearing request and returns the merged cross-node list.
- **Propagate to peers**: on every local change (coalesced through a size-1 trigger channel) the local-origin snapshot is marshalled once into a `SyncEnvelope` and `POST`ed concurrently to each pinned peer at `https://<host>:<port>/v1/errors` with a 3 s per-peer timeout, where `<host>` is whichever of the peer's published addresses this node has confirmed it can reach (§7.3).
- **Reach a multi-homed peer**: a peer publishing a LAN address and a direct-connect link address is kept with both. The first push connects to each in the peer's own order and uses the first that accepts; later pushes reuse that address until a transport failure against it, when the next push confirms again.
- **Ingest a peer snapshot**: `POST /v1/errors` decodes the envelope and reconciles it as authoritative for `env.nodeId` — upsert every entry present, evict every stored entry for that `nodeId` absent from the body — then answers `204`. Peer-driven changes emit `errors:update` locally but deliberately do not fire the outbound push hook.
- **Peer departs**: a `removed` discovery event drops the peer from the push set and evicts all of its entries, so an offline node stops haunting the list.
- **Join or leave a cluster while running**: `Mesh.Watch` re-derives membership every 2 s; a flip logs the new personality and triggers a full re-push. The listener is never rebound — its leaf is resolved per handshake.
- **Edge case — stale report**: a report whose `timestamp` is older than the stored entry's is dropped and emits no `errors:update`, so a consumer never re-renders on a dropped frame.
- **Edge case — equal timestamps**: an equal-`timestamp` report replaces the stored entry and does emit `errors:update`. A producer re-emitting a steady-state error is saying "still broken right now", so its refreshed message and metadata win.
- **Edge case — ack until re-emit**: a clear is dismissal only. A later report of the same id resurrects the entry; the producer, not the user, is authoritative.
- **Edge case — same id on two nodes**: `ollama-proxy:upstream-unreachable:x` reported by two nodes coexists as two entries because the key is composite; the wire payload stays a flat array sorted by `id` then `nodeId`.
- **Edge case — a peer claims a third node's origin**: `reconcilePeer` stamps the envelope's `nodeId` onto every ingested entry, so a peer can only ever mutate its own slice of the keyspace.
- **Edge case — a peer reconciles our own origin**: an envelope whose `nodeId` equals `localNodeID` (or is empty) is a no-op, and `handleIngest` additionally rejects an empty `nodeId` with `400` so the sender gets a signal rather than a silent accept.
- **Edge case — unclustered node**: it holds no pins, so every discovered peer is dropped from the push set and every inbound handshake is refused. The local datastore is unaffected.
- **Edge case — non-member that completes a handshake**: a host that pins us but that we do not pin back finishes TLS (the layer requires any client cert) and is then refused `403` by the per-request pin check.
- **Edge case — no-op clear**: clearing an absent id still answers `null` successfully and pushes nothing. Producers fire defensive clears on success, and treating those as errors would generate UI noise.
- **Edge case — invalid notification**: a malformed or field-missing `errors:report` / `errors:clear` notification is logged at warn and dropped, with no response and no `errors:update`.

## 4. Open Questions / Risks

- **Risk — nothing survives a restart**: the store is a `map[errKey]ServiceError` and there is no disk write anywhere in the service. A crash or restart empties the node's list until each producer re-emits, which is exactly what the broker's crash handler relies on. Consumers must treat the list as session state, not history.
- **Risk — an entry with an empty `nodeId` is unclearable**: `errKey` uses the reported `nodeId` verbatim while `clearByID` is scoped to `localNodeID`, so a report that reaches the store with no origin lands under `("", id)` and no clear can ever match it. Both broker paths and the desktop client stamp an origin specifically to avoid this; any new report path must do the same.
- **Risk — the read loop cannot tell a bad frame from a dead transport**: `readLoop` returns only on `io.EOF` or a cancelled context and otherwise logs and continues, so a persistently failing read that is not EOF spins rather than exiting. `nvpair-shared/jsonrpc` already models the difference with `DecodeError`, and the manager ignores it.
- **Risk — a bind failure takes down the local datastore too**: `runHTTPServer`'s bind error is `log.Fatalf`, so anything already holding `:14319` turns into a supervised restart loop until the attempt budget is spent. That is deliberate — a node that advertises `er` but cannot receive is worse than a loud crash — but it couples stdio availability to a port conflict.
- **Risk — eviction follows discovery, so a flap hides real errors**: a peer that momentarily drops out of the relay snapshot has all of its entries evicted and then re-learned on its next push. The anti-flap guard lives in the discovery daemon, not here.
- **Risk — convergence is bounded by the heartbeat**: delivery is best-effort with no retry queue, so a dropped or refused `POST` leaves a peer stale for up to 30 s.
- **Risk — cross-node conflict resolution trusts wall clocks**: highest-`timestamp` wins across nodes, so a node with a skewed clock can pin its entries as permanently newer than anyone else's.
- **Risk — the store is unbounded**: nothing caps entry count or evicts by age, so a producer generating unbounded ids grows the map (and every `errors:update` payload) for the life of the process.
- **Future — `clearedBy` is accepted and ignored**: the field is parsed, logged, and never stored or acted on. Cross-node clear propagation happens implicitly, because a cleared entry drops out of the origin's next snapshot, and no code consumes `clearedBy` today.
- **`severity` / `action` values (open — needs a finalized enum)**: both ship as free strings. Current values are `info` / `warning` / `error` and `dismiss` / `retry` / `none`. A final enum can replace them without a wire-shape change, so this service validates neither.
- **`errors:update` payload shape (open — needs consumer agreement before any change)**: `params` is a bare JSON array rather than an object, so no field can ever be added alongside the list without breaking every consumer at once.

## 5. Requirements

**Functional**

- Accept `errors:report` and `errors:clear` in both request and notification form, sharing one validation, mutation, and broadcast path; only the response differs.
- Require `id` and `message` on a report and `id` on a clear; pass every other field through verbatim without interpretation.
- Key by `(nodeId, id)`; resolve a report against an existing entry by highest `timestamp`, with equal timestamps replacing.
- Answer `errors:get-initial` with the merged list of every node's entries, as an empty array when the store is empty — never `null`.
- Emit `errors:update` after, and only after, a mutation that changed stored state, always carrying the full list sorted by `id` then `nodeId`.
- Serve `POST /v1/errors` with replace-by-origin reconciliation and `GET /v1/errors` with the local-origin `SyncEnvelope`, both only to a pinned cluster peer.
- Push only local-origin entries; a peer-learned entry is stored, rendered, and never forwarded.
- Subscribe to the broker relay for `er` nodes and reconcile each `discovery:nodes` snapshot into discovered / updated / removed peer events by diffing it against the previous snapshot.
- Evict a departed peer's entries and broadcast the reduced list.

**Non-functional**

- Both cross-node directions are pin-based cluster mTLS, unconditionally; there is no plaintext personality and no broker-absent fallback.
- Re-derive membership and pins from `--cluster-dir` on every ingress request and every push round, so a create, join, or leave converges in place without a restart.
- One slow or unreachable peer must not stall the others, the read loop, or the local pipeline: per-peer goroutines with a 3 s timeout, failures logged and dropped.
- Keep the local datastore fully functional with no network, no cluster, and no peers.
- Never log inference message bodies, certificates, key material, or pairing data — this service handles none of them, and its logs carry only ids, node ids, counts, timestamps, and normalized errors.

## 6. Inputs and Outputs

**Inputs** — newline-delimited JSON-RPC 2.0 frames from the supervising broker on `stdin` (or the `--ipc` socket / named pipe): `errors:report`, `errors:clear`, `errors:get-initial`, `shutdown`, `log/set-level`, and `discovery:nodes` relay snapshots. From peer nodes: `POST /v1/errors` bodies carrying a `SyncEnvelope` over cluster mTLS. From disk, read-only: the `--cluster-dir` materials (`node.crt`, `node.key`, `admission.json`, `trusted/<uuid>.json`).

**Outputs** — JSON-RPC frames on `stdout` (or the same IPC endpoint): a `ready` notification at startup, one `discovery:subscribe` when peer-sync is enabled, `errors:update` notifications, and exactly one response per id-bearing request. To peers: `POST /v1/errors` bodies carrying its own `SyncEnvelope`, and `204` / `400` / `403` / `405` responses on its own endpoint. Human-readable logs on `stderr`, which the parent captures.

`ServiceError` — the shape every producer emits and every consumer reads (`nvpair-shared/errors`). `id`, `message`, and `timestamp` are required; the rest are omitted when empty:

```json
{
  "id": "engine-manager:pull-failed:ollama:demo:1b",
  "message": "ollama experienced an error while downloading a model",
  "timestamp": 1716998400000,
  "nodeId": "8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33",
  "severity": "error",
  "action": "retry",
  "engineType": "ollama",
  "operation": "pull",
  "modelName": "demo:1b"
}
```

`ClearParams` — the `errors:clear` payload:

```json
{ "id": "engine-manager:pull-failed:ollama:demo:1b", "clearedBy": "8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33" }
```

`SyncEnvelope` — the cross-node body; `errors` is always the sender's complete local-origin set:

```json
{
  "nodeId": "8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33",
  "errors": [
    { "id": "manual-nodes:probe-failed:peer-b", "message": "peer-b probe failed", "timestamp": 1716998400000, "nodeId": "8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33" }
  ]
}
```

Example inbound report, notification form, as it reaches this service after the broker has stamped origin and time:

```json
{"jsonrpc":"2.0","method":"errors:report","params":{"id":"ollama-proxy:upstream-unreachable:peer-b","message":"Upstream node \"peer-b\" is no longer reachable (dropped from discovery)","timestamp":1716998400000,"nodeId":"8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33","severity":"warning","action":"none"}}
```

Example outbound broadcast — `params` is the bare full list, sorted by `id` then `nodeId`:

```json
{"jsonrpc":"2.0","method":"errors:update","params":[{"id":"ollama-proxy:upstream-unreachable:peer-b","message":"Upstream node \"peer-b\" is no longer reachable (dropped from discovery)","timestamp":1716998400000,"nodeId":"8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33","severity":"warning","action":"none"},{"id":"supervisor:subprocess-crashed:nvpair-node-info","message":"subprocess nvpair-node-info exited unexpectedly","timestamp":1716998401000,"nodeId":"8f1c2b40-5d9e-4a71-9d2e-6c0f2a7b1e33","severity":"error","action":"none"}]}
```

## 7. API / Interface Contract

Three interfaces: a **local JSON-RPC interface** toward the supervising broker (stdio or `--ipc`), an **inter-node HTTPS interface** toward peer `nvpair-errors` instances (`:14319`, cluster mTLS, no plaintext listener), and a **read-only on-disk interface** to the cluster trust directory that gates the second.

### 7.0 Methods and notifications

| Method | Params | Result |
|---|---|---|
| `errors:get-initial` | none (any params ignored) | `ServiceError[]` — every node's entries, sorted by `id` then `nodeId`; `[]` when empty |
| `errors:report` | `ServiceError` | `null` |
| `errors:clear` | `ClearParams` | `null` |
| `shutdown` | none | `null`, sent before cancellation |
| `log/set-level` | `{ "level": "error\|warn\|info\|debug" }` | `{ "level": "<resolved>" }` |

`errors:report`, `errors:clear`, and `log/set-level` are also accepted as notifications, taking the same path with no response frame. `discovery:nodes` is accepted only as a notification, with params `{ "nodes": [DirectoryNode, ...] }`. `errors:get-initial` and `shutdown` are request-only — as notifications they fall through to the ignored-notification log.

Outbound notifications: `ready` with `{ "version": "<build version>" }`, emitted once before the read loop starts; `errors:update` whose `params` is the full `ServiceError[]` snapshot, emitted after every committed change (local report or clear, peer reconcile, peer eviction) and never after a no-op; and `discovery:subscribe` with `{ "services": ["er"] }`, sent once at startup when `--peer-sync` is set so the broker relay begins pushing snapshots.

### 7.1 Inter-node HTTPS interface

- Endpoint `/v1/errors` on TCP `:14319` (`--port`), bound once for the process lifetime whenever `--peer-sync` is set.
- `POST /v1/errors` — body is a `SyncEnvelope`. Reconciles replace-by-origin for its `nodeId`, then `204 No Content`. An undecodable body is `400 invalid JSON body`; an empty `nodeId` is `400 "nodeId" is required`.
- `GET /v1/errors` — returns this node's `SyncEnvelope`, byte-identical to what a push carries, so the endpoint doubles as a probe of what this node would propagate.
- Any other method is `405` with `Allow: GET, POST`. A caller that is not a pinned cluster peer is `403 forbidden: not a pinned cluster peer`.
- Outbound pushes are `POST https://<host>:<port>/v1/errors` with `Content-Type: application/json`, a 3 s client timeout, and one goroutine per peer; the round waits for the batch so cadence reflects real completion. `<host>:<port>` is the address confirmed for that peer (§7.3). A non-2xx or transport failure is logged and dropped, and the next trigger or heartbeat repairs it. A transport failure also retires the confirmed address; a non-2xx does not, because an answer proves the address works.
- Each response body is drained before it is closed, bounded by `maxDrainBytes` (8 KiB). An unread body cannot be returned to the idle pool, so skipping the drain would force a fresh mTLS handshake on the next push; the bound means a peer that streams more than a `204` or a short error string forfeits its pooled connection rather than this node's bandwidth.
- `ReadHeaderTimeout` is 5 s and `IdleTimeout` is `clustertrust.PeerListenerIdleTimeout` (105 s). That is deliberately longer than the calling pool's `PeerIdleTimeout` (90 s) so the client always reaps a connection first and never selects one this side is closing. On context cancellation the server drains with a 3 s grace period.

### 7.2 Trust and mTLS gating

- `clustertrust.Open(--cluster-dir)` returns a live `Mesh` even when the directory is absent or empty. Absence of membership is a state of that mesh rather than an error, which is what lets the listener bind once and change personality later.
- The listener's `tls.Config` resolves this node's leaf per handshake. While the node is not a member no leaf resolves and every handshake is refused; the moment it becomes a member the same listener serves pinned peers with no rebind and no restart.
- The TLS layer requires any client certificate; the real gate is `Mesh.VerifyClientPin`, which reads the caller's claimed UUID from its certificate (`urn:nvpair:node:<uuid>` URI SAN, else the subject CN) and accepts only a byte-for-byte match against `trusted/<uuid>.json`. Both the ingress handler and the push path call `Mesh.Refresh()` first, so pin and membership changes apply within the request.
- The check is unconditional rather than conditional on being clustered: `VerifyClientPin` already answers false for a non-member, so an unclustered node rejects everything, and there is no membership branch a future caller could widen.
- Outbound, a `clustertrust.PeerClientPool` holds one long-lived client per pinned peer, each presenting this node's leaf and pinning that peer's exact server certificate. A peer with no current pin has no client and cannot be dialed at all, so the cluster gate is enforced client-side too. The push URL is always `https://` — there is no scheme to negotiate.
- The pool exists because a client built per push would pay a full mTLS handshake every heartbeat and leak the socket afterwards, so a long-running node would accumulate dead connections to every peer it had ever synced with. Its transport caps idle connections per host and at 64 across the cluster, keeps connections alive for 30 s, and expires an idle one after `PeerIdleTimeout` (90 s).
- Pool membership tracks pins rather than outliving them: every round calls `Mesh.Refresh()` and then `DropUnpinned()`, retiring the clients the fresh pins no longer cover, and shutdown calls `CloseIdle()`. An unpaired peer therefore loses its pooled connection in the same round its pin disappears.
- `Mesh.Watch` polls every `clustertrust.RefreshInterval` (2 s) and, on a flip, logs the new personality and triggers a full re-push, because the peer set was filtered under the previous personality.

### 7.3 Peer discovery

- This service opens no multicast socket. The broker registers the `er` service key at port `14319` with the `nvpair-node-scanner` daemon, so the node rides its single `_nvpair-node._tcp` record.
- On startup with `--peer-sync` the service sends `discovery:subscribe` for `er`; the broker subscribes it to the relay directory and pushes a `discovery:nodes` snapshot on every change. A failure to subscribe is logged, not fatal — peer-sync just stays empty.
- Each snapshot is authoritative and is diffed against the previous one: `discovered` for a newly present peer, `updated` when its dialable identity changed (`ID`, `Host`, `Port`, `Addresses`, or `TXT`), `removed` for one that dropped out. Unchanged peers emit nothing, so a snapshot storm does not become a push storm, and any prior drift self-corrects.
- A `DirectoryNode` projects to a peer only when it advertises `er` and carries at least one candidate address (`CandidateIPs()`). Peer identity is its `hostUuid` — the same value each node stamps as its errors' origin and uses as its own `localNodeID` — so the self-check and eviction key on an identity that survives a host rename. `name` is kept as the display host and last-resort dial host, the node's published address list is carried as `ip=` / `ips=` TXT (`AddressTXT()`), and `clusterUuid` is reconstructed into a `cluster-uuid=` TXT entry so pin selection is unchanged.
- A peer's push target keeps **every** published address as a `host:port` candidate, in the peer's own ranked order (`netpick.Candidates`): its `ip=` / `ips=` list first, then anything else discovery resolved, then its hostname only when it published no address. A peer with no candidate or no port is skipped with a warning. Every address is kept rather than the best one because the peer ranks from its own vantage point, and a direct-connect link only its cabled neighbour can use looks as good from there as the LAN.
- Which candidate to use is confirmed at push time by a `reach.Chooser`. With nothing remembered, it makes a TCP connection to each candidate in order, up to 1 s each (`reach.DefaultTimeout`), and uses the first that accepts; a peer with a single candidate skips the check. The answer is remembered until a transport failure retires it or the peer stops publishing that address, so a steady heartbeat costs no extra connections. If no candidate accepts, the top-ranked one is used anyway and the push's own failure is what gets logged. The confirmation has no deadline of its own: it is not bounded by the 3 s push timeout, so a first push to a peer with several dead addresses can take a few seconds longer.
- A discovery event whose id equals `localNodeID` is ignored outright, and an event for a peer this node holds no pin for removes it from the push set.

### 7.4 Reconciliation, keying, and loop prevention

- **Keying**: `errKey{nodeID, id}`, so two nodes emitting the same id coexist. Wire payloads stay flat arrays sorted by `id` first (a consumer can diff by position) then by `nodeId`.
- **Upsert**: an incoming entry loses only when a stored entry for the same key has a strictly greater `timestamp`. Equal timestamps replace, so a re-emit refreshes message and metadata.
- **Replace-by-origin**: an ingested envelope is authoritative for its `nodeId`. Every entry present is upserted with the envelope's `nodeId` stamped over whatever the entry claimed, and every stored entry under that `nodeId` absent from the body is deleted. Entries with an empty `id` are skipped, and a byte-identical entry is not counted as a change. Report, clear, and initial sync therefore collapse into one idempotent, self-healing operation with no separate clear or sync channel.
- **Loop prevention**: only `localSnapshot()` — entries whose key `nodeID` equals `localNodeID` — is ever served or pushed, so a peer-learned entry can never be re-propagated and a push cannot bounce around the cluster.
- **Local-change hook**: only a local report or clear that changed state fires the push trigger. Peer reconciles and evictions broadcast `errors:update` locally but never push outward, which is the second half of the loop guarantee and avoids an echo.
- **Clear scoping**: a clear resolves against `(localNodeID, id)` only. A peer's entry is owned by its origin, which would just re-push it, and dropping it locally for one heartbeat would only produce UI flicker.

### 7.5 Persistence format

None. The store is in-memory for the process lifetime, guarded by one `sync.RWMutex`. The only files this service touches are the read-only cluster trust materials under `--cluster-dir` (§9).

### 7.6 Field stamping by the broker

`nvpair-errors` never invents identity or time. Producers emit `timestamp: 0` and no `nodeId`; `nvpair-ui-broker` fills `nodeId` with its resolved per-host UUID and `timestamp` with the current Unix milliseconds when either is unset, stamps `clearedBy` with that same UUID on every outgoing clear (a producer-supplied `clearedBy` is discarded — the broker is its only writer), and passes the same UUID to this process as `--node-id`, so `localNodeID`, the stamped origin, and the advertised `hostUuid` are one value.

### 7.7 Error codes

- `-32602` — malformed params, a report missing `id` or `message`, a clear missing `id`, or an unparseable `log/set-level` level. Notification-form frames with the same problems are logged at warn and dropped.
- `-32601` — unknown method (`method not found: <name>`), never a silent drop.
- HTTP: `204` accepted ingest; `400` malformed body or empty `nodeId`; `403` caller is not a pinned cluster peer; `405` method other than `GET` / `POST`.

### 7.8 Versioning

- The inter-node interface is versioned by URL prefix (`/v1/errors`).
- JSON-RPC method names are namespaced and evolve additively; new behavior is a new method, never a redefinition of an existing one.
- `ServiceError` grows by optional fields only: unknown fields are ignored on decode, and every optional field is `omitempty` on encode. `severity` and `action` are plain strings so their value sets can be finalized without a wire change.
- The binary's version is stamped at build time into `main.Version`, reported in `ready`, and printed by `--version`.

## 8. Dependencies

- **Upstream**: `nvpair-ui-broker` — the parent process. It spawns this binary, feeds it every producer's report and clear, relays client requests, pushes `discovery:nodes` snapshots, forwards `log/set-level`, and supervises restarts.
- **Downstream**: `nvpair-ui-broker` again, as the consumer of `errors:update` and `errors:get-initial` results, which it relays unconditionally to the desktop client and `nvpair-tui`; and peer `nvpair-errors` instances that receive `POST /v1/errors`.
- **External**: the cluster trust directory written by `nvpair-cluster-manager` (`node.crt`, `node.key`, `admission.json`, `trusted/*.json`); the `nvpair-node-scanner` discovery daemon, indirectly, as the origin of the relay snapshots; a LAN that permits inbound TCP `14319` between cluster members. First-party packages: `nvpair-shared/errors`, `jsonrpc`, `clustertrust`, `noderec`, `discovery`, `netpick`, `reach`, `applog`, `ipc`.

## 9. Data Ownership

- **Owned**: the merged in-memory error map keyed by `(nodeId, id)`, the `er` peer set projected from the last relay snapshot, the push-target set holding each peer's candidate `host:port` list and cluster UUID, and the address confirmed reachable for each peer. All transient.
- **Source of truth**: for local-origin entries, yes, for the life of the process — it is the node's registry of record and what the UI renders. For peer entries, no: the origin node is authoritative and its next push replaces them wholesale. Across a restart there is no source of truth at all; the producers are, and they re-emit.
- **Storage**: none written. No database, no state file, and no log file of its own — logs go to `stderr`. The only on-disk reads are the cluster materials under `--cluster-dir`, which the broker defaults to `cluster/` inside the per-user data directory `<base>/Nvidia Corporation/Personal AI Router`, where `<base>` is `%LocalAppData%` on Windows, `$XDG_CONFIG_HOME` or `~/.config` on Linux, and `~/Library/Application Support` on macOS.

## 10. Design Constraints

- **Performance**: error events are rare (failure transitions, not telemetry), which is what makes resending the full snapshot on every change affordable; a push round marshals the envelope once and reuses the bytes across all peers, and reuses a pooled per-peer connection so a heartbeat costs a request rather than an mTLS handshake. `Mesh.Refresh` per ingress request and per push round is a warm-cache directory read, not X.509 work — a certificate is re-parsed only when its bytes change.
- **Scalability**: sized for a cluster of roughly a dozen nodes. Memory is O(total live errors cluster-wide) and each local change fans out to every pinned peer. Nothing is bounded or evicted by count, so payload size tracks producer behavior (§4).
- **Reliability**: push is best-effort with no retry queue; the 30 s heartbeat and the next local change are the only repair mechanisms, which full-snapshot idempotency makes sufficient. Conflict resolution is timestamp-based rather than arrival-ordered so out-of-order delivery over different paths still converges. Outbound codec writes are serialized so frames never interleave, a request's response is always written before the resulting `errors:update`, and a failed broadcast is logged rather than propagated because a consumer can re-seed with `errors:get-initial`.
- **Security**: the cross-node surface is a cluster data plane, not a LAN service. Both directions require pin-based mTLS against live membership, with no plaintext personality and no downgrade path. This service implements no cryptography of its own — it consumes `nvpair-shared/clustertrust` and never handles key material beyond passing the loaded leaf to the TLS stack. An envelope's `nodeId` bounds what a peer may modify, so a pinned-but-buggy peer can corrupt only its own entries.
- **Compliance**: payloads carry operational metadata only — ids, human-readable failure messages, engine names, model names, operations, and opaque node UUIDs. Prompts, chat messages, request or response bodies, PINs, and certificates must never enter a `ServiceError`, and nothing here logs them.

## 11. Assumptions

- The broker is always the parent: it writes frames to `stdin` and reads `stdout`. When it exits, `stdin` EOF is the shutdown signal — there is no reconnect, buffering, or standalone service mode.
- The broker resolves a stable per-host UUID and passes it as `--node-id` before the read loop and peer-sync start. `SetLocalNodeID` is called during startup wiring only, which is why the field needs no lock and why `PeerSync` may capture it at construction. Absent the flag, `localNodeID` is the hostname (or `unknown` when the hostname cannot be read), which is the bare-binary and integration-test behavior.
- Producers supply well-formed frames and own their id namespace, conventionally `<producer-shortname>:<error-class>[:<context>]` — for example `ollama-proxy:upstream-unreachable:<node-id>`, `lmstudio-proxy:upstream-unreachable:<node-id>`, `manual-nodes:probe-failed:<node-id>`, `engine-manager:install-failed:<engine>`, and `supervisor:subprocess-crashed:<name>`. Reusing a stable id across re-emits is what makes a sticky error upsert instead of pile up.
- `timestamp` is Unix milliseconds and node clocks are roughly in sync.
- A relay `DirectoryNode` always carries a `hostUuid`, so peer identity needs no name fallback.
- Only one process per host serves `:14319`, and cluster members can reach each other on it.

## 12. Failure Modes and Mitigations

- **Peer unreachable, slow, or partitioned**: that peer's list goes stale and this node's errors are missing from its UI. → Per-peer goroutine with a 3 s timeout so one peer cannot stall the fan-out or the read loop; the 30 s heartbeat re-pushes the full snapshot and repairs any divergence in one round.
- **Push refused at the pin gate (`403`), or no pin resolvable for a peer**: that peer is isolated from this node's errors. → Log with the node id and cluster UUID, skip the peer, and re-evaluate membership on the next round so a completed pairing takes effect without a restart.
- **This node joins or leaves a cluster mid-session**: the peer set was filtered under the old personality and would diverge. → `Mesh.Watch` sees the flip within 2 s and triggers a full re-push; the listener's certificate is resolved per handshake, so ingress follows the same flip with no rebind.
- **Malformed or hostile peer body**: could inject entries attributed to a third node, or overwrite local-origin state. → `400` on an undecodable body or empty `nodeId`; the envelope's `nodeId` is stamped onto every entry; an envelope for our own `nodeId` is a no-op; empty-`id` entries are skipped.
- **Stale or duplicate report**: a consumer could re-render on every dropped frame or regress to older text. → Highest-`timestamp`-wins with no `errors:update` on a no-op, and equal-timestamp replacement so a steady-state re-emit still refreshes the message.
- **Invalid local frame**: could break the parser or store a useless entry. → Validate `id` and `message` before mutating, answer `-32602` in request form, log-and-drop in notification form, and never broadcast on invalid input.
- **`:14319` already in use**: no peer can reach this node. → Treated as fatal so the supervisor logs and restarts rather than silently degrading to "advertised but unreachable"; the stdio datastore is unaffected once the port is free.
- **Broker exits (`stdin` EOF) or the process is signalled**: an orphan would accept peer pushes with nowhere to forward them. → EOF, `SIGINT`, `SIGTERM`, and the `shutdown` request all cancel one root context that unwinds the read loop, the HTTPS server, the push loop, and the membership watch together.
- **This service crashes**: the pipeline's own sink is gone, so it cannot report its own death. → The broker drops its handle (making every other worker's report a benign no-op), logs to `stderr`, and restarts it; producers resurrect their entries on the next re-emit under the ack-until-re-emit contract. The list is empty in the meantime (§4).
- **A peer flaps out of discovery**: its errors disappear and reappear. → Eviction is driven only by the relay's authoritative snapshot, and the discovery daemon's probe-before-evict guard keeps a still-reachable node across a transient mDNS miss.

## 13. Observability

- **Logging**: structured `slog` to `stderr` through `nvpair-shared/applog`, prefixed with the process name, level-gated, and changeable at runtime via `log/set-level`. Info: startup transport and peer-sync mode, listener bind with port and clustered flag, each report upsert (`id`, `nodeId`, `timestamp`), each clear (`id`, `clearedBy`), peer reconcile and eviction with counts, peer discovered with its candidate addresses and peer removed, membership flips, and shutdown. Debug: dropped stale reports, no-op clears, an ingested snapshot's node id and count, successful pushes, and peers skipped for want of a pin. Warn: invalid notification params, push failures and non-2xx responses, unreachable peer addresses, invalid relay snapshots, and failed `errors:update` sends.
- **Metrics**: none. There is no metrics endpoint, no exported counter, and no `/metrics` route — the log stream is the only quantitative signal, so any rate or peer-health figure has to be derived from it.
- **Alerts**: none emitted by this service. The signals a supervising layer can usefully watch for in the log stream are a sustained `403` or push-failure rate toward one peer (a pin or membership problem), a repeated fatal bind on `:14319`, and a monotonically growing snapshot size (a producer generating unbounded ids).

## 14. Sample usage

On node A the Ollama facade of `nvpair-proxy` sees an upstream node drop out of discovery and emits, on the proxy's stdout, `errors:report` with id `ollama-proxy:upstream-unreachable:<peer-uuid>`, a message, and `timestamp: 0`. The broker's notification demux claims the frame, fills `nodeId` with node A's per-host UUID and `timestamp` with the current milliseconds, and forwards it as a notification into `nvpair-errors`.

The store validates `id` and `message`, upserts under `(node-A-uuid, ollama-proxy:upstream-unreachable:<peer-uuid>)`, and — because stored state changed — emits `errors:update` carrying the whole sorted list. The broker relays that array verbatim: the desktop client replaces its error list and an attached `nvpair-tui` redraws its Errors tab. The same commit fires the local-change hook, so `PeerSync` coalesces a trigger, refreshes membership, marshals `{"nodeId":"<node-A-uuid>","errors":[...]}` from `LocalSnapshot()`, and `POST`s it concurrently to every pinned peer at `https://<peer-address>:14319/v1/errors`, using the address already confirmed for each.

On node B the handler refreshes its mesh, verifies node A's client certificate against `trusted/<node-A-cluster-uuid>.json`, decodes the envelope, and reconciles it as authoritative for node A: the new entry is upserted under `(node-A-uuid, ...)`, anything previously stored for node A and absent from the body is evicted, and `204` goes back. Node B emits its own `errors:update`, so its UI now shows node A's failure — and it pushes nothing outward, because the change was not local-origin.

When the peer reappears in discovery the facade emits `errors:clear` for the same id. The broker stamps `clearedBy` with node A's UUID; the store deletes `(node-A-uuid, id)`, emits `errors:update` with the reduced list, and pushes a snapshot that simply no longer contains the entry. Node B's replace-by-origin reconcile evicts it, with no dedicated clear endpoint anywhere in the path. Had that push been dropped, node B would have converged on the next 30 s heartbeat instead.

## 15. Process model, CLI, and build wiring

`nvpair-errors` is a single-binary child process. It parses flags, initializes logging, opens its transport, builds one `Manager` over a shared newline JSON-RPC codec, optionally wires the networking layer, and then runs one read loop under a root context.

Flags, with defaults exactly as declared:

- `--ipc ""` — IPC endpoint: a Unix domain socket path or Windows named pipe. Empty selects `stdin`/`stdout`. A dial failure is fatal.
- `--peer-sync false` — enable the cross-node surface: bind the HTTPS endpoint, subscribe to the discovery relay, and push to pinned peers. Off, the binary is a purely local stdio datastore with no listener and no relay subscription.
- `--port 14319` — port for the cross-node errors endpoint, which serves HTTPS only; used only with `--peer-sync`.
- `--cluster-dir ""` — cluster config directory (`node.crt` / `node.key` plus `trusted/`). Cross-node sync is pin-based cluster mTLS, so without a usable identity and live membership here the node serves and pushes nothing.
- `--node-id ""` — this node's stable origin id. Empty keeps the hostname default.
- `--log-level ""` — `debug` / `info` / `warn` / `error`; falls back to `$NVPAIR_LOG_LEVEL`, then `info`.
- `--version` — print `main.Version` and exit.

The broker spawns it as `nvpair-errors --peer-sync --log-level <level> [--cluster-dir <dir>] --node-id <uuid>`, with stdin/stdout pipes, stderr inherited, and the console window hidden on Windows. It never passes `--port`, so the default `14319` is exactly the port the broker registers as the `er` service key. The spawn is optional and non-fatal: an unresolvable binary means the error pipeline is disabled and producer reports are dropped. A crash is auto-restarted with exponential backoff, and because this worker holds a live view of the cluster dir the broker does not restart it on a membership change.

`Manager.Run` sends `ready` first, then subscribes to the relay when peer-sync is enabled, then blocks in the read loop. Shutdown has three triggers, all cancelling the same context: `stdin` EOF (how the broker stops it, by closing the pipe), `SIGINT` or `SIGTERM`, and the `shutdown` request, which responds before cancelling. The HTTPS server, push loop, and membership watch are all children of that context, so there is no separate teardown to coordinate.

Build and staging: `services/build.sh` and `services/build.bat` build it as step 6 of 12, reading its version from `services/versions.json` under `.components["nvpair-errors"]` and stamping it with `-ldflags "-X main.Version=<version>"`, then copy the artifact into `services/build/bin/`. `services/installer_build.sh` stages that binary into the Linux/macOS archive's `bin/`, and `services/installer/nvpair-setup.nsi` installs `bin\nvpair-errors.exe`, terminates it on upgrade and uninstall, and adds inbound firewall rules scoped to `remoteip=localsubnet` across all profiles. One of those rules opens UDP 5353 for this binary, which the current design does not need: this service opens no multicast socket, because discovery belongs to the `nvpair-node-scanner` daemon.
