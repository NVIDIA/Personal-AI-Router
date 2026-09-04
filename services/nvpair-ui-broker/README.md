<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-ui-broker

A Go service that exposes the NVIDIA Personal AI Router API to **UI processes**
and other clients (CLIs, dashboards, mobile companions, scripts, tests, etc.).
The shipped graphical UI is bundled alongside the backend services and launches
the broker from the same installation directory. The broker is the parent
process and canonical backend entry point: it supervises the worker subprocesses
on the UI's behalf, speaking JSON-RPC over stdio.

The broker supervises **twelve** worker subprocesses, so a client gets the whole
backend behind one endpoint. Each is spawned at startup and relayed under its own
namespace:

| Worker | Role | Client namespace |
| --- | --- | --- |
| `nvpair-node-scanner` | Discovery daemon: advertises this host's one `_nvpair-node._tcp` record and browses the LAN | `discovery:*` |
| `nvpair-node-info` | Local GPU / CPU / memory inventory over HTTP at `/v1/node-info` | — (HTTP only) |
| `ollama-proxy` | Ollama-compatible inference proxy and router | `proxy:*` |
| `lmstudio-proxy` | The LM Studio counterpart, supervised identically | `lmstudio-proxy:*` |
| `llamacpp-proxy` | OpenAI-compatible router for adopted local llama-server instances | `llamacpp-proxy:*` |
| `nvpair-engine-manager` | Local engine and model control plane; also serves `GET /v1/models` to peers | `engine:*` |
| `nvpair-cluster-manager` | Node identity, trusted-node store, PIN pairing | `cluster:*`, `nodes:*` |
| `nvpair-workload-manager` | Cluster workload relay between this node and peers | `workloads:*` |
| `nvpair-job-scheduler` | Ranks nodes by pending work and GPU pressure for the proxies | _internal_ |
| `nvpair-errors` | Service-error datastore and cross-node sync | `errors:*` |
| `nvpair-node-settings` | Typed per-node settings store | `settings/*` |
| `nvpair-manual-nodes` | User-added nodes, merged into the discovery snapshot | `node/*`, `nodes/list` |

Only the scanner is required. Every other worker is optional: a missing binary
leaves the broker running without that capability rather than failing to start.
The [worker subprocesses](#worker-subprocesses) section covers each one's link,
lifecycle, and relay rules.

Two responsibilities live in the broker itself rather than in a worker:

- **Engine advertising.** The broker polls local Ollama, LM Studio, and
  llama.cpp backends every 5 s. When a backend and its proxy are healthy, it
  registers `ol`, `lm`, or `ll` at the promoted proxy port and privately sends
  the loopback backend to that proxy through `node/set-local-backend`. Model
  lists remain on `nvpair-engine-manager`'s `em` service.
- **Workload brokering.** The broker stamps the local node id onto the
  `workload:*` events a proxy emits, applies them to its authoritative store,
  feeds the scheduler, and forwards them to the workload manager for broadcast.
  Peer-origin updates come back the other way as `workloads:upsert` /
  `workloads:remove` for subscribed clients.
- **Scheduling telemetry.** Scanner and manual-node observations populate a
  source-aware cache keyed by `hostUuid`; scanner data takes precedence. The
  broker advances sample age locally, feeds compact maximum-GPU telemetry to the
  scheduler, and replays it after a scheduler restart.

### Supervision & recovery

Every worker runs under a supervisor that auto-restarts it on an unexpected exit: exponential backoff (~1 s → 16 s), a budget of 5 attempts per unhealthy streak, and a 60 s "healthy reset" window after which a restarted worker is considered recovered. A crash is surfaced as a sticky `supervisor:subprocess-crashed:<name>` entry in the error registry (so connected tools can show "this helper is down"); recovery clears it, and a worker that exhausts its restart budget leaves the entry up. The scanner is the one worker whose **initial** spawn is fatal (the broker has no job without discovery); every other worker is optional and a failed first spawn just runs the broker without it. `nvpair-errors` is a special case: it can't report its own death into itself, so its crash logs to stderr (rather than the pipeline) while still being auto-restarted.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0 — same conventions as every other NVPAIR subprocess. By default the broker reads stdin and writes stdout; with `--ipc <path>` it dials a Unix domain socket or Windows named pipe instead. The parent UI process owns the endpoint in both cases: it either spawns the broker with piped stdio, or sets up the pipe / socket beforehand and points the broker at it via `--ipc`. The broker always talks to exactly one peer.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint to dial: Unix socket path or Windows named pipe (e.g. `\\.\pipe\nvpair-ui-broker`) |
| `--scanner-path <path>` | `./nvpair-node-scanner[.exe]` in the CWD | Explicit path to the `nvpair-node-scanner` binary the broker should spawn |
| `--node-info-path <path>` | `./nvpair-node-info[.exe]` in the CWD | Explicit path to the `nvpair-node-info` binary the broker should spawn. When omitted and no default sibling exists, the broker runs without the local inventory server (non-fatal); when set to an invalid path, the broker exits with an error |
| `--proxy-path <path>` | `./ollama-proxy[.exe]` in the CWD | Explicit path to the `ollama-proxy` binary the broker spawns for the local Ollama reverse proxy. Same optional semantics as `--node-info-path`: an absent default sibling means no local proxy (non-fatal); an invalid explicit path exits with an error |
| `--lmstudio-proxy-path <path>` | `./lmstudio-proxy[.exe]` in the CWD | Explicit path to the `lmstudio-proxy` binary the broker spawns for the local LM Studio reverse proxy. Same optional semantics as `--proxy-path` |
| `--llamacpp-proxy-path <path>` | `./llamacpp-proxy[.exe]` in the CWD | Explicit path to the `llamacpp-proxy` binary the broker spawns for an adopted local llama-server. The broker uses port 8081 for the proxy and reconciles its loopback backend through `node/set-local-backend`. Same optional semantics as `--proxy-path` |
| `--workload-manager-path <path>` | `./nvpair-workload-manager[.exe]` in the CWD | Explicit path to the `nvpair-workload-manager` binary the broker spawns for the cluster workload relay. Same optional semantics as `--node-info-path`: an absent default sibling means no workload relay (non-fatal); an invalid explicit path exits with an error |
| `--errors-path <path>` | `./nvpair-errors[.exe]` in the CWD | Explicit path to the `nvpair-errors` binary the broker spawns (with `--peer-sync`) for the service-error pipeline. Same optional semantics as `--node-info-path`: an absent default sibling means the error pipeline is disabled — producers' errors are dropped (non-fatal); an invalid explicit path exits with an error |
| `--engine-manager-path <path>` | `./nvpair-engine-manager[.exe]` in the CWD | Explicit path to the `nvpair-engine-manager` binary the broker spawns for engine management. Same optional semantics as `--node-info-path` |
| `--manual-nodes-path <path>` | `./nvpair-manual-nodes[.exe]` in the CWD | Explicit path to the `nvpair-manual-nodes` binary the broker spawns for user-added nodes. Same optional semantics as `--node-info-path` |
| `--settings-path <path>` | `./nvpair-node-settings[.exe]` in the CWD | Explicit path to the `nvpair-node-settings` binary the broker spawns for the typed settings store. Same optional semantics as `--node-info-path` |
| `--cluster-manager-path <path>` | `./nvpair-cluster-manager[.exe]` in the CWD | Explicit path to the `nvpair-cluster-manager` binary the broker spawns for cluster pairing / membership. Same optional semantics as `--node-info-path` |
| `--scheduler-path <path>` | `./nvpair-job-scheduler[.exe]` in the CWD | Explicit path to the `nvpair-job-scheduler` binary the broker spawns for responsive, node-wide workload and GPU-pressure ranking. Same optional semantics as `--node-info-path` |
| `--cluster-dir <path>` | `cluster/` in the per-user data dir (`%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `~/.config/Nvidia Corporation/Personal AI Router` on Linux) | Cluster config dir (`node.crt`/`node.key` + `trusted/`, minted by `nvpair-cluster-manager`). Threaded to the cluster-scoped workers (`nvpair-errors`, `nvpair-workload-manager`, `nvpair-node-scanner`, `nvpair-manual-nodes`, and `nvpair-engine-manager`), each of which derives its membership from it continuously — so a create, join, or leave takes effect in place and the broker does **not** restart them. The broker also passes the parent of this path to `nvpair-cluster-manager` as `--config-dir`, so the only writer of the cluster dir and the workers reading it cannot resolve different directories. `nvpair-node-info` is excluded — it stays plain HTTP even when clustered (see the repository-root `SECURITY.md`). Defaults so cluster mTLS auto-activates with nothing to pass; with an empty or cert-less dir this node is not a member, so it serves and dials no inter-node cluster traffic at all |
| `--log-level <lvl>` | _(env `NVPAIR_LOG_LEVEL` or `info`)_ | `debug` \| `info` \| `warn` \| `error` |
| `--version` | | Print version and exit |

Logs go to **stderr** (shared `applog` format, same as every other NVPAIR binary). stdout is reserved for JSON-RPC frames in stdio mode, so logging there would corrupt the protocol. Every spawned worker's stderr is forwarded to the broker's stderr unmodified, so `[nvpair-node-scanner]`, `[nvpair-node-info]`, `[ollama-proxy]`, `[lmstudio-proxy]`, `[llamacpp-proxy]`, `[nvpair-workload-manager]`, and `[nvpair-cluster-manager]` lines interleave with the broker's `[nvpair-ui-broker]` lines on a single stream.

## Worker subprocesses

On startup — **before** emitting `app:ready` — the broker spawns the scanner and (when available) node-info, ollama-proxy, lmstudio-proxy, llamacpp-proxy, the workload-manager, and the cluster-manager as child processes over stdio. The proxy is spawned up front but doesn't gate `app:ready` — it announces its listen port asynchronously (see below). None of the auxiliary workers gate `app:ready`.

**`nvpair-node-scanner`** (the consolidated discovery daemon) is spawned first. It pushes `discovery:node-discovered`, `discovery:node-updated`, and `discovery:node-removed` notifications into the broker, which maintains them in an in-memory map keyed by `id`. Clients query that map via `discovery:get-nodes` and — once they've opted in via `discovery:subscribe` — receive a `discovery:nodes-changed` notification on every store mutation. The raw `discovery:node-*` notifications are never forwarded as-is. The scanner polls healthy node-info endpoints on a staggered two-second cadence, backs consecutive remote failures off to a 30-second cap, and emits compact `discovery:node-telemetry` observations containing maximum GPU utilization, validity, and age; these remain internal to broker scheduling. The broker registers this node's local service ports (`ni`/`er`/`wl`/`cl`/`em`, plus `ol`/`lm`/`ll` from the engine pollers) with the daemon over the same link, so the daemon can advertise them all in one `_nvpair-node` record.

**`nvpair-node-info`** is spawned next. It's a server, not an event source: it stands up the local `/v1/node-info` HTTP endpoint (GPU/CPU/memory inventory). It does not advertise itself — the broker registers its `ni` port with the scanner daemon, which carries it in the node record, and a peer's daemon fetches `/v1/node-info` over plain HTTP to enrich the node. The broker doesn't read anything back from node-info's stdout (drained and discarded). Spawning it is **optional**: if the binary can't be resolved (and no `--node-info-path` override was given) the broker logs a warning and continues serving discovery without it.

**Engine advertising.** The broker runs internal 5 s poll loops for Ollama, LM Studio, and llama.cpp:

- backend healthy and proxy ready → register `ol`, `lm`, or `ll` at the promoted proxy port and send the loopback backend through `node/set-local-backend`;
- backend or proxy unavailable → unregister the service and clear that local backend.

Peers therefore reach inference through the proxy's cluster-mTLS ingress, never the loopback engine directly. llama.cpp defaults to backend port 8080 and broker-managed facade port 8081. Model lists remain on `nvpair-engine-manager`'s `em` HTTP service.

**`llamacpp-proxy`** is the optional OpenAI-compatible reverse proxy for adopted llama-server instances. The broker supervises it, relays its control plane under `llamacpp-proxy:`, forwards workload and activity events, applies scheduler priorities, advertises service `ll`, and stops it before engine-manager teardown.

**`ollama-proxy`** is a local Ollama reverse proxy: it forwards inference requests to an Ollama node it discovers on the network. Its standalone default is `:11435`; with managed port ownership enabled (the default), the broker starts settings and engine-manager first, claims `:11434` with the proxy, and only then moves a stopped default-port Ollama backend to a free port. Custom backend ports are preserved. When the inherited `OLLAMA_HOST` names a distinct local plaintext port, the broker also gives the proxy that normalized loopback-only alias so clients already using the variable enter the same routing path; `localhost` reserves both canonical loopback families atomically, while remote and HTTPS targets are ignored. The alias port is reserved against every configured engine, local or remote engine start override, both proxy control planes, and the managed Ollama and LM Studio backend port plans, so a backend that has to move can never land on the alias. A running Ollama or unknown owner on either requested port is never stopped or moved: the primary uses a safe fallback when needed, and an occupied alias remains with its owner while the broker reports a warning. For automatic model-bearing inference, all three engine proxies combine scheduler pending counts and GPU pressure with short-lived local reservations under one lock before forwarding, so concurrent requests distribute without an artificial delay or a round trip through the scheduler. Manual pins, model-owner tiers, and the complete failover list keep their existing precedence. The broker otherwise treats the proxy as **optional and non-fatal**.

The proxy is the one worker that announces its bound port **asynchronously**, via a `ready` notification it emits once its HTTP listener is up. The broker captures that port and exposes it through the `proxy:get-status` request. Because `ready` arrives after `app:ready` (and the proxy is optional), clients learn the proxy's port by **polling** `proxy:get-status` rather than assuming it from `app:ready`.

The proxy is a full bidirectional JSON-RPC peer with a control plane (node selection, manual nodes, ...) and an event stream. The broker acts as a **generic relay** in both directions: any request a client sends under the `proxy:` namespace (other than the reserved broker-local ones) is forwarded to the proxy with the `proxy:` prefix stripped and the response relayed straight back (see `proxy:<method>` below), and every notification the proxy emits is re-emitted to subscribed clients as `proxy:<method>` (see `proxy:<event>` below). The `proxy:` prefix is the only translation in either direction — everything after it is the proxy's own native method name.

Two classes of proxy notification are **not** re-emitted under the `proxy:` namespace, because neither is a proxy control-plane event:

- The `workload:*` lifecycle events the proxy fires per inference request. Those are workload-manager traffic — see the workload-manager paragraph below for how they're routed.
- `node/activity`, which a proxy raises while a peer's engine is streaming response bytes back through it. That is discovery input: the broker forwards it to `nvpair-node-scanner` as a `discovery:node-activity` notification, where it counts as proof the peer is alive and cancels the eviction it would otherwise face for failing a liveness probe it had no spare CPU to answer. No client has any use for a per-request liveness frame. The handoff is a bounded queue drained by one goroutine — reports arrive for as long as inference streams, so a wedged scanner must not be able to stall the proxy reader, and a dropped report only means the scanner falls back to probing a node that will very likely answer.

**`nvpair-workload-manager`** is the cluster workload relay, and it's the only worker the broker talks to **bidirectionally over a notification-only link** (no id-bearing request/response). It supervises it the same optional, non-fatal way as node-info / proxy: a missing default sibling (and no `--workload-manager-path`) just means no cluster workload relay. The broker plays the workload **broker** role between the proxy and the manager:

- **Outbound (proxy -> broker -> manager -> peers).** When either proxy emits a `workload:started` / `workload:completed` / `workload:errored`, the broker stamps the local stable `hostUuid` onto `params.workloadInfo.originatedFrom`, applies the transition to its authoritative workload store, and fans the accepted update to the scheduler before forwarding the original lifecycle frame to the manager. The manager broadcasts it to peer nodes. With no manager supervised the event still updates local scheduling and subscribed clients, but is not broadcast.
- **Inbound (peers -> manager -> broker).** The manager translates peer-origin lifecycle events into `workloads:upsert` and peer-origin removals into `workloads:remove` on stdout. The broker applies each accepted transition to the same store, fans it to the scheduler, and relays it to clients subscribed via `workloads:subscribe`.
- **Local echo.** Local-origin proxy workloads are also emitted to the same `workloads:*` client stream (lifecycle translated to `workloads:upsert`), so a subscribed client sees a coherent cluster-wide view — its own workloads alongside peers'.

**`nvpair-job-scheduler`** consumes the accepted workload stream, compact GPU telemetry, and discovery snapshot. It smooths fresh utilization into pressure 0–3, uses neutral pressure 1 for invalid/missing/older-than-10-second samples, and orders by `pending + gpuPressure`, then pressure, then stable UUID. Load is node-wide across Ollama, LM Studio, and llama.cpp because both normally contend for the same resources. Each engine-specific `schedule:priority` carries `{engine,nodes,ranks}` and refreshes when order, pending counts, or pressure changes. The broker caches, generation-orders, and replays the full `{nodes,ranks}` snapshot to the matching proxy, where a newly delivered snapshot resets optimistic reservation deltas. On scheduler spawn/restart the broker replays active workloads and telemetry before discovery, then resumes all three live feeds.

`schedule:priority` and `node/set-priority` are internal worker contracts: the broker does not expose either notification to its connected client.

**`nvpair-cluster-manager`** owns the node's stable cryptographic identity (a self-generated UUID + Ed25519 self-signed certificate), the trusted-node store, and the EAP-NOOB PIN-based pairing flow that bootstraps mTLS trust between machines. Like ollama-proxy it's a full bidirectional JSON-RPC peer, and like the rest it's supervised **optionally and non-fatally**: a missing default sibling (and no `--cluster-manager-path`) just means the broker runs without cluster pairing. The broker is a **generic relay** for it in both directions: any `cluster:*` or `nodes:*` request a client sends is forwarded verbatim and the response (result or JSON-RPC error) is relayed straight back, and every notification the manager pushes is forwarded to the client unchanged. Unlike the proxy stream these events are not opt-in — they're low-volume, important pairing/membership pushes (`cluster:invite-received`, `cluster:identity-changed`, `nodes:changed`, ...), so they always flow. `errors:report` / `errors:clear` are the one exception: like the proxy's, they target `nvpair-errors` (which the broker doesn't supervise) and are dropped. Because pairing calls drive multi-round-trip inter-node network exchanges, the relay's per-call timeout is more generous (30 s) than the proxy's local control-plane timeout.

Shared lifecycle for all workers:

- Path resolution: the explicit `--scanner-path` / `--node-info-path` / `--proxy-path` / `--workload-manager-path` / `--cluster-manager-path` (and the analogous flags for lmstudio-proxy / errors / engine-manager / manual-nodes / settings) if set, otherwise the same-named `./<binary>` (with `.exe` on Windows) in the broker's current working directory. No PATH fallback — a clear "not found" beats a surprising stale binary. A missing scanner is fatal (it's the broker's core job); every other worker is optional.
- Log level: the broker's currently resolved level is forwarded at spawn time via `--log-level <lvl>`. Runtime `log/set-level` requests are fanned out to every running child over its stdin (see `log/set-level` below).
- Hidden console window on Windows (`CREATE_NO_WINDOW`).
- Shutdown: when the broker exits (signal, peer EOF, or `shutdown` RPC), it first asks engine-manager to stop its engines (`engine:prepare-shutdown`) while everything is still up, then closes each running worker's stdin. The worker sees EOF and exits cleanly, and the broker waits for it to exit — no timeout and no force-kill. Each worker owns its own bounded shutdown (engine-manager bounds its engine stop internally; the HTTP workers cancel their context and drain their server on EOF), so the broker never SIGKILLs a worker mid-teardown. A grace-then-kill teardown would orphan engine processes by killing engine-manager partway through stopping them, which is why there is no timeout here.
- **Auto-restart with crash surfacing** for every supervised worker (see [Supervision & recovery](#supervision--recovery)): a crash is reported as `supervisor:subprocess-crashed:<name>` and the worker is restarted with backoff, clearing the entry once it's healthy again and leaving it up if the restart budget is exhausted.

## JSON-RPC Surface

### Notifications (broker → caller)

#### `app:ready`

Emitted once on startup, after the scanner (and, when available, node-info and ollama-proxy) have been spawned and the transport is connected. Seeing `app:ready` means the broker is accepting requests **and** discovery events are flowing into its store (even if no nodes are known yet — mDNS browsing takes a moment). It does not imply node-info came up — that worker is optional and a failure to spawn it is logged but does not block `app:ready`. Nor does it imply the proxy is serving yet: the proxy reports its bound port asynchronously, so `proxy:get-status` may briefly show `ready:false` right after `app:ready`.

```json
{"jsonrpc":"2.0","method":"app:ready","params":{"version":"0.5.0"}}
```

#### `discovery:nodes-changed`

**Opt-in.** Only delivered to a peer that has called `discovery:subscribe`; until then (and after `discovery:unsubscribe`) the stream is silent. Once subscribed, it's fired every time the discovery store mutates — that is, whenever the daemon reports a `discovery:node-discovered`, `discovery:node-updated`, or `discovery:node-removed` (or a manual node changes). The payload is the full current snapshot as a **bare array** of `AvailableNode` objects (same shape as the array element in `discovery:get-nodes`), sorted by `id`.

```json
{
  "jsonrpc":"2.0",
  "method":"discovery:nodes-changed",
  "params":[
    {
      "id":"MY-PC",
      "name":"MY-PC",
      "ipAddress":"192.168.1.10",
      "port":14318,
      "lastSeen":1748275200
    }
  ]
}
```

A few semantics worth knowing:

- **Always the full snapshot, never a diff.** Replace your local list on every event.
- **A baseline is emitted on subscribe.** The first `discovery:nodes-changed` after a fresh `discovery:subscribe` carries the current snapshot (possibly an empty array), sent right after the subscribe response. Every later event corresponds to a real store mutation. (`discovery:get-nodes` remains available for an on-demand snapshot at any time.)
- **Fires on `lastSeen`-only changes too.** A periodic re-discovery that produces an otherwise byte-identical record still bumps `lastSeen`, which is a genuine freshness signal worth surfacing.
- **Empty array is a valid payload.** If every previously-known node ages out, you'll get a `discovery:nodes-changed` with `"params": []`.
- **Best-effort delivery.** The broker tries the notification once per mutation; transient write errors are logged but not retried.

#### `proxy:<event>`

**Opt-in.** Only delivered to a peer that has called `proxy:subscribe`; silent otherwise. Once subscribed, every notification `ollama-proxy` emits is re-emitted to the client as `proxy:<method>` — the proxy's native method name with the `proxy:` prefix, and `params` passed through verbatim (the proxy's own snake_case schema). The proxy's current notifications are:

| Forwarded as | Proxy emits | Meaning |
|---|---|---|
| `proxy:ready` | `ready` | The proxy bound its HTTP port (`{version, port}`). Also replayed as the baseline on a fresh subscribe. |
| `proxy:node/discovered` | `node/discovered` | A manual node was added to the proxy (relay-sourced routing targets don't emit these). |
| `proxy:node/updated` | `node/updated` | A manual node's fields changed. |
| `proxy:node/removed` | `node/removed` | A manual node was removed. |
| `proxy:node/selection-changed` | `node/selection-changed` | The active upstream changed (`{id}`; empty `id` = auto-select). |
| `proxy:proxy/request-started` | `proxy/request-started` | An HTTP request began being forwarded. |
| `proxy:proxy/request` | `proxy/request` | An HTTP request completed (status, durations). |

```json
{"jsonrpc":"2.0","method":"proxy:node/discovered","params":{"id":"MY-GPU-BOX","host":"my-gpu-box.local.","port":11434,"addresses":["192.168.1.10"],"txt":["models=llama3"]}}
```

Two classes of proxy notification are **not** re-emitted under the `proxy:` namespace: `errors:report` / `errors:clear` are routed into the `nvpair-errors` pipeline and surface on the `errors:update` stream (see [`errors:update`](#errorsupdate)); the proxy's `workload:*` events go to the workload-manager and surface on the separate `workloads:*` stream below.

#### `workloads:upsert` / `workloads:remove`

**Opt-in.** Only delivered to a peer that has called `workloads:subscribe`; silent otherwise. Once subscribed, the broker pushes a `workloads:upsert` whenever a workload is created or its state changes, and a `workloads:remove` when one is retired. The stream is the union of two sources, in the same shape regardless of origin:

- **Local workloads** — the `workload:*` lifecycle events the supervised Ollama and LM Studio proxies emit per inference request, stamped with this host's stable `hostUuid` (`originatedFrom`) and translated to `workloads:upsert`. The proxy also fills in `scheduledOn` with the destination node's `hostUuid`; the broker passes that through unchanged.
- **Peer workloads** — the `workloads:upsert` / `workloads:remove` the `nvpair-workload-manager` relays from other nodes after validating and de-duplicating their broadcasts.

- **Inferred workloads** — a `workloads:upsert` transitioning a workload to `failed` that **no origin ever sent**. The broker synthesizes one in two situations: when a node leaves discovery while workloads are pinned to it, and when a remote origin that is still present stops re-asserting a workload this node believes is running (the origin's re-sync heartbeat asserts each of its active workloads indefinitely, so prolonged silence about one means it is finished or the origin is gone). Both are recorded as *inferred*, so the origin's next authoritative event overrides them; a client should treat a `failed` as the broker's best current answer rather than proof the origin reported a failure, and its `error` text names the reason. Workloads this node originated or is itself executing are never inferred about.

`workloads:upsert` carries `params.workloadInfo` (a full `Workload`); `workloads:remove` carries `params.workloadId` and the origin `params.originatedFrom`. See [`nvpair-workload-manager`](../nvpair-workload-manager/README.md) for the `Workload` shape.

```json
{"jsonrpc":"2.0","method":"workloads:upsert","params":{"workloadInfo":{"id":"42","model":"llama-3-70b","engine":"ollama","state":"running","originatedFrom":"MY-PC","scheduledOn":"GPU-RIG","createdAt":1716998400000,"startedAt":1716998400000,"completedAt":null,"error":null,"requesterId":null}}}
```

A few semantics worth knowing:

- **Baseline is explicit.** `workloads:subscribe` itself replays nothing; subscribe first, then call `workloads:get-initial` and merge the snapshot with any overlapping transitions by workload key.
- **`upsert`, not a method replay.** A local `workload:completed` and a peer's running-state update both arrive as `workloads:upsert`; key on `(originatedFrom, engine, runId, id)` so concurrent engines and proxy restarts cannot collide.
- **Best-effort delivery**, like the other streams: one write attempt per event, transient errors logged not retried.

#### `errors:update`

The full service-error snapshot from `nvpair-errors`, relayed verbatim on every change (a bare `ServiceError[]`). **Not opt-in:** following the established error-reporting protocol the broker forwards it unconditionally. Every supervised worker's `errors:report` / `errors:clear` — proxy upstream-unreachable, engine-manager install/exit failures, manual-node probe failures, and the supervisor's own `supervisor:subprocess-crashed:<name>` — is forwarded into `nvpair-errors`, deduped there, and re-emitted here. A client can also inject its own `errors:report` (see Requests below); it is forwarded into `nvpair-errors` the same way a worker's is.

```
ServiceError = { id, message, timestamp, nodeId?, severity?, action?, engineType?, operation?, modelName? }
```

#### `engine:<event>`

**Opt-in** (`engine:subscribe`). The notifications `nvpair-engine-manager` emits — `engine:ready`, `engine:state-changed` (full `EngineStatus`), `engine:models-changed` (`{engine, models}`, pushed when an engine's set of models loaded in memory changes), `engine:install-progress`, `engine:pull-progress` (live progress for a local model pull), `engine:remote-progress` (live progress for a remote install/pull) — re-emitted verbatim (their method names are already `engine:`-prefixed). Off by default.

#### `connection/cluster-identity` / `connection/cluster-auto-sync`

The change-only cluster signals `nvpair-node-settings` pushes when `cluster_id` / `cluster_auto_sync` change. Relayed verbatim and unconditionally — the same way the UI receives them from the settings subprocess directly.

### Requests (caller → broker)

#### `discovery:get-nodes`

Returns the broker's current snapshot of every node `nvpair-node-scanner` has reported as live on the network. The list is sorted by `id` for stable rendering.

```json
{"jsonrpc":"2.0","id":1,"method":"discovery:get-nodes"}
```

Response:

```json
{
  "jsonrpc":"2.0",
  "id":1,
  "result":{
    "nodes":[
      {
        "id":"MY-PC",
        "name":"MY-PC",
        "ipAddress":"192.168.1.10",
        "port":14318,
        "lastSeen":1748275200
      }
    ]
  }
}
```

The per-node payload uses **camelCase** keys at this external boundary, in contrast to the snake_case the internal discovery wire uses elsewhere in the codebase.

Field-by-field:

| Field | Type | Source |
|---|---|---|
| `id` | string | the node's mDNS instance name (its hostname) from the `_nvpair-node` record |
| `name` | string | Currently mirrors `id`; will diverge once a node can advertise a human-readable name. |
| `ipAddress` | string | the node's authoritative `ip=` (else the best-ranked advertised address); empty string when none is usable. |
| `port` | number | the node's `ni` (node-info) port from its `_nvpair-node` TXT, typically `14318`. |
| `lastSeen` | number | **Unix seconds**. Updated on every `discovery:node-discovered` / `discovery:node-updated` event the broker receives from the daemon; not refreshed by `discovery:get-nodes` itself (a pure read). |
| `trusted` | bool | whether this node is a paired cluster peer (the daemon holds a pin for its `cluster-uuid`); false for non-cluster/unknown nodes. |
| `clustered` | bool | whether this node belongs to some cluster (it advertises a `cluster-uuid`), independent of whether we're paired with it (`trusted`). A client uses it to suppress a cluster invite that an already-clustered peer would reject. Omitted (false) for standalone/unknown nodes. |
| `models` | string[] | the node's available model names, enriched by the daemon from the node's engine-manager `em` endpoint (`GET /v1/models`). Omitted when the node advertises no engine-manager or no engine is running. |
| `modelsByEngine` | object | the same models attributed to the engine that serves each (keyed by engine name, e.g. `ollama` / `lmstudio`). Additive alongside the flat `models` union. A present engine key with `[]` means its inventory was successfully queried and is empty; a missing key means it was not running/queryable. Omitted when no engine inventory was successfully reported. |
| `loadedByEngine` | object | the models currently **loaded in memory** for each engine (normally a subset of `modelsByEngine`), keyed by engine name. Enriched from the peer's `loadedByEngine`. An engine key with an empty list means "running, nothing loaded"; a missing key means loaded state wasn't reported. Omitted when no engine reports loaded state. |

A node that the scanner reports as removed is deleted from the snapshot; `lastSeen` is not preserved for removed nodes. The response is wrapped in `{nodes: [...]}` (rather than a bare array) so we can grow summary fields later without breaking clients. An empty list is a normal early-startup state, not an error.

The broker also stores the scanner's richer per-node payload internally (`host`, all `addresses`, `txt`, `gpus`, `cpu`, `memory`); that data isn't currently exposed but will back future RPCs without re-querying the scanner.

#### `discovery:subscribe`

Opts the peer into the `discovery:nodes-changed` notification stream. The stream is **off by default** — a freshly connected peer receives no `discovery:nodes-changed` until it calls this. On a fresh subscription the broker immediately pushes one `discovery:nodes-changed` with the current snapshot (a baseline) right after the response, so a subscriber doesn't need a separate `discovery:get-nodes` to seed its view.

```json
{"jsonrpc":"2.0","id":6,"method":"discovery:subscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":6,"result":{"subscribed":true}}
```

Idempotent: subscribing while already subscribed returns `{"subscribed":true}` and does **not** re-send a baseline (the stream is already flowing). The subscription is per-connection state — it does not persist across reconnects.

#### `discovery:unsubscribe`

Stops the `discovery:nodes-changed` stream for this peer. The discovery store keeps tracking nodes; the peer simply stops being notified. `discovery:get-nodes` still works while unsubscribed.

```json
{"jsonrpc":"2.0","id":7,"method":"discovery:unsubscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":7,"result":{"subscribed":false}}
```

Idempotent: unsubscribing while not subscribed returns `{"subscribed":false}` with no error.

#### `ping`

Liveness check. Returns the broker's version and how long it has been running.

```json
{"jsonrpc":"2.0","id":2,"method":"ping"}
```

Response:

```json
{"jsonrpc":"2.0","id":2,"result":{"pong":true,"version":"0.5.0","uptime_ms":1234}}
```

#### `version`

Returns the broker's stamped version.

```json
{"jsonrpc":"2.0","id":3,"method":"version"}
```

Response:

```json
{"jsonrpc":"2.0","id":3,"result":{"version":"0.5.0"}}
```

#### `proxy:get-status`

Reports whether the supervised `ollama-proxy` is up and, if so, the HTTP port it bound. `ready` is `false` and `port` is `0` until the proxy emits its `ready` notification — and stays that way if no proxy is being supervised or the proxy has crashed — so the call is always safe and never errors. Answered locally from the broker's captured state (no round-trip to the proxy).

```json
{"jsonrpc":"2.0","id":8,"method":"proxy:get-status"}
```

Response (once the proxy is serving):

```json
{"jsonrpc":"2.0","id":8,"result":{"ready":true,"port":11435}}
```

`port` is the proxy's HTTP reverse-proxy listener — point an Ollama client at `http://localhost:<port>` to route inference traffic through it. The proxy comes up asynchronously, so a client typically polls this shortly after `app:ready` until `ready` is `true`.

#### `proxy:<method>` (generic relay)

Any request whose method starts with `proxy:` — except the reserved broker-local methods (`proxy:get-status`, `proxy:set-port`, and the subscription methods below) — is **forwarded to `ollama-proxy`** with the `proxy:` prefix stripped, and the proxy's result or error is relayed straight back to the caller unchanged. The `proxy:` prefix is the only translation: everything after it is the proxy's own native method name, and `params` pass through verbatim. This means the proxy's whole control plane is reachable through the broker without the broker enumerating individual methods. Current proxy methods include `nodes/list`, `node/select`, `node/selected`, `node/add-manual`, and `node/remove-manual` (see the `ollama-proxy` README for their params/results).

For example, to list the Ollama nodes the proxy currently knows and pin traffic to one:

```json
{"jsonrpc":"2.0","id":10,"method":"proxy:nodes/list"}
{"jsonrpc":"2.0","id":11,"method":"proxy:node/select","params":{"id":"MY-GPU-BOX"}}
```

The responses are exactly what the proxy returns (its payloads use the proxy's own snake_case `Node` schema — `id` / `host` / `port` / `addresses` / `txt` — not the camelCase `discovery:*` shape; the `proxy:` namespace is a deliberate exception kept thin to avoid coupling).

Two relay-specific error cases:

- If no proxy is being supervised (or it has exited), the broker replies with error `-32000` `"ollama-proxy not available"`.
- `proxy:shutdown` is **refused** with error `-32601` — the broker owns the proxy's lifecycle, so a client can't terminate it independently. Shut the broker down instead (which tears the proxy down with it).

#### `proxy:set-port`

**Intercepted, not relayed verbatim** — this is where the broker coordinates the engine ↔ proxy port conflict, because it's the only component that sees both. On `proxy:set-port {port}` the broker asks `nvpair-engine-manager` (`engine:get-installed`) which ports its **running** engines hold. If the requested port is free, it relays `set-port {port}` to the proxy unchanged. If a running engine already holds it, **the engine wins**: the broker bumps the proxy to the next free port, relays `set-port {effectivePort}`, and surfaces a sticky `warning` into the errors pipeline (id `ollama-proxy:port-bumped`, `action:"none"`) explaining the move; a later non-colliding `proxy:set-port` clears that warning. The conflict path **never changes an engine's port** — only the proxy is moved. The response is the proxy's own `set-port` result (`{version, port}` with the actually-bound port).

```json
{"jsonrpc":"2.0","id":9,"method":"proxy:set-port","params":{"port":11500}}
```

The same check runs whenever the proxy announces a (re)bound port (its restored port on startup), so a proxy that comes back up on a port a running engine has since taken is steered to a free one automatically. Error `-32000 "ollama-proxy not available"` when no proxy is supervised.

#### `lmstudio-proxy:get-status` / `lmstudio-proxy:subscribe` / `lmstudio-proxy:unsubscribe` / `lmstudio-proxy:<method>` (generic relay)

The LM Studio counterpart of the `proxy:*` surface runs the supervised `lmstudio-proxy` on compatibility port `:1234` and tracks the managed LM Studio backend on `:1235`. With managed port ownership enabled (the default), the broker identifies and moves an existing LM Studio server through engine-manager before allowing the proxy to claim `1234`; unknown owners are left untouched and force a warned proxy fallback. Disabling managed ownership preserves explicit custom backend and proxy ports. `lmstudio-proxy:get-status` reports the actual bound port; `lmstudio-proxy:subscribe` / `lmstudio-proxy:unsubscribe` opt into / out of its `lmstudio-proxy:<event>` stream; and any other `lmstudio-proxy:<method>` is relayed verbatim with the prefix stripped (`nodes/list`, `node/select`, `node/add-manual`, `node/remove-manual`, ...). `lmstudio-proxy:shutdown` is refused because the broker owns lifecycle ordering. Workload and error events feed the shared streams exactly as Ollama's do.

#### `proxy:subscribe`

Opts the peer into the `proxy:<event>` stream (off by default). On a fresh subscription the broker immediately replays the proxy's last `ready` payload as a baseline `proxy:ready` (if the proxy has come up), so a subscriber learns the port without a separate `proxy:get-status`.

```json
{"jsonrpc":"2.0","id":12,"method":"proxy:subscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":12,"result":{"subscribed":true}}
```

Idempotent: re-subscribing returns `{"subscribed":true}` and does not re-send a baseline. This is a reserved broker-local method — it is not forwarded to the proxy.

#### `proxy:unsubscribe`

Stops the `proxy:<event>` stream for this peer. Reserved broker-local; not forwarded. Idempotent.

```json
{"jsonrpc":"2.0","id":13,"method":"proxy:unsubscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":13,"result":{"subscribed":false}}
```

#### `workloads:subscribe`

Opts the peer into the `workloads:upsert` / `workloads:remove` stream (off by default). No baseline is pushed automatically; subscribe first, then call `workloads:get-initial` and merge by workload key so no transition can fall into a snapshot/subscribe gap.

```json
{"jsonrpc":"2.0","id":14,"method":"workloads:subscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":14,"result":{"subscribed":true}}
```

Idempotent: re-subscribing returns `{"subscribed":true}`. Per-connection state; not persisted across reconnects.

#### `workloads:unsubscribe`

Stops the `workloads:*` stream for this peer. Idempotent.

```json
{"jsonrpc":"2.0","id":15,"method":"workloads:unsubscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":15,"result":{"subscribed":false}}
```

#### `workloads:get-initial`

Returns `{ "workloads": [Workload, ...] }`, the broker's full-fidelity current and historic workload snapshot. Active records are kept in memory; bounded completed/failed history is persisted across broker restarts. The scheduler uses an internal active-only replay of the same catalog when its subprocess restarts.

#### `errors:get-initial`

Returns the current `ServiceError[]` snapshot from `nvpair-errors` (an empty array when no datastore is supervised, so it never errors). Relayed to `nvpair-errors`' own `errors:get-initial`.

#### `errors:clear`

Clears a service error by id. Params: `{ "id": "<error id>" }`. The broker stamps the `clearedBy` attribution and forwards the clear to `nvpair-errors`; it acks `null` immediately, with the authoritative state change arriving on the next `errors:update`.

#### `errors:report`

Forwards a client-originated service error into `nvpair-errors`, letting a client surface its own operational errors (e.g. a model-pull failure the engine itself doesn't report) through the same registry that drives `errors:update`. Params are a `ServiceError` (`{ id, message, severity?, action?, ... }`); the broker fills in the origin `nodeId` and `timestamp` when omitted, exactly as it does for a supervised worker's report. Accepted in two forms: as a **notification** (fire-and-forget, mirroring how a worker emits one on its stdout) or as a **request** (acked `null` once received — like `errors:clear`, the authoritative state change arrives on the next `errors:update`; an empty `id` is rejected with `-32602`).

#### `engine:subscribe` / `engine:unsubscribe`

Opt into / out of the `engine:<event>` stream (off by default). Acks `{ subscribed: bool }`. No baseline is replayed — call `engine:get-installed` for the initial snapshot.

#### `engine:<method>` (generic relay)

Any other `engine:*` request is forwarded to `nvpair-engine-manager` verbatim and its response relayed straight back. This covers the whole engine control plane: `engine:get-installed`, `engine:describe`, `engine:status`, `engine:install`, `engine:uninstall`, `engine:start`, `engine:stop`, `engine:restart`, `engine:set-port`, `engine:action`, `engine:logs`, `engine:errors`, `engine:models`. (`engine:set-port` persists the engine's server port as a manifest override that survives a restart — see the `nvpair-engine-manager` README.) Lifecycle ops run for minutes (reporting progress via the `engine:install-progress` / `engine:state-changed` push events), so the relay imposes **no broker-side timeout** — fire the request and watch the event stream for the outcome. Error `-32000 "engine-manager not available"` when no engine-manager is supervised.

#### `settings/<method>` (generic relay)

Any `settings/*` request is forwarded to `nvpair-node-settings` and its response relayed back: `settings/get-force-ports`, `settings/set-force-ports`, `settings/get-cluster-id`, `settings/set-cluster-id`, `settings/get-cluster-auto-sync`, `settings/set-cluster-auto-sync`, `settings/get-cluster-friendly-name`, `settings/set-cluster-friendly-name`. Error `-32000 "node-settings not available"` when no settings worker is supervised.

#### `node/add` / `node/remove` / `nodes/list` (manual nodes)

Relayed to `nvpair-manual-nodes`. `node/add` (`{ address, name?, tls_port?, mtls? }`) registers a user-added node and probes it; `node/remove` (`{ id }`) drops it; `nodes/list` returns the tracked manual nodes. Manually added nodes also surface in the shared `discovery:get-nodes` / `discovery:nodes-changed` snapshot — the broker merges `nvpair-manual-nodes`' `node/discovered|updated|removed` into the same store the scanner feeds. A `nvpair-manual-nodes` restart loses the in-memory entries because neither that worker nor the broker persists an authoritative copy, so clients must re-add manual nodes after a restart. Error `-32000 "manual-nodes not available"` when no manual-nodes worker is supervised.

**Manual → proxy bridge.** When the broker supervises both `nvpair-manual-nodes` and a proxy, it also bridges a manual node whose engine is reachable into that proxy via `node/add-manual` (host/port from the node's per-engine status), so inference can route to it through `proxy:node/select` / `lmstudio-proxy:node/select` just like a relay-discovered node. This is per-engine: a node whose `ollama_*` status is up is bridged into `ollama-proxy` (host/port from `ollama_port`), and one whose `lmstudio_*` status is up into `lmstudio-proxy` (from `lmstudio_port`) — a node running both is bridged into both. Manual nodes are by definition the ones that never appear via the daemon's `_nvpair-node` discovery, so this explicit add is what makes them routable. The bridge tracks reachability: an engine that goes down (or a node that is removed, or whose prober crashes) is pulled back out with `node/remove-manual`. A proxy that isn't supervised → that leg is a no-op; manual nodes still appear in the discovery snapshot as before.

#### `cluster:<method>` / `nodes:<method>` (generic relay)

Any request whose method starts with `cluster:` or `nodes:` is **forwarded to `nvpair-cluster-manager`** verbatim (no prefix stripping — these are the manager's own native namespaces), and the manager's result or JSON-RPC error is relayed straight back to the caller unchanged. As with the proxy relay this means the cluster-manager's whole surface is reachable through the broker without enumerating individual methods. Current methods include `cluster:get-node-id`, `cluster:set-identity`, `cluster:create`, `cluster:invite-node`, `cluster:respond-to-invite`, `cluster:cancel-invite`, `cluster:invite-status`, `nodes:get-initial`, and `nodes:remove` (see the [`nvpair-cluster-manager` README](../nvpair-cluster-manager/README.md) for their params/results and app-defined error codes).

The cluster-manager also pushes notifications (`cluster:invite-received`, `cluster:invite-canceled`, `cluster:identity-changed`, `nodes:changed`, ...); the broker forwards them to the caller verbatim. Unlike `proxy:<event>` / `workloads:*` these are **not opt-in** — there is no `cluster:subscribe`; the events always flow.

If no cluster-manager is being supervised (or it has exited), the broker replies with error `-32000` `"nvpair-cluster-manager not available"`. Because pairing calls drive multi-round-trip inter-node network exchanges, the relay's per-call timeout is 30 s (vs. the proxy's 5 s).

#### `shutdown`

Acknowledges, then terminates the broker. Engine-manager is asked to stop its engines first (`engine:prepare-shutdown`), then every running worker subprocess is torn down by closing its stdin and waiting for it to exit — no grace timer, no force-kill (each worker bounds its own shutdown).

```json
{"jsonrpc":"2.0","id":4,"method":"shutdown"}
```

#### `log/set-level`

Standard NVPAIR log-level toggle (see `shared/applog`). Accepts `debug` \| `info` \| `warn` \| `error`. The broker applies the level to itself and then **fans it out to every child subprocess it supervises** (the scanner, and — when running — node-info, the proxy, the workload-manager, the cluster-manager, and the job-scheduler) by forwarding a `log/set-level` notification down each child's stdin, so a single call adjusts the whole process tree. The response reflects the broker's own resolved level; child forwarding is best-effort (a child that's mid-teardown or never started is skipped and logged, never failing the call).

```json
{"jsonrpc":"2.0","id":5,"method":"log/set-level","params":{"level":"debug"}}
```

Response:

```json
{"jsonrpc":"2.0","id":5,"result":{"level":"debug"}}
```

## Shutdown

The broker shuts down on:

- stdin EOF (parent closed the pipe) or IPC peer disconnect
- `SIGINT` / `SIGTERM`
- A `shutdown` JSON-RPC request

## Running

The broker needs `nvpair-node-scanner` reachable as a sibling file in its working directory (or via `--scanner-path`); it also looks for `nvpair-node-info` (or via `--node-info-path`), `ollama-proxy` (or via `--proxy-path`), `lmstudio-proxy` (or via `--lmstudio-proxy-path`), `llamacpp-proxy` (or via `--llamacpp-proxy-path`), `nvpair-workload-manager` (or via `--workload-manager-path`), and `nvpair-cluster-manager` (or via `--cluster-manager-path`) the same way (plus lmstudio-proxy / errors / engine-manager / manual-nodes / settings), though all but the scanner are optional. The simplest local layout is to put all the built `.exe`s next to each other — which is exactly how the installer lays them out.

Quick smoke test in stdio mode (PowerShell, run from a directory that has the worker binaries — at minimum `nvpair-node-scanner`, plus `nvpair-node-info` if you want the local inventory server, `ollama-proxy` if you want the local Ollama proxy, `nvpair-workload-manager` if you want the cluster workload relay, and `nvpair-cluster-manager` if you want cluster pairing):

```powershell
'{"jsonrpc":"2.0","id":1,"method":"discovery:get-nodes"}' | .\nvpair-ui-broker.exe
```

You should see an `app:ready` notification followed by a `discovery:get-nodes` response. The `nodes` array will likely be empty on the first request — mDNS browsing needs a few seconds to populate the store. Issue the call again after a short wait to see real entries (assuming an `_nvpair-node._tcp` record is live on the LAN). To watch live updates, subscribe to the push stream first — every store change then pushes a `discovery:nodes-changed` notification:

```powershell
'{"jsonrpc":"2.0","id":1,"method":"discovery:subscribe"}' | .\nvpair-ui-broker.exe
```

Pass explicit worker paths:

```powershell
.\nvpair-ui-broker.exe --scanner-path C:\path\to\nvpair-node-scanner.exe --node-info-path C:\path\to\nvpair-node-info.exe --proxy-path C:\path\to\ollama-proxy.exe --lmstudio-proxy-path C:\path\to\lmstudio-proxy.exe --llamacpp-proxy-path C:\path\to\llamacpp-proxy.exe --workload-manager-path C:\path\to\nvpair-workload-manager.exe --errors-path C:\path\to\nvpair-errors.exe --engine-manager-path C:\path\to\nvpair-engine-manager.exe --manual-nodes-path C:\path\to\nvpair-manual-nodes.exe --settings-path C:\path\to\nvpair-node-settings.exe --cluster-manager-path C:\path\to\nvpair-cluster-manager.exe
```

Attach to a pre-existing endpoint:

```powershell
# Windows named pipe (the UI process must already be listening on it)
.\nvpair-ui-broker.exe --ipc \\.\pipe\nvpair-ui-broker
```

```bash
# Unix domain socket (the UI process must already be listening on it)
./nvpair-ui-broker --ipc /tmp/nvpair-ui-broker.sock
```

## What this version intentionally does NOT do (yet)

- **Engine-advertise control surface.** Engine registration is auto-driven only: the broker tracks local Ollama, LM Studio, and llama.cpp and registers `ol`, `lm`, or `ll` at the corresponding promoted proxy while healthy. There is no manual-advertise RPC.
- **node-info control surface.** node-info is spawned and torn down with the broker, and the broker pushes it only two things over stdin: the log level, and this node's cluster principal (`nodeinfo:set-cluster-identity`, sent on spawn and on every membership or pin-set change, because node-info holds no cluster dir and so cannot read membership itself). Otherwise it's hands-off: the broker registers its port with the daemon (which enriches over plain HTTP) but doesn't pass through TLS material (`--cert` / `--key` / `--client-ca`) or a custom `--port`, and exposes no RPC to query or reconfigure it. It runs with its own defaults plus those two pushes.
- **Manual-node persistence across restarts.** `nvpair-manual-nodes` keeps its entries only in memory and the broker holds no authoritative copy, so a manual-nodes crash-and-restart loses the user's manual nodes (the broker evicts the orphaned entries from the snapshot; clients must re-add them).
- **Per-event push semantics.** `discovery:nodes-changed` always carries the full current snapshot, not a delta. For small N this is fine and lets the client treat the payload as authoritative without state reconciliation. `errors:update` is likewise a full snapshot.
- **BYO-TLS node-info discovery.** The daemon enriches peers over plain HTTP per the consolidated transport policy (node-info is plain on the broker path). Reading a node-info served over operator-configured BYO-TLS / mTLS is not supported through the broker; discovery works for the plain-HTTP case (the common one).
- **Per-worker log-level granularity.** `log/set-level` fans the broker's single level out to every running child, but there's no way to set a different level per worker — it's one level for the whole process tree.
- **AuthN / AuthZ.** Transport security relies on the parent's pipe / socket ACL. There is no per-message token.
- **HTTP / WebSocket transport.** JSON-RPC over stdio or pipe only — no web bridge yet. Likely lives in a separate component if/when added.
- **Automatic workload baseline on subscribe.** `workloads:subscribe` starts only the live stream; clients explicitly request `workloads:get-initial` after subscribing and merge overlapping records. The broker persists bounded terminal history, while active state is rebuilt from live workload reconciliation rather than treated as durable across a full process-tree restart.
