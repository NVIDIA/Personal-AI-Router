<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: UI Broker (`nvpair-ui-broker`)

## 1. Purpose
The single front door every PAIR client talks to, and the supervisor of every other runtime worker on the node. A client — the Electron desktop app or `nvpair-tui` — starts exactly one process, `nvpair-ui-broker`, and speaks newline-delimited JSON-RPC 2.0 to it over stdio (or `--ipc`). The broker spawns, supervises, restarts, and tears down `nvpair-node-scanner`, `nvpair-errors`, `nvpair-node-settings`, `nvpair-engine-manager`, `nvpair-node-info`, `nvpair-proxy` (one process hosting a facade per enabled engine), `nvpair-workload-manager`, `nvpair-manual-nodes`, `nvpair-cluster-manager`, and `nvpair-job-scheduler`; relays each worker's JSON-RPC surface under its own namespace; **projects** their private notifications into the client-facing push events the UI renders; and runs the engine settings operations through which every change to an engine's launch or ports is made. No client ever names, launches, or addresses a worker process directly, and there is no broker-absent fallback: if the broker is not running, PAIR has no service.

## 2. Scope
**In scope**
- Process supervision: spawn each worker as a child with stdio JSON-RPC pipes, watch it, restart it on unexpected exit under a bounded backoff policy, and stop it in a defined order on shutdown within a bounded teardown budget.
- Startup ordering and the `app:ready` gate — the one signal a client waits on before assuming the node's service tree is live.
- Namespace relay: forward `ollama-proxy:*`, `lmstudio-proxy:*`, and `llamacpp-proxy:*` to that engine's `nvpair-proxy` facade, and `engine:*`, `settings/*`, `cluster:*`, `nodes:*`, and the manual-node methods to their owning worker verbatim, and map each worker's result or JSON-RPC error straight back.
- Engine settings operations: `engine:get-settings`, `engine:preview-settings`, and `engine:apply-settings` for this node or a pinned peer, revision-checked and journaled, with every port setter routed through the same operation (§7.10).
- Notification projection: consume every worker's stdout notifications, route the error-pipeline ones into `nvpair-errors`, translate or re-emit the rest as the client-facing push stream, and drop what is internal.
- Opt-in subscription state per domain (`discovery:`, `ollama-proxy:`, `lmstudio-proxy:`, `llamacpp-proxy:`, `engine:`, `workloads:`) so a client only receives the streams it asked for.
- The node's discovery service registration: registering each worker's LAN service key and port with the scanner daemon on behalf of the worker, and replaying the registration set across every scanner respawn.
- Proxy/engine port reconciliation for the Ollama and LM Studio compatibility ports, driven by the `settings/get-force-ports` policy and live `engine:status`, before each facade is enabled. llama.cpp has no compatibility-port takeover; its facade is placed on its default port or a safe fallback without moving the engine (§7.7).
- Scheduling telemetry: a source-aware cache of compact GPU telemetry fed by the scanner and by `nvpair-manual-nodes`, aged locally and relayed to `nvpair-job-scheduler`, and replayed when the scheduler restarts.
- Discovery evidence relay: forwarding each proxy's peer-activity reports and `nvpair-node-info`'s peer-observed addresses to `nvpair-node-scanner`.
- Merging manual nodes into the same discovery snapshot the scanner feeds, so one node list answers `discovery:get-nodes`.
- Client hardware delivery: retain full source observations with their receipt
  times, expose optional `AvailableNode.info` baselines, and forward matching
  hardware updates over the client JSON-RPC connection. Unverified previews
  carry display hardware separately from service inventory and scheduling measurements.
- Workload aggregation: applying local-origin (proxy) and peer-origin (workload-manager) workload events to a durable per-node history store that backs `workloads:get-initial`, and retiring a remote-origin record whose origin has stopped re-asserting it.
- Fanning `schedule:priority` snapshots from `nvpair-job-scheduler` out to the proxy as `node/set-priority`, and replaying the latest one whenever the proxy respawns.
- Cluster identity restore: re-injecting the persisted cluster id from `nvpair-node-settings` into a freshly spawned `nvpair-cluster-manager`, and mirroring identity changes back.
- Membership convergence: pass the cluster directory to node-info and other
  cluster-scoped workers, invalidate obsolete source proofs on trust changes,
  and prompt scanner to reauthenticate matching candidates. Relay owned local
  hardware to scanner and its local registry metadata back to node-info.

**Out of scope**
- **Inference traffic** — owned by `nvpair-proxy`; the broker never proxies `/api/chat`, sees a prompt, or touches a response body.
- **Routing and ranking policy** — `nvpair-job-scheduler` computes the order and `nvpair-proxy` applies it; the broker only delivers `node/set-priority` and telemetry and never calls `node/select`.
- **Engine lifecycle, install, and model operations** — `nvpair-engine-manager`.
- **mDNS browsing, node records, and peer directory truth** — `nvpair-node-scanner`.
- **Cluster cryptography and trust policy** — cluster identity, PIN pairing,
  trusted certificates, membership, and node-to-node mTLS belong to
  `nvpair-cluster-manager` and the cluster-scoped workers. The broker validates
  worker source proofs against the current read-only mesh, but generates no
  certificates, performs no pairing, and terminates no TLS.
- **The service-error registry itself** — `nvpair-errors` owns dedup, ordering, cross-node sync, and the `errors:update` snapshot; the broker is its forwarder and its only `clearedBy` writer.
- **Durable settings** — `nvpair-node-settings`.
- **Hardware collection** — `nvpair-node-info` samples hardware; scanner and
  manual-node workers collect remote hardware and public previews. The broker
  owns their client-facing cache and relay, not collection. Compact scheduling
  observations arrive from scanner and manual-node workers; public previews
  never supply them.
- **Engine launch and port validation rules** — `nvpair-engine-manager` owns the launch grammar and applies the change; `ENGINE_SETTINGS.md` is the detailed protocol. The broker owns only the ordering, revision, and journal around it.

## 3. Key Use Cases
- **Cold start**: a client launches the broker, the broker resolves each worker binary, starts the tree in dependency order, emits `app:ready{version}`, and only then attaches the discovery change hook so no `discovery:nodes-changed` snapshot can precede readiness.
- **Baseline then stream**: `discovery:get-nodes` or the immediate
  `discovery:nodes-changed` baseline after a fresh subscribe includes optional
  full hardware in `info`. Scanner supplies live `discovery:node-info` updates;
  manual hardware arrives in merged `discovery:nodes-changed` snapshots. Queries
  read cached observations with elapsed age, without another network request.
- **Relayed control-plane call**: a client sends `engine:start {engine:"ollama"}`; the broker forwards it verbatim to `nvpair-engine-manager`, keeps serving other requests while it runs, and answers with the worker's own result or its JSON-RPC error code and message unchanged.
- **Projected push**: `nvpair-engine-manager` emits `engine:state-changed` on its stdout; the broker re-emits it verbatim to clients that called `engine:subscribe`, and drops it for those that did not.
- **Error surfacing**: any worker emits `errors:report`; the broker stamps `nodeId` and `timestamp` when unset, forwards it to `nvpair-errors`, and relays the resulting `errors:update` snapshot to the client unconditionally.
- **Worker crash**: a supervised worker exits unexpectedly; the broker drops its handle (so status reads stop returning stale state), emits the sticky `supervisor:subprocess-crashed:<name>` error, respawns after backoff, and clears the error once the worker has stayed up for the healthy-reset window.
- **Scheduler fan-out**: `nvpair-job-scheduler` computes one node-wide ranking and emits it once per engine as `schedule:priority{engine, …}`; the broker drops the byte-identical repeat, mints one generation, and delivers `node/set-priority` to the proxy process off the reader goroutine.
- **Change a port from the terminal**: `nvpair-tui`'s node detail screen reads `engine:get-settings`, validates the edited engine port, proxy port, or startup arguments with `engine:preview-settings`, and commits the normalized settings the preview returned with `engine:apply-settings` — the same operation the desktop editor uses, for this node or a pinned peer. A port already in use is reported as a conflict and refused, never silently swapped for another. The narrower `<engine>-proxy:set-port` and `engine:set-port` setters run through the same operation, but no in-tree client calls them.
- **Keep a busy peer**: while a peer's engine streams response bytes back through a facade, the proxy raises `node/activity`; the broker forwards it to the scanner as `discovery:node-activity`. Recent inference activity provides availability evidence for retaining a verified peer when announcements or authenticated inventory reads are delayed.
- **Graceful shutdown**: on `shutdown`, SIGINT/SIGTERM, or stdin EOF, the broker stops the proxy process first (every facade at once), asks `nvpair-engine-manager` to `engine:prepare-shutdown` under a 5-second cap, then closes the remaining workers' stdin and joins them concurrently — all within one 10-second teardown budget, escalating to signal and kill for a worker that does not exit.
- **Edge case — optional worker binary absent**: an unresolved default sibling for an optional worker is a warning, not a failure; the broker starts without it and every method in that worker's namespace answers `-32000 "<worker> not available"`.
- **Edge case — explicit path that does not exist**: an operator-supplied `--*-path` that cannot be stat'ed is fatal at startup, because a wrong explicit path is a mistake worth failing loudly on.
- **Edge case — replaced worker process**: a notification arriving from a proxy generation that is no longer current is discarded before any side effect, so a dying process cannot be mistaken for its successor.
- **Edge case — a stalled parent**: a client that stops reading the broker's stderr would otherwise fill the pipe and leave a worker blocked in `write()`, unable to exit. Every worker writes into its own pipe drained by a bounded in-process sink, and output the parent does not take in time spills to a log file rather than blocking anyone.
- **Edge case — proxy port contention**: with managed port ownership enabled, `:11434` is reserved for the Ollama proxy and a **stopped** engine on that port is moved to the next free managed port; a **running** or unidentified owner is never touched and the managed facade is reported blocked instead.
- **Edge case — client disconnects**: stdin EOF ends the read loop, which runs the same ordered teardown as an explicit `shutdown`; the broker never outlives its client.
- **Edge case — private method reached from outside**: `engine:set-reserved-port` and `internal:set-reserved-port` are the engine manager's internal port-coordination methods and are refused with `-32601` at the boundary instead of being relayed. So is `engine:configure-launch`, which would change a launch without the journal entry, port reservation, and proxy rebind `engine:apply-settings` owns, and so is `<engine>-proxy:shutdown` on every facade namespace, which would let a client kill a process the broker owns.
- **Edge case — a manual node proves its identity**: a manual configuration
  keeps an independent record until pinned mTLS and matching response identities
  prove its host UUID. The broker then rekeys and rebridges that source. A public
  preview's UUID cannot cause this promotion.
- **Edge case — scanner restart**: the broker withdraws the retired scanner's
  complete directory and rejects its late callbacks. It replays registrations
  and owned local hardware with the original sample age; remote records must
  authenticate again. Independent manual sources survive.
- **Edge case — a peer's terminal event never arrives**: an origin re-asserts each of its still-active workloads on a heartbeat, so continued silence about one this node believes is running means either that it finished and every copy of the terminal was lost, or that the origin stopped; a periodic sweep retires it as an **inferred** `failed` that the origin's next authoritative event overrides.
- **Edge case — a peer paired after discovery**: `cluster:trust-changed`
  invalidates obsolete proofs and prompts `discovery:reload-trust`. A new pin
  schedules candidate authentication; only a successful exact-pin inventory read
  creates a trusted peer record.

## 4. Open Questions / Risks
- **Risk — worker binary resolution is cwd-relative**: `resolveSiblingBinary` resolves an unset `--*-path` against `os.Getwd()`, not against the broker executable's own directory, and deliberately does not fall back to `PATH`. A launcher that starts the broker with a working directory other than the install `bin/` resolves **no** workers and comes up as a bare relay. Every embedder must set cwd to the directory holding the binaries or pass every `--*-path` explicitly.
- **Risk — restart budget exhaustion is terminal**: `defaultRestartPolicy` allows 5 attempts per unhealthy streak. A deterministically broken worker exhausts the budget and is never respawned for the life of the process; only the sticky `supervisor:subprocess-crashed:<name>` error marks it. There is no client-visible "gave up" event distinct from the crash report, and no method to ask the broker to retry.
- **Risk — scanner exhaustion removes scanner-sourced capability**: the
  scanner's first spawn is fatal, but later crashes use the normal restart
  budget. A crash withdraws its directory and samples; exhaustion leaves those
  sources unavailable until the broker restarts. Independent manual sources
  remain, and the crash error identifies the missing capability.
- **`noRestartPolicy` (open — dead code)**: `supervisor.go` defines a zero-valued policy that surfaces a crash without respawning, but no production call site uses it — every worker, required and optional alike, is started with `defaultRestartPolicy()`. It survives only in tests. Either wire it to the workers that genuinely should not auto-restart or delete it.
- **Risk — one session, one client**: broker state (subscription flags, the codec) is per-`Broker`, constructed per connection, and the stdio transport admits exactly one peer. Two concurrent clients on one node are not a supported configuration; a second client needs a second broker and would double-spawn the worker tree.
- **Ungated notifications**: `discovery:node-info`, `errors:update`,
  `engine:settings-changed`, `connection/cluster-identity`,
  `connection/cluster-auto-sync`, and `cluster:*` / `nodes:*` are delivered
  without a subscription. Clients must accept them from startup. A hardware
  event for a record not yet in the client baseline is ignored; the next
  snapshot or current-record update supplies it.
- **`engine:subscribe` has no baseline replay (open — needs symmetry)**: `discovery:subscribe` pushes an immediate snapshot and each `<engine>-proxy:subscribe` replays that facade's last `ready` payload, but `engine:subscribe` acks and nothing else. A client must call `engine:get-installed` itself to seed engine state. `workloads:subscribe` has the same shape, but there the gap is closed by `workloads:get-initial`.
- **Risk — port-gate timeouts surface as retry-me errors**: an `engine:*` request that needs a compatibility port to settle is parked until the gate opens or `rpcWorkerCallTimeout` (5s) elapses, after which the client gets `-32000` with "retry" wording rather than a structured, machine-readable state. Clients must treat those strings as transient.
- **Risk — an unresolvable cluster directory isolates the node quietly**: when neither the per-user data dir nor the executable directory yields a cluster path, the broker logs one warning and continues. The node then looks healthy but exchanges no cluster traffic at all, because the inter-node data plane is mTLS-only and every cluster-scoped worker learns membership from that directory.
- **Risk — a worker can be killed mid-shutdown**: teardown is bounded so the broker always finishes inside its parent's grace, which means a worker still working through its own shutdown when its share of the 10-second budget runs out is signalled, then killed. On Windows, where terminating a process is already a kill, `nvpair-engine-manager` is never signalled: its join is abandoned and it is left to finish `StopAll` on its own, briefly outliving the broker, because killing it would orphan the engines it launched. The budget, the 5-second workload-history flush, and the 15-second grace the desktop and `nvpair-tui` allow must move together.
- **Persistence asymmetry (open — needs client-side awareness)**: the broker persists workload history and the engine settings journal to disk, so `workloads:get-initial` returns records from prior sessions, while `nvpair-errors` keeps its registry purely in memory, so `errors:get-initial` returns an empty list after a restart until producers re-emit. Two adjacent "get-initial" methods therefore have opposite durability semantics, and nothing on the wire distinguishes them.
- **Risk — the origin-silence budget hardcodes another binary's heartbeat**: `workloadOriginSilenceTimeout` (5 minutes) is sized as ten missed re-assertions of `nvpair-workload-manager`'s 30s anti-entropy interval, but that interval is a private constant of that binary and is copied here as a bare number with no compile-time link. Lengthening the heartbeat there without widening the budget here makes the sweep hasty, and nothing fails to build.
- **Ambiguous workload identity is never swept (open — needs a wider client contract)**: `StaleForeign` skips any candidate that shares its `(Origin, ID)` pair with another live record, because that pair is the identity the client-facing `workloads:remove` keys on while the store keys on engine and run id as well, so a synthesized terminal could land on whichever generation currently occupies the client key. Two colliding generations that both go silent are therefore retired by nothing. Suppression is the safe direction; the durable fix is to carry engine and run id on the client contract.
- **Future — status introspection**: there is no `supervisor:get-status` method. A client cannot enumerate which workers are up, which are down, and how many restart attempts remain; it can only infer from the error list and from per-namespace `-32000` responses.
- **Future — the workload history has no retention knob on the wire**: history caps and rotation are configured in-process, so a client cannot ask for a bounded window; `workloads:get-initial` always returns the full retained set.

## 5. Requirements

**Functional**
- Serve newline-delimited JSON-RPC 2.0 over stdio by default, or over a Unix domain socket / Windows named pipe when `--ipc` is given.
- Resolve each worker binary from its `--*-path` override or the same-named sibling in the working directory; treat a bad explicit override as fatal and a missing optional default as a warning.
- Start the worker tree in the documented order, emit `app:ready{version}` once, and attach the discovery change hook only afterwards.
- Supervise every worker with exponential backoff, a bounded attempt budget, and a healthy-reset window that clears the crash error.
- Answer the broker's own methods locally (`ping`, `version`, `discovery:*`, every `<engine>-proxy:get-status`, the subscription methods, `workloads:get-initial`, `errors:*`, `shutdown`) and relay everything else by namespace.
- Own `engine:get-settings`, `engine:preview-settings`, and `engine:apply-settings`, and run `engine:set-port` and every `<engine>-proxy:set-port` through the same apply operation, refusing a busy port rather than substituting one.
- Route every producer's `errors:report` / `errors:clear` into `nvpair-errors`, stamping `nodeId` and `timestamp` when unset and always writing `clearedBy` itself.
- Maintain a source-aware discovery store keyed by host UUID for local/pinned
  records and by independent discovery record ID for unverified records. Claimed
  UUIDs, names, and addresses cannot merge unverified data into verified records.
- Cache full accepted hardware with source receipt time, retain elapsed sample
  age across queries/replay and unrelated directory changes, and withdraw it
  when its source, proof, or target changes. Expose `info` only on client views.
- Maintain the workload store from local-origin proxy events and peer-origin workload-manager events, persist its terminal history, load it on startup, and periodically retire remote-origin records whose origin has gone silent about them.
- Register this node's LAN service keys with the scanner daemon and replay them on every scanner spawn.
- Reconcile the Ollama and LM Studio compatibility ports against the `force_ports` policy and live engine status before enabling each facade, and place the llama.cpp facade on its default port or a safe fallback with no compatibility-port gate.
- Cache compact GPU telemetry from the scanner and `nvpair-manual-nodes` with the scanner taking precedence, relay it to `nvpair-job-scheduler`, and replay the cache when the scheduler respawns.
- Forward a proxy's `node/activity` to the scanner as `discovery:node-activity`, and `nvpair-node-info`'s `nodeinfo:observed-addresses` to the scanner as `discovery:set-observed-addresses`.
- Restore the persisted cluster id into `nvpair-cluster-manager` before `app:ready`, and persist a later `cluster:identity-changed` back into `nvpair-node-settings`.
- Pass host identity and cluster directory to node-info. Forward its owned
  `nodeinfo:info-updated` snapshots to scanner as `discovery:local-node-info`,
  and scanner's owned local registry as `nodeinfo:set-peer-metadata` in reverse.
  Reload scanner trust and withdraw obsolete proofs when membership or pins change.
- Refuse private or bypassing methods (`engine:set-reserved-port`, `internal:set-reserved-port`, `engine:configure-launch`, every `<engine>-proxy:shutdown`) at the boundary rather than relaying them.
- Fan the resolved log level out to every worker at spawn and on a live `log/set-level`, so one setting governs the whole tree's stderr.
- Tear down in order — the proxy process, then `engine:prepare-shutdown`, then
  remaining workers by concurrent stdin close and join — inside a 10-second
  budget, escalating a worker that does not exit to terminate and then kill.

**Non-functional**
- Compiles and runs on Windows / macOS / Linux × amd64 / arm64, user mode, no elevation at runtime.
- Never blocks the read loop: long relays (`engine:*`, manual-node probes) are asynchronous, and every worker-round-trip triggered from a reader goroutine is dispatched on its own goroutine to avoid self-deadlock.
- Stdout writes are serialized so frames never interleave.
- A parent that stops reading stderr must never block a worker: worker stderr is decoupled through a bounded in-process sink.
- Never logs a prompt, chat message, request or response body, PIN, certificate, or key; log lines carry method names, worker names, node ids, ports, and normalized error text only.
- Best-effort everywhere a worker is optional: an absent worker degrades one capability, never the process.
- Testable with fake workers and controlled local HTTP/TLS endpoints, without
  real engines, multicast discovery, or installers.

## 6. Inputs and Outputs

**Inputs** — JSON-RPC 2.0 requests and notifications from the single connected client over stdio or `--ipc`; JSON-RPC responses and notifications from each supervised worker's stdout; CLI flags and the `NVPAIR_LOG_LEVEL` environment variable; the persisted workload history and engine settings journal; and, indirectly, `nvpair-node-settings` values the broker reads during startup (`settings/get-force-ports`, `settings/get-cluster-id`, `settings/get-cluster-friendly-name`).

`ReadyParams` — the payload of `app:ready`:
```json
{ version: string }
```

`PingResult` — the response to `ping`:
```json
{ pong: boolean, version: string, uptime_ms: number }
```

`AvailableNode` — the array element of the `discovery:get-nodes` result and of the `discovery:nodes-changed` payload. JSON keys are camelCase here; this is the deliberate camelCase island at the broker's outer boundary, while the rest of the services use snake_case:
```json
{
  id: string
  name: string
  hostUuid?: string
  discoveryRecordId: string
  identityVerification: "local" | "pinned" | "unverified"
  ipAddress: string
  ipAddresses?: string[]   // every published address, ranked, ipAddress first; omitted when there is only one
  port: number
  lastSeen: number
  trusted: boolean
  clustered?: boolean
  models?: string[]
  modelsByEngine?: { [engine: string]: string[] }
  loadedByEngine?: { [engine: string]: string[] }
  info?: NodeInfo         // full hardware, validity, and elapsed sample age
}
```

`info` is the latest accepted `noderec.NodeInfo` for this exact source record.
Local data comes from owned worker IPC, pinned peer data from authenticated
reads, and unverified previews from separate public HTTP collection. Public
`info.hostUuid`, `info.clusterUuid`, and `info.peer` claims are stripped. Missing
`info` withdraws previous hardware. `lastSeen` is a record-change time, not sample
age or proof of availability. Internal scheduling snapshots omit `info`.
`info.observedAt` records local receipt of the whole display observation in Unix
milliseconds. Manual hardware retains the time assigned by the owned manual
worker; other observations are stamped by the broker. It is independent of GPU
sample age: CPU and memory observations can advance while the GPU collector
retains an older sample.
Cached snapshots, source reprojection, and live forwarding preserve that value
while `info.msSince` continues to report GPU sample age. Owned local observations
are stamped before caching and relay to scanner, which preserves the timestamp
on return. Remote `observedAt` values are overwritten. Display observation time
does not establish identity, trust, or scheduling freshness.

`NodeInfoEvent` — scanner-origin hardware forwarded as `discovery:node-info`:
`{discoveryRecordId, hostUuid, clusterUuid?, identityVerification, info}`. Clients
match record ID and verification, plus host identity for local/pinned records,
before updating hardware. Unverified previews remain under their independent
record IDs even when their announcements claim a known host UUID.

`SubscriptionResult` — the ack returned by every `*:subscribe` / `*:unsubscribe` method. Both directions are idempotent and never error:
```json
{ subscribed: boolean }
```

`ProxyStatusResult` — the response to `ollama-proxy:get-status`, `lmstudio-proxy:get-status`, and `llamacpp-proxy:get-status`, answered per engine from the broker's captured facade state with no round-trip to the proxy. It is per engine because the facades bind different ports. The zero value is a valid "not available" answer, so none of these methods errors:
```json
{ ready: boolean, port: number }
```

Example request:
```json
{"jsonrpc":"2.0","id":7,"method":"discovery:subscribe","params":{}}
```

Example client-injected error report — a client surfaces its own operational error through the same registry a worker uses, and the broker stamps `nodeId` and `timestamp` because they were left unset:
```json
{"jsonrpc":"2.0","id":9,"method":"errors:report","params":{"id":"ui:model-pull-failed:llama3.2","message":"pull was cancelled","severity":"warning","action":"retry"}}
```

**Outputs** — client JSON-RPC responses and notifications; worker control
messages, including facade enablement, priority, scheduling telemetry, discovery
registration/activity/observed addresses, owned `discovery:local-node-info`, and
`nodeinfo:set-peer-metadata`; child processes; persisted workload history,
engine settings journal, manual-node configuration, and stderr overflow log.
Every worker's stderr is drained through the broker's sink (§13).

Example readiness notification:
```json
{"jsonrpc":"2.0","method":"app:ready","params":{"version":"<version>"}}
```

`version` is the value stamped into the binary at build time from `services/versions.json`; this document deliberately does not restate it.

Example projected discovery push (a bare array, unlike the wrapped `discovery:get-nodes` result):
```json
{"jsonrpc":"2.0","method":"discovery:nodes-changed","params":[{"id":"studio","name":"studio","hostUuid":"6f1c…","discoveryRecordId":"peer:6f1c…:<hash>","identityVerification":"pinned","ipAddress":"192.168.1.24","port":14318,"lastSeen":1767225600,"trusted":true}]}
```

## 7. API / Interface Contract
The broker exposes one client-facing interface — newline-delimited JSON-RPC 2.0 over stdio (default) or `--ipc` — and consumes ten private worker interfaces of the same shape over each child's stdin/stdout. It also drives side channels through workers it owns: discovery registration and evidence through `nvpair-node-scanner`, and priority and telemetry delivery into the proxy and the scheduler. It serves no HTTP and opens no listening socket of its own.

### 7.0 Methods and notifications

Requests the broker answers itself (client → broker):

| Method | Params | Result |
|--------|--------|--------|
| `ping` | — | `PingResult` |
| `version` | — | `{ version }` |
| `discovery:get-nodes` | — | `{ nodes: [AvailableNode] }` |
| `discovery:subscribe` | — | `SubscriptionResult{subscribed:true}`, then one baseline `discovery:nodes-changed` on a fresh subscribe |
| `discovery:unsubscribe` | — | `SubscriptionResult{subscribed:false}` |
| `ollama-proxy:get-status` | — | `ProxyStatusResult` |
| `ollama-proxy:subscribe` | — | `SubscriptionResult{subscribed:true}`, then a baseline `ollama-proxy:ready` if that facade has come up |
| `ollama-proxy:unsubscribe` | — | `SubscriptionResult{subscribed:false}` |
| `ollama-proxy:set-port` | `{ port }` | `{ port }`; runs `engine:apply-settings` with the new proxy port; `-32000` when the port is in use or the change fails |
| `lmstudio-proxy:get-status`, `llamacpp-proxy:get-status` | — | `ProxyStatusResult` |
| `lmstudio-proxy:subscribe`, `llamacpp-proxy:subscribe` | — | `SubscriptionResult{subscribed:true}`, then a baseline `<engine>-proxy:ready` if that facade has come up |
| `lmstudio-proxy:unsubscribe`, `llamacpp-proxy:unsubscribe` | — | `SubscriptionResult{subscribed:false}` |
| `lmstudio-proxy:set-port`, `llamacpp-proxy:set-port` | `{ port }` | same as `ollama-proxy:set-port`, for that engine |
| `engine:get-settings` | `{ engine, nodeId? }` | the engine's full settings snapshot, local or from a pinned peer |
| `engine:preview-settings` | `{ engine, nodeId?, expectedRevision, settings, resolution? }` | normalized settings, validation errors, conflicts, and a restart/rebind summary; changes nothing |
| `engine:apply-settings` | the preview request plus `requestId` | `{ revision, phase }` receipt; progress arrives as `engine:settings-changed` |
| `engine:set-port` | `{ engine, port }` | the engine's `engine:status`; runs `engine:apply-settings` with the new server port |
| `engine:configure-launch` | — | `-32601` — use `engine:apply-settings` |
| `engine:subscribe` | — | `SubscriptionResult{subscribed:true}` (no baseline) |
| `engine:unsubscribe` | — | `SubscriptionResult{subscribed:false}` |
| `workloads:subscribe` | — | `SubscriptionResult{subscribed:true}` (no baseline — use `workloads:get-initial`) |
| `workloads:unsubscribe` | — | `SubscriptionResult{subscribed:false}` |
| `workloads:get-initial` | — | `{ workloads: [workloadInfo] }`, ordered by `createdAt`, from the persisted store |
| `errors:get-initial` | passed through | the `nvpair-errors` snapshot, or `[]` when no datastore is supervised |
| `errors:report` | `ServiceError` (`id` required) | `null` ack; the state change arrives later as `errors:update` |
| `errors:clear` | `{ id }` | `null` ack; the broker stamps `clearedBy` |
| `engine:set-reserved-port` | — | `-32601` — private engine-manager method, refused at the boundary |
| `internal:set-reserved-port` | — | `-32601` — private engine-manager method, refused at the boundary |
| `shutdown` | — | `null`, then the ordered teardown begins |
| `ollama-proxy:shutdown`, `lmstudio-proxy:shutdown`, `llamacpp-proxy:shutdown` | — | `-32601` — the broker owns the proxy lifecycle and will not let a client kill it |

The facade rows are one profile-driven handler applied to every engine in `nvpair-shared/engines`, so each `<engine>-proxy:` namespace has the same broker-local surface.

Anything else is matched by namespace prefix and relayed verbatim:

| Namespace | Owning worker | Relay shape |
|-----------|---------------|-------------|
| `ollama-proxy:*`, `lmstudio-proxy:*`, `llamacpp-proxy:*` | that engine's `nvpair-proxy` facade | bounded synchronous `Call`; the client prefix is replaced by the engine address (`ollama-proxy:nodes/list` → `ollama:nodes/list`) |
| `engine:*` | `nvpair-engine-manager` | asynchronous, **no broker-imposed timeout**, method verbatim |
| `settings/*` | `nvpair-node-settings` | bounded synchronous `Call`, method verbatim |
| `cluster:*`, `nodes:*` | `nvpair-cluster-manager` | bounded synchronous `Call`, method verbatim |
| `node/add`, `node/remove`, `nodes/list` | `nvpair-manual-nodes` | asynchronous, no broker-imposed timeout (`node/add` probes) |

`node/remove` requires exactly one selector: `{id}` names an exact manual entry
ID, while `{nodeId}` names a broker node key that the broker resolves to an entry
before forwarding `{id}`. Missing/conflicting selectors return `-32602`; an
unmatched node key returns `{removed:false}` without changing saved entries.

An unmatched method returns `-32601 "method not found: <method>"`. A namespace whose worker is not supervised returns `-32000 "<worker> not available"` — a clear error rather than a silent hang. A worker's own JSON-RPC error is relayed with its **code and message unchanged**; only broker-originated failures (transport, timeout, missing worker) use `-32000`.

Notifications the broker emits to the client, and what produces each:

| Client-facing event | Gate | Producer and projection |
|---------------------|------|-------------------------|
| `app:ready{version}` | always | the broker itself, once, after the worker tree is started |
| `discovery:nodes-changed` | `discovery:subscribe` | the merged discovery store (scanner + manual nodes); payload is a bare `AvailableNode[]` |
| `discovery:node-info` | always | scanner full hardware event, accepted only for its current source record, including sanitized unverified previews |
| `ollama-proxy:<method>`, `lmstudio-proxy:<method>`, `llamacpp-proxy:<method>` (notably `…:ready`) | that namespace's `…:subscribe` | the facade's engine-addressed notification, with the engine address replaced by the client prefix |
| `engine:ready`, `engine:state-changed`, `engine:models-changed`, `engine:install-progress`, `engine:pull-progress`, `engine:remote-progress` | `engine:subscribe` | `nvpair-engine-manager`, verbatim (already `engine:`-prefixed) |
| `engine:settings-changed` | always | the broker itself, a full settings snapshot per engine on every settings change |
| `workloads:upsert`, `workloads:remove` | `workloads:subscribe` | `nvpair-workload-manager` (peer-origin) and `nvpair-proxy` (local-origin), after the store accepts the transition |
| `errors:update` | always | `nvpair-errors`, verbatim — the full sorted `ServiceError[]` snapshot |
| `connection/cluster-identity`, `connection/cluster-auto-sync` | always | `nvpair-node-settings`, verbatim |
| `cluster:*`, `nodes:*` (e.g. `cluster:invite-received`, `cluster:identity-changed`, `cluster:trust-changed`, `nodes:changed`) | always | `nvpair-cluster-manager`, verbatim |

Because most client-facing events are forwarded verbatim, the authoritative list of what a client can receive is the forwarder set in §7.4, not a literal search for notification names in the broker.

### 7.1 Supervision inventory and lifecycle ordering
Ten workers are supervised, started in this order:

1. `nvpair-node-scanner` (**required**) — the first spawn is synchronous and a failure is fatal; the broker has no job without discovery. Started before `app:ready` so a client seeing readiness can assume discovery is live.
2. `nvpair-errors` — spawned early, right after the scanner and before the other producers, so a sink exists by the time they start emitting. Runs with `--peer-sync`.
3. `nvpair-node-settings` (optional) — must precede the proxy, because it holds the managed-port policy.
4. `nvpair-engine-manager` (optional) — must also precede the proxy, so port ownership can be resolved against live engine status.
5. `nvpair-node-info` — started with this host's UUID and cluster directory.
   Broker receives `nodeinfo:info-updated` and `nodeinfo:observed-addresses`,
   relays owned hardware to scanner, and sends local registry metadata back to
   node-info. The worker's live mesh gates protected inventory independently
   of its public preview endpoint.
6. `nvpair-proxy` — one process, one supervisor, crash key `nvpair-proxy`. It starts with no listener; the broker then sends one `facade/enable` per engine named in `--proxy-engines` (default every engine), carrying the port it planned for that engine and any alias addresses. An engine left out is neither enabled nor prepared, so the broker never relocates an engine whose facade nothing will claim. Does **not** gate `app:ready`. A crash takes every facade down and the restart brings them back together, while a terminal failure releases each engine's port-ownership gate individually.
7. `nvpair-workload-manager` — does **not** gate `app:ready`; it has no readiness handshake the broker waits on.
8. `nvpair-manual-nodes` (optional).
9. `nvpair-cluster-manager` (optional).
10. `nvpair-job-scheduler` (optional). Every spawn is seeded with a replay of the telemetry cache.

Every optional worker goes through `startOptionalWorker`, which resolves the binary, creates a supervisor with `defaultRestartPolicy()`, wires crash and recovery callbacks, and returns `nil` when the path is unset or the first spawn fails. Each carries a teardown callback that clears the state it owns, so a down worker cannot leave stale facts behind: `clearManualNodesState` drops that worker's manual claims and their telemetry from the discovery store and pulls the orphaned nodes out of the Ollama and LM Studio facades, the engine-manager callback drops the handle and its relay subscription, `setScheduler(nil)` drops the scheduler handle, and so on.

`nvpair-tui` is bundled with the product but is **not** supervised here — it is a client that spawns its own broker.

Shutdown follows dependency order. `shutdownInferenceStack` arms the teardown
clock, stops the one proxy process so no new inference arrives, then asks the
engine manager to `engine:prepare-shutdown` under `engineStopAllBudget` (5 s,
clipped to the remaining budget). Missing, failing, or slow preparation falls
through to the stdin-close backstop. Desired engine state remains persisted.
`stopWorkers` closes stdin and joins all successfully started supervisors
concurrently, so independent workers drain together rather than in LIFO order.
The proxy is stopped first on every exit path, including early startup failure.
The workload history flusher is joined after worker teardown so terminal events
are still captured.

Teardown is **bounded** so the broker always finishes inside the grace its parent allows. Everything from arming the clock to the last worker join draws on one `teardownBudget` of 10 s; the workload history's final flush adds at most 5 s; the desktop app and `nvpair-tui` each allow 15 s. Each worker's join closes its stdin and waits for it to exit on its own for `workerStopGrace` (3 s) — `engineManagerStopGrace` (6 s) for the engine manager, reserved out of the budget so earlier joins cannot spend it — clipped to what is left of the budget and floored at 250 ms. A worker still running is then sent a terminate signal, given 2 s, killed, and given 2 s more before the join is abandoned. On Windows, where terminate is already a kill, the engine manager is never signalled: its join is abandoned so it can finish `StopAll` rather than orphan the engines it launched.

### 7.2 The `app:ready` gate
`app:ready{version}` is emitted exactly once, after the last supervisor has been started. It is a statement about the *supervision tree*, not about every capability: discovery is guaranteed to be running behind it, while each facade announces its own readiness separately (`ollama-proxy:ready`, `lmstudio-proxy:ready`, or `llamacpp-proxy:ready`, or a poll of `<engine>-proxy:get-status`) because it binds asynchronously, and the workload manager has no readiness handshake at all. A worker that failed to start does not delay or suppress `app:ready`; its absence shows up as `-32000` on its namespace.

The discovery change hook is attached only **after** `app:ready` is on the wire, so a scanner event arriving during startup cannot produce a `discovery:nodes-changed` before the client has seen the broker come up. It is detached on the way out so a last-millisecond scanner event cannot push through a codec that is being torn down.

### 7.3 The relay and subscription mechanism
Every worker runs behind `startRPCWorker(name, path, args, notifyHook)` or a worker-specific equivalent, which owns the child process and wraps its pipes in a bidirectional `jsonrpc.Peer`. The peer does id correlation and runs the read pump; the handle owns the OS process lifecycle and exposes `Call` (bounded, `rpcWorkerCallTimeout` = 5s), `CallNoTimeout`, `RelayRequest` (asynchronous, response delivered to a callback), and `Notify`. The generic `startRPCWorker` links — engine-manager, manual-nodes, node-settings, and the scheduler — read frames up to `jsonrpc.WorkerFrameBytes` (8 MiB), because `engine:catalog`'s Ollama reply alone is about 1.9 MiB and an over-long line is a terminal read error that silently drops the peer. `nvpair-tui` reads the broker with the same cap, since a relayed reply must fit every hop. The purpose-built links to the scanner, errors, cluster manager, and proxy keep the shared codec's 1 MiB default.

Relay choice follows the operation's real duration. `settings/*`, `cluster:*` / `nodes:*`, and every `<engine>-proxy:*` are local request/response and use the bounded synchronous `Call`. `engine:*` and the manual-node methods use the asynchronous `RelayRequest` with **no broker-imposed deadline**, because an install or a model pull runs for minutes and reports progress via push events — the broker waits for the real response and keeps serving other requests meanwhile, rather than fabricating a timeout. The broker-owned settings methods run on their own goroutine for the same reason. Method names are forwarded verbatim for `engine:`, `settings/`, `cluster:`, `nodes:`, and the manual-node methods, because those workers' methods are already namespaced. Only the facade namespaces are rewritten: one process holds every facade, so a message on that link must name the engine it concerns, and the client's component prefix (`ollama-proxy:`) is replaced by the bare engine address (`ollama:`). The two strings are not interchangeable.

Subscriptions run in the opposite direction too. A **client** subscribing sets a per-session flag and receives the client-facing event stream. A **worker child** may itself send `discovery:subscribe` upward — `nvpair-errors` for its `er` peer set, `nvpair-workload-manager` for `wl`, `nvpair-cluster-manager` for `cl`, `nvpair-engine-manager` for `ec`, and each `nvpair-proxy` facade for its engine's `ol`, `lm`, or `lc` — and the broker registers it against the shared `relay.Directory` via `subscribeRelay`, pushing `discovery:nodes{nodes:[DirectoryNode]}` snapshots down that worker's stdin. The two are different contracts on a shared method name: the client stream is the narrow camelCase `AvailableNode[]` under `discovery:nodes-changed`, the worker stream is the richer `DirectoryNode` set under `discovery:nodes`. A re-subscribe drops the prior registration first so a worker is never double-fed, and a worker's exit unsubscribes it so nothing pushes into a closed stdin.

Worker consumers apply their own provenance requirements before using a record:
service routes and remote engine control require local or trusted pinned sources,
and workload/error peer synchronization requires trusted pinned sources. Pairing
can still display unverified connection candidates. Explicit manual engine routes
remain independently configured. Public previews never provide service evidence.
The scheduler receives its filtered node universe unconditionally, regardless of
client subscriptions, and that snapshot omits full display hardware.

### 7.4 Notification projection
Each worker's reader goroutine invokes a forwarder. Every forwarder calls `dispatchErrorsNotif(<producer>, method, params)` first, which claims `errors:report` and `errors:clear` for the error pipeline and returns `true` so the forwarder stops. The rest is per-worker:

| Forwarder | Producer | Projection |
|-----------|----------|------------|
| scanner `handleNotify` | `nvpair-node-scanner` | directory changes fold into the relay directory/store; accepted local/pinned `discovery:node-telemetry` feeds scheduling; `discovery:node-info` is validated, cached with elapsed age, and forwarded to the client |
| `forwardNodeInfoNotification` | `nvpair-node-info` | owned `nodeinfo:info-updated` is cached and sent to scanner as `discovery:local-node-info`; observed addresses become `discovery:set-observed-addresses`; other notifications are dropped |
| `forwardEngineNotification` | `nvpair-engine-manager` | `engine:settings-request` / `engine:settings-cancel` carry a paired peer's settings operation into the broker (§7.10); `engine:ready` also triggers LM Studio proxy reconciliation and re-sends the settings projection; `discovery:subscribe` is claimed as a worker relay subscription; everything else is re-emitted verbatim to `engine:subscribe`'d clients |
| `forwardProxyProcessNotification` | `nvpair-proxy` | an engine-addressed notification goes to that engine's handler, with that engine's spawn generation for Ollama and LM Studio; an unaddressed one is process-scoped and handled once — `workload:*` is stamped and routed to the workload manager, and `node/activity` is forwarded to the scanner as `discovery:node-activity` |
| `forwardProxyNotificationForGeneration` / `forwardLMStudioProxyNotificationForGeneration` | one facade | stale generations dropped; a `bind-failed` on the managed facade port blocks the facade, otherwise selects a fallback port unless the user set the port explicitly; `ready` triggers port reconciliation; the remainder is emitted as `<engine>-proxy:<method>` to that namespace's subscribers |
| `forwardDefaultEngineProxyNotification` | the llama.cpp facade | no port reconciliation and no generation check: error-pipeline frames are claimed, and the remainder is emitted as `llamacpp-proxy:<method>` to that namespace's subscribers |
| `handleManualNodesNotification` | `nvpair-manual-nodes` | source events upsert full authenticated hardware or separate display previews into `AvailableNode.info`; only pinned measurements feed scheduling; explicit reachable engine routes bridge independently; clients receive merged `discovery:nodes-changed` snapshots, not a separate manual `discovery:node-info` event |
| `forwardSettingsNotification` | `nvpair-node-settings` | only `connection/cluster-identity` and `connection/cluster-auto-sync` are re-emitted, verbatim and unconditionally; everything else is debug-logged and dropped |
| `forwardClusterManagerNotification` | `nvpair-cluster-manager` | notifications relayed verbatim; identity changes persist settings and reconverge advertisement; trust changes invalidate obsolete scanner/manual proofs and prompt scanner reauthentication; node-info follows live mesh membership |
| `forwardWorkloadManagerNotification` | `nvpair-workload-manager` | only `workloads:upsert` and `workloads:remove` are applied to the store and re-emitted to subscribers; `ready` and anything else are internal and dropped |
| `forwardSchedulerNotification` | `nvpair-job-scheduler` | `schedule:priority` is de-duplicated, cached, and delivered to the proxy (§7.5); everything else is dropped |
| `onErrorsUpdate` | `nvpair-errors` | `errors:update` relayed verbatim and unconditionally |

The **generation** variants exist because a supervisor can replace the proxy process while its predecessor's frames are still in flight. Each spawn takes a monotonically increasing generation for Ollama and for LM Studio, and a forwarder invoked for an older generation returns before any side effect — so a dying process's `ready`, `bind-failed`, or error report can never be attributed to its successor, reconfigure the new process, or resurrect state the replacement has already moved past. The llama.cpp forwarder carries no generation. None of its notifications changes a port, so a stale one can reach only `llamacpp-proxy:` subscribers or the errors pipeline.

Workload events funnel through one serialized apply-then-fan-then-notify path regardless of origin, so the client-visible order matches the applied order, and a stale or regressive upsert is dropped at the store rather than resurrecting a finished workload on the client stream.

Two sweeps run on that same path with transitions no origin ever sent, both applied as **inferred** so the origin's next authoritative event reconciles them away: one fires when a node leaves discovery with workloads still pinned to it, the other every 60 seconds for a remote-origin record whose origin has said nothing about it for 5 minutes. Neither substitutes for the other — a peer whose delivery to this node is failing stays happily in discovery. The store tracks each record's last *origin assertion* separately from its last update, so an origin's no-op heartbeat re-assertion still counts as evidence of liveness and only genuine silence ages a record out, and the sweep re-checks that sighting under the merge lock so an origin that spoke up between selection and apply is never overwritten by a guess already known to be wrong. Local-origin records are never swept, because this node's own proxies are their authority and never re-assert into the store; records this node merely *executes* are swept, because lifecycle events come from the originating proxy and the executing node holds no independent signal of its own.

### 7.5 Scheduler feeds and priority fan-out
`nvpair-job-scheduler` ranks nodes node-wide, across engines, by pending workload plus GPU pressure, and emits the same ranking once per engine as `schedule:priority{engine, …}`. The broker keeps **one process-wide cache**: a ranking byte-identical to the cached one is dropped before it mints a generation, and a new one is stored under the next generation and delivered as `node/set-priority` to every distinct live proxy handle — once, not once per engine, since one process hosts every facade. Delivery runs **on a separate goroutine**, because the round-trip blocks on the proxy's response and calling it inline would stall the scheduler's notification stream; an older generation is never delivered over a newer one.

A fresh proxy process has no ranking, so every proxy spawn replays the cached snapshot from the supervisor's spawn hook — not from the child's `ready`, which can arrive before the broker has published the new handle. The replay reuses the current generation, so a repeat to a proxy that never lost its state is an acknowledged no-op.

The scheduler receives the filtered scheduling node universe and compact
`scheduler:telemetry`. Only owned local or currently pinned sources supply GPU
measurements. The source-aware cache is keyed by host UUID, with scanner samples
preferred over manual samples. Relays and restart replay include elapsed age and
recheck current source approval. Removing one source preserves a surviving
independent source. Unverified previews never enter this cache, and scheduling
node snapshots omit `AvailableNode.info`.

The broker implements no ranking policy of its own. Routing precedence is explicit manual selection, then scheduler priority, then the proxy's deterministic default order; the broker is only the delivery path for the middle tier. It never calls `node/select`. The scheduler is GPU-pressure-aware but not VRAM- or capability-aware.

### 7.6 Error stamping and crash reporting
`dispatchErrorsNotif` is the single funnel. On `errors:report` it decodes `errors.ServiceError`, fills `nodeId` with the local node id when the producer left it empty (so the upsert is not anonymous and a later clear can match it) and fills `timestamp` when it is zero, then forwards to `nvpair-errors`. On `errors:clear` it decodes `errors.ClearParams` and **ignores any producer-supplied `clearedBy`** — the broker is the only writer of that attribution and stamps it with the local node id. A client may inject `errors:report` as a notification or call it as a request; both take the identical stamping path, which is how a client surfaces its own operational errors through the same registry.

When `nvpair-errors` is unavailable, `forwardErrorsReport` and `forwardErrorsClear` are benign no-ops with a debug log, so no producer needs its own nil guard and a missing datastore degrades to local logs rather than hard failures.

Crash reporting uses the sticky id `supervisor:subprocess-crashed:<name>` — one id per worker with no timestamp suffix, so repeated crashes upsert the same entry instead of piling up, and a flapping worker just refreshes its timestamp. `onCrash` drops the worker's handle first (so a status read reflects reality during the down window instead of returning the dead process's cached state) and then reports with `severity:"error"`, `action:"none"`. `onRecovered` clears the id once the worker has stayed up past the healthy-reset window, and logs the recovery.

`nvpair-errors` is the one exception: it cannot report its own death into itself, so its crash callback logs to stderr and drops the handle. The supervisor still restarts it; on recovery producers re-emit and their entries come back.

### 7.7 Port reconciliation
The broker owns the ordering between the compatibility ports the ecosystem expects (`11434` for Ollama, `1234` for LM Studio) and the ports the engines themselves want. It reads the `settings/get-force-ports` policy and the live `engine:status` before enabling each facade, and treats an unreadable, undecodable, or unverifiable policy as **blocked** rather than permitted — the facade is not claimed when the broker cannot prove it is safe to claim it.

`force_ports` is policy consent only; it never grants process-kill authority. A **stopped** engine whose configured port is the facade or is otherwise occupied is moved to the next free managed port. A **running** engine, or a listener whose owner cannot be identified, is left completely untouched, and the managed facade is reported blocked with a specific reason (`"Ollama is already running on the compatibility port"`, `"the compatibility port is already in use"`, `"managed-port policy could not be verified"`, and so on) through the sticky `ollama-proxy:port-ownership-blocked` (or `lmstudio-proxy:port-ownership-blocked`) error. When managed ownership is off, the engine-wins bump applies instead and the sticky warning is cleared. An inherited `OLLAMA_HOST` loopback alias is reserved under the same policy, and a collision is reported as `ollama-proxy:ollama-host-alias-blocked`. Explicit user settings from the engine settings journal take precedence over these automatic defaults on later starts.

llama.cpp has **no compatibility-port takeover**. `nvpair-engine-manager` owns its process and starts it on its configured loopback port (`:8081` by default); the broker records that port without a policy read, a startup gate, or any relocation of the engine. It requests the facade on `:8080`, or on the port already chosen for this broker lifetime, and on a bind failure retries once on the next free port that avoids the failed port, the engine's default port, the inherited `OLLAMA_HOST` alias, and every other facade's planned, backend, and persisted ports. A facade port set explicitly through the settings journal is never replaced by a fallback; its bind failure is logged and the facade stays down. llama.cpp never reports a port-ownership block. Its only `llamacpp-proxy:port-ownership-blocked` entry is the warning every engine gets when the `nvpair-proxy` binary does not resolve ("NVPAIR is not fronting llama.cpp on port 8080: proxy binary not resolved.").

A port the user chooses is never silently moved. `engine:set-port` and every `<engine>-proxy:set-port` are intercepted and run through `engine:apply-settings` (§7.10), which validates both of the engine's ports against registered services, aliases, other engines and facades, and occupied listeners. A port already in use is refused with `-32000`, and a collision with the `OLLAMA_HOST` alias is refused with a message naming it. Automatic steering remains only for a port the user did not just choose: when a facade announces a restored port a running engine has since taken, the broker moves the facade to a free port and surfaces the sticky warning `ollama-proxy:port-bumped`, never changing the engine's port. An `engine:*` request that depends on a facade port still being reconciled is parked on the corresponding gate and either replayed when the gate opens or answered with a retry-me error after 5 seconds.

### 7.8 Discovery registration and relay
No worker registers itself. The broker holds a `relay.RegistrationCache` of this node's LAN services and pushes each entry into the scanner daemon on the worker's behalf, then **replays the whole cache on every scanner spawn** — so a scanner restart re-advertises the node's full service set with no reconnect logic in any worker. Registration is idempotent; only a real change is pushed.

The canonical service keys and the ports the broker registers are `ni` = 14318 (`nvpair-node-info`), `er` = 14319 (`nvpair-errors`), `wl` = 14320 (`nvpair-workload-manager`), `cl` = 14321 (`nvpair-cluster-manager`), `em` = 14322 (`nvpair-engine-manager`'s model-list HTTP endpoint), and `ec` = 14323 (`nvpair-engine-manager`'s cluster-scoped remote-control surface, registered only when a cluster directory is configured). `ol`, `lm`, and `lc` are registered dynamically with that engine's **facade** listen port — not the engine's port — while the broker's 5-second health poll of the engine's backend port answers (`/` for Ollama, `/v1/models` for LM Studio, `/health` for llama.cpp), and are unregistered whenever the engine or its facade goes away, so a peer never routes to a facade that has nothing behind it.

The broker also relays two kinds of evidence into the scanner that it cannot gather itself. A proxy's `node/activity`, raised while a peer's engine streams response bytes back, becomes `discovery:node-activity`. The scanner uses that recent inference evidence when deciding whether to retain a verified peer independently of announcement expiry. `nvpair-node-info`'s `nodeinfo:observed-addresses`, the local addresses remote peers have actually reached, becomes `discovery:set-observed-addresses`, which the scanner uses to rank the addresses it publishes. Both are sent off the producing worker's reader goroutine.

`convergeScannerIdentity` closes the fresh-host startup-order window in which the scanner minted a node id before `nvpair-cluster-manager` wrote its identity: it waits for `cluster:get-node-id` to answer, then calls the scanner's `discovery:reload-identity` so the advertised `uuid=` matches the cluster principal. It is best-effort and idempotent, and a later membership change reconverges through the identity-change path and the scanner's own membership watch.

`cluster:trust-changed` makes the broker remove pinned scanner records whose
proof no longer matches the live mesh. Affected manual entries lose verification,
hardware, and scheduling measurements and return to independent configuration
records; their explicit engine routes are preserved. The broker then prompts
`discovery:reload-trust`.
Announcements remain connection candidates. A saved pin chooses which identity
to authenticate; it cannot establish endpoint ownership by itself. Only a
successful pinned-mTLS read with matching response identities promotes a peer.
Each verified record ID is valid only for this node's current identity and
cluster membership, and for the peer's exact approved certificate. Leaving and
rejoining the cluster, or changing the peer's certificate pin, invalidates cached
data and in-flight results tied to that ID.
Replacing a verified scanner record withdraws its previous record ID from the
worker relay before publishing the replacement. A delayed removal still retires
its own relay entry, while the current host record, telemetry, and workloads
remain intact.

#### Hardware snapshots and previews

The node-info worker started by the broker emits hardware at startup and every
two seconds over stdio. The broker caches it with receipt time and forwards
`discovery:local-node-info` to scanner. Scanner attaches it to this host's local
registry record and emits a full `discovery:node-info` event alongside compact
scheduling telemetry.

Scanner sends this host's name, addresses, and service ports through the broker
to node-info using `nodeinfo:set-peer-metadata`. Remote announcements cannot
supply local measurements or registry metadata. Scanner restarts replay the
local observation with elapsed age; remote records authenticate again.

Broker accepts a full scanner event only for the current source record and
verification. It stores `info` and receipt time separately from service inventory,
then forwards the event. For a valid utilization sample, the broker adds time
spent waiting for its locks and in its cache to `msSince`. A sample received at
an age of 200 ms is reported as 5,200 ms old five seconds after receipt.

Scanner supplies identity, addresses, and models when scanner and manual records
describe the same host. Its hardware takes precedence too. If scanner has no
hardware, a display snapshot may use manual hardware only when both records are
trusted and have the same pinned host identity and nonempty verification ID.
The manual sample keeps its original receipt time. This display choice does not
change directory metadata or scheduling telemetry precedence.

Unrelated directory/model updates retain the original observation only
while its source, proof, trust, and target are unchanged. Removing a source or
replacing its endpoint/proof withdraws that hardware without erasing independent
sources. Client queries and subscription baselines share an output lock with
hardware pushes so an old baseline cannot follow a newer update. Snapshot capture
uses the store's own lock, which is released before writing the client reply.
Queries and subscriptions do not wait for worker delivery; they return the latest
cached state even while a worker's stdin blocks the discovery update handler.

Public HTTP supplies automatic display previews of eligible unverified
records, independently of whether a pairing screen is open. Scanner uses a
separate bounded two-second loop with at most four public HTTP requests in flight
across all nodes and addresses, address failover, and failure backoff capped at
30 seconds. Manual nodes use a separate two-second loop with four readers for
entries without TLS/mTLS hints that have never authenticated.
A paired entry's later transport failure or trust withdrawal
does not downgrade it to public measurements. Manual previews have their own
receipt time and retry on the next batch without additional backoff.

Both collectors use `noderec.ReadHardwarePreview`: 1.5-second read limit, 64 KiB
maximum body, no redirects, and stripped response identity/registry claims.
Scanner also bounds its complete address sweep to 1.5 seconds. Results must still
match the current unverified record/entry after reading. Preview data changes
display hardware only, never verified identity, liveness, routing, or scheduling.
Manual previews and authenticated hardware appear in `AvailableNode.info` on
`discovery:nodes-changed`; scanner events also use `discovery:node-info`.
Desktop and TUI receive these observations through the broker's JSON-RPC connection.

### 7.9 Cluster identity restore
`nvpair-cluster-manager` persists its member roster but not the cluster id; that lives in `nvpair-node-settings`. On the manager's first spawn — synchronously, **before** `app:ready` and the read loop, so a client's `cluster:get-node-id` is never racy — the broker reads `settings/get-cluster-id` and `settings/get-cluster-friendly-name` and injects them via `cluster:set-identity`. It repeats this on every crash respawn, and no-ops when the node is unclustered or settings is unavailable.

The loop is closed in the other direction: a `cluster:identity-changed` notification (create, adopt-on-join, or leave) is written back through `settings/set-cluster-id` and `settings/set-cluster-friendly-name` on a goroutine, so joining survives a restart and — just as importantly — leaving sticks instead of being undone by a stale restore on the next boot.

`nvpair-node-info` receives this host's UUID and the shared cluster directory at
spawn. Its live mesh supplies membership and gates protected peer inventory on
every request; leaving disables that surface even if a keypair remains. Broker
supplies the owned registry metadata via `nodeinfo:set-peer-metadata`, with
matching host/principal and replay after restart. Public hardware supplies
unverified previews independently of the protected peer inventory handler.

Cluster-manager owns membership and pins. Broker retains a read-only live mesh
to reject source proofs invalidated by trust changes. Worker transports perform
mTLS; the broker generates no certificates, performs no pairing, and terminates
no TLS. Removed verification cannot be restored by a retired worker's callback.

### 7.10 Engine settings operations
The broker owns the combined settings operation for each engine: its server port, its facade port, and its launch text. `ENGINE_SETTINGS.md` is the detailed protocol; this section states the broker's part.

- `engine:get-settings` returns a full snapshot; `engine:preview-settings` validates a candidate and reports errors, conflicts, and what would restart or rebind, changing nothing; `engine:apply-settings` accepts it and answers with a `{ revision, phase }` receipt. The receipt is not a state update: progress and the outcome arrive as `engine:settings-changed`, a full snapshot, relayed to the client unconditionally.
- Every apply carries the revision it was built against, and a stale one is refused. The broker holds one node-wide configuration lock across validation, journal acceptance, and application, so operations on one node are serialized.
- Acceptance is written and synced to `engine-settings-operations.json` in the per-user data directory before any component changes, together with the previous and desired settings and a resume intent. An operation interrupted by a crash is replayed on the next start before enabled engines are restored, and unreadable journal data suppresses automatic component rewrites rather than guessing.
- `nodeId` selects a pinned peer instead of this node. The broker relays the call to `nvpair-engine-manager` as `engine:remote-<operation>` (for example `engine:remote-apply-settings`), which carries it to the peer's pinned-mTLS engine-control listener. A peer's operation arrives here the other way, as `engine:settings-request` / `engine:settings-cancel` from the engine manager carrying the authenticated caller, and the broker rechecks that the caller is still pinned once it holds the node lock. Settings never enter mDNS or discovery metadata.
- The broker sends the engine manager `engine:settings-projection`, the local snapshots its subscription hub serves to peers, whenever settings change and again on `engine:ready`.
- An engine's launch configuration changes only through `engine:apply-settings` and the port setters built on it. A direct `engine:configure-launch` is refused with `-32601`, because it would skip the journal, the port reservation, and the proxy rebind.

### 7.11 Versioning
- The component version is stamped at build time via `-ldflags "-X main.Version=…"` from `services/versions.json`, and is reported by `--version`, by `version`, by `ping`, and in the `app:ready` payload. A client should read it from `app:ready` rather than shelling out.
- Client-facing method names are namespaced and evolve **additively**; the relayed namespaces are owned by their workers, and the broker forwards new methods within a namespace with no change here.
- Notification payloads grow by adding optional fields. `discovery:get-nodes` deliberately wraps its array in an object so summary fields can be added later; the `discovery:nodes-changed` payload stays a bare array.
- `AvailableNode`'s camelCase keys are a fixed external contract and do not migrate to the snake_case used elsewhere in the services.

## 8. Dependencies
- **Upstream**: the client that spawns it and owns the pipe — the Electron desktop app, or `nvpair-tui` (which spawns its own broker via `--broker-path`). Exactly one client per broker process.
- **Downstream**: `nvpair-node-scanner` (required), `nvpair-errors`, `nvpair-node-settings`, `nvpair-engine-manager`, `nvpair-node-info`, `nvpair-proxy`, `nvpair-workload-manager`, `nvpair-manual-nodes`, `nvpair-cluster-manager`, `nvpair-job-scheduler`.
- **External**: first-party shared packages only — `nvpair-shared/applog` (structured logging plus `--log-level` and the `log/set-level` fan-out), `nvpair-shared/appdir` (per-user data dir), `nvpair-shared/clustertrust` (read-only membership and node-principal lookup against the cluster dir), `nvpair-shared/errors` (the `ServiceError` / `ClearParams` wire shapes), `nvpair-shared/nodeid`, `nvpair-shared/noderec` (service keys, `DirectoryNode`, the register/subscribe wire), `nvpair-shared/schedulerwire` (priority and telemetry wire), `nvpair-shared/engines` (engine identity, facade names, and addressed methods), `nvpair-shared/enginesettings` (the settings snapshot and request shapes), plus the local `relay` and `workloadstore` packages. No third-party runtime services and no network calls of its own.

## 9. Data Ownership
- **Owned**: the source-aware discovery store and its full hardware observations
  with receipt times; the owned local hardware replay cache; durable manual-node
  configuration; the workload index/history; service registrations; priority
  snapshot/generation; compact scheduling telemetry cache; engine settings
  records/journal; subscriptions; worker generations and handles; managed ports.
- **Source of truth**: for the *merged node list* and the *workload index*, yes — no other component holds the union of mDNS and manual nodes, or of local-origin and peer-origin workloads. For everything else, no: `nvpair-node-scanner` owns node records, `nvpair-errors` owns the error registry, `nvpair-node-settings` owns durable settings, `nvpair-engine-manager` owns engine state, `nvpair-cluster-manager` owns identity and membership, and `nvpair-proxy` owns routing. The broker is the source of truth for the engine settings operation's revision and journal, while the engine manager owns the launch it applies.
- **Storage**: hardware observations and proofs remain in memory. Durable files
  are workload history (`workloads-history.json`), settings operations
  (`engine-settings-operations.json`), manual configurations
  (`configs/manual-nodes.json`), and the stderr overflow log
  (`logs/nvpair-broker-unsent.log`). Manual replay restores configuration, not
  proof or hardware. Workload flushing remains detached from shutdown and joins
  after worker teardown under its 5-second cap. The error registry remains
  in-memory and repopulates from producer notifications after restart.

## 10. Design Constraints
- **Performance**: a control plane, not a data plane. Broker-local methods answer from memory with no worker round-trip; relayed methods add one process hop. The read loop is never blocked: long operations are asynchronous and any worker call triggered from a reader goroutine is dispatched onto its own goroutine, because the response would otherwise arrive on the very goroutine that is waiting.
- **Scalability**: one broker per node, one client per broker, ten supervised children. Discovery and workload volumes are LAN-scale — tens of nodes, not thousands. High-frequency routing detail stays below the default log level.
- **Reliability**: exponential backoff from 1s to 16s, a budget of 5 attempts per unhealthy streak, and a 60s healthy-reset that clears the crash error and resets the counter. Only the scanner's *first* spawn is fatal; every other startup failure degrades one capability. Crash callbacks drop handles so no stale state is served during a down window, and generation checks stop a replaced process from being mistaken for its successor. Teardown is bounded end to end (§7.1), so a hung worker costs its grace, not the whole shutdown.
- **Security**: cluster-manager owns membership and certificate pins; workers
  own mTLS. Broker passes the shared cluster directory, including to node-info,
  and validates current source proof/provenance before caching or forwarding
  authoritative state. It generates no certificates, performs no pairing, and
  terminates no TLS. Public previews are sanitized display data only. Prompts,
  message/response bodies, PINs, certificates, and keys are never logged.
- **Compliance**: no PII. Logged and relayed payloads carry node ids, host names, service keys, ports, engine and model identifiers, workload ids, and normalized error messages.

## 11. Assumptions
- Exactly one client owns the pipe for the life of the process; on stdin EOF the broker shuts the whole tree down. There is no reconnect, no buffering, and no second-client support.
- The broker's working directory contains the worker binaries, or every worker path is passed explicitly. This is a hard requirement of the resolution rule, not a preference.
- Workers are cooperative children: they speak newline-delimited JSON-RPC on stdio, exit when their stdin closes, and write diagnostics to stderr.
- A worker's JSON-RPC error is meaningful to the client as-is, which is why it is relayed with its original code and message rather than being rewrapped.
- `nvpair-errors` is best-effort; its absence degrades error surfacing to local logs and never fails an operation.
- `nvpair-engine-manager` bounds its own `StopAll` well inside the 5 seconds the broker allows `engine:prepare-shutdown`.
- The parent — the desktop app or `nvpair-tui` — allows at least 15 seconds for the broker to exit after asking it to shut down, which covers the 10-second teardown budget plus the 5-second workload-history flush.
- The per-user data dir is writable; if it is not, workload history runs purely in memory and startup still succeeds.
- `nvpair-cluster-manager` and `nvpair-node-settings` resolve the same cluster directory the broker resolved, because a mismatch would leave a node that looks clustered while no worker reads the tree the manager writes.
- A client treats `app:ready` as "discovery is live", not as "every capability is live", and learns each facade's readiness from its `<engine>-proxy:ready` or a `<engine>-proxy:get-status` poll.

## 12. Failure Modes and Mitigations
- **A supervised worker crashes**: its namespace is unavailable and any state it fed goes stale. → The crash callback drops the handle so status reads reflect reality, emits the sticky `supervisor:subprocess-crashed:<name>` error, and the supervisor respawns after backoff; teardown callbacks clear the worker's derived state (manual claims removed from the discovery store and from the proxy, relay subscriptions dropped, cached handles nulled). Recovery clears the error; for the proxy, the respawn replays the cached priority list, and for the scheduler, the telemetry cache.
- **A worker exceeds its restart budget**: it stays down for the life of the broker process. → The sticky crash error remains in the error list as the standing signal, and its namespace answers `-32000 "<worker> not available"` so callers fail fast instead of hanging. Recovery requires restarting the broker; see §4, which flags the absence of a client-visible "gave up" event.
- **An optional worker's binary is absent**: that capability never exists in this session. → An unset `--*-path` whose default sibling is missing produces one warning and startup continues; the namespace answers `-32000`. An explicit `--*-path` that cannot be stat'ed is fatal instead, because a wrong operator-supplied path should not degrade silently.
- **The scanner fails its first spawn**: the broker cannot do its core job. → `Serve` returns an error and the process exits non-zero; the client sees the broker fail to start rather than a broker that answers `discovery:get-nodes` with a permanently empty list.
- **The client disconnects (stdin EOF or transport close)**: the session is over. → The read loop treats EOF as terminal, cancels the context, and runs the same ordered, bounded teardown as `shutdown`: the proxy stopped first, `engine:prepare-shutdown` next, then every remaining worker's stdin closed and joined. The workload history flusher is deliberately detached from that cancellation so terminal events emitted during teardown are persisted, and is joined (bounded) before exit.
- **Partial startup — some workers up, some not**: the node runs with a subset of its capabilities. → `app:ready` is still emitted; each missing capability is visible as `-32000` on its namespace and, for a crashed worker, as an entry in the error list. Clients must hide operations whose owning worker is absent rather than assume the full surface.
- **`nvpair-errors` is down**: reports and clears have nowhere to go. → Both forwards become debug-logged no-ops, `errors:get-initial` returns an empty array rather than an error so a client can always seed its view, and everything still reaches stderr. Producers re-emit on recovery.
- **`engine:prepare-shutdown` fails, or the engine manager does not answer in 5 seconds**: engines might not have drained. → The broker logs it and falls through to the stdin-close backstop, where the engine manager gets its reserved 6-second grace to finish `StopAll` before it is signalled (never signalled on Windows).
- **A worker does not exit after its stdin closes**: an unbounded join would hang the whole teardown until the parent force-killed the broker, skipping its remaining shutdown work. → Each join is clipped to the shared budget and escalates to terminate and then kill; a process that survives the kill is abandoned so the rest of teardown still runs.
- **The parent stops reading the broker's stderr**: a worker writing to a full pipe blocks in `write()` and can never exit. → Every worker's stderr goes through its own pipe into a bounded in-process sink (4096 chunks, 4 MiB) whose writes never block. Output that does not fit, and whatever is still queued after a 2-second drain at shutdown, spills to `logs/nvpair-broker-unsent.log` instead of being lost or blocking exit.
- **A replaced proxy's frames arrive after respawn**: stale readiness or errors could reconfigure the new process. → Generation checks in `forwardProxyNotificationForGeneration` and `forwardLMStudioProxyNotificationForGeneration` discard them before any side effect. The llama.cpp forwarder has no generation check, but none of its notifications changes a port, so a stale one reaches only `llamacpp-proxy:` subscribers or the errors pipeline.
- **The compatibility port is held by an unidentified process**: the managed facade cannot be claimed safely. → The broker refuses to kill an owner it cannot identify, blocks the facade with a specific reason, and surfaces the sticky `<engine>-proxy:port-ownership-blocked` error; the facade comes up on an explicit fallback instead.
- **No cluster directory resolves**: the node can neither join nor serve a cluster. → One explicit warning at startup names the condition; this line is the single diagnostic that distinguishes a healthy-looking but isolated node from a working one. Clustered features simply never engage — there is no silent fallback to plain HTTP.

## 13. Observability
- **Logging**: structured `slog` to stderr via `nvpair-shared/applog`, with every worker's stderr carried through unmodified so one stream holds the whole tree. The broker and every worker write into one non-blocking sink that forwards to the broker's real stderr; output the parent does not read in time goes to `logs/nvpair-broker-unsent.log` in the per-user data dir rather than being dropped. The level comes from `--log-level` or `$NVPAIR_LOG_LEVEL` (default `info`) and is fanned out to every worker at spawn and on a live `log/set-level`. Key events: resolved cluster directory, each worker's path and pid at spawn, crash and recovery with the attempt number, relay failures, subscription changes at debug, port-ownership decisions and their reasons, cluster identity restore, identity and pin-set convergence, each workload retired after origin silence with how long that origin was quiet, the engine-shutdown deadline and every worker join that needed a terminate or kill, and every dropped-notification case at debug. Prompts, message content, request and response bodies, PINs, and certificates are never logged.
- **Metrics**: none. There is no metrics endpoint, no counter export, and no `supervisor:get-status` method. Operational state is inferred from the error list (`errors:get-initial` / `errors:update`), from each `<engine>-proxy:get-status`, from `engine:get-settings`, and from `-32000` responses on an unavailable namespace.
- **Alerts**: the errors pipeline is the alerting surface. `supervisor:subprocess-crashed:<name>` is the standing signal for a down worker (`nvpair-proxy` for the proxy process), `ollama-proxy:port-ownership-blocked`, `lmstudio-proxy:port-ownership-blocked`, `ollama-proxy:ollama-host-alias-blocked`, and `ollama-proxy:port-bumped` for port contention, `<engine>-proxy:port-ownership-blocked` for every engine, llama.cpp included, when the `nvpair-proxy` binary does not resolve, and each worker's own `errors:report` ids for its domain. All are sticky and cleared by the recovery path, so the client's error list is a live health view rather than a log.

## 14. Sample usage
The desktop app launches `nvpair-ui-broker` with its working directory set to the installed `bin/` folder and speaks JSON-RPC over the child's stdio. The broker resolves each worker binary next to itself, logs the cluster directory it resolved, loads the persisted workload history, and starts the tree: the scanner first (synchronously — a failure here would abort startup), then `nvpair-errors` with `--peer-sync`, then `nvpair-node-settings` and `nvpair-engine-manager`, whose combination lets it read the `force_ports` policy and Ollama's live status and decide that `:11434` can be claimed for the proxy while the stopped engine moves to `:11435`. `nvpair-node-info` comes up and is registered as `ni` on port 14318; `nvpair-proxy` starts and the broker enables its Ollama facade on `:11434`, its LM Studio facade on `:1234`, and its llama.cpp facade on `:8080`; then the workload manager, manual nodes, cluster manager, and job scheduler, the last seeded with the telemetry cache. Before the cluster manager is considered started, the broker reads the persisted cluster id out of settings and injects it via `cluster:set-identity`, so identity is already correct when the client asks.

The broker writes `app:ready`, then attaches the discovery change hook. A fresh
`discovery:subscribe` returns `{"subscribed":true}` followed by a current node
baseline with any available full hardware in `info`. Scanner hardware updates
and manual snapshot pushes keep the client's observations current.
Queries return the same cached observations with elapsed age. Engine/proxy
subscriptions remain separate; facade subscriptions replay their last readiness.

The user starts an engine. The client sends `{"jsonrpc":"2.0","id":12,"method":"engine:start","params":{"engine":"ollama"}}`. The broker checks that no compatibility-port gate is pending, forwards the method verbatim to `nvpair-engine-manager` with no timeout, and goes back to serving other requests. While the engine boots, the manager pushes `engine:state-changed`, which the broker re-emits verbatim to the subscribed client; when readiness lands, the original response is delivered against id 12. The engine coming up makes the advertiser register `ol` with the **facade's** listen port so peers route to the facade rather than to the engine. Moments later `nvpair-job-scheduler` emits the node-wide ranking once each for `ollama`, `lmstudio`, and `llamacpp`; the broker keeps the first, drops the identical repeats, and delivers `node/set-priority` to the proxy process once, on its own goroutine.

An inference request arrives at the proxy and produces `workload:submitted`; the broker stamps the local node id, applies it to the workload store, forwards it to `nvpair-workload-manager` for cluster broadcast, and emits `workloads:upsert` to the subscribed client. When the engine later reports a failure, the manager emits `errors:report`; the broker fills in `nodeId` and `timestamp`, forwards it to `nvpair-errors`, and relays the resulting `errors:update` snapshot to the client — which needs no subscription for that stream.

The user quits. Broker acknowledges `shutdown`, stops proxy ingress, and waits
up to 5 seconds for `engine:prepare-shutdown`. It then closes and joins the
remaining successfully started workers concurrently inside the shared teardown
budget. The detached workload flusher captures terminal events and finishes
after those joins, before `shutdown complete`.

## 15. Process model, CLI, and build wiring

The broker is a single Go binary that runs as a child of its client and as the parent of every other worker. It is the backend entry point of the bundle: nothing else in the product starts a worker.

| Flag | Default | Purpose |
|------|---------|---------|
| `--ipc` | *(empty — use stdio)* | Unix domain socket path or Windows named pipe to dial instead of stdin/stdout |
| `--scanner-path` | `./nvpair-node-scanner` in the working directory | required discovery worker |
| `--errors-path` | `./nvpair-errors` | service-error datastore |
| `--settings-path` | `./nvpair-node-settings` | durable node settings |
| `--engine-manager-path` | `./nvpair-engine-manager` | engine lifecycle and model operations |
| `--node-info-path` | `./nvpair-node-info` | local hardware advertisement |
| `--proxy-path` | `./nvpair-proxy` | the one proxy process that hosts every engine's facade |
| `--proxy-engines` | every engine in `nvpair-shared/engines` (today `ollama,lmstudio,llamacpp`) | which engines to enable a facade for; an unrecognized name is fatal, and an engine left out is neither enabled nor prepared |
| `--workload-manager-path` | `./nvpair-workload-manager` | cluster workload relay |
| `--manual-nodes-path` | `./nvpair-manual-nodes` | user-entered nodes |
| `--cluster-manager-path` | `./nvpair-cluster-manager` | identity, pairing, membership |
| `--scheduler-path` | `./nvpair-job-scheduler` | node ranking for the proxy |
| `--cluster-dir` | the per-user `Nvidia Corporation/Personal AI Router` `cluster/` directory, else `cluster/` next to the executable | passed to the cluster-scoped workers so they can do their own mTLS |
| `--log-level` | `$NVPAIR_LOG_LEVEL`, else `info` | `debug`\|`info`\|`warn`\|`error`, fanned out to every worker |
| `--version` | — | print the component version and exit |

Each `*-path` default is the same-named binary (with `.exe` on Windows) in the broker's **working directory**. There is deliberately no `PATH` lookup and no search of adjacent directories: a confusing match against a stale build is worse than a clear not-found error. An explicit override that cannot be stat'ed is fatal; a missing default is fatal only for the scanner.

Transport is stdio unless `--ipc` names an endpoint to dial. In stdio mode the client owns both pipes, and stdin EOF is the disconnect signal: the read loop treats EOF as terminal, cancels the context, and runs the ordered teardown. SIGINT and SIGTERM cancel the same context, and the read is run on its own goroutine selected against cancellation so a signal can never leave the process parked in a blocking read.

Each worker is spawned in its own process group — `Setpgid` on Unix, `CREATE_NEW_PROCESS_GROUP` with `CREATE_NO_WINDOW` on Windows — so a terminal's Ctrl+C reaches the broker but not the workers directly. A worker therefore exits only when the broker stops it, which keeps an orderly shutdown from being misread as a crash to restart; if the broker dies abruptly, workers still exit on the EOF of their closed stdin. Each worker's stderr is its own pipe into the broker's stderr sink (§12).

Electron starts **only** this binary from the worker tree, from the app's `cli-bin` directory with every worker's `--*-path` passed explicitly, and reaches every capability through it; no worker is ever launched by both Electron and the broker, and there is no broker-absent path. `nvpair-tui` is a peer client, not a supervised worker: it spawns and supervises its own `nvpair-ui-broker` child (`--broker-path`, defaulting to the sibling binary) and speaks the same contract described here. Each front end also launches the Inference Demo's `inference-dispatcher` itself; that binary is an ordinary HTTP client of the local facades, not a worker, and the broker neither spawns nor sees it.

Build and staging: `services/build.sh` and `services/build.bat` read the component version out of `services/versions.json`, build the broker with that version stamped into `main.Version`, and stage it into `services/build/bin/` alongside the eleven other component binaries and `inference-dispatcher`. `services/installer/nvpair-setup.nsi` (with the `linux/` and `macos/` counterparts) packages that bundle, installs the broker as the process the graphical UI and the TUI launch, and owns the firewall rules for the workers that actually listen — the broker itself needs none, because it opens no socket. When a JSON-RPC contract on any relayed namespace changes, the producing Go service, the Electron bridge under `desktop/src/electron/service-bridge/`, and `npm --prefix desktop run service-contracts:check` must all be updated in the same change.
