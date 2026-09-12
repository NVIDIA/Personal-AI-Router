<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-engine-manager

A config-driven control plane for local inference engines (Ollama today;
Intel/others via a dropped-in manifest). It manages everything about an
engine **except serving inference**: detect, user-mode install,
start/stop/restart, health, and config-declared actions. Adding an engine
is a JSON manifest, not code.

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
default removes the override file. Because the chosen port lives in the
effective manifest, restore is automatic: a normal `engine:start` (no explicit
port) and the adopt-on-fixed-port path both come up on the retained port.
Moving a **running, adopted** engine is **refused** with an error (nothing is
persisted) — NVPAIR can't relocate a process it didn't start; see Adoption below.

## Lifecycle

```
NotInstalled --engine:install--> (HTTPS download + sha256 verify, fail closed + user-mode run) --> Stopped
Stopped      --engine:start----> (adopt if already serving the port, else spawn) --> Running --health--> Running
Running      --engine:stop-----> (stop signal, wait up to the manifest grace, then SIGKILL/pgid) --> Stopped
```

Detect uses the manifest's `detect` paths. Install is one-shot and
user-mode — an HTTPS download, verified against the manifest's `sha256`
pin. A manifest without a pin fails the install closed (any executed
artifact must be pinned in the reviewed manifest); the escape hatch is
`NVPAIR_ALLOW_UNPINNED_DOWNLOADS=1`, which runs the unpinned fetch with a
loud warning instead. The bundled manifests all carry live-verified
digests. Start
waits for the readiness probe, then runs a periodic health probe; an
unexpected exit is reported. The bundled Ollama manifest allows up to ten
minutes for startup because GPU discovery can exceed the previous 30-second
allowance on supported Windows systems. The deadline remains finite: if Ollama
never serves its readiness endpoint, engine-manager stops the owned process and
reports the failed start. Stop sends one stop signal and waits for the engine
to exit up to the manifest's stop grace (default 5s, `grace_s` from the
manifest's stop spec): SIGTERM to the process group on Unix, `taskkill /T /F`
on Windows — where the windowless engines we spawn can't receive a graceful
(non-`/F`) close, so a forced terminate is the only signal that actually stops
them. An engine still alive after the grace is escalated to a forced kill
(SIGKILL/pgid on Unix) so a hung engine cannot block shutdown forever.

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
