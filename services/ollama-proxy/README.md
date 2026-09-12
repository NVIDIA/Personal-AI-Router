<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Ollama Proxy

A discovery-aware HTTP reverse proxy for Ollama nodes on the local network. It runs no mDNS browse of its own: its routing targets come from the broker's discovery relay (it sends `discovery:subscribe {services:[ol]}` and replaces its routing overlay from each pushed `discovery:nodes` snapshot) plus user-added manual nodes. It forwards HTTP requests to the selected node, aggregates the model-list routes across candidate nodes, and exposes a bidirectional JSON-RPC 2.0 control channel over stdio (or an IPC socket).

## Build

```bash
go build -o ollama-proxy .
```

## Usage

```
ollama-proxy [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `11435` | HTTP listen port for request forwarding |
| `--alias-address` | *(empty)* | Optional secondary `host:port` for the same routing handler. Repeat the flag to reserve both loopback families for `localhost`. Only literal loopback addresses are accepted; the broker uses this for a safe inherited local `OLLAMA_HOST`, and the aliases are not advertised to peers. |
| `--ignore-persisted-port` | `false` | Use `--port` even when `proxy-port.json` contains a saved port (used by broker-managed startup) |
| `--ipc` | *(empty — use stdio)* | Path to a Unix domain socket or Windows named pipe for IPC |
| `--cluster-dir` | *(empty)* | Cluster trust directory (`node.crt`/`node.key` plus trusted pins). Enables the LAN mTLS inference ingress while this node is a cluster member; empty means no ingress and no peer candidates. |
| `--log-level` | *(`$NVPAIR_LOG_LEVEL`, else `info`)* | Initial log level: `debug`, `info`, `warn`, or `error`. Changeable at runtime with `log/set-level`. |
| `--version` | | Print version and exit |

### HTTP Reverse Proxy

The proxy listens on `--port` (default 11435) and forwards incoming HTTP requests to the currently active Ollama node — except the model-list routes `GET /api/tags` and `GET /v1/models`, which are queried across every candidate node concurrently and merged into one de-duplicated inventory. Point your Ollama client at `http://localhost:11435` and the proxy handles routing. When the broker supplies `--alias-address`, the proxy reserves that loopback-only endpoint before reporting ready and serves it through the same routing and workload-lifecycle handler. A `localhost` alias reserves `127.0.0.1` and `::1` atomically so client resolution cannot bypass the router. An occupied alias is non-fatal: its existing owner is untouched and the proxy reports an actionable warning while the primary listener stays available.

**Cluster ingress.** The listener carries two personalities, demultiplexed by each connection's first byte. Plaintext HTTP is accepted only from loopback; a LAN caller is refused. When `--cluster-dir` shows this node is a cluster member, the same listener also terminates cluster mTLS: a peer whose client certificate matches one of this node's pins is forwarded straight to the local engine reported by `node/set-local-backend`, and is never re-routed onward to another node. Membership and pins are re-derived per request, so joining or leaving a cluster needs no restart.

**Persisted port.** A port chosen at runtime via the `set-port` request (see below) is saved as `proxy-port.json` in the per-user data dir (`%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `~/.config/Nvidia Corporation/Personal AI Router` on Linux) and **restored on startup**, taking precedence over `--port`/the default — so the proxy comes back up where it was last put. `--ignore-persisted-port` deliberately bypasses that restoration for a broker-coordinated start. Delete the file (or `set-port` back to the default) to revert.

**Browser clients (CORS).** The policy is deny-by-default. A browser page on any origin can reach the proxy's loopback listener, so with an allow-everything policy any website could drive the local engine and read its responses — using the models as a free oracle. Browser callers are therefore admitted only from exact origins the operator lists in `NVPAIR_PROXY_ALLOWED_ORIGINS` (comma-separated, scheme+host[:port], compared exactly); with no entry configured, every browser origin is refused. Non-browser callers (the Electron main process, CLI tools, health probes) send no `Origin` header and are unaffected. A cross-origin POST whose origin is not allowlisted receives `403` `origin-not-allowed` before it can drive an engine — a simple cross-origin POST needs no preflight, so preflight handling alone cannot provide this gate.

The proxy answers an allowlisted origin's `OPTIONS` preflight locally with `204` and a static method/header grant (`GET, POST, PUT, DELETE, OPTIONS` / `Content-Type, Authorization`); arbitrary request headers are deliberately not echoed, so a preflight naming a header outside the static grant simply fails. Responses the proxy authors itself — including rejections such as the `502` when no node is available and the `403` refusing a non-loopback plaintext caller — carry CORS headers so an allowlisted browser can read the status and reason; without them every failure surfaces as a generic "CORS error". The policy is shared with [`lmstudio-proxy`](../lmstudio-proxy/README.md) through `nvpair-shared/cors`, so both proxies answer identically.

A response *forwarded from an engine* is different: if the engine set its own `Access-Control-Allow-Origin` (Ollama with `OLLAMA_ORIGINS`), that header is passed through untouched rather than replaced with the proxy's grant, so a deliberately narrow engine policy is never silently widened and a credentialed response is not broken. On a preflight, an engine that declares an `Access-Control-Allow-Origin` keeps its exact origin and `Access-Control-Allow-Credentials` unchanged; an engine that declares no CORS policy gets the proxy's deny-by-default fallback instead.

One limit is outside the proxy's control: current Chromium-based browsers gate a request from a public origin to a local or loopback address behind the user's [Local Network Access](https://chromestatus.com/feature/5152728072060928) permission, which replaced the old server-side opt-in header. No header the proxy sends can grant that. A hosted page needs the permission plus a `fetch(url, { targetAddressSpace: 'loopback' })` annotation; a page served from the local machine is unaffected.

Node selection:
- **Eligibility**: Before routing model-bearing inference, the proxy keeps only nodes whose current Ollama inventory advertises the requested model. Ollama's implicit `:latest` tag is normalized. An empty or non-matching inventory is excluded until a later discovery update; if no advertised owner is routable, the proxy returns a local `502`.
- **Auto**: When no eligible node is explicitly selected, the proxy follows `node/set-priority` (see below), then discovered nodes in stable ID order.
- **Priority (scheduler-driven)**: The Job Scheduler ranks the cluster least-loaded-first by pending workload plus smoothed GPU pressure and, via `nvpair-ui-broker`, pushes the ordered node list with those per-node counts to this proxy with `node/set-priority`. Auto routing sends the request to the listed node carrying the least estimated load. See [`nvpair-job-scheduler`](../nvpair-job-scheduler/README.md).
- **Manual**: Use the `node/select` JSON-RPC method to pin traffic to a specific node. A manual pin **overrides the priority list only when that node is eligible** for the requested model.
- **Failover**: If the selected node disappears from the discovery set, the proxy falls back to auto-select and emits a `node/selection-changed` notification. A transport error or retryable status, including a model `404` from an advertised owner with stale inventory, steps to the next eligible owner.

### IPC Transport

By default the proxy communicates over **stdin/stdout** using newline-delimited JSON-RPC 2.0 (one message per line). All diagnostic logging goes to **stderr**.

For environments where stdout may conflict with the host process (e.g. Electron), pass `--ipc` to redirect the JSON-RPC channel to a named socket or pipe. The parent process should create and listen on the endpoint before spawning the proxy.

```bash
# Default: stdin/stdout
ollama-proxy

# Unix domain socket
ollama-proxy --ipc /tmp/ollama-proxy.sock

# Windows named pipe
ollama-proxy --ipc \\.\pipe\ollama-proxy
```

## JSON-RPC 2.0 Protocol

All messages conform to the [JSON-RPC 2.0 specification](https://www.jsonrpc.org/specification). Messages are newline-delimited (one JSON object per `\n`).

### Node Object

Nodes are represented throughout the protocol with this shape:

```json
{
  "id": "22222222-2222-2222-2222-222222222222",
  "host": "my-workstation",
  "port": 11434,
  "addresses": ["192.168.1.50"],
  "txt": ["uuid=22222222-2222-2222-2222-222222222222", "ol=11434"],
  "models": ["llama3.2:latest", "gemma3:4b"],
  "ip": "192.168.1.50"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Stable per-host UUID from the discovery record (the ID you supply, for a manual node) |
| `host` | string | Hostname, for display — routing never keys on it |
| `port` | int | Ollama port from the discovery record's `ol` service entry |
| `addresses` | string[] | Addresses to dial. A node fed by the discovery relay always carries exactly one canonical address; several only ever appear on a manual node |
| `txt` | string[] | The discovery record's TXT pairs, carried verbatim |
| `models` | string[] | The node's Ollama model inventory from the discovery snapshot. Model-bearing inference is eligible only when this list advertises the requested model. An omitted or empty list excludes the node from that request until inventory updates; it remains available for non-inference routes and model-list aggregation |
| `ip` | string | The single canonical LAN address to dial or display, resolved from the node's `ip=` TXT if present and otherwise the best-scored advertised IPv4. Stamped onto outbound `node/*` notifications so consumers agree with the address the proxy routes to |

---

### Notifications (proxy → client)

Notifications have no `id` field and do not expect a response.

#### `ready`

Sent after startup, before discovery begins, and again after every successful `set-port` rebind — `port` carries the port now bound.

```json
{"jsonrpc":"2.0","method":"ready","params":{"version":"0.1.0","port":11435}}
```

| Param | Type | Description |
|-------|------|-------------|
| `version` | string | Proxy version |
| `port` | int | HTTP listen port |

#### `error`

Sent when a fatal startup condition stops the proxy from serving — currently only a failed bind — immediately before the process exits non-zero.

```json
{"jsonrpc":"2.0","method":"error","params":{"code":"bind-failed","message":"failed to bind port 11435: ...","port":11435}}
```

#### `node/discovered`

A new Ollama node appeared on the network.

```json
{"jsonrpc":"2.0","method":"node/discovered","params":{"id":"22222222-2222-2222-2222-222222222222","host":"my-workstation","port":11434,"addresses":["192.168.1.50"]}}
```

#### `node/updated`

A previously discovered node changed its host, port, or addresses.

```json
{"jsonrpc":"2.0","method":"node/updated","params":{"id":"22222222-2222-2222-2222-222222222222","host":"my-workstation","port":11434,"addresses":["192.168.1.51"]}}
```

#### `node/removed`

A node is no longer present in the discovery set (it left the relay's `ol` nodes, or a manual node was removed).

```json
{"jsonrpc":"2.0","method":"node/removed","params":{"id":"22222222-2222-2222-2222-222222222222","host":"my-workstation","port":11434,"addresses":["192.168.1.50"]}}
```

#### `node/selection-changed`

The active node selection changed (either explicitly via `node/select` or because the selected node was removed).

```json
{"jsonrpc":"2.0","method":"node/selection-changed","params":{"id":"22222222-2222-2222-2222-222222222222"}}
```

An empty `id` means the proxy has reverted to auto-select mode.

#### `proxy/request-started`

A request has been committed to a target and its response body is about to stream. It pairs by `id` with the matching `proxy/request`, so a consumer can keep an in-flight count per node. Model-list aggregation has no single target, so it reports `"target":"cluster"` with no `node_id`; a request rejected before forwarding was never in flight and gets no started event.

```json
{"jsonrpc":"2.0","method":"proxy/request-started","params":{"id":"17","node_id":"22222222-2222-2222-2222-222222222222","method":"POST","path":"/api/chat","target":"192.168.1.50:11434"}}
```

#### `proxy/request`

A proxied request finished, or was rejected before forwarding. `duration_ms` covers the whole request; `ttfb_ms` is the time to the upstream's status line and is omitted where no response header arrived (rejection and transport-error paths). `error` carries normalized error text. This is operational metadata only — request and response bodies are never reported.

```json
{"jsonrpc":"2.0","method":"proxy/request","params":{"id":"17","node_id":"22222222-2222-2222-2222-222222222222","method":"POST","path":"/api/chat","target":"192.168.1.50:11434","status":200,"duration_ms":6120,"ttfb_ms":95}}
```

#### `workload:started` / `workload:completed` / `workload:errored`

One lifecycle transition per forwarded inference request, carrying a single `workloadInfo`. `engine` is always `ollama`; `originatedFrom` is left empty for the broker to stamp, and `scheduledOn` names the node that actually served (re-pointed if failover moved the request). The broker relays these to `nvpair-workload-manager`. The proxy never emits `workload:submitted` — it forwards immediately rather than queueing.

```json
{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"17","model":"llama3:latest","engine":"ollama","runId":"3ce8a1740b62df95","state":"running","originatedFrom":"","scheduledOn":"22222222-2222-2222-2222-222222222222","createdAt":1716998400000,"startedAt":1716998400000,"completedAt":null,"error":null,"requesterId":null}}}
```

#### `node/activity`

Raised while a node's engine is streaming a response back through the proxy: every successful write of upstream body bytes reports the node that produced them. The broker relays it to `nvpair-node-scanner`, which treats it as proof of life and cancels that node's eviction — a node saturated by inference cannot answer a liveness probe, but it is demonstrably alive precisely because it is streaming. Coalesced to one report per node per 2s (`nvpair-shared/nodeactivity`), since a generation writes hundreds of chunks and the scanner treats one report as good for a minute. `msSince` is the age of the observation; the broker adds its own relay delay before passing it on.

Only bytes that came from the upstream count. The proxy's own error bodies travel through the same writer and are never reported, because they say nothing about the node.

```json
{"jsonrpc":"2.0","method":"node/activity","params":{"hostUuid":"22222222-2222-2222-2222-222222222222","msSince":0}}
```

#### `errors:report` / `errors:clear`

Entries for the `nvpair-errors` pipeline, keyed by a stable `id` so a report and its clear cannot drift. A node dropping out of the discovery set raises `ollama-proxy:upstream-unreachable:<nodeId>`; its reappearance clears the same id. An `--alias-address` the proxy could not claim raises `ollama-proxy:ollama-host-alias-blocked`. `nodeId` and `timestamp` are left unset for the broker to stamp.

```json
{"jsonrpc":"2.0","method":"errors:report","params":{"id":"ollama-proxy:upstream-unreachable:22222222-2222-2222-2222-222222222222","message":"Upstream node \"my-workstation\" is no longer reachable (dropped from discovery)","severity":"warning","action":"none"}}
```

---

### Requests (client → proxy)

Requests carry an `id` and receive a response.

#### `nodes/list`

Returns all currently discovered nodes.

**Request:**
```json
{"jsonrpc":"2.0","id":1,"method":"nodes/list"}
```

**Response:**
```json
{"jsonrpc":"2.0","id":1,"result":{"nodes":[{"id":"22222222-2222-2222-2222-222222222222","host":"my-workstation","port":11434,"addresses":["192.168.1.50"]}]}}
```

#### `node/select`

Pin the proxy to route HTTP traffic to a specific node. Pass an empty `id` to return to auto-select.

**Request:**
```json
{"jsonrpc":"2.0","id":2,"method":"node/select","params":{"id":"22222222-2222-2222-2222-222222222222"}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":2,"result":{"id":"22222222-2222-2222-2222-222222222222"}}
```

**Error** (node not found):
```json
{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"node \"xyz\" not found"}}
```

#### `node/selected`

Query the currently selected node.

**Request:**
```json
{"jsonrpc":"2.0","id":3,"method":"node/selected"}
```

**Response:**
```json
{"jsonrpc":"2.0","id":3,"result":{"id":"22222222-2222-2222-2222-222222222222"}}
```

An empty `id` means auto-select is active.

#### `node/set-priority`

Set the **auto-routing priority order** — an ordered list of node IDs, highest
priority first, optionally with each node's pending-work count and GPU pressure
in `ranks`. Delivered by `nvpair-ui-broker` on behalf of `nvpair-job-scheduler`,
which ranks the cluster least-loaded-first by total pending workload across
engines plus smoothed GPU pressure (see
[`nvpair-job-scheduler`](../nvpair-job-scheduler/README.md)). The snapshot is
stored verbatim and applied at request time. A `nodes`-only payload is valid and
supplies zero pending and GPU-pressure baselines.

**Request:**
```json
{"jsonrpc":"2.0","id":8,"method":"node/set-priority","params":{"nodes":["MY-PC","LAB-DESK-B","GPU-RIG"],"ranks":[{"id":"MY-PC","pending":0,"gpuPressure":0,"rank":0},{"id":"LAB-DESK-B","pending":1,"gpuPressure":1,"rank":1},{"id":"GPU-RIG","pending":3,"gpuPressure":3,"rank":2}]}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":8,"result":{"count":3}}
```

| Field | Type | Description |
|-------|------|-------------|
| `count` | int | Number of node IDs stored (the length of the accepted list) |

Semantics:
- **Capability gate.** A model-bearing inference request first intersects the
  discovery snapshot with nodes advertising that Ollama model. Selection,
  priority, reservations, and failover operate only on that request-local owner
  set. If it is empty, the proxy returns `502` without contacting an engine.
- **Auto ordering.** Within the eligible owner set, the proxy picks the listed
  node carrying the least estimated load —
  `pending + gpuPressure` from the last snapshot plus the proxy's own
  reservations for requests it has already dispatched but whose workload feedback
  has not come back yet — breaking ties by position in `nodes`. It increments the
  chosen node's reservation before forwarding, so a concurrent burst spreads
  instead of repeatedly choosing from the same stale snapshot. That node moves to
  the front of this request's failover list and the rest keeps its order, so a
  transport error or retryable status (the existing failover trigger) steps to
  the next candidate.
- **Snapshot reset.** Each new snapshot replaces the pending and GPU-pressure
  baselines and clears the reservations. GPU pressure is clamped to the
  scheduler's 0–3 range.
- **Eligible manual pin wins.** An active `node/select` pin takes precedence when
  it is in the request's owner set. An ineligible pin is ignored for that request,
  so automatic reservations still apply among eligible owners. Clearing the pin
  (`node/select` with an empty `id`) activates the most-recently-set list. Setting
  a priority list does **not** emit `node/selection-changed` (the manual selection
  is unchanged).
- **Unknown IDs are ignored.** IDs not currently in discovery are kept in the
  stored list (a node may appear later) but contribute nothing until discovered.
- **Eligible unlisted nodes are a lowest-priority fallback.** An advertised owner
  absent from the list stays routable, but only after every listed owner —
  ordered among themselves by the default stable ID sort. This ensures an
  eligible manually-added node the scheduler never saw is never stranded.
- **Empty list reverts to default.** `{"nodes":[]}` clears the scheduler's
  influence and returns the proxy to its default auto ordering (eligible
  discovered nodes by stable ID).

The list persists only in memory for the proxy's lifetime; it is not saved across
restarts. On restart the proxy comes back with an empty list, and the broker
re-pushes the last order once the proxy re-announces `ready`.

> **`lmstudio-proxy` parity.** [`lmstudio-proxy`](../lmstudio-proxy/README.md) is a
> deliberate clone of this proxy (identical routing/failover/selection). It
> implements `node/set-priority` with the exact same contract; the only differences
> are engine-specific — it subscribes to the discovery relay for `lm` nodes and
> receives the `lmstudio` engine's list from the broker.

#### `set-port`

Change the HTTP listen port at runtime and persist the choice. The proxy
binds the new port first (so a bind failure leaves the current listener
serving), starts serving on it, then closes the old listener — in-flight
connections on the old port drain naturally. The new port is saved to
`proxy-port.json` and a fresh `ready` notification announces it.

**Request:**
```json
{"jsonrpc":"2.0","id":7,"method":"set-port","params":{"port":11500}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":7,"result":{"version":"0.9.0","port":11500}}
```

**Error** (port in use / out of range):
```json
{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"failed to bind port 11500: ..."}}
```

When supervised by `nvpair-ui-broker`, callers reach this as `proxy:set-port`,
and the broker first steers the port clear of any running engine's port
(engines take precedence) before handing it down — see the broker README.

#### `node/add-manual`

Add a node manually (for networks where mDNS is blocked). If the node ID already exists as a manual node, it is updated.

**Request:**
```json
{"jsonrpc":"2.0","id":5,"method":"node/add-manual","params":{"id":"remote-server","host":"remote-server","port":11434,"addresses":["10.0.1.50"]}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":5,"result":{"added":true}}
```

The proxy emits a `node/discovered` notification (or `node/updated` if the node was already registered). Manual nodes are a separate overlay that discovery snapshots never touch — they persist until explicitly removed.

#### `node/remove-manual`

Remove a previously added manual node.

**Request:**
```json
{"jsonrpc":"2.0","id":6,"method":"node/remove-manual","params":{"id":"remote-server"}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":6,"result":{"removed":true}}
```

The proxy emits a `node/removed` notification and clears the active selection if it pointed to this node.

#### `node/set-local-backend`

Tell the proxy which loopback engine this node's own traffic terminates on. The broker sends it once the local Ollama address and health are known. It is the target the cluster mTLS ingress forwards to, and the substitute used when discovery advertises this node's own proxy endpoint as a candidate. A zero `port` or `"healthy":false` effectively clears it, and the ingress then answers `503`.

**Request:**
```json
{"jsonrpc":"2.0","id":9,"method":"node/set-local-backend","params":{"engine":"ollama","host":"127.0.0.1","port":11434,"healthy":true}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":9,"result":{"ok":true}}
```

#### `log/set-level`

Change the active log level at runtime (`debug`, `info`, `warn`, `error`). Accepted as a request or a notification; as a request it responds with the resolved level and rejects an unknown one with `-32602`.

**Request:**
```json
{"jsonrpc":"2.0","id":10,"method":"log/set-level","params":{"level":"debug"}}
```

**Response:**
```json
{"jsonrpc":"2.0","id":10,"result":{"level":"debug"}}
```

#### `shutdown`

Gracefully shuts down the proxy.

**Request:**
```json
{"jsonrpc":"2.0","id":4,"method":"shutdown"}
```

**Response:**
```json
{"jsonrpc":"2.0","id":4,"result":null}
```

---

### Error Codes

Standard JSON-RPC 2.0 error codes apply:

| Code | Meaning |
|------|---------|
| `-32601` | Method not found |
| `-32602` | Invalid params |
| `-32000` | The request was well-formed but could not be carried out — returned by `set-port` when the new port cannot be bound |

---

## Shutdown

The proxy shuts down gracefully on any of:

1. **stdin EOF** — parent closes stdin (stdio mode only)
2. **`shutdown` JSON-RPC request** — programmatic shutdown
3. **SIGINT / SIGTERM** — standard OS signals

## Discovery

The proxy does not browse mDNS. On startup it subscribes to the broker's discovery relay for `ol` (Ollama) nodes (`discovery:subscribe {services:[ol]}`). Targets then arrive as `discovery:nodes` notifications carrying the relay's full filtered node set, and each snapshot replaces the routing overlay wholesale — a departed node is simply absent from the next one — while the diff against the previous overlay is what produces the `node/discovered`, `node/updated`, and `node/removed` notifications. User-added manual nodes are merged on top. Nodes are keyed by the discovery record's stable per-host UUID, so routing survives a machine being renamed. The single `_nvpair-node` browse that feeds the relay lives in the `nvpair-node-scanner` daemon (see its README) — this proxy is a pure consumer of the resulting routing set.
