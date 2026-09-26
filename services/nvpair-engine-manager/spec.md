<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Engine Manager (`nvpair-engine-manager`)

## 1. Purpose
A declarative, config-driven control plane for **local inference engines** (Ollama, LM Studio, llama.cpp and managed vLLM today), plus the narrow typed orchestrator for PAIR-managed vLLM serving groups and temporary Spark fabric. It owns an engine's entire lifecycle *except serving inference* — locate, install, launch, stop, restart, health, and config-declared actions — so one uniform API manages any engine across OSes with no per-engine code. A third party drops in a JSON manifest and their engine's install/launch/controls "just appear" over the same API: the core extensibility story for an open-source product. The explicit serving-group and fabric APIs bind current participants, fixed helpers, durable cleanup ownership, and proxy readiness; they do not carry inference traffic.

**Current implementation gap.** Lifecycle, probes, HTTP and CLI actions, model inventory extraction and launch settings are manifest-only for every bundled engine. llama.cpp's staged official-app installer (`install.driver`), its on-disk model cache operations (`action.builtin`) and its router identity probe (`probe.identity`) are backed by engine-specific Go that the manifest merely names, as are managed vLLM's sealed Python runtime (`install.driver`/`runtime.driver` `vllm-python`), its exact-revision model acquisition (`action.builtin` `vllm`) and its identity probe. The runner accepts each only for its bundled engine manifest, so they are a bounded, documented limitation relative to this requirement, not an extension point and not a change to it ([MANIFEST.md](MANIFEST.md) records the fields). Closing the gap means expressing staged promotion, cache-layout scanning and identity probes as manifest-declared capabilities.

## 2. Scope
**In scope**
- Broker-coordinated argument/server/proxy settings, literal argument
  persistence, ownership validation and pinned peer settings synchronization.
  The normative operation and failure contract is
  [ENGINE_SETTINGS.md](../nvpair-ui-broker/ENGINE_SETTINGS.md); launch grammar is
  [LAUNCH_TEXT.md](LAUNCH_TEXT.md).
- Detect installation; **user-mode** install (HTTPS download + checksum-verify-when-pinned + run) per OS/arch.
- Lifecycle: start (with readiness probe), stop, restart, status, periodic health.
- Persistent server-port changes preserve arguments, environment and unrelated
  manifest overrides. Resetting to the bundled port removes only shared and
  host-platform port overrides, deleting the file only when it has no other
  settings. Host-platform precedence must not override a successfully saved port
  on reload; malformed existing overrides fail the save and remain intact.
- Config-declared **actions** covering the full model lifecycle — Ollama: `list_models`, `loaded_models`, `pull_model`, `run_model`, `unload_model`, `delete_model`; LM Studio: `list_models`/`list_downloaded`, `loaded_models`, `pull_model`, `load_model`, `chat`, `unload_model`, `delete_model` (`remove_path` with `lms-disk-path` resolution); managed vLLM: `list_downloaded`, `pull_model`, and exact-operation `cancel_pull` — mapped to the engine API or a bounded Engine Manager builtin. `loaded_models` reports the models currently resident in memory (Ollama `GET /api/ps`, LM Studio `GET /api/v1/models` filtered by nonempty `loaded_instances`), name-extracted via the same declarative `result` spec (with an optional `match` row filter).
- Managed vLLM model pulls accept only public, ungated Hugging Face `owner/repository@40-character-commit` identities and never consume Hub credentials. The owner binds exact metadata, declared license, selected file sizes and LFS SHA-256/Git blob identities; requires account-available capacity plus a 32 GiB reserve; supports exact-stage resume and exact-operation cancellation; then atomically promotes a `pair-model.json` receipt. Local acquisition and notice retention do not grant redistribution rights.
- Paired-node vLLM distribution is target-pull over pinned `ec` mTLS. The source exports only an immutable receipt plan and bounded 32 MiB chunks. Each plan re-hashes the retained model outside the source's export lease, then re-proves under the lease that the same unmodified files are still retained, so one source keeps serving chunks to other targets while it verifies. The target reviews capacity, verifies every chunk/prefix/file/final receipt hash, journals resumable progress, and atomically promotes through the same retained-model owner. Broker and renderer carry no payload bytes, credentials, commands, or host paths. Exact source/target/model operation identity owns retry and cancellation cleanup.
- The fabric-owning controller routes each distribution. When one active fabric links the source and target, it freshly re-proves that fabric, passes the target the source's end of the lowest-index reciprocal lane as `sourceAddress`, and atomically journals a bounded, exact source/target/model/operation hold before the request starts. The hold remains through an interrupted stream or controller restart and refuses rollback/recovery until the bound target sends a terminal result/error or exact cancellation closure. The target dials only that address; the pinned mTLS identity, not the address, authenticates the source. With no fabric linking the pair the copy uses the management network, and a linking fabric that is not active or cannot be re-proven refuses the copy. Callers never choose `sourceAddress`, and distribution progress reports `network` as `fabric` or `management`.
- Per-engine stdout/stderr log capture and structured operational error records, surfaced via the errors pipeline.
- A normalized node-level model result (`engine:models`): `models` / `modelsByEngine` contain only models served by running engines; `loadedByEngine` contains current memory residency; and `retainedByEngine` separately reports bounded PAIR-owned downloaded catalogs such as stopped vLLM receipts. Retained entries never become proxy routing truth. A successful explicit empty inventory remains an engine key with `[]`; a missing/malformed/failed inventory omits that engine key instead of being mislabeled as authoritative empty. A watcher polls the loaded set and pushes `engine:models-changed` when it changes (explicit load/unload, JIT auto-load, TTL/idle eviction).
- Expose all of the above over the `engine:*` JSON-RPC surface to whatever orchestrates the service, plus an optional plain-HTTP LAN endpoint (`--http-port`, `GET /v1/models`) that serves the model list to a peer's discovery daemon (the list moved off the size-limited mDNS TXT onto HTTP).
- Relay canonical device bootstrap control to the target's fixed helper: strict catalog/public-key metadata, access- and host-key-bound inspect/review/apply/status/recover/verify, and target-produced status/receipt validation. The target—not engine-manager—owns privileged effects and durable bootstrap authority.
- Review, check, start, stop, and reconcile one exact two- or three-node managed
  vLLM group. Engine Manager owns the retained generation and cleanup hold,
  while each Linux participant's fixed system owner owns its rank.
- The Qwen3.8 profile serves only as TP2+EP2 on exactly two Sparks. Review
  refuses a three-Spark selection before inspecting any member: the model's two
  key-value heads cannot split three ways, a full copy of its shared weights on
  each of three ranks does not fit in Spark memory, and the runtime cannot run
  it as pipeline stages. Its recipe declares one override of the upstream
  dependency graph, NCCL 2.29.7 to 2.30.7, and the offline `pip check` accepts
  only the conflict that override creates. Its RoCE transport binds both direct
  lanes with merged NICs, subnet-aware routing off, and no Socket payload
  fallback. Preparation admits a target only when its observed driver and
  OS-library provider closure matches one complete cataloged profile, and group
  review requires every member to report the same closure. Recipes v1–v3 are
  ownership-only: an installed environment can be inspected, replaced, rolled
  back to a stopped pointer, or removed, but is never admitted for a new review,
  start, or recovery.
- Review, apply, inspect, recover, and roll back PAIR-owned temporary direct or
  ring fabric configuration. An active qualified fabric may be leased to an
  exact serving-group generation; rollback is refused until that consumer is
  clean.

**Out of scope**
- **Inference traffic** — stays with `nvpair-proxy`; this service never proxies `/api/chat` etc.
- **Executing privileged bootstrap effects locally** — `nvpair-host-bootstrap` and `nvpair-host-helper` run on the target and are never supervised by engine-manager, Electron, or the broker.
- **General multi-instance per engine and an MCP server** — future-additive,
  not v1. The managed vLLM rank owner is the only current multi-process
  exception.
- **The node's error list** — owned by `nvpair-errors`, which holds it as in-memory session state; this service only emits `errors:report` / `errors:clear`.

## 3. Key Use Cases
- **Install an engine, user-mode**: `engine:install {engine:"ollama"}` downloads the per-OS user-scoped package (Windows/Linux standalone archive extracted into a user dir; macOS app bundle — never an elevated `Setup.exe` or `curl | sh`), checksum-verifies, extracts, re-detects.
- **Run lifecycle**: `engine:start` / `engine:stop` / `engine:restart` / `engine:status`, with readiness and health probes against the engine's loopback port.
- **Run a declared action**: `engine:action {engine, action, params}` → the manifest-declared HTTP call to the engine's loopback control API (e.g. `127.0.0.1:{port}/api/pull`). (Methods, notifications, and UI events all use the colon form `engine:*`, matching the POC UI and the `errors:*` notifications.)
- **Onboard a new engine (no code)**: a vendor adds `engines/<vendor>.json`; the generic runner exposes their lifecycle + actions immediately. This is the generic-manager surface; routing, discovery and client integration for an engine PAIR bundles are separate, profile-driven changes in the proxy, broker and clients.
- **Prepare a device**: expose the exact six-target catalog and controller public identities, bind a Quick Connect or Zero Touch operation to current access and an explicitly reviewed SSH host key, relay only fixed helper actions, and accept completion only from the target's receipt.
- **Serve one managed model across paired Linux nodes**: build an expiring review
  from current model, runtime, GPU, network, pin, and optional fabric facts;
  check every participant; start the exact generation through fixed rank
  helpers; publish it only after proxy-route confirmation; and retain cleanup
  custody until every attempted rank is clean.
- **Apply temporary Spark fabric**: turn a reviewed cable selection into an
  exact address and port plan, qualify the selected links, retain the operation
  across restart, and roll back only its proven-owned effects. Fabric
  qualification is not itself proof of RDMA, NCCL payload, bandwidth, or
  inference.
- **Edge case — already installed**: detect short-circuits install (idempotent).
- **Edge case — checksum mismatch**: install fails before `run`, is reported, and never executes an unverified payload.
- **Edge case — readiness timeout / crash**: failed readiness returns to Stopped; a later crash flips health and is reported.

## 4. Open Questions / Risks
- **`engine:*` method stability**: the Broker forwards the namespace verbatim; evolve it additively and preserve `engine:get-installed` as the installed-engine query.
- **`severity` / `action` enums**: current wire values are `info|warning|error` and `dismiss|retry|none`; coordinate any future shared-enum expansion without changing existing values.
- **Risk — auto-restart policy**: off by default (opt-in manifest flag); revisit once a supervisor / Warden-equivalent exists.
- **Risk — dynamic port assignment**: manifest `port: 0` reserves the option; range/policy TBD (may move to a future supervisor).
- **Risk — admin-only installers**: `mode: "admin"` is a flagged, refused exception, not the default; product direction is strictly user-mode.
- **Risk — LAN-open inference bind (interim)**: inference engines default `runtime.bind` to `0.0.0.0` (ordinary engines stay loopback; a per-call `bind` re-pins). This is a deliberate, temporary exception to the loopback-only posture; narrow it once authenticated inference transport exists. The engine-manager `ec` control surface is already protected independently by pin-based mTLS.
- **Future — declared/tunable env layer**: `runtime.env` is static today. A "declared tunables" layer (manifest-declared knobs, UI/broker-overridable per start) is worth adding; by env-richness the priority is Ollama → llama.cpp/Jan → vLLM (LM Studio / GPT4All are flags/settings-driven, not env). Related: `runtime.env` is process-mode-only — extending it to command-mode start commands is a deliberate, still-open choice. See `MANIFEST.md` → "Engine config reference".
- **Model deletion where the vendor has no command (LM Studio)**: implemented via the generic **`remove_path`** action kind — a manifest-declared, param-templated path the runner removes with safety rails (must resolve under a declared allowed root, reject `..`/symlink escapes). LM Studio's `delete_model` uses `model_resolution: "lms-disk-path"` to map logical ids to on-disk files via `lms ls --json` before deleting under `{models_dir}`, then `restart_after` to bounce a running server: LM Studio answers `/v1/models` from an index built at startup and exposes no rescan, so clients keep being offered the deleted model until it restarts. Ollama deletes via `DELETE /api/delete` (no restart needed) and ejects via `unload_model` (`POST /api/generate` with `keep_alive: 0`).

## 5. Requirements

**Functional**
- Load + validate per-engine JSON manifests (bundled + user dir); select the host `<goos>/<goarch>` block; resolve placeholders (`{bin}`, `{cli}`, `{port}`, `{download}`, `{install_dir}`).
- Support both `process` (owned foreground) and `command` (daemon + control-CLI) runtimes; execute detect / install / uninstall / start / stop / restart / status / health and HTTP **or** CLI actions; emit `engine:*` results and notifications.
- Load and strictly validate the packaged bootstrap catalog; enumerate public controller keys only; require candidate/access/host-key/account/artifact binding; relay target bootstrap inspect/review/apply/status/recover/verify without accepting controller-authored observations or receipts.
- Build serving-group plans only from current authenticated participant facts;
  bind every action to review or run identity, generation, plan digest, and rank;
  retain cleanup-required state across restart; and withhold routability until
  the existing proxy confirms the selected coordinator/model route.
- Journal fabric review/application/rollback state, preserve exact target and
  address ownership, require fresh rollback admission after restart, and fence
  cancellation while an exact serving-group consumer lease is active.
- Emit `errors:report` / `errors:clear` on its stdio for the Broker to forward to `nvpair-errors`.

**Non-functional**
- Compiles and runs on Windows / macOS / Linux × amd64 / arm64.
- **User mode, no admin/sudo inside engine-manager**; target bootstrap privilege is isolated in the signed target package and fixed local helper. Stdio / `--ipc` is the primary control channel, with an optional plain-HTTP model-list listener and a clustered pin-gated mTLS control listener; engines bind loopback by default, but a manifest's `runtime.bind` can open an inference engine to the LAN (Ollama ships `0.0.0.0` to serve the cluster), overridable per-call via `engine:start {bind}`; HTTPS-only downloads, checksum-verified when the manifest pins a `sha256`.
- Fully testable end-to-end with no real engines, network, or installers in the test path.
- Small, readable, minimal-dependency.

## 6. Inputs and Outputs

**Inputs** — from the orchestrator (`nvpair-ui-broker`) over stdio / `--ipc`, plus engine manifests from disk. Newline-delimited JSON-RPC 2.0 `engine:*` requests; manifest JSON (`engine`, `display_name`, `manifest_version`, `platforms{<os/arch>{detect, install, runtime}}`, `actions`). Every manifest is validated on load and rejected with a specific, human-readable error if malformed; manifests are read-only to this service.

`EngineStatus` object (returned by `engine:status` and `engine:get-installed`):
```json
{
  engine: string
  display_name: string
  installed: boolean
  running: boolean
  healthy: boolean
  port: number
  install_supported: boolean   // this platform has an install recipe the host can run
  install_reason?: string      // why not, or what the recipe will do
  managed: boolean             // the running instance is one this service owns
  acceleration?: string        // llama.cpp: backend verified at install ("cuda", "vulkan", "metal", "cpu", ...)
  devices?: string[]           // llama.cpp: `llama cli --list-devices` rows recorded in the install receipt
}
```

`LogLine` object (entry in `engine:logs`):
```json
{ time: string, stream: "stdout" | "stderr", text: string }
```

`engine:logs` retains the newest snapshot within three bounds: 2,000 lines,
256 KiB per input line, and 1 MiB of aggregate UTF-8 child-output text. A
single line that cannot fit by itself is omitted.

Errors are emitted with the `nvpair-shared/errors.ServiceError` wire shape (`id`, `message`, `severity`, `action`, …), mirrored locally in `reporter.go` with identical JSON tags. `id` follows `engine-manager:<class>:<engine>`; the Broker stamps `nodeId` / `timestamp`.

Example request:
```json
{"jsonrpc":"2.0","id":1,"method":"engine:install","params":{"engine":"ollama"}}
```

**Outputs** — JSON-RPC responses/notifications to the orchestrator; spawned engine OS processes; loopback HTTP to the engine's control API; `errors:report` / `errors:clear` notifications the Broker forwards to `nvpair-errors`. Lifecycle is not a hot path; error emits are best-effort (no-op if `nvpair-errors` is down). stdout writes are serialized so frames never interleave.

Example error notification:
```json
{"jsonrpc":"2.0","method":"errors:report","params":{"id":"engine-manager:install-failed:ollama","message":"sha256 mismatch","severity":"error"}}
```

## 7. API / Interface Contract
The stdio interface toward the local orchestrator (`nvpair-ui-broker`) is the primary surface. In addition, a clustered node serves a cluster-scoped remote-control surface (the `ec` service) directly over pin-based mTLS — engine-manager terminates that mTLS itself (see §7.2), the one place inter-node crypto lives here rather than in the orchestrator.

### 7.0 Methods and notifications
All traffic is JSON-RPC 2.0, newline-delimited over stdio (default) or `--ipc` (Unix domain socket / Windows named pipe).

Requests (caller → service):

| Method | Params | Result |
|--------|--------|--------|
| `engine:get-installed` | — | `{ engines: [EngineStatus] }` |
| `engine:describe` | `{ engine }` | the engine's manifest |
| `engine:status` | `{ engine }` | `EngineStatus` |
| `engine:install` | `{ engine }` | `EngineStatus` (after install) |
| `engine:uninstall` | `{ engine }` | `EngineStatus` (after removal) |
| `engine:start` | `{ engine }` | `EngineStatus` (after readiness) |
| `engine:stop` | `{ engine }` | `EngineStatus` |
| `engine:restart` | `{ engine }` | `EngineStatus` |
| `engine:action` | `{ engine, action, params }` | the engine's raw response |
| `engine:logs` | `{ engine }` | `{ lines: [LogLine] }` |
| `engine:errors` | — | `{ errors: [ServiceError] }` |
| `engine:vllm-group-review` | `{ selection: { nodeIds, model, parallelism? } }` | expiring immutable review built from current participant facts |
| `engine:vllm-group-check` | `{ reviewId }` | capability result for every reviewed participant; no effects or administrator input |
| `engine:vllm-group-start` | `{ reviewId, elevation? }` | retained exact-generation run; readiness includes proxy-route confirmation |
| `engine:vllm-group-status` | — | activation/reservation state plus the retained run, if any |
| `engine:vllm-group-stop` | `{ runId, generation }` | status after ordinary exact-generation stop |
| `engine:vllm-group-reconcile` | `{ runId, generation, elevation? }` | status after exact unresolved-rank cleanup reconciliation |
| `engine:vllm-group-cleanup` | `{ runId, generation, planDigest }` | cleanup-only reconciliation for the exact retained owner; never starts ranks |
| `engine:vllm-qwen38-prepare` | `{ operationId, cancel? }` | provider-gated exact runtime acquisition, resumable hash publication and schema-v2 offline preparation; never starts standalone serving |
| `engine:vllm-distribute-model` | `{ engine:"vllm", operationId, sourceNode, model, cancel? }` | exact local-target paired transfer result or exact checkpoint cancellation |
| `engine:cable-review` | selected 2-node direct or 3-node ring topology plus reviewed target access | current physical-cable review and finite permission plan |
| `engine:cable-start` | `{ reviewId }` | retained bounded cable-check run |
| `engine:cable-status` / `engine:cable-cancel` | exact review or run identity | current result or exact-run cancellation/cleanup |
| `engine:fabric-inventory` | `{ nodeIds }` (1-3 current paired nodes) | bounded read-only eligible physical-port projection from fresh node-info facts; no routes, addresses, commands, credentials, or mutation authority |
| `engine:fabric-review` | cable selection plus optional `inspectSelectedProfiles` | reviewed temporary address/port plan and blockers |
| `engine:fabric-approve` | `{ reviewId, administratorApproved, selectedPortPauseApproved }` | retained fabric operation after apply and qualification |
| `engine:fabric-status` | `{ operationId }` | current exact operation and cleanup/lease state |
| `engine:fabric-cancel` / `engine:fabric-recover` | `{ operationId, administratorApproved }` | exact owned rollback or recovery result |
| `engine:fabric-retained-operations` | — | bounded retained fabric inventory for UI reconciliation |
| `engine:onboarding-bootstrap-catalog` | — | strict six-target artifact/signature catalog |
| `engine:onboarding-bootstrap-controller-keys` | — | controller public-key identities |
| `engine:onboarding-bootstrap-inspect` | target reference + canonical request | target-produced inspect status |
| `engine:onboarding-bootstrap-review` | target reference + canonical request | deterministic reviewed plan from fresh target state |
| `engine:onboarding-bootstrap-apply` | target reference + reviewed plan | target-produced apply/verify status |
| `engine:onboarding-bootstrap-status` | target reference + operation ID | current target status |
| `engine:onboarding-bootstrap-recover` | target reference + operation ID | target recovery status |
| `engine:onboarding-bootstrap-verify` | target reference + reviewed plan | target-produced complete receipt |
| `engine:remote-get-installed` | `{ node }` | `{ engines: [EngineStatus] }` from the remote node |
| `engine:remote-install` | `{ node, engine, start? }` | `{ opId, status }` after the remote install |
| `engine:remote-pull-model` | `{ node, engine, model?, params? }` | `{ opId, result }` after the remote pull |
| `engine:remote-cancel-pull` | `{ node, engine:"vllm", model, operationId }` | `{ accepted }` from the exact remote pull owner |
| `engine:remote-distribute-model` | `{ node, engine:"vllm", operationId, sourceNode, model, cancel? }` | exact remote-target paired transfer result or exact checkpoint cancellation |
| `engine:remote-load-model` | `{ node, engine, model }` | the remote action result |
| `engine:remote-unload-model` | `{ node, engine, model }` | the remote action result |
| `engine:remote-delete-model` | `{ node, engine, model }` | the remote action result |
| `engine:remote-cancel-pull` | `{ node, engine, model }` | the remote cancel receipt (managed llama.cpp downloads) |
| `engine:remote-start` | `{ node, engine, port? }` | `EngineStatus` from the remote node (manifest `runtime.bind`; no per-call bind on the remote path) |
| `engine:remote-stop` | `{ node, engine }` | `EngineStatus` from the remote node |
| `shutdown` | — | `null` |
| `log/set-level` | `{ level }` | `{ level }` |

Notifications (service → caller): `ready{version}`, `engine:state-changed{EngineStatus}`, `engine:models-changed{engine, models}` (pushed when an engine's loaded-in-memory model set changes; `models` is the full `engine:models` shape incl. `loadedByEngine`), `engine:install-progress{engine, stage, percent}`, `engine:pull-progress{engine, op, stage, percent, message, operationId?}` (live progress for a local model pull driven via `engine:action{action:"pull_model"}` — managed vLLM includes the exact cancellation binding), `engine:remote-progress{opId, node, engine, op, stage, percent, message}` (live progress relayed from a remote install/pull), and `errors:report` / `errors:clear` (consumed by `nvpair-errors` via the Broker). `install` / `start` / `stop` / `restart` / `action` / `remote-*` each run in their own goroutine so the read loop never blocks; their responses arrive when the op completes.

Example `engine:install-progress` (stdout):
```json
{"jsonrpc":"2.0","method":"engine:install-progress","params":{"engine":"ollama","stage":"downloading","percent":42}}
```

Example `engine:pull-progress` (stdout):
```json
{"jsonrpc":"2.0","method":"engine:pull-progress","params":{"engine":"ollama","op":"pull","stage":"pulling","percent":62,"message":"pulling"}}
```

### 7.1 Versioning
- `engine:*` method names are namespaced and additive — new methods, never breaking changes to existing ones.
- `manifest_version` gates manifest-schema evolution: unknown optional fields are ignored (backward-compatible growth); a version higher than supported is rejected.

### 7.2 Remote engine management (the `ec` surface)
With `--control-port` and a clustered `--cluster-dir`, engine-manager serves a cluster-scoped remote-control surface over pin-based mTLS (`nvpair-shared/clustertrust`): it presents this node's cluster leaf, requires a client cert, and `403`s any caller that isn't a byte-for-byte pinned cluster peer. The listener is bound whenever `--control-port` is set and admits callers by live membership — its leaf is resolved per handshake, so an unclustered node presents none and every handshake is refused — so no restart is needed on `cluster:identity-changed`; the broker registers `ec` whenever a cluster dir is configured. Routes under `/v1`: `GET /engines`, streaming `POST /engines/install` and `/models/pull` (chunked NDJSON — zero+ `{"type":"progress"}` frames then one terminal `{"type":"result"}`/`{"type":"error"}` frame), non-streaming `POST /models/{load,unload,delete,cancel-pull}`, and non-streaming `POST /engines/{start,stop}` returning `EngineStatus`.

The `engine:remote-*` methods are the client half: engine-manager resolves the target `node` in an `ec` peer directory (fed by its own `discovery:subscribe{services:[ec]}` to the broker relay), dials the peer's `ec` surface with the same pinned identity, relays each streamed progress frame up as `engine:remote-progress` (keyed by a minted `opId`), and settles the request on the terminal frame. Remote install/pull run for the operation's full duration (no broker-imposed timeout); remote stop and model unload/delete are fast request/response, remote Ollama model load uses the readiness-sized response budget, and remote start waits for the engine's bounded readiness result. They error if this node isn't clustered or the target isn't a pinned peer.

### 7.3 Target bootstrap relay

The canonical target matrix is the ordered Cartesian product of
`windows|darwin|linux` and `amd64|arm64`; Linux native inspection additionally
requires Debian or Ubuntu. Quick Connect and Zero Touch are immutable lane
values on the same contract. `auto` is resolved by the client to Desktop on
Windows/macOS and Headless on Linux; explicit roles must equal the resolved
owner.

Every bootstrap envelope includes `candidateId`, `accessId`, and
`hostKeySha256`. The embedded request binds target, lane, role, runtime owner,
account paths, public controller key, endpoint, product, and helper. Catalog
validation requires exact target order, fixed artifact identities/paths,
Desktop and Headless role availability, consistent provenance, catalog
checksum, and coherent signature records. `official-release` records fail
closed without required signed metadata, detached material, and macOS
notarization where applicable. `engineering` records must be explicitly
unsigned and are not official.

The helper request sent through SSH contains only schema, operation ID, and a
closed action. The target helper loads the durable request/plan and invokes the
fixed bootstrap executable. Inspect and status therefore return target state;
review reconciles a fresh target inspection; apply and verify validate exact
operation/binding/decision correlation; verify returns the target's complete
receipt. The controller cannot send observations or a receipt.

Recovery replays the target journal by operation ID. Repair authority comes
from the complete receipt plus target ownership markers and cannot broaden to a
foreign resource. Uninstall stays target-local and requires the original
request and receipt.

Access credentials are volatile. Passwords, passphrases, elevation passwords,
and provider-supplied private key bytes are cleared/expired and never enter
bootstrap documents or retained history. Controller-key metadata is public
material only. Bootstrap completion does not trust SSH: the current host
fingerprint must still be observed and explicitly approved before
authentication.

## 8. Dependencies
- **Upstream**: the orchestrator that spawns it and forwards `engine:*` (`nvpair-ui-broker`).
- **Downstream**: `nvpair-errors` (via the Broker's `errors:report` / `errors:clear` forwarding); the managed engine processes; and, for device setup, the target's fixed `nvpair-host-helper` command over an already reviewed SSH channel.
- **External**: engine vendors' download URLs (HTTPS; checksum-pinned when a `sha256` is set, else HTTPS-only with a warning); first-party `nvpair-shared/applog` (logging + `log/set-level`) and `nvpair-shared/clustertrust` (pin-based mTLS). The `nvpair-shared/errors` wire shape is mirrored locally with identical JSON tags. No third-party runtime services.

## 9. Data Ownership
- **Owned**: the in-memory engine registry (parsed manifests + per-engine runtime state) and per-engine log/error ring buffers — transient only.
- **Serving-group authority**: the retained vLLM group journal owns one exact
  run/generation/plan and its cleanup hold. Participant system services own rank
  effects; `nvpair-proxy` owns inference transport and routability.
- **Fabric authority**: retained fabric operation records own only the reviewed
  temporary addresses, reservations, qualification receipt, rollback state,
  any exact serving-group consumer lease, and bounded exact transfer holds that
  fence rollback. A transfer hold records dependency ownership; it does not
  assert model payload progress or completion.
- **Bootstrap authority**: target-produced state, journals, ownership markers, and receipts belong to `nvpair-host-bootstrap`/`nvpair-host-helper`. Engine-manager retains volatile target access and relays exact typed results; it is not the source of truth for target state.
- **Other sources of truth**: `nvpair-errors` owns the node's error list (in
  memory, for the session); model inventories belong to the engines; manifests
  and the packaged bootstrap catalog are authored elsewhere. This service is
  authoritative only for its retained serving-group and fabric ownership
  records described above.
- **Storage**: manifests come from the per-user data dir's `engines/*.json`
  (`%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows,
  `~/.config/Nvidia Corporation/Personal AI Router` on Linux,
  `~/Library/Application Support/Nvidia Corporation/Personal AI Router` on
  macOS) plus bundled `manifests/*.json`. Runtime state and logs are in memory;
  managed vLLM serving groups and fabric operations also use bounded atomic
  JSON journals under the same product data root. There is no database.

## 10. Design Constraints
- **Performance**: control plane, not inference; sub-second RPCs except install (network-bound) and start (bounded by the readiness timeout).
- **Scalability**: a handful of engines per node; one managed instance per engine in v1.
- **Reliability**: best-effort; readiness + health probes; automatic restart on crash is planned but **not yet implemented** (see §4); install is one-shot and idempotent (detect short-circuits).
- **Serving/fabric recovery**: unknown or incomplete retained state fails closed.
  A serving group keeps vLLM mutations reserved until every attempted rank has
  cleanup proof. A fabric operation keeps its owned effects and recovery hold
  until exact rollback succeeds; an active consumer lease blocks rollback.
- **Security**: **engine-manager remains user mode**; target bootstrap administrator work is isolated in a signed target-local executable and fixed ACL-protected helper with no network listener or shell. Both engine-manager LAN listeners terminate pin-based mTLS and reject unpinned peers — the read-only model-list listener (`em`) because a node's model inventory is cluster data, and the `ec` control listener because its routes are privileged; `em` additionally serves plaintext on loopback only, for this node's own scanner; engines bind loopback by default, but a manifest's `runtime.bind` may open an inference engine to the LAN (Ollama defaults to `0.0.0.0`, overridable per-call); downloads are HTTPS-only (plain HTTP only from loopback) and checksum-verified before execution when the manifest pins a `sha256` (an unpinned fetch is HTTPS-only with a loud warning, like a `script` install).
- **Compliance**: no PII; payloads carry engine/model identifiers and error messages only.

## 11. Assumptions
- The orchestrator owns the stdio/IPC pipe and is the only peer; on parent exit (stdin EOF) the service shuts down cleanly — no reconnect or buffering.
- Manifests are either well-formed (validated) or rejected; authors keep `engine:*` and manifest shapes stable per the versioning rules.
- Engines bind loopback unless the manifest's `runtime.bind` opts otherwise (inference engines default to `0.0.0.0`; a per-call `bind` overrides); the engine's own startup banner is not trusted — the readiness probe confirms the actual bind.
- `nvpair-errors` is best-effort; its absence degrades to local logs only.

## 12. Failure Modes and Mitigations
- **Download / checksum failure on install**: engine stays NotInstalled. → Fail fast before `run`; `engine:install-progress` error + `errors:report` (`engine-manager:install-failed:<engine>`); never execute an unverified payload.
- **Engine fails its readiness probe**: start never reaches Running. → Bounded ready timeout → back to Stopped; structured error; no half-started state.
- **Engine process crashes**: engine unavailable. → A watcher flips state, emits `engine:state-changed` + `errors:report` (`engine-manager:exited:<engine>`); the parent's supervisor reports if the *manager itself* dies. (Automatic restart is planned — see §4 — not yet implemented.)
- **`nvpair-errors` unavailable**: failures absent from the node's error list. → The Broker no-ops the forward; local applog + ring buffers still hold everything.
- **Invalid / unknown-OS manifest**: that engine is unusable. → Reject at load with a specific error; other engines unaffected.
- **Bootstrap catalog/signature metadata missing or inconsistent**: target identity cannot be established. → Fail before building or relaying a binding; never reinterpret unsigned engineering output as official.
- **SSH host key absent, changed, or rejected by existing trust**: target access is not authorized. → Require a fresh observation and explicit exact-fingerprint approval; never replace trust from a bootstrap receipt.

## 13. Observability
- **Logging**: structured `slog` to stderr via `nvpair-shared/applog` (captured by the parent's debug panel); per-engine stdout/stderr ring buffers; key events — install fetch/verify/run outcomes, start/ready/stop transitions, health flips, action invocations, every failure.
- **Metrics**: lightweight in-process counters (installs, starts, failures, health transitions per engine), surfaced via `engine:status` and `engine:errors`; no separate metrics endpoint in v1.
- **Alerts**: surfaced through the errors pipeline — `errors:report` for install/start/health failures and manager crashes; the UI's error list is the alerting surface for v1.

## 14. Sample usage
The operator opens the UI; the Broker calls `engine:get-installed`, which returns each manifest's `EngineStatus` (e.g. Ollama `installed:true, running:false`). To set up a missing engine, the Broker sends `engine:install {engine:"ollama"}`: the service downloads the per-OS user-scoped package, checksum-verifies it, runs it user-mode, and re-detects — progress streaming as `engine:install-progress` notifications.

The operator starts it: `engine:start {engine:"ollama"}` resolves the manifest runtime (`OLLAMA_HOST` = `{host}:{port}`, `{host}` from `runtime.bind` defaulting to `0.0.0.0`; per-call `port`/`bind` override either), spawns the process, and waits for the readiness probe before reporting `running:true, healthy:true` in the response and an `engine:state-changed` notification. A periodic health probe keeps the state current; an unexpected exit emits `engine:state-changed` + `errors:report`. If something is already serving the port, `start` **adopts** it instead of spawning a duplicate.

The operator stops it: `engine:stop {engine:"ollama"}` signals a process the service owns. For an **adopted** engine (no owned process), it resolves the PID bound to the port and terminates it only when that process is running the binary we manage — reclaiming an orphan a prior run left on our own managed port; a genuinely foreign listener (a different image on a different port) is declined with an error naming its PID and image. A user-initiated `stop` records the OFF intent regardless (even when the RPC returns an error), so the health loop and restore-on-restart don't flip the engine back on — clients must not treat a stop error as proof the OFF choice was discarded. The cluster `ec` stop endpoint shares this semantics and may return HTTP 500 while OFF is persisted.

The operator pulls a model: `engine:action {engine:"ollama", action:"pull_model", params:{name:"llama3.2"}}` issues the manifest-declared `POST 127.0.0.1:{port}/api/pull`. Because the action is `pull_model`, the request is routed through the streaming pull path (not the buffered `engine:action` reader): each `/api/pull` status line is emitted as an `engine:pull-progress` notification — so a local pull shows live download progress just like a remote pull's `engine:remote-progress` — and the request settles with the pull's terminal result line. Frames are coalesced (only a change in `stage` or `percent` is emitted) so a chatty engine that streams many byte-progress lines per layer doesn't flood subscribers. The engine's terminal `{"status":"success"}` surfaces as a `stage:"success"` frame; a **failed** pull emits a terminal `stage:"error", percent:-1, message:<why>` frame in addition to the JSON-RPC error, so a UI whose synchronous call already timed out on a long download still converges off "pulling". A CLI-driven pull (LM Studio's `lms get`) has no line-level progress, so it emits one `stage:"pulling"` marker and returns the command's result. On `shutdown` (or stdin EOF) the service stops every running engine first, so none are orphaned.

## 15. Current integration / wiring

The engine manager builds standalone (`go build ./...`) and is tested end to
end over stdio. Current product wiring is:

1. `build.bat` and `build.sh` build the version from `versions.json` and stage
   the binary in the repository-root `build/bin/` bundle.
2. `installer/nvpair-setup.nsi` bundles the Windows binary and owns the local
   subnet firewall rules for its model-list and cluster-control listeners.
3. `nvpair-ui-broker` supervises the process, relays its `engine:*` JSON-RPC
   surface, forwards engine events and errors, and registers its applicable
   discovery services.
4. The bundled graphical UI, `nvpair-tui`, and other clients use the broker
   contract rather than owning engine-manager process wiring. Product packaging
   places the graphical UI alongside the broker and workers in the same
   installation directory.
5. `nvpair-engine-manager/README.md`, `nvpair-ui-broker/README.md`, and the
   cross-process tests are the current architecture, protocol, and
   integration references.

The retired desktop wiring is intentionally omitted; its implementation record
remains available in Git history and is not part of the current architecture.
