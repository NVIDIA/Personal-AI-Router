<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Manual Nodes (`nvpair-manual-nodes`)

## 1. Purpose

`nvpair-manual-nodes` owns the set of user-entered peer node addresses on networks where mDNS discovery is unavailable, filtered, or incomplete. A client hands it a bare host (`node/add`), and it becomes the authority for whether that host is currently reachable and what it serves: it probes each entered address for an Ollama engine, an LM Studio engine, and an `nvpair-node-info` telemetry endpoint every ten seconds, and reports the aggregate as a `ManualNodeStatus` on `node/discovered` / `node/updated` / `node/removed`. Its purpose is to make an address the user asserts routable — the supervising `nvpair-ui-broker` merges its events into the same discovery snapshot `nvpair-node-scanner` feeds, bridges reachable manual nodes into the matching engine facade of `nvpair-proxy`, and feeds their GPU telemetry to `nvpair-job-scheduler`, so a manual node is routable and schedulable exactly like a discovered one.

It exists because `nvpair-node-scanner` has no manual concept at all: the scanner only reports what it browses over mDNS, and it has no `node/add` surface. Every non-multicast path into the fleet is this worker's. It is deliberately a probe engine and nothing more — it opens no network listener, keeps its entries only in `map[string]*trackedNode`, and writes no file. The durable list and its replay after a restart live in the PAIR desktop app (§7.6, §9).

## 2. Scope

**In scope**

- Accept, track, and drop user-entered node addresses (`node/add`, `node/remove`, `nodes/list`).
- Probe each tracked address on a fixed ten-second cycle for Ollama, LM Studio, and `nvpair-node-info`.
- Derive per-engine model inventories and node-info telemetry (GPU, CPU, memory, GPU sample validity and age, `hostUuid`) from those probes.
- Emit change-gated status notifications and, on sustained unreachability, `errors:report` / `errors:clear`.
- Choose the probe transport for the node-info leg: plain HTTP, an operator-supplied TLS client, or cluster mTLS.

**Out of scope**

- Durable storage of the entry list — the desktop app persists `manual-nodes.json` and replays it (§7.6); neither this worker nor `nvpair-ui-broker` holds an authoritative copy.
- mDNS browsing and advertisement — `nvpair-node-scanner`.
- Merging manual and discovered nodes into one snapshot, the per-engine proxy bridge, and the scheduler telemetry feed — `nvpair-ui-broker`.
- Inference routing and node ranking — `nvpair-proxy` and `nvpair-job-scheduler`.
- Cluster identity, PIN pairing, certificate minting, pins, and membership — `nvpair-cluster-manager`.
- Service-error storage, ack state, and cross-node error sync — `nvpair-errors`.
- Local engine lifecycle, ports, and model operations — `nvpair-engine-manager`.

## 3. Key Use Cases

- **Add a node**: a client sends `node/add` with `{"address": "10.0.1.50"}`. The worker upserts the entry, responds immediately with an unprobed `ManualNodeStatus` (ports filled, all `*_up` false), then probes on a goroutine and emits `node/discovered` with the real outcome.
- **Track reachability**: every `probeInterval` (10 s) the probe loop walks a snapshot of the entries and re-probes each. A `node/updated` fires only when a watched field actually changed (§7.2).
- **Learn a stable identity**: once the remote's `/v1/node-info` answers with `hostUuid`, that value rides on every subsequent status, and `nvpair-ui-broker` rekeys the node from the manual id onto the real host UUID — collapsing a machine that is both manually added and mDNS-discovered into one entry.
- **Remove a node**: `node/remove` with `{"id": "lab"}` deletes the tracked entry, emits `node/removed` with just the id, and fires a defensive `errors:clear` for that node's probe-failed id.
- **List for a UI**: `nvpair-tui`'s Manual view polls `nodes/list` every 10 s to keep its reachability columns current.
- **Edge case — probe still in flight when removed**: `node/remove` between `node/add` and its initial probe completing wins. The probe goroutine re-reads the map, finds the id gone, and returns without emitting `node/discovered`; `probeNode` likewise drops its result if the id vanished mid-probe.
- **Edge case — node-info blip while an engine stays up**: a failed node-info probe returns no `hostUuid`. The previously learned value is carried forward rather than blanked, so the broker's store key does not flap from UUID to manual id and back.
- **Edge case — engine reachable, model list unparseable**: LM Studio answering `200` with a body that fails to decode is still reported `lmstudio_up: true` with no models. Ollama answering `200` on `/` but failing `/api/tags` is `ollama_up: true` with no models.
- **Edge case — duplicate add**: `node/add` with a name (or address) that resolves to an existing id overwrites that entry's `ManualEntry` in place and re-probes it. The same address added under two different names produces two independent ids that only collapse downstream, after `hostUuid` is learned.
- **Edge case — malformed params**: `node/add` whose params are not an object, or whose `address` is empty, is rejected `-32602` and adds nothing.
- **Edge case — notification instead of request**: an id-less `node/add` frame is logged and ignored; state is not mutated and nothing is emitted.

## 4. Open Questions / Risks

- **Entry durability (known limitation — fix in progress)**: the worker keeps entries only in memory and `nvpair-ui-broker` keeps no authoritative copy, so a manual-nodes restart loses the user's nodes. The broker's `clearManualNodesState` evicts the orphaned entries from the discovery store and every engine facade so the view stays honest, and the client must re-add them. This is a known limitation, and open pull requests will fix it soon.
- **Durable list has readers but no writer (open — needs a product decision)**: the desktop app reads `manual-nodes.json` (`listManualNodeEntries`) and replays it through `node/add` after broker ready, and prunes it on member removal (`removeManualNodeEntry`), but no code path appends a newly added node to it. Entries added through `nvpair-tui`'s `node/add` are never persisted. In practice the replay list stays empty unless the file is authored by hand.
- **Risk — `node/updated` on every probe**: change detection compares `cpu`, `memory`, and `gpus` by value, and also `telemetryValid` and `msSince`. Because `msSince` is the remote GPU sample's age at response time, it differs on essentially every probe, so any node whose `nvpair-node-info` reports valid GPU telemetry emits a full `node/updated` every 10 s. Each one makes the broker re-project the discovery snapshot, re-bridge the node into `nvpair-proxy`, and re-ingest its telemetry.
- **Risk — fixed engine and telemetry ports**: `11434` (Ollama), `1234` (LM Studio), and `14318` (plain-HTTP node-info) are compiled in. A remote engine on a non-default port is invisible, and an address entered as `host:port` produces a malformed probe URL, so the node reads permanently down. Only `tls_port` lets a caller move a probe port, and only for the node-info leg.
- **Risk — non-EOF read errors loop**: `readLoop` treats every non-`io.EOF` read error as recoverable and continues. `nvpair-shared/jsonrpc` distinguishes a per-frame `DecodeError` from a terminal transport error, but the manager ignores that distinction, so a permanently failing pipe spins the loop logging on each iteration.
- **Risk — IP-literal entries cannot self-heal**: an entry added by raw IP is dead once the device is reassigned an address; only a hostname entry re-resolves (keep-alives are disabled precisely so each probe re-resolves). The probe-failed message says so, but nothing re-discovers the node.
- **Risk — no address validation**: any non-empty string is accepted as an address. A typo is indistinguishable from a powered-off node until the 30-second failure threshold reports it.
- **Risk — inert `mtls` field**: `mtls` is persisted and echoed as `mtls_required`, but the probe path never reads it; the transport decision is driven entirely by `tls_port` and the cluster mesh. A caller could reasonably read it as a requirement it is not.
- **Firewall inventory divergence (open — needs reconciliation)**: the Windows NSIS installer adds an inbound rule `"NVPAIR Manual Nodes"` for a binary that opens no listening socket, while `MODULAR_RUNTIME_BINARIES` marks it `needsFirewallAccess: false` and the macOS privileged helper grants it no rule. The inbound rule is unnecessary.
- **Future — engine port discovery**: if `nvpair-engine-manager` ever reports remote engine ports, the hardcoded `11434` / `1234` probes should be replaced by the reported values rather than extended with more entry-level hints.

## 5. Requirements

**Functional**

- Accept `node/add` with a required `address` and optional `name`, `tls_port`, `mtls`; reject an absent or empty `address`.
- Key each entry by `nodeID(entry)`: the caller's `name` when non-empty, else `"manual:" + address`.
- Respond to `node/add` before probing, with the entry's ports and TLS flags already populated.
- Probe every tracked entry every `probeInterval` (10 s) with a `probeTimeout` (3 s) per HTTP request and idle connection reuse disabled.
- Emit `node/discovered` once after an entry's initial probe, and `node/updated` on every subsequent probe whose watched fields changed.
- Preserve the last-learned `hostUuid` when a node-info probe fails and reports none.
- Emit exactly one `errors:report` per failure episode, on the probe where `consecutiveFails` first reaches `probeFailThreshold` (3), and one `errors:clear` on the first reachable probe after having been at or above the threshold.
- Emit `errors:clear` for a removed node's probe-failed id unconditionally on `node/remove`.
- Return `{"removed": false}` for `node/remove` of an unknown id, without emitting `node/removed`.
- Answer `nodes/list` with every tracked entry's latest status.
- Handle `log/set-level` as both a request and a notification, and `shutdown` as a request.

**Non-functional**

- Open no listening socket; all network activity is outbound probe traffic.
- Never block the read loop on a probe: the initial probe after `node/add` runs on its own goroutine.
- Guard all entry state with a single `sync.RWMutex`; hold no lock across an HTTP request.
- Serialize JSON-RPC writes so frames never interleave (`nvpair-shared/jsonrpc` `Codec` holds a write mutex).
- Require TLS 1.2 or better on every TLS probe.
- Exit cleanly on transport EOF, `SIGINT`/`SIGTERM`, or a `shutdown` request.

## 6. Inputs and Outputs

**Inputs** — newline-delimited JSON-RPC 2.0 frames on `stdin`, or on the `--ipc` Unix socket / Windows named pipe. Requests are `node/add`, `node/remove`, `nodes/list`, `shutdown`, and `log/set-level`; `log/set-level` is also accepted as a notification. Every other inbound notification is logged and dropped. HTTP responses from probed hosts are the second input class: an Ollama root and `/api/tags`, an LM Studio `/v1/models`, and an `nvpair-node-info` `/v1/node-info`.

**Outputs** — JSON-RPC responses and notifications on the same transport. Notifications are `ready`, `node/discovered`, `node/updated`, `node/removed`, `errors:report`, and `errors:clear`. Human-readable logs go to `stderr` (the supervising broker inherits the child's `stderr` into its own).

`ManualEntry` — the `node/add` params:

```json
{
  "address": "10.0.1.50",
  "name": "lab",
  "tls_port": 14319,
  "mtls": true
}
```

`ManualNodeStatus` — the `node/add` result and the params of `node/discovered` / `node/updated`:

```json
{
  "id": "lab",
  "name": "lab",
  "address": "10.0.1.50",
  "ollama_up": true,
  "ollama_port": 11434,
  "ollama_models": ["llama3.2:latest", "gemma3:4b"],
  "lmstudio_up": true,
  "lmstudio_port": 1234,
  "lmstudio_models": ["qwen2.5-7b-instruct"],
  "node_info_up": true,
  "node_info_port": 14318,
  "tls_enabled": false,
  "mtls_required": false,
  "gpus": [
    {
      "name": "NVIDIA GeForce RTX 3080",
      "vram_bytes": 10737418240,
      "vram_used_bytes": 2147483648,
      "utilization_percent": 42
    }
  ],
  "cpu": { "name": "Threadripper", "cores": 64, "utilization_percent": 7 },
  "memory": { "total_bytes": 137438953472, "used_bytes": 34359738368 },
  "telemetryValid": true,
  "msSince": 137,
  "hostUuid": "b1f0c2de-8a44-4d0e-9c1a-77e2f5b3a901"
}
```

`cpu` and `memory` are pointer-optional and absent entirely until a real response carries them, so a routing-only target that runs no `nvpair-node-info` does not falsely surface an unknown CPU and zero memory.

`telemetryValid` and `msSince` are always present. They copy node-info's GPU sample validity and the sample's age in milliseconds, preserving its distinction between an idle 0% reading and no reading at all, and they read `false` and `0` whenever node-info is down. Consumers ignore `msSince` while `telemetryValid` is false.

Example emitted frame (`node/discovered`, an unnamed entry whose LM Studio is absent):

```json
{"jsonrpc":"2.0","method":"node/discovered","params":{"id":"manual:10.0.1.50","address":"10.0.1.50","ollama_up":true,"ollama_port":11434,"ollama_models":["llama3.2:latest"],"lmstudio_up":false,"lmstudio_port":1234,"node_info_up":false,"node_info_port":14318,"telemetryValid":false,"msSince":0}}
```

## 7. API / Interface Contract

One interface: newline-delimited JSON-RPC 2.0 over `stdin`/`stdout`, or over the `--ipc` endpoint. Outbound, three probe clients speak plain HTTP or HTTPS to the entered host. There is no inbound network interface.

### 7.0 Methods and notifications

| Method | Params | Result |
|---|---|---|
| `node/add` | `ManualEntry` — `address` (required), `name`, `tls_port`, `mtls` | `ManualNodeStatus` (unprobed) |
| `node/remove` | `{ "id": string }` | `{ "removed": boolean }` |
| `nodes/list` | none | `{ "nodes": ManualNodeStatus[] }` |
| `shutdown` | none | `null` |
| `log/set-level` | `{ "level": "debug"\|"info"\|"warn"\|"error" }` | `{ "level": string }` |

Notifications emitted: `ready` with `{ "version": string }` (once, before the probe loop starts); `node/discovered` with a full `ManualNodeStatus` (once per entry, after its initial probe); `node/updated` with a full `ManualNodeStatus` (on a changed re-probe); `node/removed` with a `ManualNodeStatus` carrying only `id`, plus `address`, `ollama_up`, `ollama_port`, `lmstudio_up`, `lmstudio_port`, `node_info_up`, `node_info_port`, `telemetryValid`, and `msSince` at their zero values; `errors:report` with an `nvpair-shared/errors` `ServiceError`; and `errors:clear` with a `ClearParams` (`{ "id": string }`).

### 7.1 Address entry, identity, and idempotency

`nodeID(entry)` is the entry's identity: `name` when non-empty, otherwise `"manual:" + address`. Adds are upserts into `map[string]*trackedNode` — re-adding the same id replaces its `ManualEntry` and its probe baseline, so a second `node/add` under an existing name silently repoints that id at the new address. Two names for one address are two independent entries; each probes the host separately, and `nvpair-ui-broker` collapses them onto one discovery key only once `hostUuid` is learned. Removal is idempotent: the second `node/remove` of an id returns `{"removed": false}` and emits nothing.

### 7.2 Probe protocol and reachability

Each probe cycle performs up to four HTTP GETs against the entered address, each bounded by `probeTimeout` (3 s) on a transport with `DisableKeepAlives: true` so every cycle re-resolves the address:

- Ollama: `GET http://<address>:11434/` must return `200`; on success `GET http://<address>:11434/api/tags` supplies `ollama_models` from `models[].name`.
- LM Studio: `GET http://<address>:1234/v1/models` must return `200`; the same response supplies `lmstudio_models` from `data[].id`.
- node-info: `GET <scheme>://<address>:<port>/v1/node-info` must return `200` and decode into `NodeInfoResponse` (`GPUs`, `cpu`, `memory`, `telemetryValid`, `msSince`, `hostUuid`). A decode failure counts as down.

`node_info_port` is `14318` over `http` unless the entry set `tls_port > 0`, in which case it is `tls_port` over `https`. The two engine legs are always plain HTTP. `reachable` is `ollama_up || lmstudio_up || node_info_up`. A `node/updated` fires only when `ollama_up`, `lmstudio_up`, `node_info_up`, `hostUuid`, `ollama_models`, `lmstudio_models`, `gpus`, `cpu`, `memory`, `telemetryValid`, or `msSince` differs from the previous status; a stable probe logs at debug and emits nothing. In practice `msSince` makes every probe of a node with valid GPU telemetry a change (§4).

### 7.3 Probe transport: operator TLS and cluster mTLS

Three transports exist for the node-info leg, selected in this order:

1. **Plain HTTP** (`tls_port` absent or zero) — the default client, port `14318`.
2. **Operator-supplied TLS** (`tls_port > 0`) — the client built from `--client-cert` / `--client-key` / `--ca-bundle`, at TLS 1.2 minimum. `--ca-bundle` is appended to the system pool, so enterprise and public roots both work; `--client-cert` requires `--client-key` and vice versa, and a mismatch is a fatal startup error. With none of the three set, the client falls back to Go's default system trust store, so an HTTPS node with a publicly trusted certificate is probed with no flags at all.
3. **Cluster mTLS** (`tls_port > 0` and `--cluster-dir` resolves to a clustered mesh) — `clustertrust.Mesh.Refresh()` runs first so a cluster joined or a peer paired after startup is picked up without a restart, then `ClientTLSConfigAny()` yields a client presenting this node's leaf and accepting any currently-pinned peer certificate by exact DER match. This is the only way to reach a clustered peer's node-info, which is pin-gated and serves no plaintext listener. A manual node carries no discovery record to key a specific pin on, hence "any pinned cert".

A successful probe proves only that *something* at that address answered an expected HTTP shape. On the plain-HTTP and operator-TLS paths nothing authenticates the peer to PAIR, and `hostUuid` is self-reported by the remote. Only the cluster-mTLS path binds the answer to a certificate `nvpair-cluster-manager` already pinned. A manually entered address is therefore a user assertion of reachability, never proof of a trusted peer.

### 7.4 Failure reporting through the errors pipeline

`consecutiveFails` increments on each fully unreachable probe and resets to zero on any reachable one. On the probe where it first reaches `probeFailThreshold` (3 — about 30 s of unreachability), one `errors:report` is emitted with `id` = `manual-nodes:probe-failed:<node-id>`, `severity` `"warning"`, `action` `"none"`, and a message naming the node and the elapsed window; an IP-literal address appends the advice to re-add by current address or by hostname. Recovery emits `errors:clear` with the same id. `NodeID` and `Timestamp` are deliberately left empty and zero — `nvpair-ui-broker`'s `dispatchErrorsNotif` stamps both with the local node id and wall clock before forwarding to `nvpair-errors`. The report fires once per episode so `nvpair-errors`' ack-until-reemit contract is not defeated. The threshold only surfaces an error and never evicts, so it is deliberately shorter than the minute of silence the shared discovery layer waits before dropping an mDNS node.

### 7.5 Downstream projection

`nvpair-ui-broker` consumes the notifications in `forwardManualNodesNotification`. `node/discovered` and `node/updated` go through `manualToEnriched` into the same discovery store `nvpair-node-scanner` feeds, under source `sourceManual`: `Host` and `Addresses` come from `address`, `Port` from `node_info_port`, `Models` from the de-duplicated union of both engines' lists, and `ModelsByEngine` from `{"ollama": …, "lmstudio": …}`. The store key is `hostUuid` when reported, else the manual id, so manual and mDNS nodes share one `discovery:get-nodes` / `discovery:nodes-changed` snapshot. In parallel, `bridgeManualNode` upserts the node into each `nvpair-proxy` engine facade whose engine is reachable, with the engine-addressed `ollama:node/add-manual` or `lmstudio:node/add-manual`, and withdraws it from a facade whose engine is not with `<engine>:node/remove-manual`. The candidate is keyed identically, so `nvpair-job-scheduler` priority and `scheduledOn` resolve to it.

The broker also reduces each status to the highest `utilization_percent` across its GPUs plus `telemetryValid` and `msSince`, and caches that as the manual telemetry source for the same key. A scanner observation of the same host takes precedence. The broker ages the cached sample and relays it to `nvpair-job-scheduler`, which is how a manual node gets GPU pressure in scheduling.

`node/removed` drops the alias and either reprojects the key from a surviving alias or releases the store, telemetry, and proxy claims. `nvpair-node-scanner` participates in none of this — it has no manual concept.

### 7.6 Durable entry list and replay

This worker persists nothing. The durable list is a JSON array of `{ id, address, name }` at `configs/manual-nodes.json` under the PAIR desktop user-data directory (§9), written atomically (temp file plus rename). The desktop app reads it after broker `app:ready` and replays each entry as `node/add` with `{ address, name }`, and prunes an entry when the corresponding node is removed. Malformed files, non-array roots, and entries without an `address` are skipped rather than treated as an error.

### 7.7 Error codes

`-32602` for `node/add` params that are not a `ManualEntry` object, for a missing or empty `address`, for `node/remove` params that are not an object, and for an unparseable `log/set-level` level. `-32601` for any other method. Notification frames get no error response. `-32000 "manual-nodes not available"` is `nvpair-ui-broker`'s code when no manual-nodes worker is supervised; this worker never emits it.

### 7.8 Versioning

`Version` is stamped at build time via `-ldflags "-X main.Version=…"` from `services/versions.json` (`components["nvpair-manual-nodes"]`), printed by `--version`, and carried in the `ready` notification's `version`. Method names are additive: a new capability is a new method, never a changed one. `ManualEntry` and `ManualNodeStatus` only gain fields, so a consumer that predates one simply ignores it. Most added fields are `omitempty` (`tls_port`, `mtls`, `lmstudio_models`, `cpu`, `memory`, `hostUuid`); `lmstudio_up`, `lmstudio_port`, `telemetryValid`, and `msSince` are always present.

## 8. Dependencies

- **Upstream**: `nvpair-ui-broker` — the parent that spawns this worker (`--manual-nodes-path`), feeds it `node/add` / `node/remove` / `nodes/list` relayed from clients, and closes its `stdin` to stop it. The PAIR desktop app and `nvpair-tui` reach it only through that relay.
- **Downstream**: `nvpair-ui-broker`'s discovery store, per-engine proxy bridge, and telemetry cache; `nvpair-errors` (via the broker) for probe-failed reports; `nvpair-proxy`, whose engine facades receive bridged manual nodes; `nvpair-job-scheduler`, which ranks them alongside discovered nodes using the GPU telemetry the broker relays.
- **External**: each probed host's Ollama server (`:11434`), LM Studio OpenAI-compatible server (`:1234`), and `nvpair-node-info` endpoint (`:14318` plain, or the entry's `tls_port` over TLS); the cluster directory minted by `nvpair-cluster-manager` when `--cluster-dir` is passed; `github.com/Microsoft/go-winio` for the Windows named-pipe transport.

## 9. Data Ownership

- **Owned**: the in-memory `map[string]*trackedNode` — each entry's `ManualEntry`, its latest `ManualNodeStatus`, and its `consecutiveFails` counter. Nothing durable.
- **Source of truth**: authoritative for *current* manual-node reachability, per-engine model lists, and node-info telemetry while the process lives. Not the source of truth for the entry list — the desktop app's `manual-nodes.json` is, and the user is. Not the source of truth for `hostUuid`, which the remote's `nvpair-node-info` reports.
- **Storage**: none written by this binary. The durable list is `configs/manual-nodes.json` under the per-user PAIR data directory: `%LOCALAPPDATA%\Nvidia Corporation\Personal AI Router\configs\manual-nodes.json` on Windows, `$XDG_CONFIG_HOME/Nvidia Corporation/Personal AI Router/configs/manual-nodes.json` (falling back to `~/.config`) on Linux, and `~/Library/Application Support/Nvidia Corporation/Personal AI Router/configs/manual-nodes.json` on macOS. `services/shared/appdir` resolves that same `<base>/Nvidia Corporation/Personal AI Router` root for Go components, and the broker's default `--cluster-dir` is `cluster/` beneath it; this worker imports neither and only ever reads the cluster directory it is handed.

## 10. Design Constraints

- **Performance**: probe cost is bounded by `probeTimeout` (3 s) per request, and entries are probed sequentially within a cycle, so a cycle over N nodes can take up to roughly 4 × 3 s × N in the worst case — with the fixed 10 s ticker, unreachable nodes can push a cycle past its interval. Keep-alives are disabled by design; a fresh dial per probe is cheap at this cadence and guarantees re-resolution.
- **Scalability**: sized for a handful of hand-entered nodes on a small cluster, not for fleet-scale scanning. State is one map entry per node.
- **Reliability**: probes are best-effort with no retry inside a cycle; the next cycle is the retry. A three-cycle grace window absorbs transient failures before the user sees an error. Restarting the worker loses all entries; the broker evicts the orphans so no stale node lingers.
- **Security**: no listening socket, so no inbound attack surface. A manually entered address is a user assertion, not proof of a trusted peer, and a successful plain-HTTP or operator-TLS probe authenticates nothing. Cluster identity, pins, pairing, and membership belong to `nvpair-cluster-manager`; this worker only consumes the mesh read-only to build a client config. Certificate and key material is read from the paths given on the command line and never logged or emitted.
- **Compliance**: probe traffic touches only capability and telemetry endpoints. No prompt, chat message, request body, or inference response is read, forwarded, or logged.

## 11. Assumptions

- `nvpair-ui-broker` is the parent: it spawns this worker, relays client requests to it, and closing its `stdin` is the shutdown signal.
- The entered address is a bare host or IP, with no port and no scheme; the worker appends its own ports.
- Remote engines listen on their defaults (`11434`, `1234`), and remote `nvpair-node-info` on `14318` for plain HTTP.
- A clustered peer's node-info is pin-gated mTLS with no plaintext listener, so reaching it requires `tls_port` plus a populated cluster directory.
- `hostUuid` reported by a remote is stable for that host and unique in the fleet, which is what makes UUID-keyed deduplication with mDNS-discovered nodes correct.
- A client re-adds its manual nodes after this worker restarts.

## 12. Failure Modes and Mitigations

- **Worker restart or crash**: every entry is lost, since state is memory-only. → `nvpair-ui-broker`'s `clearManualNodesState` drops the worker handle, evicts every manual-origin node from the discovery store (leaving a co-located scanner record intact), drops its manual telemetry, and withdraws it from both engine facades, so nothing stale is routed to; the client re-adds. Losing entries on restart is a known limitation with a fix in progress (§4).
- **Host unreachable for three consecutive cycles**: the node reads down everywhere and inference cannot route to it. → One `errors:report` under `manual-nodes:probe-failed:<node-id>` surfaces it in the UI after roughly 30 s, with re-add guidance for IP-literal entries; a reachable probe emits `errors:clear`.
- **Entered address is a typo or a `host:port` string**: the probe URL is unreachable or malformed and the node reads permanently down. → The same probe-failed report fires; the address is not otherwise validated, so the operator has to correct the entry.
- **node-info drops while an engine stays up**: a blanked `hostUuid` would rekey the broker's discovery entry from UUID to manual id and back. → The last-learned `hostUuid` is carried forward whenever a failed node-info probe reports none.
- **Remote returns `200` with an unparseable model list**: a strict parse would report a live engine as down. → The engine is reported up with an empty model list; only the models are lost.
- **Remove races the initial probe**: a removed node could be resurrected by an in-flight probe's `node/discovered`. → Both `addNode`'s probe goroutine and `probeNode` re-check the map under lock after probing and drop the result when the id is gone.
- **TLS material misconfigured** (`--client-cert` without `--client-key`, unreadable bundle, no PEM in the bundle): probes would silently fall back to an unintended transport. → `tlsClientOptions.validate()` and `buildTLSClient` fail at startup with a fatal log rather than starting degraded.
- **Malformed inbound frame**: a bad params blob could panic a handler or corrupt state. → Every handler unmarshals into a typed struct and answers `-32602` on failure; unknown methods answer `-32601`; notifications are logged and dropped.
- **Parent pipe severed** (`stdin` EOF): an orphaned prober would keep probing with nowhere to report. → EOF returns from `readLoop`, which cancels the context, stops the probe loop, and exits zero. `SIGINT`/`SIGTERM` and a `shutdown` request take the same path.

## 13. Observability

- **Logging**: `applog.Init("nvpair-manual-nodes", …)` installs a level-gated `slog` handler on `stderr` and bridges the stdlib `log` package into it; the supervising broker inherits that `stderr`. Info covers startup transport choice, each add and remove, and every state change (`node_id`, `addr`, `ollama_up`, `node_info_up`, model count, GPU count). Debug covers each individual probe with address, port, scheme, status or error, and `duration_ms`, plus stable no-change probes. `--log-level` sets the initial level (default `$NVPAIR_LOG_LEVEL`, else info); `log/set-level` changes it at runtime. Certificate and key contents, and all probe response bodies, are never logged.
- **Metrics**: none. There is no metrics endpoint, no counters, and no histograms — this binary opens no listener of any kind. Probe latency exists only as the `duration_ms` field on debug log lines.
- **Alerts**: none in-process. The operator-visible signal is the `errors:report` under `manual-nodes:probe-failed:<node-id>`, which reaches the UI through `nvpair-ui-broker` and `nvpair-errors`.

## 14. Sample usage

`nvpair-ui-broker` spawns the worker as `nvpair-manual-nodes --log-level info --cluster-dir <dir>` over stdio pipes. The worker emits `ready` carrying the version stamped into the binary at build time, which the broker logs, then starts its 10-second probe ticker.

A user enters `10.0.1.50` and names it `lab`. The client's request reaches the broker, which relays it with `RelayRequest` (no timeout, because `node/add` triggers a probe) as `{"jsonrpc":"2.0","id":1,"method":"node/add","params":{"address":"10.0.1.50","name":"lab"}}`. The worker computes `nodeID` = `"lab"`, stores the entry, and answers immediately with an unprobed `ManualNodeStatus` — `ollama_port` `11434`, `lmstudio_port` `1234`, `node_info_port` `14318`, every `*_up` false — which the broker relays back so the UI can render the row at once.

On its goroutine the worker probes: `GET http://10.0.1.50:11434/` returns `200`, so `GET /api/tags` yields `["llama3.2:latest"]`; `GET http://10.0.1.50:1234/v1/models` fails to connect, so LM Studio is down; `GET http://10.0.1.50:14318/v1/node-info` returns one GPU, a CPU object, a memory object, a valid GPU sample with its age, and `hostUuid`. It emits `node/discovered` with the filled `ManualNodeStatus`. The broker's `forwardManualNodesNotification` upserts it through `manualToEnriched` into the discovery store keyed by the reported `hostUuid`, caches its GPU telemetry for the scheduler under the same key, and `bridgeManualNode` sends `nvpair-proxy` `ollama:node/add-manual` under that key and `lmstudio:node/remove-manual`. The node now appears in `discovery:nodes-changed` beside mDNS nodes and is a routable, schedulable Ollama target.

The host is then powered off. The first cycle that finds nothing emits `node/updated` with every `*_up` false, which withdraws the proxy candidate. After three consecutive such cycles the worker emits one `errors:report` with `id` `manual-nodes:probe-failed:lab` and `severity` `"warning"`; the broker stamps the local node id and timestamp and forwards it to `nvpair-errors`. When the host returns, the next probe emits `node/updated` and `errors:clear` for the same id. If the user instead removes the node, `node/remove` answers `{"removed": true}`, emits `node/removed` with `{"id":"lab"}` and an `errors:clear`, and the broker releases the store and proxy claims.

## 15. Process model, CLI, and build wiring

`main` parses these flags:

| Flag | Default | Effect |
|---|---|---|
| `--ipc <path>` | `""` (stdio) | Dial a Unix domain socket or Windows named pipe via `nvpair-shared/ipc` instead of using `stdin`/`stdout`. A dial failure is fatal. |
| `--client-cert <path>` | `""` | PEM client certificate presented on every TLS probe. Requires `--client-key`. |
| `--client-key <path>` | `""` | PEM private key matching `--client-cert`. Requires `--client-cert`. |
| `--ca-bundle <path>` | `""` | PEM CA bundle for verifying probe server certificates, appended to the system trust store. |
| `--cluster-dir <path>` | `""` | Cluster config dir (`node.crt` / `node.key` plus `trusted/`). When it resolves to a clustered mesh, a `tls_port` probe uses cluster mTLS. |
| `--log-level <level>` | `$NVPAIR_LOG_LEVEL`, else `info` | Initial `slog` level (`debug`\|`info`\|`warn`\|`error`); registered by `applog.RegisterFlag`. |
| `--version` | off | Print `Version` and exit `0`. |

Startup order: parse flags, print version and exit if asked, `applog.Init`, `clustertrust.Open(--cluster-dir)` (a live view, so a cluster joined later is picked up without a restart), validate the TLS option triple (fatal on a half-configured pair), open the transport, install the `SIGINT`/`SIGTERM` handler, construct the `Manager`, emit `ready`, start the probe loop, and enter the read loop.

The process is always a child. `nvpair-ui-broker` spawns the binary named by its `--manual-nodes-path` flag, which the desktop application always passes, and otherwise looks for `nvpair-manual-nodes` in its own working directory. It spawns it through `startRPCWorker` with `--log-level <current>` plus `--cluster-dir <dir>` when the broker resolved one, wires `stdin`/`stdout` pipes, and inherits the child's `stderr`. It is registered as an optional worker under a restart policy, so a crash respawns it and triggers `clearManualNodesState`. The broker's `Stop` closes `stdin`; the child observes EOF, returns from `readLoop`, cancels its context to stop the probe loop, closes the transport, and exits `0`. A `shutdown` request and a caught signal follow the same cancellation path. Electron never launches this worker directly — only the broker does.

Build and staging: `services/build.sh` (`build_subbinary 4 nvpair-manual-nodes`) and `services/build.bat` (`[4/12]`) read the version from `services/versions.json` with `jq` and run `go build -ldflags "-X main.Version=$V_MNODES"`, then copy the binary into `services/build/bin/`. `services/installer_build.sh` stages it into the tarball's `bin/`, and `services/installer/nvpair-setup.nsi` installs, firewall-registers, taskkills, and deletes `nvpair-manual-nodes.exe`. For the desktop app, `npm run build:modular-binaries` produces `desktop/cli-bin/`, where `MODULAR_RUNTIME_BINARIES` declares it `processName: 'manual-nodes'`, `launchOwner: 'broker'`, `optional: true`, `args: []`, and `needsFirewallAccess: false` — it is client-only and needs no firewall rule.
