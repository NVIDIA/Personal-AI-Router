<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-engine-manager

A config-driven control plane for local inference engines, including Ollama,
LM Studio and llama.cpp. It manages everything about an engine **except serving
inference**: detect, user-mode install, start/stop/restart, health, and
config-declared actions. Manifests describe common operations; engine-specific
backend drivers handle ownership or lifecycle behavior a command recipe cannot
safely express. The desktop and TUI remain clients of this control plane.

The bundled manifests under `manifests/` are the working reference for manifest
authoring.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0 — the same conventions as
every other NVPAIR subprocess. Stdio by default; `--ipc <path>` dials a Unix
domain socket or Windows named pipe instead. The service is
**parent-agnostic**: it speaks to whatever owns its pipe (in practice
`nvpair-ui-broker`) and carries no front-end dependency, so the
same binary runs under any supervisor with zero code change. The `engine:*`
namespace is the surface the orchestrator/broker forwards from the UI.

## JSON-RPC surface

Requests (caller → service):

| Method | Params | Result |
|---|---|---|
| `engine:get-installed` | — | `{ engines: [EngineStatus] }` |
| `engine:describe` | `{ engine }` | the engine's manifest |
| `engine:status` | `{ engine }` | `EngineStatus` |
| `engine:install` | `{ engine, start?, port?, bind? }` | `EngineStatus` (after install; also starts it if `start:true`) |
| `engine:uninstall` | `{ engine }` | `EngineStatus` (after removal) |
| `engine:start` | `{ engine, port?, bind? }` | `EngineStatus` (after readiness) |
| `engine:stop` | `{ engine }` | `EngineStatus` |
| `engine:restart` | `{ engine }` | `EngineStatus` |
| `engine:set-port` | `{ engine, port }` | `EngineStatus` (after rebind) |
| `engine:action` | `{ engine, action, params }` | the engine's raw response. `action:"pull_model"` is streamed: it emits live `engine:pull-progress` notifications and returns the pull's terminal result (see below). An action whose manifest declares `restart_after` (LM Studio's `delete_model`) restarts a running engine before replying, so the response also means the engine is back and healthy |
| `engine:logs` | `{ engine }` | `{ lines: [LogLine] }` |
| `engine:errors` | — | `{ errors: [ServiceError] }` |
| `engine:models` | — | `{ models: [string], modelsByEngine: { <engine>: [string] }, loadedByEngine: { <engine>: [string] } }` — the flat de-duplicated union of every running engine's models, the per-engine breakdown keyed by engine name, and the per-engine set of models currently **loaded in memory** (all normalized from each engine's `list_models` / `loaded_models` action `result` spec). `modelsByEngine` carries a key for every running engine whose inventory was successfully queried, including an empty list = "running, no models available"; a missing key means not running / not queryable / invalid response. `loadedByEngine` uses the same known-empty distinction for residency and also omits engines with no loaded endpoint. The `/v1/models` HTTP surface returns the same shape. |
| `engine:remote-get-installed` | `{ node }` | `{ engines: [EngineStatus] }` fetched from the remote node over `ec` mTLS |
| `engine:remote-install` | `{ node, engine, start? }` | `{ opId, status: EngineStatus }` after the remote install (live progress via `engine:remote-progress`) |
| `engine:remote-pull-model` | `{ node, engine, model?, params? }` | `{ opId, result }` after the remote pull (live progress via `engine:remote-progress`) |
| `engine:remote-start` | `{ node, engine, port? }` | `EngineStatus` from the remote node (always the manifest's `runtime.bind`; no per-call bind override on the remote path) |
| `engine:remote-stop` | `{ node, engine }` | `EngineStatus` from the remote node |
| `shutdown` | — | `null` |
| `log/set-level` | `{ level }` | `{ level }` |

`EngineStatus` = `{ engine, display_name, installed, running, healthy, port }`.

Notifications (service → caller): `engine:ready{version}`,
`engine:state-changed{EngineStatus}`,
`engine:models-changed{engine, models}` — pushed when an engine's set of
loaded (in-memory) models changes (explicit load/unload, JIT auto-load, or
TTL/idle eviction); `models` is the full `engine:models` shape (incl.
`loadedByEngine`) so a consumer swaps its whole snapshot,
`engine:install-progress{engine, stage, percent}`,
`engine:pull-progress{engine, op, stage, percent, message}` (live progress for a
local model pull driven via `engine:action{action:"pull_model"}` — the local
counterpart of `engine:remote-progress`; frames are coalesced to changes in
stage/percent, the engine's terminal success surfaces as `stage:"success"`, and
a failed pull emits a terminal `stage:"error", percent:-1, message` frame so a
UI converges even if its synchronous call already timed out),
`engine:remote-progress{opId, node, engine, op, stage, percent, message}`
(relayed live progress for a remote install/pull), and — for the error
pipeline — `errors:report` / `errors:clear` (consumed by `nvpair-errors`
via the broker; see below).

The `engine:remote-*` methods are the client half of remote engine
management: engine-manager resolves the target `node` in an `ec` peer
directory (fed by its own `discovery:subscribe{services:[ec]}` to the broker
relay), dials that peer's `ec` surface over pin-based cluster mTLS, and — for
`remote-install` / `remote-pull-model` — mints an `opId`, relays each streamed
progress frame up as `engine:remote-progress`, and settles the request with the
terminal result. They fail if this node isn't clustered or the target isn't a
pinned cluster peer. See "Remote engine management" below.

`engine:install`, `engine:start`, `engine:stop`, `engine:restart`,
`engine:set-port`, and `engine:action` run in their own goroutine on the
service side, so the read loop never blocks and their responses arrive when
the op finishes.

`engine:set-port` is the **persistent** port setter (distinct from the
one-shot `engine:start {port}` override, which reverts on the next restart).
It validates `1-65535`, persists the choice as a manifest override (a
`{ engine, runtime: { port } }` delta written
to the per-user `engines/` dir that deep-merges onto the bundled manifest, so
`runtime.port` becomes the single source of truth and the port is restored on
the next start with no separate store), and applies it — bouncing the engine
onto the new port if it was running. Setting the port back to the bundled
default removes only the shared and host-platform port overrides; the file is
removed only when no other overrides remain. Arguments, environment, install
settings and other platform overrides are preserved. A host-platform port is
updated when necessary so it cannot shadow the saved value on restart.
Malformed override files fail the save instead of being replaced.
Because the chosen port lives in the
effective manifest, restore is automatic: a normal `engine:start` (no explicit
port) and the adopt-on-fixed-port path both come up on the retained port.
Moving a **running, adopted** engine is **refused** with an error (nothing is
persisted) — NVPAIR can't relocate a process it didn't start; see Adoption below.

## Lifecycle

Combined launch/server/proxy edits use the broker's
[engine settings protocol](../nvpair-ui-broker/ENGINE_SETTINGS.md). The worker
checks basic argument syntax and adapter-declared networking/CORS controls
using [`pair-arguments-v1`](LAUNCH_TEXT.md), persists host-platform `launch_args` and `launch_env`
with the port, and holds its operation lock through stop/rebind/start. The
paired engine-control surface relays settings to its local broker and streams
full authoritative snapshots to pinned peers.

```
NotInstalled --engine:install--> (HTTPS download + verify-if-pinned + user-mode run) --> Stopped
Stopped      --engine:start----> (adopt if already serving the port, else spawn) --> Running --health--> Running
Running      --engine:stop-----> (stop signal, wait for exit; no timeout) --> Stopped
```

Detect uses the manifest's `detect` paths. Install is one-shot and
user-mode — an HTTPS download, verified against the manifest's `sha256`
when one is pinned (an unpinned fetch runs with a loud warning). Start
waits for the readiness probe, then runs a periodic health probe; an
unexpected exit is reported. The bundled Ollama manifest allows up to ten
minutes for startup because GPU discovery can exceed the previous 30-second
allowance on supported Windows systems. The deadline remains finite: if Ollama
never serves its readiness endpoint, engine-manager stops the owned process and
reports the failed start. Stop sends one graceful stop signal (SIGTERM to the
process group on Unix; `taskkill /T /F` on Windows, where the windowless engines
we spawn can't receive a graceful close) and waits for the engine to exit; if
the engine, or a model child it spawned, is still alive fifteen seconds later
the whole process group is force-killed, so a stop is complete only when
nothing PAIR started is left running.

### Managed install/uninstall contract

This is the required recipe standard shared by Ollama, LM Studio and llama.cpp.
It defines acceptance requirements, not a blanket certification of legacy
recipes. Each engine must provide evidence for its actual platform and layout.

| Operation or boundary | Required behavior |
| --- | --- |
| Install | Put the managed runtime in a PAIR-owned location using the supported vendor path. Do not overwrite a detected external installation or take ownership of its data. |
| Detection/adoption | Finding an executable or serving endpoint does not grant uninstall authority. Keep existing engine-specific detection, start/stop and port behavior; prove ownership separately before removal. |
| Uninstall | Stop the correct managed instance, then remove only its owned runtime. Refuse removal of external, shared or legacy installations when ownership cannot be established, including command-mode engines. |
| Retention | Keep normal separate model libraries, settings and user data. Runtime removal is not profile reset or model cleanup. Persisting the user's Off intent is allowed; erasing their configuration is not. |
| Reinstall | Reuse retained data without requiring models to be downloaded again. A vendor cache is not disposable merely because the runtime was removed. |
| Failure | Return an actionable error and the observed state. A successful command exit alone does not prove runtime removal or data preservation. |

Parity here is **install/uninstall ownership and retention**. It does not add a
new detach interface, job-drain mechanism, profile-reset/model-cleanup feature,
API-first rewrite or generic updater redesign. Update behavior remains
engine-specific: Ollama and LM Studio keep their existing update paths, and
managed llama has no update action. Uninstall-then-install is not a substitute
for one and must not be wired up as such for superficial similarity.

Minimum recipe evidence: managed install/detection, bounded start/stop, runtime
removal with model/settings retention, reinstall using retained data, and
external/shared-install refusal. A manifest or mock alone does not establish
native vendor-package behavior. For llama, only `runtime` and `previous` are
removable installation slots; model/cache and settings paths remain separate.

### Adoption — start may attach to an engine it didn't launch

Before spawning, `engine:start` **probes the chosen port's readiness
endpoint**. If something already answers there — the engine's own desktop
app (e.g. the Ollama tray app on `11434`), or an instance left running from a
previous session — the service **adopts** that instance: it marks the engine
`running` without launching its own, rather than spawning a duplicate that
would only collide on the port. Consequences worth knowing:

- **An adopted engine has no child process the service owns**, so `engine:stop`
  (and `engine:restart`'s stop phase, and shutdown's cleanup) resolves the PID
  bound to the engine's port and terminates it **only when that process is
  running the very binary NVPAIR manages for the engine** — reclaiming an orphan
  a prior run left on our own managed port (e.g. an `ollama serve` on `11435`
  whose handle was lost after a crash). This is precise to the port, so a
  genuine third-party listener on a *different* port (Ollama's own desktop app
  on `11434` while NVPAIR manages `11435`) is never touched. A listener whose
  image is **not** our managed binary is declined with an actionable error
  naming the offending PID and image path — the user / desktop app owns that
  process, and NVPAIR won't terminate it out from under them.
- **`engine:stop` may return an error while still saving OFF.** When stop
  declines a foreign listener it returns an actionable error, but the user's
  OFF choice is persisted anyway — UI layers should treat the saved desired
  state as authoritative (the engine will not restore on restart) and surface
  the error as guidance, not as proof the OFF intent was lost. The same applies
  to cluster `POST /v1/engines/stop`, which may answer HTTP 500 even though OFF
  was recorded.
- **`engine:set-port` on a running adopted engine is refused** (returns an
  error, persists nothing). Moving it would mean killing the old listener and
  spawning a new one; since NVPAIR can't kill what it didn't start, it errors
  rather than leaving a duplicate serving the new port while the original keeps
  serving the old one. Stop the engine in its own app first, then set the port.
- **`engine:install` short-circuits the same way.** `detect` honors existing
  system installs (Ollama's detect paths include `/Applications/Ollama.app`,
  `%LOCALAPPDATA%\Programs\Ollama`, etc.), so "installing" an engine that's
  already present downloads nothing and reports `installed: true` — and a
  following `start:true` then adopts the running instance.
- **Liveness reconciliation uses the same probe.** A fixed-port engine found
  already serving is reported `running: true` even though NVPAIR never started it.

To get a **NVPAIR-owned, stoppable** instance, start it on a port nothing is
already serving — the probe misses, so the service spawns and owns the child
(tracked in `st.proc`), and a later `engine:stop` / `engine:restart` actually
terminates it. `engine:set-port` to a free port does exactly this; quitting the
external app first and re-starting on its usual port works too. Auto-assigned
ports (manifest `runtime.port: 0`) never adopt — there's no fixed address to
probe — so they always spawn an owned process.

## Remote engine management

When the parent passes `--control-port`, engine-manager serves the **`ec`
surface** — a cluster-scoped remote-control endpoint over **pin-based mutual
TLS** (`nvpair-shared/clustertrust`, the same identity + per-peer pins
`nvpair-cluster-manager` mints). It presents this node's cluster leaf, requires a
client cert, and rejects any caller that isn't a byte-for-byte pinned cluster
peer with a `403`. It differs from `em` (`--http-port`) only in what it permits:
`ec` performs privileged operations, while `em` is a read-only inventory. Both are
locked to pinned cluster peers on the LAN — a node's model list tells a caller
which models that machine holds, which is cluster data like any other. `em` also
keeps a plaintext personality on loopback, because this node's own scanner reads
its own inventory that way and must be able to while unclustered.

Membership is evaluated **live**, per handshake and per request, from
`--cluster-dir`. While this node is not a cluster member it presents no leaf, so
every handshake is refused and nothing privileged is reachable; the moment it
becomes a member the same listener serves pinned peers. The listener is therefore
bound for the life of the process rather than only when clustered at startup — a
node joins and leaves a cluster while engine-manager runs, and a surface chosen
at bind time would stay dark until the process was restarted.

Endpoints (all under `/v1`):

| Route | Shape | Purpose |
|---|---|---|
| `GET /v1/engines` | JSON | remote `engine:get-installed` |
| `POST /v1/engines/install` | NDJSON stream | remote install (+ optional start) with live progress |
| `POST /v1/models/pull` | NDJSON stream | remote model pull with live progress |
| `POST /v1/engines/start` | JSON | remote start → `EngineStatus` |
| `POST /v1/engines/stop` | JSON | remote stop → `EngineStatus` |

The streaming routes emit zero or more `{"type":"progress",...}` frames followed
by exactly one terminal `{"type":"result",...}` or `{"type":"error",...}` frame.
The initiating node's engine-manager consumes that stream, relays each progress
frame up as `engine:remote-progress`, and settles the originating
`engine:remote-*` request on the terminal frame — so a UI gets one synchronous
response plus a live progress feed keyed by `opId`.

The broker wires both directions: it passes `--control-port`/`--cluster-dir`,
registers `ec` with the discovery daemon whenever a cluster dir is configured, and
relays engine-manager's `discovery:subscribe{services:[ec]}` into the relay
directory so the peer directory stays current. It does **not** restart
engine-manager on `cluster:identity-changed` — the surface follows membership on
its own.

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--http-port <port>` | `0` (off) | Serve the plain LAN model-list surface (`GET /v1/models`, the `em` service) on this port; the broker passes `:14322` |
| `--control-port <port>` | `0` (off) | Serve the cluster-scoped mTLS remote-control surface (the `ec` service) on this port; callers are admitted only while this node is a cluster member. The broker passes `:14323` |
| `--reserved-port <port>` | `0` (off) | Refuse local or remote engine starts and persisted port changes on a parent-owned proxy alias; the broker configures this from `OLLAMA_HOST` |
| `--cluster-dir <dir>` | _(none)_ | Cluster identity/pin directory; gates the `ec` surface on and supplies the leaf/pins used to serve it and to dial peers |
| `--loaded-poll-interval <sec>` | `5` | Seconds between loaded-model polls that drive `engine:models-changed`; `0` disables the watcher |
| `--log-level <level>` | _(env `NVPAIR_LOG_LEVEL` or `info`)_ | `debug` \| `info` \| `warn` \| `error` |
| `--version` | | Print version and exit |

Logs go to **stderr** via the shared `nvpair-shared/applog` format; stdout is
reserved for JSON-RPC frames in stdio mode.

## Logging & errors

Each managed engine's stdout/stderr is captured into a bounded ring
(queryable via `engine:logs`). Operational failures (install/start/health)
are recorded and surfaced as `errors:report` / `errors:clear`
notifications using the `nvpair-shared/errors` wire shape. The broker
(`nvpair-ui-broker`) forwards them to the `nvpair-errors` registry.
Ids follow `engine-manager:<class>:<engine>`.

## Security posture

Runs **user mode only** — no admin/sudo at runtime (privilege escalation
is reserved for NVPAIR's own install time). It has two optional LAN listeners, and
**both are pin-based cluster mutual TLS with a live membership check** — every
caller is authorized against a per-peer pin, a non-member is refused with a `403`,
and while this node belongs to no cluster it presents no leaf so no handshake
completes:

- **`--http-port`** — the model-list surface (`em`, `GET /v1/models`; the broker
  passes `:14322`). A node's model inventory is cluster data, so LAN callers must
  be pinned peers. This port additionally serves **plaintext on loopback only**,
  which is how this node's own scanner enriches its own card — including when the
  node belongs to no cluster, so a standalone machine still shows its own models.
- **`--control-port`** — the remote-control surface (`ec`, the `engine:remote-*`
  targets; the broker passes `:14323`). mTLS only, no plaintext personality, since
  every route performs a privileged operation.

Unlike the rest of NVPAIR, engine-manager therefore **does terminate inter-node
mTLS itself** (and dials peers' `ec` surfaces with the same pinned identity),
matching how `nvpair-errors` and `nvpair-workload-manager` handle their own
cluster traffic. Managed engines bind **loopback by default**, but a manifest's
`runtime.bind` may open an inference engine to the LAN — Ollama ships
`0.0.0.0` to serve the cluster — overridable per call via
`engine:start {bind}`; readiness/health probes always target loopback.
Downloads are **HTTPS-only** (plain `http` only from loopback) and verified
against the manifest's `sha256` when one is pinned; an unpinned fetch runs
with a loud warning.

## Cross-platform

One binary compiles and runs on Windows, Linux, and macOS × amd64/arm64.
Per-OS variance lives in the manifest first; OS primitives (process
termination, console hiding) are the only build-tagged Go
(`proc_windows.go` / `proc_unix.go`).

## Shutdown

Shuts down on stdin EOF (parent closed the pipe), `SIGINT`/`SIGTERM`, or a
`shutdown` JSON-RPC request — stopping any running engines first so none
are orphaned.

## Managed llama app

On Windows x64, the qualified b10826 / 73a43d1f6 Vulkan runtime automatically
receives a B580 compatibility profile before a managed start when
`llama cli --list-devices` actually enumerates Intel Arc B580. Automatic device
selection and explicit device lists containing that B580 receive the profile;
explicit CPU (including zero GPU layers), CUDA, or other Vulkan device selections retain their options.
PAIR checks the existing managed install receipt, pinned installer identity,
and exact qualified executable SHA-256 before enumeration. Unknown or adopted
runtimes do not receive defaults. Windows on ARM with CUDA, Apple Silicon,
other engines, and other Vulkan devices do not acquire this profile.

The process-local defaults are `GGML_VK_DISABLE_COOPMAT=1`,
`GGML_VK_DISABLE_COOPMAT2=1`, `GGML_VK_DISABLE_INTEGER_DOT_PRODUCT=1`,
`GGML_VK_DISABLE_F16=1`, `GGML_VK_DISABLE_BFLOAT16=1`, and
`LLAMA_ARG_FLASH_ATTN=off`. No ASYNC override is added. These defaults also
reach model-serving children of `llama serve`. They apply to fresh and older
managed installations on their next start after a PAIR upgrade; no reinstall,
model change, or receipt rewrite is needed. Install on an already-installed
runtime stays a no-op, and saved Off stays Off. No global environment, registry, driver,
upstream source, or safety/bounds check is changed.

Existing runtime environment options override inherited environment options;
explicit CLI options take precedence where the vendor supports them. Compatible
explicit values are preserved. A conflicting flash-attention value, ambiguous
Windows spelling of a value-bearing option, or a per-model preset that could override flash attention stops Start
with an actionable retry error. Remove or correct the named override in the
per-user engine manifest or inherited environment before retrying. Disable
variables use presence semantics: any existing value, including empty or `0`,
already disables that feature and is preserved. Missing disables receive `1`.
The vendor's equivalent flash-attention-off values (`off`, `disabled`, `false`,
and `0`) are accepted without rewriting the user's option.
The profile ID `b10826-73a43d1f6-windows-vulkan-b580` and its effective options
appear in the existing manager and engine logs without inference content.

This is a bounded compatibility workaround based on repeated correct requests,
not a uniquely isolated root cause, globally minimal option set, or broad
numerical guarantee. Options affect the whole serving process, including other
Vulkan adapters used together with B580. Maintainers must requalify or remove
the profile when changing vendor identity; `llamacompat.go` pins the qualified
executable so an unrelated future build cannot silently inherit the workaround.
Automatic mixed-device inference still requires native runtime validation.

`llamacpp` installs the official llama app. Windows x64, Linux and Apple Silicon
use the checksum-pinned installer from ggml-org/llama-install.sh commit
`27a82f3a6e0f259f88c2c31cd6b20d858a975f27`. Pins refer to raw repository
bytes, before Windows checkout line-ending conversion. Their supported runtime
is `b10826`. Install reports an already-installed managed runtime as
`already-installed` and changes nothing; there is no update action that moves an
older managed runtime to a newer build, and uninstall-then-install is not run as
a substitute for one.

NVIDIA Windows ARM64 Install stages the two checksum-pinned b10826 CUDA 13.4
archives declared in `install.archives` directly and requires the CUDA device
check below before promotion. The official installer's CUDA path needs a CUDA
Toolkit on the host; without one it produced a CPU build that failed the check
and fell back to these same archives after a wasted attempt, so that attempt is
no longer the default. Setting `install.upstream_first` (restricted to this
platform and driver, with the two pinned archives as fallback) opts back into
trying the current official `ggml-org/llama-install.sh` PowerShell installer
first: the fixed official version endpoint resolves one numeric build, which is
passed to the installer; the whole attempt, including validation, is limited to
three minutes; the version response is limited to 64 bytes and the script to
1 MiB; builds older than the qualified fallback are refused; the installer gets
CUDA enabled and Vulkan skipped.

Before promotion, PAIR checks the exact selected build, nonempty vendor licenses,
and an actual `CUDA0:` (or other numbered CUDA device) row from
`llama cli --list-devices`. This check starts no server and loads no model.
If acquisition, the installer, or validation fails or times out, PAIR uses the
tested b10826 app ZIP plus CUDA 13.4 runtime ZIP declared in `install.archives`,
in a separate clean stage. Parent cancellation stops the operation without
starting a fallback. CUDA on this platform is an upstream preview and requires
a compatible NVIDIA driver; PAIR installs no driver or toolkit.

Each fallback archive is verified before bounded extraction into its owned stage.
Unsafe paths, nonregular entries and file collisions are refused. Both bundles
move together through the existing validation, promotion, rollback and runtime-only
removal flow. Whichever source succeeds is what Install promotes; an installed
managed runtime is never re-fetched or replaced by a later Install request.

`runtime/pair-install.json` records the actual source, selected build, executable
hash, CUDA validation, and any fallback reason/attempt metadata. The latest
installer is pinned to an upstream commit and **verified against a prequalified
SHA-256 before it is executed**, because it is a script PAIR runs rather than an
artifact it only unpacks; an upstream change fails the download and the pinned
CUDA archives take over. The build installed is still whatever the version
endpoint resolves to, so pinning the installer does not pin the engine. The
fallback retains its verified archive pins and recipe identity. Maintainers update those pins together
and repeat platform/failure/retention checks when changing the fallback. Acquired
runtime provenance and vendor license output remain with the managed installation;
normal release signing/notarization belongs to CI/CD, not a local bypass.
Security reports follow the repository's `SECURITY.md` process.

Windows ARM policy is selected inside the install transaction from successful
native CPU and PNP inventory. NVIDIA CPU/hardware identity or a retained verified
CUDA receipt keeps the CUDA-required path above, including with an unbound or
broken driver. Failed/incomplete inventory never selects CPU. Confirmed
non-NVIDIA ARM uses `install.cpu_fetch`: the pinned b10826 official PowerShell
installer with CUDA/Vulkan probes explicitly skipped. Its receipt records
`source: official-pinned-cpu`, `acceleration_policy: cpu`, installer/binary hashes
and `cuda_device_verified: false`.

Intel macOS uses the checksum-pinned official b10826 x64 CPU unified-app tar
archive. `install.archive_root` selects its fixed `llama-b10826` prefix. Bounded
extraction rejects escaping/duplicate paths, hardlinks and special files;
contained versioned dylib links become regular files, never filesystem symlinks.
The existing version/license, candidate promotion/rollback and persistent model
cache lifecycle apply. This artifact requires macOS 13.3 or newer and does not
provide Radeon acceleration.

On Windows x64 the official installer selects its CUDA build only when the CUDA
Toolkit is installed; with just the NVIDIA driver it selects Vulkan. Install
therefore reads one compute capability per GPU from `nvidia-smi` first. When
every NVIDIA GPU reports 7.5 (Turing) or newer, PAIR stages the checksum-pinned
b10826 CUDA 13.3 app ZIP plus CUDA runtime ZIP declared in
`install.cuda_archives`, which need no toolkit, and requires the same `CUDA0:`
device check before promotion. No NVIDIA GPU, an older or unreadable one, an
unavailable archive or a failed device check runs the pinned installer below in
a separate stage instead, and its receipt records the reason as
`cuda_not_used`. Parent cancellation never starts that installer. A CUDA receipt
records `source: pinned-cuda-archives`, `acceleration_policy: cuda`, the archive
recipe and the reported compute capabilities.

Linux applies the same gate with the same pinned installer. When every NVIDIA
GPU reports 7.5 or newer, Install runs the installer restricted to its CUDA
payload (`SKIP_VULKAN=1 SKIP_ROCM=1`; the script has no CPU skip, so a CPU
landing is rejected by the device check) in a private stage, requires the `CUDA0:`
device check before promotion, and retries the transfer once, because the
unrestricted installer falls through to Vulkan or CPU without saying so when
the CUDA payload download fails or no CUDA build exists for the GPU (Jetson
Thor at b10826). If both attempts fail, the unrestricted installer runs in a
separate stage and the receipt records the reason as `cuda_not_used`. A CUDA
receipt records `source: official-installer-cuda`, `acceleration_policy: cuda`
and the reported compute capabilities.

Every install path records the accelerator the promoted runtime actually has:
`pair-install.json` carries `acceleration_policy` (`cuda`, `vulkan`, `metal`,
`cpu`, ...) and `devices`, the rows `llama cli --list-devices` printed, and
`engine:status` exposes them as `acceleration` and `devices` for a managed
runtime so a Vulkan or CPU landing is visible rather than silent. The Windows
x64 installer path and Apple Silicon retain the vendor's accelerator selection.
Selection is not automatic recovery from a GPU hang or incorrect model answer.
`install_supported` and `install_reason` describe recipe availability and
selection requirements, not proof of a GPU or a particular model. Native
validation verifies the actual host.

Downloads and the vendor installer are bounded by lack of progress rather than
a fixed budget: an install or model pull fails when nothing has been transferred
for ten minutes (or after six hours in total), with that reason in the error,
instead of failing a slow but live link at thirty minutes. Progress heartbeats
are emitted while the installer's staging tree grows and while the vendor
downloader prints transfer output.

The per-user `engine-bin/llamacpp` directory contains `runtime` and `previous`.
Models live outside removable application data, in the sibling
`Nvidia Corporation/Personal AI Router Models/llamacpp` directory under the
platform configuration base: LocalAppData on Windows, XDG_CONFIG_HOME (or
`~/.config`) on Linux, and `~/Library/Application Support` on macOS.
An old `engine-bin/llamacpp/models` cache is atomically migrated before use.
Migration refuses existing-destination collisions, redirected/absolute/external
links, and a live configured listener; it never merges or overwrites caches.
Reset/uninstall preserve an unmigrated cache rather than deleting it.
Script installer subprocesses receive a fresh private home,
`SKIP_INSTALL=1`, and the selected vendor build, so user-global llama binaries and
PATH are untouched. Version and bundled license output are checked before
promotion; `runtime/pair-install.json` records installer and executable identity.
On Windows script attempts, the acquired official installer runs
with a process-scoped PowerShell execution-policy override; no saved execution
policy is changed. The Windows ARM64 fallback stage extracts archives without
executing another installer script.
Install stages and verifies the candidate before promoting it: promotion moves
any existing `runtime` slot to `previous` and the candidate into `runtime` with
two renames. If the manager exits between those renames, the next manager
restores the retained runtime when the current slot is absent. Saved Off
remains Off. Uninstall removes
the two runtime slots while retaining models and failed diagnostic stages.
Redirected managed directories are refused rather than mutating external data.
The first headless mutation detects the installed runtime and reconciles listener
ownership itself; it does not require a preceding status request or UI poll.

The foreground `llama serve` process binds loopback, uses the same `LLAMA_CACHE`
and `HF_HUB_CACHE` as downloads, and runs with `--models-autoload`: a cached
model loads on the first request that names it, and the router keeps up to four
models resident (vendor `--models-max` default), evicting idle ones. Cached,
unloaded, loading, and loaded are separate states. Readiness checks the llama.cpp
server identity and router model-list shape. An externally started instance can
be inspected but cannot be mutated; `managed` is false for an adopted listener.
On Windows, both subprocess paths use the standard extended-length cache path
form to support long Hugging Face filenames without changing OS settings.

Launch settings follow the shared editable-launch contract: the fixed startup arguments are `serve --models-autoload`, the reviewed networking controls are `--port` ({server.port}) and `--host` ({server.host}, loopback only), no CORS switch is declared, and the owned model cache environment (`LLAMA_CACHE`, `HF_HUB_CACHE`) is injected on every launch rather than edited; the settings preview rejects assignments to those two names.

These actions use the existing `engine:action` request with `engine: "llamacpp"`:

| Action | Parameters and result |
| --- | --- |
| `list_models` | Current router `/models` response; `data[].id`, nested `status.value` |
| `loaded_models` | Same response; residency extraction keeps only `status.value == loaded` |
| `list_downloaded` | Managed cache IDs as `data[].id`; works while stopped or after uninstall |
| `pull_model` | `{model: "owner/repository:TAG", file?: "file.gguf"}`; official CLI download |
| `import_model` | `{path: "/absolute/model-Q4_K_M.gguf"}`; copies a single GGUF into managed cache |
| `load_model`, `unload_model`, `delete_model` | `{model: "exact ID from inventory"}` |
| `cancel_pull` | `{model: "same requested model"}`; acknowledges the cancellation request |
| `get_version` | Vendor version string |

There is no `update` action for `llamacpp`; a request for one is refused rather
than translated into uninstall followed by install.

Import preserves the source file and requires a single primary GGUF with a
quantization suffix; split-file and auxiliary-only imports are refused. Pulls
require returned owned GGUF files with a supported header and model tensors;
preset/configuration-only results are explicitly unsupported even if the vendor
downloader exits successfully. This format check is not full tensor validation.
Downloads run as cancellable owned CLI processes. Their progress is indeterminate until
completion because vendor CLI output does not provide a reliable percentage.
Terminal completion/cancellation is distinct from a cancellation request.
Load/unload responses wait for observed vendor state instead of treating the
vendor's asynchronous acceptance response as completed work; failed loads surface
their exit status and waiting honors cancellation.
Downloads/imports/deletes refresh the router catalogue; deletion unloads an
observed loaded model first and removes only matching cache artifacts.

Paired control adds `engine:remote-cancel-pull {node, engine, model}` through
the existing pinned-mTLS boundary at `POST /v1/models/cancel-pull`. Mixed-version
peers that lack that route return an explicit error. This layer does not claim
vendor acknowledgement of an inference cancellation or aggregate GPU memory.
