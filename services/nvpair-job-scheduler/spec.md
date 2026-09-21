<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Job Scheduler (`nvpair-job-scheduler`)

> **Status: implemented.** House spec style (`nvpair-cluster-manager`,
> `nvpair-workload-manager`, `nvpair-engine-manager`). Current policy decisions and
> residual risks are recorded in §4.

## 1. Purpose
Decides **where inference should go**. On every accepted workload, discovery, or
effective GPU-pressure change, with a periodic timer as reconciliation, the Job
Scheduler ranks the cluster's nodes by total pending work plus GPU pressure. It
emits that node-wide order, pending count, and pressure so the Broker can push the
snapshot to the matching local `nvpair-proxy` instance. Before
forwarding, a proxy atomically adds its own not-yet-observed reservations and
chooses the least-busy capable node; its existing failover walks the remaining
candidates.

It is a **policy component**, not a data plane: it never sees inference traffic,
never talks to the proxies directly, and owns no durable state. The initial policy
uses a deliberately coarse, smoothed GPU signal; richer policies such as VRAM
capacity, model locality, latency, and affinity can grow later behind the same
"emit an ordered list" contract.

## 2. Scope
**In scope**
- Compute one ordered priority list from the discovered-node set (ranking universe),
  cluster workload picture, and fresh maximum-GPU utilization.
- Recompute immediately after a meaningful workload, discovery, or pressure
  update, and periodically as a reconciliation and telemetry-aging fallback.
- **Emit** the order and ranks to the Broker (`schedule:priority`) for each
  engine-specific proxy contract when that engine snapshot changes.

**Out of scope**
- **Serving/forwarding inference traffic** — the proxies' job; the scheduler only
  says which order to prefer.
- **Talking to the proxies / tracking their liveness** — the Broker (which already
  supervises them) fans lists out, no-ops for an absent proxy, and re-pushes on
  restart (§7.4). The scheduler holds no proxy-availability state.
- **Owning the workload catalog** — produced by the proxies, relayed by
  `nvpair-workload-manager`; the scheduler is a read-only consumer (§4).
- **mDNS / a network listener** — like `nvpair-engine-manager`, stdio/`--ipc` only, no
  port. It *reads* the Broker's discovery snapshot but does no browsing/advertising.
- **Cross-node coordination** — each node's scheduler ranks the cluster from its own
  view; no election or shared schedule (§4 risk).

## 3. Key Use Cases
- **Immediate rebalance**: after each accepted workload transition, count all
  pending (queued+running) workloads per node, sort ascending, and emit if either
  the order or counts changed. Requests arriving after the feedback reaches a
  proxy start from that authoritative snapshot.
- **Simultaneous local burst**: before each auto-routed inference forward, the
  proxy atomically increments a local reservation for its chosen node. Concurrent
  requests therefore see one another even before workload feedback completes.
- **Cross-engine contention**: an Ollama workload and an LM Studio workload both
  consume the destination node's execution resources, so either one changes both
  engine outputs.
- **Periodic reconciliation**: the runtime-adjustable timer recomputes the same
  order as a safety net even when no fresh event arrives.
- **Load shifts**: as a node drains it rises toward the top; as it fills it sinks; a
  newly discovered node starts at count 0 and sorts to the top.
- **No workload data yet**: all nodes rank equal → the stable tie-break (§7.2) gives
  a deterministic list, not thrash.
- **GPU pressure changes**: smooth fresh utilization into coarse pressure bands;
  invalid, missing, or stale telemetry receives a neutral penalty rather than
  looking idle.
- **Proxy down / manual pin**: both handled downstream — the Broker drops/replays
  the push for an absent proxy (§7.4); a user `node/select` pin outranks the list
  until cleared (§7.3). Either way the scheduler keeps emitting.

## 4. Decisions / Risks
- **Workload source (decided — broker fan plus restart baseline).** The Broker fans
  every accepted `workloads:upsert`/`workloads:remove` to the scheduler, which keeps
  an active-only in-memory catalog keyed by
  `(originatedFrom, engine, runId, id)` (the same global identity the broker's store
  and workload-manager use — the bare `(originatedFrom, id)` collides across
  concurrent engines and proxy restarts). On scheduler spawn/restart the Broker
  replays its authoritative active snapshot as ordinary upserts before the
  discovery baseline, then resumes live fanout. No workload-manager change or new
  wire method is required.
- **Node universe (decided — the Broker's discovery stream).** Since the scheduler
  no longer asks the proxies for node lists (§7.4), it ranks the nodes from the
  `discovery:nodes-changed` stream the Broker fans to it (with a full snapshot pushed
  on spawn as the baseline). This includes **idle nodes** (zero workloads) so
  they rank at the top. Each proxy applies the subset it can route to and ignores
  the rest (§7.1); a proxy-only manual node absent from discovery is handled as an
  unlisted fallback (§7.1).
- **GPU pressure (decided — EWMA, bands, freshness).** The broker feeds the maximum
  utilization across a node's GPUs. The scheduler uses a 0.35 EWMA, pressure bands
  0/1/2/3 at 40%/70%/85%, downward hysteresis at 35%/65%/80%, and a 10-second
  freshness limit. Invalid, missing, and stale telemetry contributes neutral
  pressure 1. A fresh stream after a gap starts a new EWMA.
- **Cold-start / tie-break (decided — pressure then stable identity).** Rank by
  `pending + gpuPressure`, then lower pressure, then `hostUuid`. A fixed final key
  makes equal-load nodes deterministic; nodes with no telemetry all receive the
  same neutral pressure and collapse to UUID order.
- **Manual pin precedence (decided — manual pin wins).** A user `node/select` pin
  overrides the list; the list governs only auto routing (§7.3).
- **`node/set-priority` proxy semantics (decided — GPU-aware reservations).**
  Both proxies store the ordered nodes and optional ranks. For auto-routed
  model-bearing inference they atomically minimize
  `rank.pending + rank.gpuPressure + localReservations` within the best
  model-eligibility tier,
  using scheduler order as the tie-break. They retain known unlisted nodes as
  fallbacks and treat an empty list as default auto order. A nodes-only payload
  remains valid and supplies a zero baseline for backward compatibility.
- **Reconciliation interval (decided — 1 s default, runtime get/set).** Default **1 s**
  (`--interval`), read/changed live via `scheduler:get-interval` /
  `scheduler:set-interval`, clamped to a **200 ms** floor (§7.5).
  Workload and discovery events do not wait for this timer; emit-only-on-snapshot-
  change plus the stable tie-break keep timer recomputes from churning.
- **Pending-count (decided — node-wide).** *Pending* = `queued` or `running`,
  regardless of engine. The test is an allow-list, not a terminal-state
  exclusion list, so `completed`, `failed`, `cancelled`, and any state added
  later count as zero load without needing a change here. Counted by **`scheduledOn`** (where work runs), not `originatedFrom`. A
  workload with no `scheduledOn` is unplaced and counts toward no node.
- **Identity alignment (decided).** Discovery `hostUuid`, proxy candidate ids, and
  workload `scheduledOn` use the same stable UUID namespace. A malformed or legacy
  `scheduledOn` matching no discovered node contributes to no rank, so mismatch
  degrades ranking quality but never misroutes.
- **Risk — divergent views / thundering herd.** Each node ranks from its own
  eventually-consistent view, so two may briefly steer work to the same idle node.
  Acceptable for this feedback policy; local `workload:started` events trigger an
  immediate rerank, while peer views converge through workload-manager relay.
- **Reservation reconciliation (decided — snapshot reset).** Each delivered
  snapshot replaces the proxy's baseline and clears its optimistic deltas; the
  scheduler snapshot is authoritative. Reservations are taken under one short
  mutex before forwarding, so requests within one proxy cannot select from the
  same stale local state. A reservation that fails over is harmless: it pushes the
  failed first choice down locally, while the re-pointed workload event corrects
  the next authoritative snapshot.
- **Risk — stale/lagging remote views.** Peer ranking trails reality by relay
  latency; workload-manager anti-entropy and periodic scheduler reconciliation make
  it self-correcting.

## 5. Requirements

**Functional**
- Accept `scheduler:get-status`, `scheduler:get-interval`, `scheduler:set-interval`,
  `scheduler:tick`, `log/set-level`, and `shutdown` over `stdin`/`--ipc` and return
  JSON-RPC results (§7.0).
- Keep three live in-memory views from Broker streams: the workload catalog
  (`workloads:*`), GPU telemetry (`scheduler:telemetry`), and the node universe
  (`discovery:nodes-changed`).
- After each meaningful input change, and on a fixed reconciliation interval
  (default 1 s, runtime-adjustable), compute one node-wide GPU-aware order (§7.2)
  and publish it through both engine outputs. **Emit `schedule:priority` only when
  its order, pending counts, or pressure changed** (a forced `scheduler:tick`
  re-emits regardless).
- Emit `errors:report` / `errors:clear` for the Broker to forward to `nvpair-errors`
  on an internal fault worth surfacing (§13).

**Non-functional**
- No network listener, no mDNS, no port — stdio/`--ipc` only.
- Never addresses a proxy; all delivery/liveness/resync is the Broker's (§7.4).
- Stateless across restarts; recomputed from live inputs.
- A fault on one engine must not block the other or crash the loop.
- Windows / macOS / Linux × amd64 / arm64. Serialize `stdout` so frames never
  interleave.

## 6. Inputs and Outputs

**Inputs** (Broker → scheduler over `stdin`/`--ipc`): JSON-RPC requests (§7.0) and
three fanned notification streams — `workloads:*` (workload picture),
`scheduler:telemetry` (GPU utilization), and `discovery:nodes-changed` (node
universe), self-accumulated into in-memory views.

It reads only `id`, `engine`, `runId`, `state`, `originatedFrom`, `scheduledOn`
from the `Workload` object ([WM spec §6](../nvpair-workload-manager/spec.md)), and
only `hostUuid` from each discovered node
([broker `discovery:get-nodes`](../nvpair-ui-broker/README.md)). Telemetry carries
`hostUuid`, maximum `gpuUtilizationPercent`, `telemetryValid`, and `msSince`.

`NodeRank` (internal, surfaced in `scheduler:get-status`):
```json
{
  id: string          // stable node hostUuid
  pending: number     // queued+running workloads scheduledOn this node, all engines
  gpuPressure: number // coarse 0–3 pressure; unknown/stale is neutral 1
  rank: number        // 0-based position (0 = highest priority)
}
```

**Outputs** (scheduler → Broker): JSON-RPC results; a `schedule:priority`
notification per engine when its rank snapshot changes; `errors:report` /
`errors:clear`. No inference traffic and no requests addressed to other workers.

```json
{"jsonrpc":"2.0","method":"schedule:priority","params":{"engine":"ollama","nodes":["MY-PC","LAB-DESK-B","GPU-RIG"],"ranks":[{"id":"MY-PC","pending":0,"gpuPressure":0,"rank":0},{"id":"LAB-DESK-B","pending":1,"gpuPressure":1,"rank":1},{"id":"GPU-RIG","pending":3,"gpuPressure":3,"rank":2}]}}
```

## 7. API / Interface Contract

A single local interface toward the Broker (no inter-node interface). The Broker
sends requests and fans three streams; the scheduler answers requests and emits
`schedule:priority` notifications the Broker acts on. It never sends the Broker a
*request*. Newline-delimited JSON-RPC 2.0 over stdio (default) or `--ipc`.

### 7.0 Methods and notifications

Requests (Broker → scheduler):

| Method | Params | Result |
|--------|--------|--------|
| `scheduler:get-status` | — | `{ interval_ms, engines: { ollama: EngineSchedule, lmstudio: EngineSchedule } }` |
| `scheduler:get-interval` | — | `{ interval_ms }` |
| `scheduler:set-interval` | `{ interval_ms }` | `{ interval_ms }` — live; clamped to the floor (§7.5); effective next tick |
| `scheduler:tick` | — | `{ ticked: true }` — force an immediate recompute + re-emit (tests/debug) |
| `log/set-level` | `{ level }` | `{ level }` |
| `shutdown` | — | `null` |

`EngineSchedule`:
```json
{
  engine: string          // "ollama" | "lmstudio"
  emitted: [NodeRank]      // last order emitted (empty if none yet)
  lastEmittedAt: number    // epoch ms; 0 if never
}
```

Notifications:

| Method | Direction | Params | Meaning |
|--------|-----------|--------|---------|
| `schedule:priority` | scheduler → Broker | `{ engine, nodes: [string], ranks?: [NodeRank] }` | New GPU-aware priority snapshot; Broker delivers it to the matching proxy. Emitted only on snapshot change (or forced tick). |
| `errors:report` / `errors:clear` | scheduler → Broker | `ServiceError` / `{ id }` | Errors pipeline (§13). |
| `workloads:upsert` / `workloads:remove` | Broker → scheduler | (WM shapes) | Workload stream (§6). |
| `scheduler:telemetry` | Broker → scheduler | `{ hostUuid, gpuUtilizationPercent, telemetryValid, msSince }` | Compact maximum-GPU telemetry stream (§6). |
| `discovery:nodes-changed` | Broker → scheduler | `[AvailableNode]` | Node-universe stream (§6). |

### 7.1 The `node/set-priority` proxy contract (delivered by the Broker)

The new proxy method the *Broker* calls, carrying the scheduler's order (defined
proxy-side, §4). The scheduler never calls it — it only emits `schedule:priority`.

- **Params**: `{ generation: uint64, nodes: [string], ranks?: [NodeRank] }` —
  a monotonic delivery generation, ordered node ids, and their authoritative
  pending counts and GPU pressure. The Broker mints `generation` (always ≥ 1);
  the scheduler does not send it. **Required**: omitted or `0` is rejected with
  `-32602`, because a snapshot that cleared the proxy's reservations without
  advancing its applied epoch would let one taken before it release one taken
  after — the undercount this field exists to prevent.
- **Semantics** (proxy): for each auto-routed model-bearing inference request,
  atomically choose the minimum authoritative pending count plus GPU pressure
  plus local reservations, then reserve that node before forwarding.
  Consider only scheduler-listed nodes in the best available model tier; use
  scheduler order as the tie-break. **Unknown ids are ignored.** A **known node
  not in the list** remains a lowest-priority fallback, and an **empty list**
  reverts to the proxy's default auto heuristic.
- **Semantics** (proxy, generations): a snapshot is applied **at most once**. A
  `generation` less than or equal to the newest applied one is ignored whole.
  Applying a snapshot supersedes the reservations taken against earlier ones, so
  a redelivery must not clear reservations a second time — and a reservation
  released after a newer snapshot arrived is dropped rather than decremented,
  since that snapshot's pending counts already account for the completed work.
  Without both rules a node's estimated load can fall below zero, which reads as
  permanently idle and attracts every subsequent dispatch.
- **Semantics** (proxy, reservations): reservations are **process-wide**, shared
  by every engine facade in the proxy process — two facades bursting at once
  compete for the same node's GPU. A reservation is released when its request
  ends and moved to the node a failover actually lands on.
- **Result**: the reply shape is defined proxy-side, not here.
- **Precedence**: a user `node/select` pin overrides the list (§7.3).

### 7.2 Ranking algorithm

Let `D` = current discovered-node `hostUuid`s:

1. **Count**: from the active workload catalog, keep workloads with
   `state ∈ {queued, running}` and `scheduledOn ∈ D`, regardless of engine; group
   by `scheduledOn` → `pending[id]`. Every `id ∈ D` not present gets `0` (idle
   nodes included).
2. **Pressure**: for each valid sample with effective age
   `msSince + elapsedSinceReceipt ≤ 10 s`, clamp utilization to 0–100 and update
   an EWMA with alpha 0.35. Map it to pressure 0 below 40%, 1 below 70%, 2 below
   85%, else 3. A band drops only below 35%/65%/80%. Invalid, missing, or stale
   telemetry yields pressure 1; the first fresh sample after a gap resets the
   EWMA. The input is the maximum across GPUs, deliberately conservative.
3. **Sort** `D` ascending by `pending[id] + gpuPressure[id]`, then
   `gpuPressure[id]`, then `id`.
4. **Publish** the shared snapshot through
   `schedule:priority {engine, nodes, ranks}` for each supported engine. Emit only
   when its order, pending counts, or pressure differ from the last emission (a
   forced tick re-emits). Delivery, liveness, and restart-resync are the Broker's
   (§7.4).

`scheduledOn` (not `originatedFrom`) is the key: we care where work runs, not where
it came from. Engine remains part of workload identity and selects the downstream
proxy output, but it does not partition the load metric: Ollama and LM Studio
normally contend for the same node-level GPU, VRAM, CPU, and memory. Until resource
affinity is observable, total node queue depth is the conservative signal.

### 7.3 Selection-state (proxy side, for reference)

Effective target: `manual pin (node/select)` → else
`pending + gpuPressure + local reservations within the priority list` → else
`default auto`. A snapshot delivered while a pin is active is stored but doesn't
move traffic; clearing the pin activates the most-recent snapshot.
`node/selection-changed` fires only for the active target.

### 7.4 Emit-and-fan-out

The scheduler emits `schedule:priority` **notifications**; the Broker owns
everything downstream. This mirrors an existing pattern — the Broker already
consumes the proxy's `workload:*` and routes it to the workload-manager
(`routeProxyWorkload`); here it consumes `schedule:priority` and routes it to the
proxies. **Not** a new "originated request" direction. Broker responsibilities
(implemented in `nvpair-ui-broker`, §15):
- **Fan-out**: on `schedule:priority`, mint a generation and call
  `node/set-priority` once per **distinct live proxy handle** — every engine
  resolves to the same process, so a per-engine call would deliver the same
  snapshot twice. An absent proxy → logged no-op.
- **Ingestion dedupe**: a snapshot whose ranking matches the cached one is
  coalesced without minting a generation, so a redundant emission does not
  cause a redundant delivery.
- **Cache + restart-resync**: keep the last complete snapshot **process-wide**
  (the ranking is node-global and not engine-filtered — see §7.2) and re-push it
  when the proxy (re)spawns, from the supervisor's spawn hook rather than from
  `ready`, so the replay cannot race the handle's publication.
- **Feed and seed the streams**: on scheduler spawn, replay active workloads,
  replay cached telemetry, then send discovery so the first non-empty ranking has
  all baselines. Resume live workload, telemetry, and discovery fanout afterward.

### 7.5 Local-interface errors

Standard JSON-RPC 2.0. Negative outcomes are normal `result`s; protocol problems
use `error`:

| Code | Raised when |
|------|-------------|
| `-32700` / `-32600` / `-32601` | unparseable / non-JSON-RPC / unknown method |
| `-32602` | non-positive/non-numeric `interval_ms`, or unknown `log/set-level` level. Below the 200 ms floor is clamped and returned, not an error. |
| `-32603` | unexpected internal failure |

The scheduler issues no requests upward, so there are no relay-error paths on its
side.

### 7.6 Versioning
- `scheduler:*` / `schedule:*` / `node/set-priority` names are namespaced and
  additive — new capabilities are new methods.
- The added `ranks` field is optional: older consumers continue using
  `{engine,nodes}`, while current proxies use pending counts and GPU pressure for
  local reservations.

## 8. Dependencies
- **Upstream**: the supervising parent — `nvpair-ui-broker` in practice
  (supervisor-agnostic: any parent that speaks §7.0, feeds the three streams, and
  delivers `schedule:priority` to the proxies).
- **Downstream (via the Broker)**: each `nvpair-proxy` instance (receives
  `node/set-priority`); `nvpair-errors` (receives `errors:*`). No direct link to any.
- **Data sources (via the Broker)**: `nvpair-workload-manager` + proxies
  (workloads), `nvpair-node-scanner` / manual nodes (discovery and GPU telemetry).
- **External**: `nvpair-shared/applog`, `nvpair-shared/jsonrpc`, `nvpair-shared/errors`. No
  third-party services, no network stack.

## 9. Data Ownership
- **Owned**: transient in-memory state only — the accumulated workload catalog,
  discovered-node view, smoothed telemetry state, and last-emitted rank snapshot
  per engine. The proxy process owns only its short-lived optimistic reservation
  deltas, shared across the engine facades it hosts.
- **Source of truth**: no. Workloads → proxies/WM; discovery and telemetry →
  scanner/manual-nodes; the active routing decision → the proxy; delivery/caching
  → the Broker. The scheduler owns only the policy computation.
- **Storage**: none.

## 10. Design Constraints
- **Performance**: each accepted state change and periodic reconciliation is an
  O(#nodes + #active workloads) count-and-sort plus at most two small
  notifications. Terminal workloads are removed from the scheduler catalog.
- **Scalability**: ~dozen nodes, low workload volume; small state.
- **Reliability**: best-effort, self-correcting; event-driven updates minimize local
  lag and the periodic pass reconciles the current view. Serialized `stdout`.
  Proxy delivery reliability is the Broker's.
- **Security**: no listener, no inter-node traffic. Payloads carry node ids,
  workload metadata, and aggregate utilization only (no inference data, no PII);
  ids are opaque.

## 11. Assumptions
- The parent owns the pipe; on `stdin` EOF the scheduler exits cleanly (no
  reconnect/buffering).
- The parent feeds the three streams and delivers `schedule:priority` to the proxies;
  without them the scheduler runs but has nothing to rank / nowhere to land.
- Discovery, proxies, and `scheduledOn` use the same stable `hostUuid` namespace.
- Small cluster and modest control-plane event volume; immediate O(nodes + active
  workloads) recomputation is inexpensive.

## 12. Failure Modes and Mitigations
- **Cold start / stale views**: → The Broker replays active workloads and cached
  telemetry before the discovery baseline, then live deltas. Missing or stale
  telemetry receives neutral pressure and stable UUID ordering.
- **Proxy down / restarted**: not the scheduler's problem. → The Broker no-ops the
  push and re-pushes the cached snapshot on return (§7.4).
- **Rank thrash**: → EWMA, coarse bands, downward hysteresis, deterministic
  tie-break, and emit only on pressure-relevant snapshot changes.
- **Thundering herd** (§4): → local `workload:started` updates rerank immediately;
  the periodic pass and peer relay reconcile eventual views.
- **Interface severed** (`stdin` EOF / `stdout` `EPIPE`): → treat as shutdown; stop
  the timer and exit. A new Broker spawns a fresh scheduler.
- **Malformed frame**: → validate every envelope; JSON-RPC `error` for anything with
  an `id`, drop-and-log uncorrelatable lines; never crash the loop.

## 13. Observability
- **Logging**: per-recompute outcome (trigger, node count, top ids + counts, emitted
  vs. unchanged); view sizes; interval changes; startup/clean shutdown. Order
  *changes* log at `info`.
- **Status** (via `scheduler:get-status`): current interval and each engine's last
  emitted ranks and emission time.
- **Alerts** (errors pipeline): a workload, telemetry, or discovery stream gone
  silent far longer than expected. Proxy-reachability alerting belongs to the
  Broker.

## 14. Sample usage
The Broker spawns `nvpair-job-scheduler`, replays active workloads and telemetry,
then sends discovery and resumes all three live streams. Discovery seeds
`GPU-RIG`, `MY-PC`, `LAB-DESK-B`. Pending counts are `3`, `0`, `1` and GPU
pressures are `3`, `0`, `1`, so combined loads are `6`, `0`, `2`. The scheduler
emits `["MY-PC","LAB-DESK-B","GPU-RIG"]` for both engines. The ranking is
node-global, so the Broker coalesces the duplicate and delivers it once per
distinct proxy process via `node/set-priority`; each facade applies the subset it
can route to.

As load and smoothed pressure change, only a new pressure band, pending count, or
order triggers another snapshot. If the Ollama proxy is not supervised, the Broker
drops the push and re-delivers once it is back — the scheduler is unaware.

## 15. Process model, CLI, and build wiring

**Process model.** A single Go binary speaking newline-delimited JSON-RPC 2.0 on
stdio (`--ipc` for a Unix socket / Windows named pipe). No network listener, no
mDNS. On Windows the parent hides the console via
`syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}`, like every
other subprocess.

**CLI flags**:
- `--version` — print `main.Version` (ldflag-stamped from `versions.json`) and exit.
- `--ipc <path>` — Unix socket / named pipe instead of stdio.
- `--log-level <level>` — initial `applog` level; mutable via `log/set-level`.
- `--interval <dur>` (default `1s`) — periodic reconciliation cadence; also
  `scheduler:get-interval` / `scheduler:set-interval` at runtime. Workload and
  discovery changes and effective pressure transitions recompute immediately
  regardless of this setting.

No flag carries node/cluster identity — the scheduler holds none.

**Broker wiring** (`nvpair-ui-broker`):
- a `--scheduler-path` flag (default `./nvpair-job-scheduler[.exe]`, optional/non-fatal),
  spawned + supervised (auto-restart with crash surfacing);
- replay active workloads and cached telemetry on scheduler spawn, send the
  discovery baseline, then fan all three live streams to the child;
- consume its `schedule:priority` and fan out via `node/set-priority` to each
  distinct live proxy handle, caching the last snapshot process-wide and
  re-pushing on proxy (re)spawn (§7.4) — reuses the proxy `workload:*` →
  workload-manager routing pattern, not a new direction;
- forward its `errors:*` to `nvpair-errors`.

**Build wiring**: `nvpair-job-scheduler` is one of the Go binaries in the product
bundle. In the same change:
- a `components.nvpair-job-scheduler` entry in `versions.json`, plus `installer` and
  `product` bumps (see `VERSIONING.md`);
- build + copy in **both** `build.bat` and `build.sh` (→ the repo-root
  `build/bin/`), with the `-X main.Version=…` ldflag;
- inclusion in `installer/nvpair-setup.nsi` and `bom.md` (no firewall rule — no
  listener);
- a bump declared in the pull request's release-intent block (see
  `VERSIONING.md`);
- the root `readme.md` binary inventory / architecture table.

Launched on demand by `nvpair-ui-broker` (which expects workers as siblings in its CWD),
so it must land in the same `bin/` dir.
