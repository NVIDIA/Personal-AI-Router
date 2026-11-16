<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-ui-broker

The [engine settings protocol](ENGINE_SETTINGS.md) documents local/paired
launch and port editing, revision checks, serialized application and recovery.

A Go service that exposes the NVIDIA PAIR API to **UI processes**
and other clients (CLIs, dashboards, mobile companions, scripts, tests, etc.).
The shipped graphical UI is bundled alongside the backend services and launches
the broker from the same installation directory. The broker is the parent
process and canonical backend entry point: it supervises the worker subprocesses
on the UI's behalf, speaking JSON-RPC over stdio.

The broker supervises **ten** worker subprocesses, so a client gets the whole
backend behind one endpoint. Each is spawned at startup and relayed under its own
namespace:

| Worker | Role | Client namespace |
| --- | --- | --- |
| `nvpair-node-scanner` | Discovery daemon: advertises this host's one `_nvpair-node._tcp` record and browses the LAN | `discovery:*` |
| `nvpair-node-info` | Owned inventory IPC, public display HTTP, and peer node inventory over pinned mTLS at `/v1/node-info` | _internal_ |
| `nvpair-proxy` | One process hosting an inference proxy and router facade per enabled engine | `ollama-proxy:*`, `lmstudio-proxy:*`, `llamacpp-proxy:*`, `vllm-proxy:*` |
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

- **Engine advertising.** The broker polls local Ollama, LM Studio,
  llama.cpp, and vLLM every 5 s and registers each running engine's promoted facade port
  (`ol` / `lm` / `lc` / `vl`) with the discovery daemon, so they are carried in this
  host's single `_nvpair-node` record. The
  model list is not part of that record — it is served over HTTP by
  `nvpair-engine-manager` on the `em` service and fetched by a peer's daemon
  during discovery enrichment.
- **Workload brokering.** The broker stamps the local node id onto the
  `workload:*` events a proxy emits, applies them to its authoritative store,
  feeds the scheduler, and forwards them to the workload manager for broadcast.
  Peer-origin updates come back the other way as `workloads:upsert` /
  `workloads:remove` for subscribed clients.
- **Scheduling telemetry.** Owned local and pinned remote observations populate a
  source-aware cache keyed by `hostUuid`; scanner data takes precedence. The
  broker advances sample age locally, feeds compact maximum-GPU telemetry to the
  scheduler, and replays it after a scheduler restart.

Unverified discoveries retain independent `discoveryRecordId` keys throughout
the store and relay. They cannot overwrite a verified host, clear its telemetry,
fail its workloads, or enter the scheduler's node universe. Explicit manual
engine targets keep their own operational keys until authenticated. The broker
forwards scanner `discovery:node-info` hardware snapshots when their identity
and verification match the current scanner record.

When scanner and manual records describe the same verified host, scanner supplies
identity, addresses, models, and hardware. If its hardware is missing, display
snapshots may use manual hardware with matching trust and verification. The
sample keeps its original age; scheduling telemetry still uses its own source
selection.

### Supervision & recovery

Every worker runs under a supervisor that auto-restarts it on an unexpected exit: exponential backoff (~1 s → 16 s), a budget of 5 attempts per unhealthy streak, and a 60 s "healthy reset" window after which a restarted worker is considered recovered. A crash is surfaced as a sticky `supervisor:subprocess-crashed:<name>` entry in the error registry (so connected tools can show "this helper is down"); recovery clears it, and a worker that exhausts its restart budget leaves the entry up. The scanner is the one worker whose **initial** spawn is fatal (the broker has no job without discovery); every other worker is optional and a failed first spawn just runs the broker without it. `nvpair-errors` is a special case: it can't report its own death into itself, so its crash logs to stderr (rather than the pipeline) while still being auto-restarted.

There is **one supervisor per process, not per engine**. `nvpair-proxy` hosts a facade for every enabled engine, so its crash entry names the process (`nvpair-proxy`) and its restart brings every facade back together; a terminal failure releases each engine's port-ownership gate individually, so a caller waiting on one engine is never held by another's state.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0 — same conventions as every other NVPAIR subprocess. By default the broker reads stdin and writes stdout; with `--ipc <path>` it dials a Unix domain socket or Windows named pipe instead. The parent UI process owns the endpoint in both cases: it either spawns the broker with piped stdio, or sets up the pipe / socket beforehand and points the broker at it via `--ipc`. The broker always talks to exactly one peer.

The owned node-info worker emits `nodeinfo:info-updated {info, capturedAt}` at startup and
every two seconds. The broker retains the latest snapshot and sends it to scanner
as `discovery:local-node-info {info, capturedAt}`, including after scanner restarts.
Each hop uses the shared host clock to capture Unix milliseconds before writing;
receivers include elapsed pipe delivery in GPU age. Missing or future capture
times make GPU telemetry unavailable. Queue and cache time also increase the
reported sample age. Display receipt time `info.observedAt` remains separate.
The relay queue is bounded, and
worker silence cannot make an old sample freshly idle. A snapshot naming a host
other than this broker's host is rejected.

Local scanner directory events marked `identityVerification:local` supply
`nodeinfo:set-peer-metadata {hostUuid, clusterUuid, peer}` to node-info. The
complete registry metadata replaces the prior snapshot when names, addresses,
membership, or registrations change, and is replayed after node-info restarts.
Remote discovery claims cannot populate this metadata. These links are internal
worker IPC. Scanner emits full local hardware back to the broker, which exposes
it to desktop and TUI in discovery snapshots and live hardware updates.

Verified record IDs bind scanner and manual proofs to current pins and local
membership. Trust changes remove affected pinned scanner records and clear
verification, hardware, and scheduling samples for affected manual entries.
Manual configurations and independently reachable engine routes are preserved.
Queued events and scheduler replay
recheck that binding. A verified scanner replacement retires the previous record
ID from the worker relay before publishing the replacement. Delayed removals
retire only their own relay entry and preserve the current host record and
measurements. A scanner crash withdraws its entire directory; its
replacement authenticates remote records again while local IPC replays retain
their age. Notifications from a previous worker cannot restore its records.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint to dial: Unix socket path or Windows named pipe (e.g. `\\.\pipe\nvpair-ui-broker`) |
| `--scanner-path <path>` | `./nvpair-node-scanner[.exe]` in the CWD | Explicit path to the `nvpair-node-scanner` binary the broker should spawn |
| `--node-info-path <path>` | `./nvpair-node-info[.exe]` in the CWD | Explicit path to the `nvpair-node-info` binary the broker should spawn. When omitted and no default sibling exists, the broker runs without the local inventory server (non-fatal); when set to an invalid path, the broker exits with an error |
| `--proxy-path <path>` | `./nvpair-proxy[.exe]` in the CWD | Explicit path to the `nvpair-proxy` binary. One process fronts every engine: the broker spawns it once and then sends a `facade/enable` per entry in `--proxy-engines`. Same optional semantics as `--node-info-path`: an absent default sibling means no local proxies (non-fatal); an invalid explicit path exits with an error |
| `--proxy-engines <csv>` | `ollama,lmstudio,llamacpp,vllm` | Which engines to front with a proxy. An unrecognized name exits with an error rather than being skipped, so a typo cannot look like it worked. An engine left out is not started **and not prepared** — the broker will not relocate an engine whose facade nothing is going to claim |
| `--workload-manager-path <path>` | `./nvpair-workload-manager[.exe]` in the CWD | Explicit path to the `nvpair-workload-manager` binary the broker spawns for the cluster workload relay. Same optional semantics as `--node-info-path`: an absent default sibling means no workload relay (non-fatal); an invalid explicit path exits with an error |
| `--errors-path <path>` | `./nvpair-errors[.exe]` in the CWD | Explicit path to the `nvpair-errors` binary the broker spawns (with `--peer-sync`) for the service-error pipeline. Same optional semantics as `--node-info-path`: an absent default sibling means the error pipeline is disabled — producers' errors are dropped (non-fatal); an invalid explicit path exits with an error |
| `--engine-manager-path <path>` | `./nvpair-engine-manager[.exe]` in the CWD | Explicit path to the `nvpair-engine-manager` binary the broker spawns for engine management. Same optional semantics as `--node-info-path` |
| `--manual-nodes-path <path>` | `./nvpair-manual-nodes[.exe]` in the CWD | Explicit path to the `nvpair-manual-nodes` binary the broker spawns for user-added nodes. Same optional semantics as `--node-info-path` |
| `--settings-path <path>` | `./nvpair-node-settings[.exe]` in the CWD | Explicit path to the `nvpair-node-settings` binary the broker spawns for the typed settings store. Same optional semantics as `--node-info-path` |
| `--cluster-manager-path <path>` | `./nvpair-cluster-manager[.exe]` in the CWD | Explicit path to the `nvpair-cluster-manager` binary the broker spawns for cluster pairing / membership. Same optional semantics as `--node-info-path` |
| `--scheduler-path <path>` | `./nvpair-job-scheduler[.exe]` in the CWD | Explicit path to the `nvpair-job-scheduler` binary the broker spawns for responsive, node-wide workload and GPU-pressure ranking. Same optional semantics as `--node-info-path` |
| `--cluster-dir <path>` | `cluster/` in the per-user data dir (`%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `~/.config/Nvidia Corporation/Personal AI Router` on Linux) | Cluster identity and pins, threaded to the cluster-scoped workers including node-info. Membership changes take effect without worker restarts. The cluster manager receives its parent as `--config-dir` so readers and the sole writer agree on one directory. Node-info keeps public display HTTP available independently of its peer node inventory handler |
| `--log-level <lvl>` | _(env `NVPAIR_LOG_LEVEL` or `info`)_ | `debug` \| `info` \| `warn` \| `error` |
| `--version` | | Print version and exit |

Logs go to **stderr** (shared `applog` format, same as every other NVPAIR binary). stdout is reserved for JSON-RPC frames in stdio mode, so logging there would corrupt the protocol. Every spawned worker's stderr is forwarded to the broker's stderr unmodified, so `[nvpair-node-scanner]`, `[nvpair-node-info]`, `[nvpair-proxy]`, `[nvpair-workload-manager]`, and `[nvpair-cluster-manager]` lines interleave with the broker's `[nvpair-ui-broker]` lines on a single stream. The proxy logs under the process name because that is what it is; a line about one engine's facade carries an `engine` field instead of a distinct prefix.

## Worker subprocesses

On startup — **before** emitting `app:ready` — the broker spawns the scanner and (when available) node-info, `nvpair-proxy`, the workload-manager, and the cluster-manager as child processes over stdio. The proxy is spawned up front but doesn't gate `app:ready` — each of its facades announces its listen port asynchronously (see below). None of the auxiliary workers gate `app:ready`.

**`nvpair-node-scanner`** (the consolidated discovery daemon) is spawned first. It pushes `discovery:node-discovered`, `discovery:node-updated`, and `discovery:node-removed` notifications into the broker, which maintains them in an in-memory map keyed by verified host identity or independent discovery record identity. Clients query that map via `discovery:get-nodes` and — once they've opted in via `discovery:subscribe` — receive a `discovery:nodes-changed` notification on every store mutation. Raw directory-change notifications are not forwarded as-is. Scanner polls healthy verified node-info endpoints over pinned mTLS on a staggered two-second cadence, backs failures off to a 30-second cap, and emits compact `discovery:node-telemetry` observations for internal scheduling plus full `discovery:node-info` client hardware updates. A separate bounded scanner loop collects public previews for unverified records. The broker registers this node's local service ports (`ni`/`er`/`wl`/`cl`/`em`, plus `ol`/`lm`/`lc`/`vl` from the engine poller) with the daemon over the same link, so the daemon advertises them all in one `_nvpair-node` record.

**`nvpair-node-info`** serves public display HTTP and peer node inventory over
pinned mTLS on its registered `ni` port. The broker passes its cluster directory and host UUID,
relays owned hardware snapshots from stdout to scanner, and sends local registry
metadata back for peer node inventory responses. Spawning it is optional: if no binary
is resolved and no explicit path was given, the broker continues without it.

**Engine advertising.** The broker runs an internal 5 s poll loop against local
Ollama (`GET /`), LM Studio (`GET /v1/models`), llama.cpp (`GET /health`), and vLLM (`GET /health`)
at their configured backend ports and reconciles this node's engine registration
with the scanner daemon:

- engine **up** and facade **ready** → register `ol` / `lm` / `lc` / `vl` at the
  promoted facade port, never the private backend port;
- engine **down** → unregister it.

The daemon folds those registrations into this host's single `_nvpair-node` record, so a peer discovers the engine through the shared channel. The model list is not part of that registration — it's served over HTTP by `nvpair-engine-manager` (the `em` service, `GET /v1/models`) and enriched onto each node by the peer's daemon. There is no separate advertiser subprocess and no manual-advertise RPC.

**`nvpair-proxy`** is one process that fronts every enabled engine. It starts with no engine and no listener; the broker then sends it a `facade/enable` per engine, carrying that engine's port and any alias addresses. A flag could not express this, because the broker plans a different port for each engine. Each facade forwards inference to a node it discovers on the network and speaks its own engine's dialect.

One process for all of them is deliberate. Between scheduler snapshots a facade
takes short-lived reservations for work it has dispatched, and those live in
the process; separate processes would each hold only part of that picture, so
simultaneous bursts across engines could select the same node believing it idle.
The cost is **shared fate**: a crash takes every facade down and the supervisor
brings them all back together, reported once as
`supervisor:subprocess-crashed:nvpair-proxy` rather than against one engine.
Within the process the boundaries are finer — a facade that loses its bind
race, cannot be moved to a free port, or panics while handling a request is
withdrawn or answered with an error on its own, leaving the others serving.

Ollama's standalone default is `:11435`; with managed port ownership enabled (the default), the broker starts settings and engine-manager first, claims `:11434` with that facade, and only then moves a stopped default-port Ollama backend to a free port. Custom backend ports are preserved. When the inherited `OLLAMA_HOST` names a distinct local plaintext port, the broker also gives the facade that normalized loopback-only alias so clients already using the variable enter the same routing path; `localhost` reserves both canonical loopback families atomically, while remote and HTTPS targets are ignored. The alias port is reserved against every configured engine, local or remote engine start override, every facade's control plane, and every managed engine's backend port plan, so a backend that has to move can never land on the alias. A running Ollama or unknown owner on either requested port is never stopped or moved: the primary uses a safe fallback when needed, and an occupied alias remains with its owner while the broker reports a warning.

vLLM is a managed engine handled exactly like llama.cpp below, with the
`vllm serve` process on `:8001` and its default-enabled facade on `:8000` or a
safe fallback.

llama.cpp is a managed engine: engine-manager owns its lifecycle and starts it
on its configured loopback port, `:8081` by default. The broker places its
default-enabled facade on `:8080` or a safe fallback, without automatically
relocating the engine or adding a compatibility-port ownership gate. Fallback
selection excludes the default engine port and other engines' reserved ports.
Explicit engine and proxy settings are preserved; a bind failure on an explicitly
chosen proxy port is reported instead of selecting a fallback.

For automatic model-bearing inference, every facade combines scheduler pending counts and GPU pressure with the process-wide reservation map under one lock before forwarding, so concurrent requests distribute without an artificial delay or a round trip through the scheduler. A reservation is released when its request ends and moves with a failover, so a node stops counting as loaded as soon as it stops working. Manual pins, model-owner tiers, and the complete failover list keep their existing precedence. The broker otherwise treats the proxy as **optional and non-fatal**.

Each facade announces its bound port **asynchronously**, via an engine-addressed `ready` notification emitted once its HTTP listener is up. The broker records readiness per engine and exposes it through that engine's `<engine>-proxy:get-status` request — per engine, because the facades bind different ports and a single port for the process would be whichever readied last. Because `ready` arrives after `app:ready` (and the proxy is optional), clients learn a port by **polling** `<engine>-proxy:get-status` rather than assuming it from `app:ready`.

The proxy is a full bidirectional JSON-RPC peer with a control plane (node
selection, manual nodes, ...) and an event stream. The broker acts as a
**generic relay** in both directions: any request a client sends under an
enabled `<engine>-proxy:` namespace (other than reserved broker-local ones) is
forwarded to that engine's facade and the response relayed straight back, and
every facade notification is re-emitted to subscribed clients under the same
component namespace.

Client requests use component namespaces such as `ollama-proxy:`. The broker
translates that prefix into the `ollama:` facade address on the proxy process's
worker link. Each worker-link message carries the engine it concerns.
Process-scoped methods (`log/set-level`, the scheduler's `node/set-priority`)
carry no address, and `facade/enable` names its engine in the payload because it
runs before that facade exists.

Two classes of proxy notification are **not** re-emitted under any
`<engine>-proxy:` namespace, because neither is a proxy control-plane event:

- The `workload:*` lifecycle events the proxy fires per inference request. Those are workload-manager traffic — see the workload-manager paragraph below for how they're routed.
- `node/activity`, which a proxy raises while a peer's engine is streaming response bytes back through it. The broker forwards it to `nvpair-node-scanner` as `discovery:node-activity`. Recent inference activity provides availability evidence for retaining a verified peer independently of multicast announcement expiry. The handoff uses a bounded queue drained by one goroutine so a blocked scanner cannot stall the proxy reader. Dropping a report loses one availability hint; the scanner continues independent authenticated inventory reads and retains verified peers while recent accepted inventory or inference evidence remains fresh.

**`nvpair-workload-manager`** is the cluster workload relay, and it's the only worker the broker talks to **bidirectionally over a notification-only link** (no id-bearing request/response). It supervises it the same optional, non-fatal way as node-info / proxy: a missing default sibling (and no `--workload-manager-path`) just means no cluster workload relay. The broker plays the workload **broker** role between the proxy and the manager:

- **Outbound (proxy -> broker -> manager -> peers).** When a facade emits a `workload:started` / `workload:completed` / `workload:errored`, the broker stamps the local stable `hostUuid` onto `params.workloadInfo.originatedFrom`, applies the transition to its authoritative workload store, and fans the accepted update to the scheduler before forwarding the original lifecycle frame to the manager. The manager broadcasts it to peer nodes. With no manager supervised the event still updates local scheduling and subscribed clients, but is not broadcast.
- **Inbound (peers -> manager -> broker).** The manager translates peer-origin lifecycle events into `workloads:upsert` and peer-origin removals into `workloads:remove` on stdout. The broker applies each accepted transition to the same store, fans it to the scheduler, and relays it to clients subscribed via `workloads:subscribe`.
- **Local echo.** Local-origin proxy workloads are also emitted to the same `workloads:*` client stream (lifecycle translated to `workloads:upsert`), so a subscribed client sees a coherent cluster-wide view — its own workloads alongside peers'.

**`nvpair-job-scheduler`** consumes the accepted workload stream, compact GPU
telemetry, and discovery snapshot. It smooths fresh utilization into pressure
0–3, uses neutral pressure 1 for invalid/missing/older-than-10-second samples,
and orders by `pending + gpuPressure`, then pressure, then stable UUID. Load is
node-wide across Ollama, LM Studio, llama.cpp, and vLLM because they normally contend
for the same resources. Each engine-specific `schedule:priority` carries
`{engine,nodes,ranks}` and refreshes when order, pending counts, or pressure
changes. The broker caches, generation-orders, and replays the full
`{nodes,ranks}` snapshot to the matching facade, where a newly delivered
snapshot resets optimistic reservation deltas. On scheduler spawn/restart the
broker replays active workloads and telemetry before discovery, then resumes all
three live feeds.

`schedule:priority` and `node/set-priority` are internal worker contracts: the broker does not expose either notification to its connected client.

**`nvpair-cluster-manager`** owns the node's stable cryptographic identity (a self-generated UUID + Ed25519 self-signed certificate), the trusted-node store, and the EAP-NOOB PIN-based pairing flow that bootstraps mTLS trust between machines. Like ollama-proxy it's a full bidirectional JSON-RPC peer, and like the rest it's supervised **optionally and non-fatally**: a missing default sibling (and no `--cluster-manager-path`) just means the broker runs without cluster pairing. The broker is a **generic relay** for it in both directions: any `cluster:*` or `nodes:*` request a client sends is forwarded verbatim and the response (result or JSON-RPC error) is relayed straight back, and every notification the manager pushes is forwarded to the client unchanged. Unlike the proxy stream these events are not opt-in — they're low-volume, important pairing/membership pushes (`cluster:invite-received`, `cluster:identity-changed`, `nodes:changed`, ...), so they always flow. `errors:report` / `errors:clear` are the one exception: like the proxy's, they target `nvpair-errors` (which the broker doesn't supervise) and are dropped. Because pairing calls drive multi-round-trip inter-node network exchanges, the relay's per-call timeout is more generous (30 s) than the proxy's local control-plane timeout.

Shared lifecycle for all workers:

- Path resolution: the explicit `--scanner-path` / `--node-info-path` / `--proxy-path` / `--workload-manager-path` / `--cluster-manager-path` (and the analogous flags for errors / engine-manager / manual-nodes / settings; every engine's facade lives in the single process at `--proxy-path`, with `--proxy-engines` choosing which are enabled) if set, otherwise the same-named `./<binary>` (with `.exe` on Windows) in the broker's current working directory. No PATH fallback — a clear "not found" beats a surprising stale binary. A missing scanner is fatal (it's the broker's core job); every other worker is optional.
- Log level: the broker's currently resolved level is forwarded at spawn time via `--log-level <lvl>`. Runtime `log/set-level` requests are fanned out to every running child over its stdin (see `log/set-level` below).
- Hidden console window on Windows (`CREATE_NO_WINDOW`).
- Shutdown: when the broker exits (signal, peer EOF, or `shutdown` RPC) it closes ingress at the proxy so no new inference arrives, asks engine-manager to stop its engines (`engine:prepare-shutdown`) while everything else is still up, then closes every remaining worker's stdin.
  Clients leave this ordering to the broker; one that calls `engine:prepare-shutdown` itself stops the engines while the proxy is still routing to them.
  A worker sees EOF as its shutdown signal and exits on its own; each owns its own bounded shutdown (engine-manager bounds its engine stop internally, the HTTP workers cancel their context and drain their server).
  The proxy is stopped first on every exit path, including one that returns before the engine-manager request.
  The remaining joins run **concurrently**, so teardown costs the slowest worker rather than the sum.
  The engine-manager wait and each worker's grace draw on one shared teardown budget (`stopwait.go`).
  A worker that misses its grace escalates to terminate and then kill, each with a fixed 2s wait, so escalation can run up to 4s past the budget, and a single wedged worker cannot park the whole quit.
  The escalation deliberately spares engine-manager on Windows, where a terminate is already a kill and would orphan the engines it is part-way through stopping.
  When teardown is slow the broker logs one breakdown at **warn** level, the level the desktop runs it at: the total from the start of teardown, the proxy and engine-sweep phases, and each worker's join.
- **Auto-restart with crash surfacing** for every supervised worker (see [Supervision & recovery](#supervision--recovery)): a crash is reported as `supervisor:subprocess-crashed:<name>` and the worker is restarted with backoff, clearing the entry once it's healthy again and leaving it up if the restart budget is exhausted.

## JSON-RPC Surface

### Notifications (broker → caller)

#### `app:ready`

Emitted once on startup, after the scanner (and, when available, node-info and ollama-proxy) have been spawned and the transport is connected. Seeing `app:ready` means the broker is accepting requests **and** discovery events are flowing into its store (even if no nodes are known yet — mDNS browsing takes a moment). It does not imply node-info came up — that worker is optional and a failure to spawn it is logged but does not block `app:ready`. Nor does it imply the proxy is serving yet: the proxy reports its bound port asynchronously, so `ollama-proxy:get-status` may briefly show `ready:false` right after `app:ready`.

```json
{"jsonrpc":"2.0","method":"app:ready","params":{"version":"0.5.0"}}
```

#### `discovery:nodes-changed`

**Opt-in.** Only delivered to a peer that has called `discovery:subscribe`; until then (and after `discovery:unsubscribe`) the stream is silent. Once subscribed, it's fired every time the discovery store mutates — that is, whenever the daemon reports a `discovery:node-discovered`, `discovery:node-updated`, or `discovery:node-removed` (or a manual node changes). The payload is the full current snapshot as a **bare array** of `AvailableNode` objects (same shape as the array element in `discovery:get-nodes`), sorted by `id`.

Snapshots carry `discoveryRecordId` and `identityVerification` (`local`, `pinned`,
or `unverified`). A manual entry is `unverified` under its independent
`manual:<entry-id>` record ID until pinned-mTLS authentication succeeds. An
authenticated manual entry is `pinned` and carries its current peer record ID.
These fields describe provenance separately from the `trusted` annotation; a
claimed UUID or a saved pin alone does not establish authenticated endpoint
ownership.

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
- **Hardware baseline.** Optional `info` carries the full latest hardware for
  that record, with source validity and elapsed sample age. Local data comes
  from owned IPC, paired data from pinned mTLS, and unverified previews from
  separate worker HTTP reads. Missing `info` clears previous hardware. Live
  `discovery:node-info` updates follow the same record/provenance contract.
- **A baseline is emitted on subscribe.** The first `discovery:nodes-changed` after a fresh `discovery:subscribe` carries the current snapshot (possibly an empty array), sent right after the subscribe response. Every later event corresponds to a real store mutation. (`discovery:get-nodes` remains available for an on-demand snapshot at any time.)
- **`lastSeen` is a record-change time.** It is not a hardware sample timestamp
  or an independent liveness verdict. Hardware validity and age live in `info`.
- **Empty array is a valid payload.** If every previously-known node ages out, you'll get a `discovery:nodes-changed` with `"params": []`.
- **Best-effort delivery.** The broker tries the notification once per mutation; transient write errors are logged but not retried.

#### `discovery:node-info`

Full hardware updates carry `{discoveryRecordId, hostUuid, clusterUuid?,
identityVerification, info}`. Clients apply them only to the matching current
record and verification. Local and pinned records also require matching hardware
host identity. Unverified previews are keyed by discovery record ID and have
their response `info.hostUuid`, `info.clusterUuid`, and `info.peer` stripped.
They cannot change a row's identity, trust, endpoints, models, routing, or
scheduling. Manual hardware uses the same client contract.

The broker retains source observations and receipt times so fresh subscribers
and on-demand queries see the latest accepted hardware. Reads and replay add
elapsed cache time without reducing sample age. Source removal, proof changes,
worker replacement, and endpoint replacement withdraw the affected hardware.
For client display, `info.observedAt` records local receipt time in Unix
milliseconds. Manual hardware retains the time assigned by the owned manual
worker; other observations are stamped by the broker. It identifies the whole
display observation, independently of GPU sample age, so fresh CPU and memory
readings remain visible if GPU collection stalls. That timestamp stays fixed
across cached delivery while `msSince`
increases, so snapshots and live pushes identify the same observation. Owned local
observations are stamped before caching and relay to scanner, preserving their
timestamp through scanner restart replay.
The full `info` payload is omitted from internal scheduling snapshots.

#### `ollama-proxy:<event>`

**Opt-in.** Only delivered to a peer that has called `ollama-proxy:subscribe`; silent otherwise. Once subscribed, every notification `ollama-proxy` emits is re-emitted to the client as `ollama-proxy:<method>` — the proxy's native method name with the `ollama-proxy:` prefix, and `params` passed through verbatim (the proxy's own snake_case schema). The proxy's current notifications are:

| Forwarded as | Proxy emits | Meaning |
|---|---|---|
| `ollama-proxy:ready` | `ready` | The proxy bound its HTTP port (`{version, port}`). Also replayed as the baseline on a fresh subscribe. |
| `ollama-proxy:node/discovered` | `node/discovered` | A manual node was added to the proxy (relay-sourced routing targets don't emit these). |
| `ollama-proxy:node/updated` | `node/updated` | A manual node's fields changed. |
| `ollama-proxy:node/removed` | `node/removed` | A manual node was removed. |
| `ollama-proxy:node/selection-changed` | `node/selection-changed` | The active upstream changed (`{id}`; empty `id` = auto-select). |
| `ollama-proxy:proxy/request-started` | `proxy/request-started` | An HTTP request began being forwarded. |
| `ollama-proxy:proxy/request` | `proxy/request` | An HTTP request completed (status, durations). |

```json
{"jsonrpc":"2.0","method":"ollama-proxy:node/discovered","params":{"id":"MY-GPU-BOX","host":"my-gpu-box.local.","port":11434,"addresses":["192.168.1.10"],"txt":["models=llama3"]}}
```

Two classes of proxy notification are **not** re-emitted under the `ollama-proxy:` namespace: `errors:report` / `errors:clear` are routed into the `nvpair-errors` pipeline and surface on the `errors:update` stream (see [`errors:update`](#errorsupdate)); the proxy's `workload:*` events go to the workload-manager and surface on the separate `workloads:*` stream below.

#### `workloads:upsert` / `workloads:remove`

**Opt-in.** Only delivered to a peer that has called `workloads:subscribe`; silent otherwise. Once subscribed, the broker pushes a `workloads:upsert` whenever a workload is created or its state changes, and a `workloads:remove` when one is retired. The stream is the union of two sources, in the same shape regardless of origin:

- **Local workloads** — the `workload:*` lifecycle events the enabled proxy
  facades emit per inference request, stamped with this host's stable `hostUuid`
  (`originatedFrom`) and translated to `workloads:upsert`. The proxy also fills
  in `scheduledOn` with the destination node's `hostUuid`; the broker passes
  that through unchanged.
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

**Opt-in** (`engine:subscribe`). The notifications `nvpair-engine-manager` emits — `engine:ready`, `engine:state-changed` (full `EngineStatus`), `engine:models-changed` (`{engine, models}`, pushed when an engine's served, loaded-in-memory or downloaded model list changes), `engine:install-progress`, `engine:pull-progress` (live progress for a local model pull), `engine:remote-progress` (live progress for a remote install/pull) — re-emitted verbatim (their method names are already `engine:`-prefixed). Off by default.

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
| `hostUuid` | string | verified host identity for local/pinned rows, or an advertised claim for unverified discoveries |
| `discoveryRecordId` | string | opaque source record identity, independent of an unverified UUID claim |
| `identityVerification` | string | `local`, `pinned`, or `unverified` |
| `name` | string | Currently mirrors `id`; will diverge once a node can advertise a human-readable name. |
| `ipAddress` | string | the node's authoritative `ip=` (else the best-ranked advertised address); empty string when none is usable. |
| `port` | number | the node's `ni` (node-info) port from its `_nvpair-node` TXT, typically `14318`. |
| `lastSeen` | number | **Unix seconds**. Updated on every `discovery:node-discovered` / `discovery:node-updated` event the broker receives from the daemon; not refreshed by `discovery:get-nodes` itself (a pure read). |
| `trusted` | bool | whether the pinned peer's authenticated record still has current approval; an advertised UUID or saved pin alone cannot establish this |
| `clustered` | bool | whether this node belongs to some cluster (it advertises a `cluster-uuid`), independent of whether we're paired with it (`trusted`). A client uses it to suppress a cluster invite that an already-clustered peer would reject. Omitted (false) for standalone/unknown nodes. |
| `models` | string[] | the node's available model names, enriched by the daemon from the node's engine-manager `em` endpoint (`GET /v1/models`). Omitted when the node advertises no engine-manager or no engine is running. |
| `modelsByEngine` | object | the same models attributed to the engine that serves each (keyed by engine name, e.g. `ollama` / `lmstudio`). Additive alongside the flat `models` union. A present engine key with `[]` means its inventory was successfully queried and is empty; a missing key means it was not running/queryable. Omitted when no engine inventory was successfully reported. |
| `loadedByEngine` | object | the models currently **loaded in memory** for each engine (normally a subset of `modelsByEngine`), keyed by engine name. Enriched from the peer's `loadedByEngine`. An engine key with an empty list means "running, nothing loaded"; a missing key means loaded state wasn't reported. Omitted when no engine reports loaded state. |
| `downloadedByEngine` | object | every model an engine can load, for an engine that advertises only the model it is serving (vLLM, whose `modelsByEngine` entry is just that served name). Reported even while the engine is stopped, so a remote model manager keeps its Load and Delete rows; its `loadedByEngine` entry names models the same way. An engine key with an empty list means the inventory was read and is empty; a missing key means it was not read. Omitted when no engine reports one. |
| `info` | object, optional | full `noderec.NodeInfo` hardware snapshot, including `GPUs`, optional `cpu`/`memory`, `telemetryValid`, and `msSince`; unverified previews are sanitized display data |

A node that the scanner reports as removed is deleted from the snapshot; `lastSeen` is not preserved for removed nodes. The response is wrapped in `{nodes: [...]}` (rather than a bare array) so we can grow summary fields later without breaking clients. An empty list is a normal early-startup state, not an error.

The broker stores source records and hardware observations internally. Queries
read those observations without making another network request. Scanner data
takes precedence over a manual source for the same verified host; removing one
source preserves a surviving independent observation.

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

#### `ollama-proxy:get-status`

Reports whether the supervised `ollama-proxy` is up and, if so, the HTTP port it bound. `ready` is `false` and `port` is `0` until the proxy emits its `ready` notification — and stays that way if no proxy is being supervised or the proxy has crashed — so the call is always safe and never errors. Answered locally from the broker's captured state (no round-trip to the proxy).

```json
{"jsonrpc":"2.0","id":8,"method":"ollama-proxy:get-status"}
```

Response (once the proxy is serving):

```json
{"jsonrpc":"2.0","id":8,"result":{"ready":true,"port":11435}}
```

`port` is the proxy's HTTP reverse-proxy listener — point an Ollama client at `http://localhost:<port>` to route inference traffic through it. The proxy comes up asynchronously, so a client typically polls this shortly after `app:ready` until `ready` is `true`.

#### `ollama-proxy:<method>` (generic relay)

Any request whose method starts with `ollama-proxy:` — except the reserved broker-local methods (`ollama-proxy:get-status`, `ollama-proxy:set-port`, and the subscription methods below) — is **forwarded to `ollama-proxy`** with the `ollama-proxy:` prefix stripped, and the proxy's result or error is relayed straight back to the caller unchanged. The `ollama-proxy:` prefix is the only translation: everything after it is the proxy's own native method name, and `params` pass through verbatim. This means the proxy's whole control plane is reachable through the broker without the broker enumerating individual methods. Current proxy methods include `nodes/list`, `node/select`, `node/selected`, `node/add-manual`, and `node/remove-manual` (see the `ollama-proxy` README for their params/results).

For example, to list the Ollama nodes the proxy currently knows and pin traffic to one:

```json
{"jsonrpc":"2.0","id":10,"method":"ollama-proxy:nodes/list"}
{"jsonrpc":"2.0","id":11,"method":"ollama-proxy:node/select","params":{"id":"MY-GPU-BOX"}}
```

The responses are exactly what the proxy returns (its payloads use the proxy's own snake_case `Node` schema — `id` / `host` / `port` / `addresses` / `txt` — not the camelCase `discovery:*` shape; the `ollama-proxy:` namespace is a deliberate exception kept thin to avoid coupling).

Two relay-specific error cases:

- If no proxy is being supervised (or it has exited), the broker replies with error `-32000` `"ollama-proxy not available"`.
- `ollama-proxy:shutdown` is **refused** with error `-32601` — the broker owns the proxy's lifecycle, so a client can't terminate it independently. Shut the broker down instead (which tears the proxy down with it).

#### `ollama-proxy:set-port` / `lmstudio-proxy:set-port` / `llamacpp-proxy:set-port` / `vllm-proxy:set-port`

**Intercepted, not relayed verbatim.** For every engine profile, a port-only caller (none in tree; `nvpair-tui` uses `engine:apply-settings`) gets to move a single port without rendering the whole launch settings form, but the change still runs through the same authoritative settings operation the desktop editor uses, so a port set from the terminal cannot diverge from one set from the UI. The namespace selects the engine. The broker reads that engine's current settings, substitutes the requested proxy port, and applies the result while preserving the server port and launch arguments. Accepted settings are saved in the settings journal.

A malformed request, missing port, or port outside `1`–`65535` is refused with error `-32602 "port must be between 1 and 65535"`. A **requested port that is already in use or reserved** by a PAIR service, another configured engine/proxy, or the engine's own server port is refused with error `-32000 "resolve settings errors and port conflicts before applying"`. The broker does not pick a different port on the caller's behalf. Retry with a free port. A request that collides with an inherited `OLLAMA_HOST` alias is refused with its own message naming that alias. The response echoes the requested port (`{"port": <requested>}`) once it is bound.

```json
{"jsonrpc":"2.0","id":9,"method":"ollama-proxy:set-port","params":{"port":11500}}
{"jsonrpc":"2.0","id":10,"method":"llamacpp-proxy:set-port","params":{"port":8082}}
```

Automatic conflict resolution still exists, but only for a port the user did not just choose: when the Ollama proxy announces a (re)bound port on startup and a running engine has since taken it, the broker steers the proxy to a free port and surfaces a sticky `warning` into the errors pipeline (id `ollama-proxy:port-bumped`, `action:"none"`) explaining the move. That path **never changes an engine's port** — only the proxy is moved. Port setters also return `-32000` if the settings operation cannot run, including when the engine manager or proxy is unavailable.

#### `lmstudio-proxy:get-status` / `lmstudio-proxy:subscribe` / `lmstudio-proxy:unsubscribe` / `lmstudio-proxy:<method>` (generic relay)

The LM Studio counterpart of the `ollama-proxy:*` surface uses the LM Studio facade in the supervised `nvpair-proxy` process on compatibility port `:1234` and tracks the managed LM Studio engine on `:1235`. With managed port ownership enabled (the default), the broker identifies and moves an existing LM Studio server through engine-manager before allowing the proxy to claim `1234`; unknown owners are left untouched and force a warned proxy fallback. Disabling managed ownership preserves explicit custom engine and proxy ports. `lmstudio-proxy:get-status` reports the actual bound port; `lmstudio-proxy:subscribe` / `lmstudio-proxy:unsubscribe` opt into / out of its `lmstudio-proxy:<event>` stream; `lmstudio-proxy:set-port` uses the settings operation described above; and other `lmstudio-proxy:<method>` requests are relayed with the prefix translated (`nodes/list`, `node/select`, `node/add-manual`, `node/remove-manual`, ...). `lmstudio-proxy:shutdown` is refused because the broker owns lifecycle ordering. Workload and error events feed the shared streams exactly as Ollama's do.

The same broker-local status, subscription, and port-setting methods apply to `llamacpp-proxy:*` and `vllm-proxy:*`. llama.cpp port changes use the same validation, journal, and application path; remaining methods use the generic facade relay, with `llamacpp-proxy:shutdown` refused.

#### `ollama-proxy:subscribe`

Opts the peer into the `ollama-proxy:<event>` stream (off by default). On a fresh subscription the broker immediately replays the proxy's last `ready` payload as a baseline `ollama-proxy:ready` (if the proxy has come up), so a subscriber learns the port without a separate `ollama-proxy:get-status`.

```json
{"jsonrpc":"2.0","id":12,"method":"ollama-proxy:subscribe"}
```

Response:

```json
{"jsonrpc":"2.0","id":12,"result":{"subscribed":true}}
```

Idempotent: re-subscribing returns `{"subscribed":true}` and does not re-send a baseline. This is a reserved broker-local method — it is not forwarded to the proxy.

#### `ollama-proxy:unsubscribe`

Stops the `ollama-proxy:<event>` stream for this peer. Reserved broker-local; not forwarded. Idempotent.

```json
{"jsonrpc":"2.0","id":13,"method":"ollama-proxy:unsubscribe"}
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

Any other `engine:*` request is forwarded to `nvpair-engine-manager` verbatim and its response relayed straight back. This covers the whole engine control plane: `engine:get-installed`, `engine:describe`, `engine:status`, `engine:install`, `engine:uninstall`, `engine:start`, `engine:stop`, `engine:restart`, `engine:action`, `engine:logs`, `engine:errors`, `engine:models`. Lifecycle ops run for minutes (reporting progress via the `engine:install-progress` / `engine:state-changed` push events), so the relay imposes **no broker-side timeout** — fire the request and watch the event stream for the outcome. Error `-32000 "engine-manager not available"` when no engine-manager is supervised.

`engine:set-port` is **not** in that generic set. Like `<engine>-proxy:set-port` it is intercepted and run through the authoritative settings operation, so moving an engine's server port from a port-only caller validates and restarts exactly as the full editor does, and persists as a manifest override that survives a restart. Its response is the engine's `engine:status` result.

#### `settings/<method>` (generic relay)

Any `settings/*` request is forwarded to `nvpair-node-settings` and its response relayed back: `settings/get-force-ports`, `settings/set-force-ports`, `settings/get-cluster-id`, `settings/set-cluster-id`, `settings/get-cluster-auto-sync`, `settings/set-cluster-auto-sync`, `settings/get-cluster-friendly-name`, `settings/set-cluster-friendly-name`. Error `-32000 "node-settings not available"` when no settings worker is supervised.

#### `node/add` / `node/remove` / `nodes/list` (manual nodes)

Relayed to `nvpair-manual-nodes`. `node/add` (`{ address, name?, tls_port?, mtls? }`) registers a user-added node and probes it; `node/remove` drops it; `nodes/list` returns the tracked manual nodes. For removal, supply exactly one of `{ id }` (the worker's exact entry ID, as used by the TUI) or `{ nodeId }` (the broker's node key, as used by the desktop). The broker resolves `nodeId` to an entry ID before forwarding `{ id }` to the worker. An unknown node key returns `{ removed: false }` without contacting the worker or changing saved entries. Missing or conflicting selectors return error `-32602`.

Manually added nodes also surface in the shared `discovery:get-nodes` / `discovery:nodes-changed` snapshot — the broker merges `nvpair-manual-nodes`' `node/discovered|updated|removed` into the same store the scanner feeds. The broker persists successful `node/add` operations to `configs/manual-nodes.json` in the per-user data directory and replays them into a replacement `nvpair-manual-nodes` worker after startup. Successful `node/remove` operations update the same durable list. Error `-32000 "manual-nodes not available"` when no manual-nodes worker is supervised.

**Manual → proxy bridge.** When the broker supervises both `nvpair-manual-nodes` and a proxy, it bridges a manual node whose engine is reachable into that proxy via `node/add-manual` (host/port from the node's per-engine status). This is per-engine: a node whose `ollama_*` status is up is bridged into `ollama-proxy` (host/port from `ollama_port`), and one whose `lmstudio_*` status is up into `lmstudio-proxy` (from `lmstudio_port`) — a node running both is bridged into both. Manual configuration provides an engine route independently of mDNS discovery. The bridge tracks reachability: an engine that goes down (or a node that is removed, or whose prober crashes) is pulled back out with `node/remove-manual`. Manual nodes appear in discovery snapshots independently of proxy supervision; bridge updates apply to supervised proxies.

Each engine has one ordered bridge sender outside the discovery lock. It keeps the latest desired target per key, coalescing intermediate status updates while the proxy is slow. Withdrawals follow any in-flight add, and a later status update retries a failed add or removal. A replacement proxy receives the current targets after its facades are enabled; a canceled configuration is not replayed. Manual bridge calls therefore do not hold the discovery lock while waiting for a proxy.

Proxy discovery subscriptions likewise queue their latest complete snapshot per engine. One sender per proxy process writes these notifications outside the discovery and relay locks, preserving empty withdrawals and replacing pending snapshots from retired subscriptions. A blocked proxy therefore cannot hold those locks through either its manual bridge or its discovery subscription.

#### `cluster:<method>` / `nodes:<method>` (generic relay)

Any request whose method starts with `cluster:` or `nodes:` is **forwarded to `nvpair-cluster-manager`** verbatim (no prefix stripping — these are the manager's own native namespaces), and the manager's result or JSON-RPC error is relayed straight back to the caller unchanged. As with the proxy relay this means the cluster-manager's whole surface is reachable through the broker without enumerating individual methods. Current methods include `cluster:get-node-id`, `cluster:set-identity`, `cluster:create`, `cluster:invite-node`, `cluster:respond-to-invite`, `cluster:cancel-invite`, `cluster:invite-status`, `nodes:get-initial`, and `nodes:remove` (see the [`nvpair-cluster-manager` README](../nvpair-cluster-manager/README.md) for their params/results and app-defined error codes).

The cluster-manager also pushes notifications (`cluster:invite-received`, `cluster:invite-canceled`, `cluster:identity-changed`, `nodes:changed`, ...); the broker forwards them to the caller verbatim. Unlike `ollama-proxy:<event>` / `workloads:*` these are **not opt-in** — there is no `cluster:subscribe`; the events always flow.

If no cluster-manager is being supervised (or it has exited), the broker replies with error `-32000` `"nvpair-cluster-manager not available"`. Because pairing calls drive multi-round-trip inter-node network exchanges, the relay's per-call timeout is 30 s (vs. the proxy's 5 s).

#### `shutdown`

Acknowledges, then terminates the broker. Ingress is closed at the proxy and engine-manager is asked to stop its engines first (`engine:prepare-shutdown`), then every remaining worker subprocess is torn down by closing its stdin. Those joins run concurrently and are bounded by a shared teardown budget; see the shutdown bullet under [Worker subprocesses](#worker-subprocesses).

The broker's teardown sends `engine:prepare-shutdown`, and closing engine-manager's stdin afterwards reaches the same engine sweep. Engine-manager sweeps its engines once; later callers wait for that sweep rather than starting another, and retry only an engine whose stop failed through a process handle or a stop command.

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

The broker needs `nvpair-node-scanner` reachable as a sibling file in its working directory (or via `--scanner-path`); it also looks for `nvpair-node-info` (or via `--node-info-path`), `nvpair-proxy` (or via `--proxy-path`), `nvpair-workload-manager` (or via `--workload-manager-path`), and `nvpair-cluster-manager` (or via `--cluster-manager-path`) the same way (plus errors / engine-manager / manual-nodes / settings), though all but the scanner are optional. The simplest local layout is to put all the built `.exe`s next to each other — which is exactly how the installer lays them out.

Quick smoke test in stdio mode (PowerShell, run from a directory that has the worker binaries — at minimum `nvpair-node-scanner`, plus `nvpair-node-info` if you want the local inventory server, `nvpair-proxy` if you want the local engine proxies, `nvpair-workload-manager` if you want the cluster workload relay, and `nvpair-cluster-manager` if you want cluster pairing):

```powershell
'{"jsonrpc":"2.0","id":1,"method":"discovery:get-nodes"}' | .\nvpair-ui-broker.exe
```

You should see an `app:ready` notification followed by a `discovery:get-nodes` response. The `nodes` array will likely be empty on the first request — mDNS browsing needs a few seconds to populate the store. Issue the call again after a short wait to see real entries (assuming an `_nvpair-node._tcp` record is live on the LAN). To watch live updates, subscribe to the push stream first — every store change then pushes a `discovery:nodes-changed` notification:

```powershell
'{"jsonrpc":"2.0","id":1,"method":"discovery:subscribe"}' | .\nvpair-ui-broker.exe
```

Pass explicit worker paths:

```powershell
.\nvpair-ui-broker.exe --scanner-path C:\path\to\nvpair-node-scanner.exe --node-info-path C:\path\to\nvpair-node-info.exe --proxy-path C:\path\to\nvpair-proxy.exe --workload-manager-path C:\path\to\nvpair-workload-manager.exe --errors-path C:\path\to\nvpair-errors.exe --engine-manager-path C:\path\to\nvpair-engine-manager.exe --manual-nodes-path C:\path\to\nvpair-manual-nodes.exe --settings-path C:\path\to\nvpair-node-settings.exe --cluster-manager-path C:\path\to\nvpair-cluster-manager.exe
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

## Integration and configuration boundaries

- **Engine-advertise control surface.** Engine registration is auto-driven only:
  the broker tracks local Ollama, LM Studio, llama.cpp, and vLLM and registers `ol`,
  `lm`, `lc`, or `vl` with the daemon while each is up. There's no manual-advertise
  RPC (custom service, port, name, or TXT), and no way to advertise anything
  other than the detected engines.
- **Node-info integration.** The broker passes node-info its cluster directory
  and host UUID, relays hardware snapshots from stdout, and sends registry
  metadata and log-level updates over stdin. Membership comes from the live trust
  store. Clients read hardware through discovery queries and subscriptions. The
  broker registers the fixed `ni` port with the scanner and supplies the shared
  cluster trust configuration to node-info.
- **Manual-node persistence across restarts.** `nvpair-manual-nodes` keeps live state in memory; the broker owns `configs/manual-nodes.json`, clears stale live projections when the worker exits, and replays the durable entries into its replacement.
- **Per-event push semantics.** `discovery:nodes-changed` always carries the full current snapshot, not a delta. For small N this is fine and lets the client treat the payload as authoritative without state reconciliation. `errors:update` is likewise a full snapshot.
- **Hardware provenance.** Scanner reads verified remote inventory over exact-pin
  mTLS and local inventory over private worker IPC. The
  [scanner spec](../nvpair-node-scanner/spec.md) defines the ownership and
  authentication requirements for hardware used by services.
- **Per-worker log-level granularity.** `log/set-level` fans the broker's single level out to every running child, but there's no way to set a different level per worker — it's one level for the whole process tree.
- **AuthN / AuthZ.** Transport security relies on the parent's pipe / socket ACL. There is no per-message token.
- **Client transport.** The broker speaks newline-delimited JSON-RPC over stdio, a Unix domain socket, or a Windows named pipe.
- **Automatic workload baseline on subscribe.** `workloads:subscribe` starts only the live stream; clients explicitly request `workloads:get-initial` after subscribing and merge overlapping records. The broker persists bounded terminal history, while active state is rebuilt from live workload reconciliation rather than treated as durable across a full process-tree restart.
