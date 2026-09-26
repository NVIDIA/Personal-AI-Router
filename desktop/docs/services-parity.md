<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Services Capability Status

This document records the current integration status between Personal AI Router
and the `services/` backend. It is a present-state reference, not a release
history.

- Backend versions: `services/versions.json` (rendered in `docs/services-api.md`)
- Method-level contract: `docs/services-api.md`
- Exceptions: `docs/service-contract-exceptions.json`
- Integration runbook: `docs/services-backend.md`

## Status summary

| Domain                     | Status                          | Current state                                                                                                                                   |
| -------------------------- | ------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| Process supervision        | Complete                        | Electron starts only `nvpair-ui-broker`; the broker supervises all workers                                                                      |
| Discovery                  | Complete                        | Broker discovery snapshots drive available nodes and node state                                                                                 |
| Node telemetry             | Integrated with direct poll     | Electron polls advertised `/v1/node-info` (plain HTTP) for identity-bound OS and telemetry; unreachable peers remain telemetry-limited          |
| Manual nodes               | Complete with local persistence | Broker owns probing and proxy registration; Electron persists entries for replay                                                                |
| Ollama routing             | Complete                        | Broker relay and backend scheduler drive proxy routing                                                                                          |
| LM Studio routing          | Complete                        | Parallel broker relay and scheduler path                                                                                                        |
| llama.cpp routing          | Source integration; native validation pending | `nvpair-proxy` facade on llama.cpp's own `8080`, relayed as `llamacpp-proxy:` |
| Local engine lifecycle     | Complete                        | Install, start, stop, uninstall, update, and port configuration                                                                                 |
| Remote engine lifecycle    | Partial                         | Remote install, start, stop, status, and model pull are supported                                                                               |
| Engine models              | Partial                         | Core list, pull, load, unload, and supported delete actions are wired                                                                           |
| Engine version metadata    | Backend-limited                 | The backend status surface does not provide a complete version/install-owner/update contract                                                    |
| Errors                     | Complete                        | Broker-relayed registry with peer synchronization                                                                                               |
| Workloads                  | Complete after subscription     | Local and peer workload events feed one Electron catalog                                                                                        |
| Cluster pairing            | Complete                        | PIN pairing, identity, membership, leave, and removal                                                                                           |
| Cluster transport security | Backend-owned                   | Node-to-node transport security, including the proxies' cluster-mTLS inference ingress, is entirely backend; Personal AI Router implements none |
| Settings                   | Partial                         | Cluster identity plus per-engine ports and engine arguments, local and remote; inert backend settings are not surfaced                            |
| Model catalog search       | Electron-owned                  | Curated Ollama and LM Studio catalogs are fetched in Electron main                                                                              |
| Device bootstrap           | Integrated contract             | Desktop and TUI share target-produced plans, recovery, and receipts for the exact Windows/macOS/Debian-or-Ubuntu × x64/arm64 matrix; native matrix acceptance is not asserted |

## Supervision

`nvpair-ui-broker` is the sole Electron child. It owns:

- `nvpair-proxy`, one process hosting a facade per enabled engine, under one
  supervisor;
- `nvpair-node-scanner`;
- `nvpair-node-info`;
- `nvpair-manual-nodes`;
- `nvpair-workload-manager`;
- `nvpair-cluster-manager`;
- `nvpair-node-settings`;
- `nvpair-engine-manager`;
- `nvpair-errors`;
- `nvpair-job-scheduler`.

Worker crashes are handled by the broker. Electron reports a broker crash and
allows the user to restart the service.

`nvpair-host-bootstrap` and `nvpair-host-helper` are bundled tools, not workers.
Electron and the broker never supervise them.

## Device bootstrap

The complete bootstrap channel set is wired through the service bridge:
catalog, controller public keys, inspect, review, apply, status, recover, and
verify. Desktop **Set up a device** and TUI **Setup** consume the same methods
and canonical types.

Current behavior:

- Quick Connect and Zero Touch select the reviewed and enterprise lanes of the
  same target-local contract.
- The catalog contains exactly Windows amd64/arm64, macOS amd64/arm64, and
  Linux amd64/arm64. The Linux target then admits Debian and Ubuntu only.
- Auto resolves to Desktop on Windows/macOS and Headless on Linux. Explicit
  roles must match one resolved owner.
- Desktop ownership excludes the fixed headless owner. Headless uses the
  Windows fixed helper wrapper, macOS system launchd, or Linux system service.
- The helper has a closed action protocol on an ACL-protected local pipe or
  socket. It exposes neither a shell nor a network listener.
- Inspect/status/recover state and the complete receipt come from the target.
  Electron accepts apply and verify only for an exact plan returned by the
  current review.
- Repair is restricted to receipt-and-marker-proven owned drift. Foreign state
  blocks mutation. Target-local uninstall requires the original request and
  receipt and removes only proven-owned resources.
- Passwords, passphrases, elevation passwords, and private key bytes are
  volatile access material. Durable bootstrap identity is public-key-only.
- SSH host-key observation and explicit fingerprint approval remain mandatory
  after bootstrap.
- Official provenance fails closed on incomplete catalog/checksum/signature
  metadata. Unsigned engineering artifacts stay explicitly non-official.

The current source and local tests establish strict contracts, deterministic
packaging, cross-compilation, and simulated platform behavior. They do not
establish that official signed combinations have been produced, nor do they
constitute native acceptance of all six targets.

## Discovery and nodes

Personal AI Router consumes `discovery:get-nodes` and `discovery:nodes-changed` from the
broker. The bridge:

- keys every node by the backend's stable per-host UUID (`AvailableNode.hostUuid`),
  the same identity workloads (`originatedFrom`/`scheduledOn`), errors (`nodeId`),
  proxy routing (`Node.ID`), `/v1/node-info` (`hostUuid`), and cluster membership
  (`ClusterNode.nodeUuid`) all use; the hostname is display only;
- preserves the backend's canonical reachable address;
- reflects the discovery `trusted` (locally pinned peer) and `clustered` (belongs
  to some cluster) flags, and uses `clustered` to mark an already-clustered
  discovered node non-invitable. `trusted` is a display annotation the scanner
  re-derives whenever the cluster-manager reports a pin-set change, and again on
  its periodic sweep of each peer's reported membership; it is not the routing
  gate. Each proxy answers "do I hold a pin for this peer?" from its own
  mesh when it resolves candidates, so PAIR must never treat `trusted` as
  authoritative for whether work can reach a node;
- merges per-engine proxy presence;
- carries flat and per-engine remote model inventories;
- emits node and discovery pushes to the renderer.

`selfId` is the cluster-manager's `nodeUuid` (from `cluster:get-node-id`), minted
at startup and resolved as soon as the broker is ready, so self is identified by
the same UUID key rather than a hostname guess.

The broker discovery surface does not include full dynamic telemetry, so
Electron polls `/v1/node-info` for CPU, memory, GPU, and VRAM values.

One limitation follows from the current discovery and node-info contract:

- **Remote telemetry.** The poll is plain HTTP. A remote node that does not
  answer `/v1/node-info` over plain HTTP contributes no live CPU/GPU/VRAM
  telemetry; Personal AI Router still shows its discovery-level data. This resolves when the
  backend exposes a plaintext metrics path for such nodes.

`/v1/node-info` also reports the target's canonical operating system. Electron
accepts that value only from an identity-matched response and retains it across
broker/proxy refreshes. Older or offline peers remain unknown instead of being
shown as Windows; an explicit remote Engine Manager `installSupported` result
remains authoritative for install controls when that legacy OS field is absent.

Manual nodes use the broker's `node/add`, `node/remove`, and `nodes/list`
surface. Electron persists user entries and replays them after broker startup so
they survive worker restarts.

### Workload display

The desktop displays workload snapshots and live updates, retaining origin,
engine, proxy run and request identity. Execution labels and connection lines
use the reported destination, not the request origin. Workload cancellation is
not exposed by the desktop API or UI. Engine lifecycle and model-download
cancellation are separate controls and remain supported.

### Multi-node UI acceptance

Engine integration must preserve each participating desktop's view of the
cluster, not only the request origin's view. During the same bounded inference
run, verify every available participating desktop independently:

- Cluster and member UUIDs, engine availability and loaded-model ownership agree
  after discovery converges; offline members do not look live or routable.
- New workloads agree by their full `(originatedFrom, engine, runId, id)`
  identity on model, destination and terminal state. Record propagation delay;
  do not require identical historical catalog totals or instantaneous equality
  during a state transition.
- Capture each desktop's original Performance view during actual work. A remote
  backend response or the origin's aggregate UI does not prove another native UI.

Record unavailable or untested desktop cells explicitly. This checklist states
the acceptance requirement; it does not assert that every platform has passed.

## Routing and inference

Every text-engine facade is broker-owned and cluster-aware. They live in one
`nvpair-proxy` process, each enabled after spawn on its own port, and each
serves its engine's dialect:

- the Ollama facade serves the Ollama-compatible surface;
- the LM Studio facade serves the LM Studio/OpenAI-compatible surface;
- the llama.cpp facade serves its OpenAI-compatible surface on `8080`.

Sharing a process is what lets them share the burst reservations the scheduler
depends on: facades bursting at once compete for the same node's GPU, so a
dispatch through any of them has to be visible to the others.

Routing precedence is manual selection, scheduler priority, then deterministic
proxy ordering. Personal AI Router leaves proxies in automatic mode.

`nvpair-job-scheduler` combines total queued and running workload across all
engines with a smoothed 0–3 GPU-pressure signal. The backend scanner and manual
node worker provide maximum-GPU utilization, while invalid, missing, or
older-than-10-second samples receive neutral pressure. The scheduler emits order,
pending count, and pressure; the broker forwards each `schedule:priority`
snapshot to the matching proxy through `node/set-priority`. Each proxy adds
local reservations, so its estimate is
`pending + gpuPressure + localReservations` during concurrent bursts.

Current limitation: pressure does not represent GPU capability or available
VRAM, and a multi-GPU node is represented by its busiest device rather than
engine-to-device affinity.

### Secure inference transport

An NVPAIR-launched engine binds to loopback only; it is never directly
LAN-reachable. Each node's proxy exposes two personalities on one listener: a
loopback-only plaintext path for local clients, and a LAN ingress gated by
cluster mTLS that forwards trusted-peer requests to the loopback engine. Because
the engine port is private, discovery advertises the **promoted proxy port** for
`ol`/`lm`, and the peer's real engine port is knowable only from authoritative
`engine:remote-get-installed` facts.

Personal AI Router consequences (all reflection, no security implementation):

- The proxy `node/*` presence port is the peer's promoted proxy port. Personal AI Router
  surfaces it as `EngineStatusData.proxyPort` and never labels it as the engine
  port; a remote engine's port comes only from `ec` facts.
- Because both peers must speak the mTLS channel, mixed-version clusters cannot
  run inference across the version boundary. Local use and the shared
  nearby-model list are unaffected.

### Serving groups and the fast fabric

A managed vLLM serving group places each rank on the one private subnet that
every selected member shares, which on DGX Spark is the management network.
Pipeline-parallel groups of two or three Sparks serve over that network.
Tensor parallelism is admitted only when the model's attention and KV heads
divide by the member count.

An ordinary two-Spark tensor-parallel group runs only over an active, freshly
qualified direct fabric between exactly those two Sparks. Its reviewed plan
binds one reciprocal fabric lane, and only NCCL Socket uses it
(`NCCL_NET=Socket`, `NCCL_IB_DISABLE=1`, `NCCL_SOCKET_IFNAME` set to the lane
interface). The master address, `VLLM_HOST_IP`, Gloo, control and SSH stay on
the management network, and the rank gains no RDMA device or `AF_IB` socket.
Without an explicit mode, two Sparks use that lane when the fabric is present
and fall back to pipeline parallelism when no fabric involves them; a stale,
recovering or ambiguous fabric (including the three-Spark ring) refuses the
review instead. Explicit pipeline parallelism never depends on a fabric.

Such a group leases its fabric before any rank starts. Fabric rollback and
recovery are refused while the lease is held, and the lease is released only
after every rank's cleanup is confirmed; it is retained across Engine Manager
restarts. Ordinary ranks on the managed runtime bound the NCCL and Gloo
process-group timeouts to 180 seconds. Only the Qwen3.8 profile binds its
ranks to qualified RoCE fabric lanes.

Copying a model to missing Sparks also uses the fabric when one links the source
and target: the controller re-proves the fabric, gives the target the source's
end of one reciprocal lane, and journals a hold that refuses fabric rollback and
recovery until the target confirms that copy ended, including across a
controller restart. The target still authenticates the source by its pinned
identity. With no fabric linking the pair the copy uses the management network,
and a stale or unprovable linking fabric refuses the copy. The copy's progress
row says which network it uses. One source serves copies to several targets at
once: each copy's content verification runs outside the source's export lease,
so it never stalls chunks already flowing to another target.

Serving-group review re-hashes the retained model on every member; members
inspect concurrently, so a Qwen3.8 review takes about one member's
verification time rather than the sum.

The Qwen3.8 checkpoint holds 65.6 GiB of expert weights and 57.9 GiB of shared
weights. Its one layout, TP2+EP2 on exactly two Sparks, splits both, about
62 GiB per rank, and serves over the two RoCE lanes of one direct cable. No
three-Spark layout fits: the model's two key-value heads cannot split three
ways, a data-parallel layout would copy the shared weights to every rank (about
80 GiB each) and does not load on a 128 GB DGX Spark, and the pinned vLLM
refuses pipeline stages because the model's N-gram PLE embedding needs raw
input IDs on every rank. Review refuses a three-Spark selection with that
reason before inspecting any member. Qwen3.8 ranks pass
`--gpu-memory-utilization 0.8`: DGX Spark GPU memory is shared with the OS and
page cache, and vLLM's default 0.92 startup check fails whenever a few GiB are
cached.

Managed vLLM installs only on Linux amd64 and arm64. Windows reports that no
native recipe exists and names the missing WSL2 gate: PAIR does not yet own an
explicitly selected user-owned distro, its child Engine Manager, path/endpoint
bridging, or cancellation. Detecting or installing WSL alone therefore does not
make managed vLLM available. macOS remains unsupported. Serving groups also
need a non-root Engine Manager on Linux with the systemd system owner, and
report activation as unavailable otherwise. Hardware acceptance covers Linux
arm64 (DGX Spark) only; Linux amd64 and WSL are not natively validated.

The Setup card owns the zero-to-review model journey without handling payload
bytes or credentials. It pulls one immutable public
`owner/repository@40-character-commit` snapshot on a selected stopped managed
node, records the model license/notices, prepares the fixed Qwen3.8 runtime
locally or through the paired remote control plane, and asks each missing target
to pull receipt-bound 32 MiB chunks directly from the selected source over
cluster mTLS. Every target reviews capacity, checkpoints exact prefix hashes for
resume, verifies per-chunk/full-file/final receipt hashes, and promotes through
the same retained model owner. Progress and cancellation stay bound to one exact
operation; model paths and bytes never enter the renderer or broker.

For Qwen3.8, Review and Start remain disabled until every selected node reports
the exact model/runtime and the same qualified provider closure. The UI shows a
compact fleet matrix for PAIR, Engine Manager, vLLM, provider, and model
revisions, leaving unavailable peer component revisions explicitly unknown.
Already enrolled peers can enter the existing typed multi-target PAIR package
inspection/review/update operation from that matrix. Provider drift links to
NVIDIA's DGX Dashboard update guide. The renderer never runs package-manager or
operating-system commands; a DGX OS update remains separate maintenance.

A group is published ready only after the coordinator's broker confirms that
the local proxy lists the coordinator among the model's inference owners;
until then the coordinator reports itself as routing so the broker can
advertise it. A group that the proxy does not route within 60 seconds fails
and cleans up. The served-model inventory the scanner refreshes every 15
seconds reads vLLM status without queueing behind lifecycle operations; when
another status refresh holds the lock it shares that refresh's result instead
of omitting the served model, which would otherwise withdraw the route for a
whole refresh interval. Fabric admission requires every participant's managed engines
to be disabled and idle; a refusal currently surfaces only as a generic
preparation failure.

The fabric review always requires the read-only administrator inspection when
it is available, because only that inspection can bind NetworkManager's
generated default profiles for pause consent. On consented ports, apply
tolerates NetworkManager replacing its generated DHCP attempts, and pauses the
attempt that is pending at pause time. Rollback of an applied operation
re-accepts each node's current SSH candidate only for the host-key fingerprint
approved for that node in the operation, so it survives the Engine Manager
restart of a package update. It still needs fresh temporary account access,
and every participant's installed worker must match the controller's running
image or a verified PAIR package, so update peers through PAIR after updating
the controller. Before review, the fabric card asks Engine Manager for a bounded
projection of fresh node-info physical-port facts for one to three paired nodes.
It automatically prefers an exact three-node p0/p1 ring, then an exact two-node
direct pair, renders the expected wiring, and names missing, duplicate, or
misplaced endpoints/edges. Manual switch and port identities are available only
under Advanced fallback. The projection carries no routes, addresses, commands,
credentials, or mutation authority; only the finite cable run proves reciprocal
peer edges. On mount the fabric card adopts the newest operation whose
cleanup is unconfirmed from `engine:fabric-retained-operations`, so an applied
fabric and its rollback stay visible after Desktop restarts or when another
client applied it.

## Engine lifecycle

`nvpair-engine-manager` is authoritative for installed, running, healthy, and
port state.

Personal AI Router supports local:

- install and uninstall;
- start and stop;
- managed update;
- engine and proxy port changes;
- desired-state restoration across app restarts;
- engine and model progress.

Managed update covers Ollama and LM Studio. llama.cpp has no managed update;
the bridge refuses `update` for it instead of substituting an uninstall and
reinstall.

Before shutdown, Personal AI Router calls `engine:prepare-shutdown`. This stops managed engine
processes without changing the persisted desired state; the broker restores
enabled engines on the next launch. The broker also self-initiates
`engine:prepare-shutdown` before tearing down its workers and waits for each
worker to exit without force-killing the worker, so engines are not orphaned
during teardown. Stopping a managed engine itself sends one stop signal and
waits for it to exit with no timeout: SIGTERM to the process group on Unix
(never escalated to SIGKILL) and `taskkill /T /F` on Windows (its windowless
engines cannot receive a graceful close).

`engine:stop` records the OFF intent even when it returns an error. The backend
reclaims an orphaned managed engine left on its own port, and declines only a
genuinely foreign listener (with an actionable error). Personal AI Router treats the saved
desired state as authoritative and surfaces a stop error as guidance, not as a
sign the OFF choice was discarded.

For ease of use, Personal AI Router issues starts in two additional cases and
keeps no auto-start list of its own: every install sends `engine:install` with
`start: true`, so a successful install (or managed update) starts the engine;
and on the first app open Personal AI Router starts every already-installed local
engine once. Both paths rely on the backend recording desired-enabled as a side
effect of start, so `nvpair-engine-manager` remains the single owner of
desired-state persistence and restoration.

Personal AI Router synthesizes one centralized pending-action state while waiting for
terminal backend notifications. It never treats optimistic state as
authoritative.

Remote cluster support includes status, install, start, stop, model pull, and
remote model load, unload (eject), and delete via the `ec` surface. Uninstall
and update remain local-only.

Server port, proxy port, and engine arguments are one authoritative record owned
by the node running the engine, read and written through the broker's
`engines:{get,preview,apply}-settings` channels. A request naming another node
is relayed to that peer, so a clustered device is edited like the local one. The
one exception is managed CORS origin settings: the owning node rejects a change to
browser access policy relayed from a peer. Other environment assignments pass
through literally, with no maintained catalog of engine options or variables.

## Models

Local model state comes from engine-manager actions. Remote model state comes
from node-scanner enrichment and is attributed per engine.

Personal AI Router uses:

- `list_models`;
- `pull_model`;
- Ollama `run_model`, `unload_model` (`keep_alive: 0`), and `delete_model`;
- LM Studio `load_model`, `unload_model`, and `delete_model` (`remove_path`);
- llama.cpp `load_model`, `unload_model` (router `/models/load` and
  `/models/unload`, settled on observed residency), `delete_model`,
  `pull_model` and `import_model` (managed cache builtin), and `cancel_pull`.

All three engines expose Load, Eject, and Delete in the model manager when the
backend action exists. Keep-alive / expiry controls remain unsupported.

LM Studio's `delete_model` declares `restart_after`, so the engine manager
restarts a running LM Studio once the files are removed — its `/v1/models` is
served from an index built at startup and it exposes no rescan operation, so
clients would otherwise keep being offered a deleted model. Because that makes
Delete interrupt inference, `EngineCapabilities['lm-studio'].restartsOnModelDelete`
tells `ModelManager.tsx` to confirm the deletion first (`ConfirmModal`). The
restart is entirely backend-owned: PAIR sends the same `deleteModel` command as
for any other engine and never issues `engine:restart` itself, so the bundled
`nvpair` terminal UI and a remote peer's deletion get the same behavior.

**This is LM Studio only.** Ollama reflects a deletion immediately, so its
manifest omits `restart_after` and its capability entry omits
`restartsOnModelDelete`: no bounce, no confirmation, no interrupted inference.
Those two facts have to stay in step across a Go manifest and a TypeScript
constant, which nothing in either type system enforces — so
`tests/modular/delete-model-restart.test.ts` reads the shipped manifests and
asserts the pair agrees, and `TestBundledManifestsRestartOnlyLMStudio` guards the
same thing from the Go side.

Several model-action timeouts have to be ordered correctly. For a
restart-backed delete, the reply is withheld until the engine is ready again
(LM Studio's `ready.timeout_s` alone is 60s), so
`MODULAR_MODEL_ACTION_TIMEOUT_MS` anchors the delete-specific observers. Ollama
Load uses a separate 10/11-minute response-header budget:

| Budget                                                        | Value | Why                                                                                                                                                                                                                           |
| ------------------------------------------------------------- | ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MODULAR_MODEL_ACTION_TIMEOUT_MS` (`modular-runtime.ts`)      | 120s  | The RPC must outlast a full stop + readiness-probed start.                                                                                                                                                                    |
| `RESTART_DELETE_TIMEOUT_MS` (`pending-actions.store.ts`)      | 180s  | The optimistic-spinner safety net must outlast the RPC, or it expires mid-flight and drops the spinner while the delete is still running.                                                                                     |
| `engineResponseHeaderTimeout` (`executor.go`)                 | 30s   | Downloads and ordinary local HTTP actions retain a prompt response-header bound; probe contexts stay shorter.                                                                                                                 |
| `ollamaLoadResponseHeaderTimeout` (`executor.go`)             | 10min | Only Ollama's local `run_model` action gets the cold-load allowance.                                                                                                                                                          |
| `OLLAMA_LOAD_PENDING_TIMEOUT_MS` (`pending-actions.store.ts`) | 12min | Ollama's Load control stays locked beyond the remote path's 11-minute header budget, while backend success or failure still clears it immediately.                                                                            |
| `PENDING_TIMEOUT_MS`                                          | 60s   | Unchanged for every other command, including an Ollama delete.                                                                                                                                                                |
| `remoteReadyResponseHeaderTimeout` (`remoteclient.go`)        | 11min | Remote start/delete responses can wait on engine readiness, and a cold Ollama model load can withhold headers for minutes, so those calls use the readiness-sized client. Other model actions retain the ordinary 30s budget. |

The net is a backstop, not the mechanism: a delete now ends the spinner on real
backend truth either way. On success the refreshed `list_models` drops the model;
on failure the reported error carries `nodeId` + `engineType` + `modelName`, which
is what lets the store attribute it and clear that row.

Local Ollama Load also observes its eventual JSON-RPC rejection and reports the
same model context; the resulting attributed `errors:update` clears `Loading…`
without waiting for the 12-minute net.

A failed restart is reported as `Deleted <model>, but <engine> failed to
restart: …`, never as a failed delete — the files really are gone, and telling
the user otherwise invites a retry that hits `model … not found on disk`. The
bounce itself raises no crash or unhealthy alert: `markStopped` cancels the
health loop and clears `unhealthy`/`exited`, and `doStart` clears them again, so
the engine simply shows as stopped and then running.

A local `pull_model` streams live download progress: the engine-manager routes
`engine:action{pull_model}` through its streaming pull path and emits
`engine:pull-progress` (`{ engine, op, stage, percent, message }`) — the local
counterpart of `engine:remote-progress`. Personal AI Router consumes it in
`applyLocalEngineProgress` (`modular-supervisor.ts` → `modular-state.ts`),
backfilling the dispatched model (the frame carries none) and advancing the
optimistic pull entry's percent in place, so a local pull shows "Pulling · N%"
to completion just like a remote pull. The awaited action response owns
completion (clearing the entry and refreshing the model list); a CLI-driven pull
(LM Studio) emits a single `pulling` marker and degrades to the indeterminate
spinner.

`engine:models` (and the `em` `GET /v1/models` surface) returns the flat served
model union, the served per-engine breakdown (`modelsByEngine`), the per-engine
set loaded in memory (`loadedByEngine`), and a separate downloaded catalog
(`retainedByEngine`, currently bounded PAIR-owned vLLM receipts). Retained
entries never join the served union or proxy routing candidates. The engine-manager watches each
running engine's loaded set and pushes `engine:models-changed`
(`{ engine, models }`, the full `engine:models` shape) on explicit load/unload,
LM Studio JIT auto-load, and TTL/idle eviction. Discovery carries
`loadedByEngine` onto each node's `AvailableNode` so remote cards reflect
residency too, and carries `retainedByEngine` independently for stopped managed
model selection.

Personal AI Router consumes both: `parseBrokerNode` records `loadedByEngine` and
`retainedByEngine` on every node
(local via loopback self-enrichment, remote via the peer's enrichment), and
`applyLocalLoadedModels` (`modular-supervisor.ts` → `modular-state.ts`) applies
the `engine:models-changed` snapshot to the local node immediately. Model rows
are stamped `ModelItem.status: 'loaded'` when a name is in that engine's loaded
set, which drives the loaded dot, disables **Load** for a resident model, and
gates **Eject** to loaded models only (`ModelRow.tsx`). The optimistic
load/eject pending-action clears on the resulting model patch instead of its
safety-net timeout (`pending-actions.store.ts`). Loaded state carries no
`sizeVram`/`expiresAt` — the backend delivers the simpler `loadedByEngine`
name-set, not structured details. A stopped managed vLLM drops any stale live
cache and exposes its retained catalog as downloaded, idle rows to the exact
model selector; other stopped engines keep an authoritative empty list.

The model hub is intentionally outside the backend: Electron main fetches
curated catalogs and sends selected pull-ready IDs to the engine manager.

## Errors

`nvpair-errors` is broker-owned and is the authoritative error registry. Personal AI Router:

- fetches `errors:get-initial`;
- consumes full `errors:update` snapshots;
- forwards clear and report actions through the broker;
- uses engine operation metadata to clear matching pending actions;
- renders supported retry hints.

Peer error synchronization and authentication are backend-owned.

## Workloads

Personal AI Router subscribes to the broker workload stream and maintains an Electron-local
catalog keyed by workload origin and ID.

`scheduledOn` identifies the execution node. Workloads that have not yet been
scheduled are not attributed to a node.

The broker backs workloads with a durable, order-independent store and answers
`workloads:get-initial` with an authoritative baseline (current plus
recently-terminal jobs), and the workload-manager backfills a joining node with
peers' in-flight work and periodically re-syncs. This makes cluster job counts
converge across restarts and network hiccups on the backend side. Personal AI Router's
`workloads:get-initial` handler fetches that broker baseline (falling back to its
own subscription-built catalog if the call fails), and the supervisor seeds the
catalog from it right after `workloads:subscribe`, so a freshly started or
restarted app immediately shows cluster-wide in-flight jobs. The renderer store
subscribes before fetching the baseline so a transition during init is never
dropped.

## Cluster identity and membership

`nvpair-cluster-manager` owns:

- node crypto identity;
- interactive six-digit PIN pairing;
- trusted certificate pins;
- membership snapshots;
- cluster leave and member removal;
- trust endorsements, roster reconciliation, and durable admission-bound
  removal proofs.

Personal AI Router creates a cluster automatically when the first outbound invite requires
one. A failed invite can abandon that unused solo cluster. The UI displays the
inviter's PIN and requires the invitee to enter it.

The cluster-manager emits `cluster:invite-received` exactly once per inbound
invite (always `pending`) and exposes no list-pending-invites RPC. To give the
receiver a durable, recoverable set of pending invites, Electron main accumulates
them (`ModularState`), prunes them as they resolve — via membership
(`nodes:changed`), the local accept/decline result, the receiver-side
`cluster:invite-canceled` and `cluster:invite-expired` pushes (both consumed
directly so the PIN prompt dismisses at once when the inviter cancels or the
invite times out), and a per-invite `cluster:invite-status` sweep (pruning
terminal or evicted sessions). The authoritative set is served by
`cluster:get-initial` (`pendingInvites`) and broadcast on every change via
`cluster:pending-invites-changed`.

The cluster-manager expires unanswered invites after a TTL (default 5m) on
**both** sides: it expires its own **outbound** invite and, on the receiver,
expires the **inbound** invite and signals the inviter for immediate teardown. It
emits `cluster:invite-expired` on the receiver — which Personal AI Router
consumes to prune the inbound invite — and on the inviter, alongside
`cluster:invite-declined`. Personal AI Router reconciles the **outbound** side
through the `cluster:invite-status` poll in `useInvitePairing` and dissolves any
solo cluster it auto-created only to back the invite via
`cluster:abandon-if-solo`, so `cluster:invite-declined` stays an inviter-side
latency optimization it does not consume (recorded in
`docs/service-contract-exceptions.json`). Because the backend owns receiver-side
expiry, Personal AI Router keeps no client-side inbound TTL; the
`cluster:invite-status` sweep is the sole backstop for a missed push.

Personal AI Router treats the `canceled` and `rejected` invite states as terminal non-paired
outcomes (coerced by `parseInvite`, which also carries the backend `reason`). The
outbound Cancel action calls the `cluster:cancel-invite` RPC, so canceling an
in-flight invite invalidates the PIN backend-side and best-effort notifies the
joiner to drop its prompt (the receiver reflects this via the already-consumed
`cluster:invite-canceled` push). A `rejected` result surfaces its reason
distinctly — `already-clustered` renders as "That node is already in a cluster" —
and Personal AI Router pre-empts the invite entirely by disabling an already-`clustered`
discovered node.

A wrong PIN is a terminal, non-retryable failure: the invitation ends on both
sides and the user must request a fresh one. The cluster-manager mirrors the
failure — the joiner fails the completion with `reason: 'incorrect-pin'`, clears
its `pending-inbound` member (`nodes:changed`), and best-effort notifies the
inviter over the pairing channel (`phase: "fail"`), which flips its outbound
invite to `failed` (same reason), tears down the EAP session (invalidating the
PIN), and pushes `cluster:invite-failed`. Personal AI Router consumes this end to end: the
inviter surfaces the "Incorrect PIN" error from the `cluster:invite-status` poll
returning `failed` with `reason: 'incorrect-pin'` (rendered by
`InvitePairingPanel`), and `cluster:invite-failed` is consumed
(`modular-supervisor.ts`) to prune any matching inbound invite. The joiner's
`pending-inbound` row clears via `nodes:changed`, and both surfaces normalize
`-32001` / "invite session evicted" to friendly copy (`cluster-invite-error.ts`)
as a backstop. Other `failed` causes (peer unreachable, malformed) carry an empty
`reason`, so Personal AI Router falls back to generic "Pairing failed" copy.

The PIN is a convenience pairing code and should not be described as a strong
authenticator.

## Security

Cluster identity, pairing, trust, membership, and all node-to-node transport
security are owned entirely by the backend. Personal AI Router implements none of it: it does
not read private keys, build TLS contexts, or authenticate peers. This includes
cross-node inference: the proxies terminate the cluster-mTLS ingress and gate it
on cluster pins, and NVPAIR-launched engines bind to loopback so they are
reachable off-box only through that ingress.

The one Personal AI Router-facing constraint is that trust begins with a low-entropy human
PIN, so UI copy must not present the PIN as a strong authenticator.

## Settings

Personal AI Router uses node settings for persisted cluster identity and friendly name
synchronization.

Per-engine settings are separate and owned by `nvpair-engine-manager`: the
server port, the proxy port, and the engine arguments, persisted as a manifest
override that survives a restart. The broker owns the combined operation —
journal, validation, stop, proxy rebind, restart, and crash recovery — so both
the full editor and the port-only `engine:set-port` RPC used by the TUI go
through the same path.

Backend settings with no active behavior are intentionally not exposed in the
UI. A setting should be surfaced only when a backend component consumes it and
its effect is observable.

## Current client-side responsibilities

The following responsibilities remain in Personal AI Router because the backend does not
provide an equivalent client-facing contract:

| Responsibility                                              | Location                                         |
| ----------------------------------------------------------- | ------------------------------------------------ |
| Poll node telemetry over `/v1/node-info`                    | `node-info-poller.ts`                            |
| Persist and replay manual node entries                      | `manual-nodes-store.ts`, `modular-supervisor.ts` |
| Bridge the local node into engine proxies                   | `modular-supervisor.ts`                          |
| Present optimistic engine transition state                  | `pending-actions.store.ts`, bridge state         |
| Serve the model hub (Ollama committed list, LM Studio live) | `src/electron/model-hub/`                        |
| Accumulate and reconcile receiver-side pending invites      | `modular-state.ts`, `modular-supervisor.ts`      |
| Mirror backend-coupled runtime defaults not yet reported    | `modular-runtime.ts`                             |
| Collapse a superseded node row before the scanner proves it | `modular-state.ts`, `modular-runtime.ts`         |

These are current integration boundaries. Do not add compatibility shims or
duplicate backend services around them.

The last row is a display-side collapse, not a directory decision. The scanner
treats a matching address, hostname, and `lastSeen` gap as grounds to ask the
machine who it is, and evicts only on a node-info identity mismatch; the bridge
has no such proof and hides the row on the suspicion alone. It must never be
extended into anything a consumer reads as an eviction.

## Unused backend methods

The generated `docs/services-api.md` is authoritative for methods handled by
the backend but not called by Personal AI Router. Typical optional opportunities include:

- engine manifest description;
- engine log and error-ring views;
- scheduler status and interval controls;
- proxy selected-node observability;
- explicit unsubscribe calls for process-lifetime subscriptions.

Unused methods are not automatically product gaps. Adopt them only when they
serve a current Personal AI Router feature.

## Maintenance

On every backend update:

1. run `npm run service-contracts`;
2. resolve missing notifications or document why they are internal;
3. verify meaningful payload fields are consumed;
4. update this present-state status;
5. run `npm run service-contracts:write`;
6. run `npm run service-contracts:check` and `npm run typecheck`.

Do not append release diaries or prior-state narratives. Git history is the
record of how this integration changed.
