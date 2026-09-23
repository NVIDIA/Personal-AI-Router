<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Node Settings (`nvpair-node-settings`)

## 1. Purpose
A typed key-value datastore for per-node preferences, persisted to a single JSON file in the per-user data directory and exposed to the supervising broker over newline-delimited JSON-RPC 2.0. It owns exactly four settings — `force_ports`, `cluster_auto_sync`, `cluster_id`, `cluster_friendly_name` — and guarantees that a value acknowledged to a caller is a value already committed to disk.

It is deliberately a *pure datastore*: no business logic, no value generation, no cross-setting inference, and no defaulting beyond one product default (`force_ports`). Every consumer that acts on a setting — the broker's managed-port reconciliation, the cluster identity restore path, the UI's cluster affordances — interprets the stored value itself. That separation is what lets the clustering and node-joining semantics evolve without touching this service's wire contract.

## 2. Scope
**In scope**
- Load, validate the *shape* of, and persist `settings.json` for the current user.
- Serve a getter and a setter for each of the four settings over JSON-RPC.
- Guarantee atomic, permission-pinned writes and a durable-before-acknowledged ordering.
- Recover from a malformed settings file without wedging its callers.
- Push `connection/cluster-identity` and `connection/cluster-auto-sync` on change so live consumers do not poll.

**Out of scope**
- **Interpreting any setting.** `force_ports` is acted on by `nvpair-ui-broker`'s managed-port reconciliation; `cluster_id` and `cluster_friendly_name` mirror the identity `nvpair-cluster-manager` owns, and the broker replays them into it on startup.
- **Cluster identity, pairing, trust, and membership** — owned entirely by `nvpair-cluster-manager`, which mints or adopts the cluster id and keeps its own durable admission state. This service stores the broker-facing mirror as an opaque `cluster_id` string and never derives, validates, or authorizes anything from it.
- **Port ownership or process termination.** `force_ports` is a policy flag only; the authority to reconcile a port belongs to the broker and `nvpair-engine-manager`.
- **Engine and proxy port persistence** — owned by `nvpair-engine-manager` and by `nvpair-proxy`, which keeps one persisted-port file per engine.
- **Manual peer addresses** — owned by `nvpair-manual-nodes`, which persists `manual-nodes.json` in the same directory.
- **Error reporting and the node's error list** — owned by `nvpair-errors`, which holds it as session state in memory.
- Any network listener. This service has no socket and no cluster surface.

## 3. Key Use Cases
- **Read a setting**: the broker relays `settings/get-cluster-id`; the service answers from its in-memory snapshot with `{"value": "<id>"}`. Reads never touch the disk after startup.
- **Write a setting**: the broker relays `settings/set-force-ports {"value": false}`; the service copies the current settings, applies the candidate value, persists the whole file, and only then commits the new value in memory and responds `{"ok": true}`.
- **Mirror a cluster change**: when `nvpair-cluster-manager` reports `cluster:identity-changed` (founding, adopting an inviter's cluster, or leaving), the broker persists it with `settings/set-cluster-id` and then `settings/set-cluster-friendly-name`.
- **Restore cluster identity**: every time the broker spawns `nvpair-cluster-manager`, it reads `settings/get-cluster-id` and, when non-empty, `settings/get-cluster-friendly-name`, and replays both with `cluster:set-identity`. The first spawn does this synchronously before the broker reports `app:ready`.
- **Live cluster identity**: after a successful `settings/set-cluster-id`, the service emits `connection/cluster-identity {"id": "<id>"}`. The broker re-emits it verbatim; the desktop client re-reads the friendly name, refreshes the cluster manager with `cluster:set-identity`, and updates the UI without polling.
- **Cold-start a live consumer**: because the pushes are change-only, a client that attaches mid-session calls `settings/get-cluster-id` once for the current value and lives on the push thereafter.
- **Managed-port policy check**: the broker calls `settings/get-force-ports` while preparing the managed Ollama (`:11434`) and LM Studio (`:1234`) compatibility facades, and before reserving an inherited `OLLAMA_HOST` loopback alias. It treats an unreadable policy as *blocked* rather than as permission to act.
- **Edge case — first run**: no `settings.json` exists. The service starts with product defaults (`force_ports: true`, everything else zero-valued) and writes the file on the first successful setter.
- **Edge case — malformed file**: the file is unparseable. The service renames it aside to `<path>.corrupt-<unix-ts>`, starts with defaults, and stays fully responsive; it never refuses to start, because a parse error must not wedge every settings call behind a file the user has to find and delete by hand.
- **Edge case — persist failure**: the write or rename fails (a lock from an anti-malware scanner, a full or read-only volume). The setter answers `-32603` and the in-memory value is left untouched, so the service never advertises a value the disk does not agree with.
- **Edge case — missing key in an older file**: a file written before `force_ports` existed is decoded *over* the product defaults, so the absent key resolves to `true`. An explicitly persisted `false` still wins and remains a real opt-out.
- **Edge case — unknown keys on disk**: keys the current schema does not define are dropped on load and are absent from the next write. Schema growth and shrinkage both need no migration step.

## 4. Open Questions / Risks
- **`cluster_id` semantics (closed)**: `cluster_id` is the broker-facing mirror of the cluster identity `nvpair-cluster-manager` owns. The cluster manager's durable admission state is authoritative for authorization and recovery, and wins over an empty or stale mirror when the broker replays it. The mirror authorizes nothing on its own. See `nvpair-cluster-manager/spec.md` §4.
- **`cluster_id` format (closed)**: a founder mints a random UUID v4 and a joiner adopts its inviter's id. The format belongs to the cluster manager; this service still treats the value as an opaque string and must not start validating it without a contract change.
- **`cluster_auto_sync` has no runtime consumer (open — needs a product decision)**: the TUI settings screen displays and edits it and the broker relays its push, but no component acts on the value and the desktop client ignores the push. It may be withdrawn, or given a consumer, once cluster-managed state sync is designed.
- **`force_ports` scope (open — needs a product decision)**: a single boolean policy switch may be withdrawn if the product concludes it must never reconcile a port a user opened. Consumers already treat it as *policy*, never as process-kill authority, which keeps that withdrawal cheap.
- **Membership predicate deliberately not on the wire**: `connection/cluster-identity` carries the raw `cluster_id` and no `clustered` boolean. Consumers derive "are we in a cluster?" locally from `id != ""`. That derivation is duplicated at every consumer on purpose, so the predicate can grow beyond a non-empty id without a wire-format change. The cost is that the duplicated check must be updated in every consumer when it does.
- **Risk — two writers mirror the same cluster change**: both the broker and the desktop client write `cluster_id` and `cluster_friendly_name` when the cluster manager reports `cluster:identity-changed`, in opposite orders. Each setter is atomic and both write the same values, so the file converges, but every cluster change emits `connection/cluster-identity` twice and the first push can precede the matching friendly name.
- **Risk — settings path fallback divergence**: `defaultSettingsPath()` falls back from the per-user data directory to the executable's directory and then to a bare relative `settings.json`. If `appdir.Path()` ever fails, two processes started from different working directories resolve different files and silently disagree about settings. The canonical location is the per-user data directory; the fallbacks are not a supported configuration and callers that care should pass `--settings` explicitly.
- **Risk — dropped push**: a push is emitted after the response and its failure is non-fatal. A consumer that misses one shows stale cluster state until it re-queries. The getters are the recovery path; there is no push replay.
- **Risk — no file watch**: the service is the only writer it knows about. An external edit to `settings.json` while it runs is invisible until restart, and the next setter overwrites it wholesale.
- **Risk — no size or content bound on string settings**: `cluster_id` and `cluster_friendly_name` accept any JSON string. The 1 MiB codec frame cap is the only practical limit.

## 5. Requirements

**Functional**
- Expose a getter and a setter for each of `force_ports`, `cluster_auto_sync`, `cluster_id`, and `cluster_friendly_name` as `settings/get-*` and `settings/set-*` JSON-RPC requests.
- Return `{"value": <T>}` from every getter and `{"ok": true}` from every successful setter.
- Reject a setter whose `params` is not an object carrying a present, correctly-typed `value` with `-32602`, without mutating state.
- Persist the complete settings file before acknowledging any setter, and leave in-memory state unchanged if the write fails.
- Emit `ready {"version": …}` once on startup, and emit `connection/cluster-identity` / `connection/cluster-auto-sync` after each successful `settings/set-cluster-id` / `settings/set-cluster-auto-sync` respectively. Emit no push for the other two setters.
- Answer `-32601` for any unknown method, including the `connection/*` notification names, which are outbound-only and are not requests.
- Handle `log/set-level` as both a request and a notification, and `shutdown` as a request that acknowledges and then stops the read loop.

**Non-functional**
- Compile and run on Windows, macOS, and Linux without per-OS behavior differences in the stored file's shape.
- Write atomically: never leave a torn or half-written `settings.json` on any crash or power loss.
- Pin the data directory to `0o700` and the settings file to `0o600` regardless of umask, and re-apply the file mode after replacement so Windows and POSIX stay aligned.
- Process one frame at a time, so a snapshot-modify-commit window can never be interleaved by another setter.
- Stay responsive after any recoverable input or storage fault; the only fatal startup condition is an unreadable existing file that is not a parse failure.
- Depend on no network, no other worker, and no third-party runtime service.

## 6. Inputs and Outputs

**Inputs** — JSON-RPC 2.0 requests and notifications from the supervising broker over `stdin`, or over the `--ipc` endpoint when one is given. Frames are newline-delimited and capped at 1 MiB by the shared codec. Also read at startup: `settings.json` from disk, and the `--log-level` flag or `$NVPAIR_LOG_LEVEL`.

`Settings` — the on-disk schema, and the authoritative field/tag list:
```json
{
  "force_ports": true,
  "cluster_auto_sync": false,
  "cluster_id": "",
  "cluster_friendly_name": ""
}
```

Setter params are one of two canonical shapes, defined once so a malformed `value` produces an identical error whichever setter it reaches:
```json
{ "value": true }
```
```json
{ "value": "cluster-xyz" }
```

Example request:
```json
{"jsonrpc":"2.0","id":1,"method":"settings/set-cluster-id","params":{"value":"cluster-xyz"}}
```

**Outputs** — JSON-RPC responses and notifications on `stdout` (or the `--ipc` connection), plus the `settings.json` file itself. Structured `slog` diagnostics go to `stderr` via `nvpair-shared/applog` and are captured by the parent. Writes are serialized by the shared codec so frames never interleave, and short writes are looped so a frame is never truncated mid-JSON.

Example response and the push that follows it:
```json
{"jsonrpc":"2.0","id":1,"result":{"ok":true}}
```
```json
{"jsonrpc":"2.0","method":"connection/cluster-identity","params":{"id":"cluster-xyz"}}
```

## 7. API / Interface Contract

There is exactly one interface: a **local interface** toward the same-node supervising broker. This service has no inter-node interface, no HTTP surface, and no listening port.

### 7.0 Methods and notifications

All traffic is newline-delimited JSON-RPC 2.0 over `stdio` (default) or `--ipc` (Unix domain socket / Windows named pipe).

Requests (broker → service):

| Method | Params | Result |
|--------|--------|--------|
| `settings/get-force-ports` | — | `{ value: boolean }` |
| `settings/set-force-ports` | `{ value: boolean }` | `{ ok: true }` |
| `settings/get-cluster-auto-sync` | — | `{ value: boolean }` |
| `settings/set-cluster-auto-sync` | `{ value: boolean }` | `{ ok: true }` |
| `settings/get-cluster-id` | — | `{ value: string }` |
| `settings/set-cluster-id` | `{ value: string }` | `{ ok: true }` |
| `settings/get-cluster-friendly-name` | — | `{ value: string }` |
| `settings/set-cluster-friendly-name` | `{ value: string }` | `{ ok: true }` |
| `log/set-level` | `{ level }` | `{ level }` |
| `shutdown` | — | `null` |

Notifications (service → broker): `ready {version}` once on startup; `connection/cluster-identity {id}` after each successful `settings/set-cluster-id`; `connection/cluster-auto-sync {value}` after each successful `settings/set-cluster-auto-sync`. `log/set-level` is also accepted as an inbound notification, in which case the level changes with no response. Any other inbound notification is logged and ignored.

### 7.1 Local interface (UI Broker ↔ Node Settings)

`nvpair-ui-broker` spawns this service as a child and owns the pipe. It relays every `settings/*` request **verbatim** — the method names are already `settings/`-prefixed, so no translation happens — using a bounded synchronous call, because settings operations are local and fast. If the worker is absent the broker answers the client `-32000 node-settings not available`; if the call itself fails it answers `-32000 settings call failed: <error>`; if the worker returns a JSON-RPC error the broker relays that error's code and message unchanged.

The broker is also a direct caller in its own right, not only a relay:

- `settings/get-force-ports` while preparing the managed Ollama and LM Studio facades and the inherited `OLLAMA_HOST` alias.
- `settings/get-cluster-id` and `settings/get-cluster-friendly-name` each time it spawns `nvpair-cluster-manager`, to replay the identity with `cluster:set-identity`.
- `settings/set-cluster-id` then `settings/set-cluster-friendly-name` on each `cluster:identity-changed` from the cluster manager. These writes run off the cluster manager's reader and log failures rather than surfacing them, because the cluster manager already holds the authoritative state.

The broker re-emits `connection/cluster-identity` and `connection/cluster-auto-sync` to its own clients verbatim and unconditionally — there is no opt-in subscription for these two. Every other notification from this service is ignored by the broker, except `errors:report` / `errors:clear`, which the broker routes into the errors pipeline like any other producer's.

### 7.2 Settings schema and defaults

| Setting | Type | Default | Push on change | Consumed by |
|---------|------|---------|----------------|-------------|
| `force_ports` | boolean | `true` | no | `nvpair-ui-broker` managed Ollama and LM Studio facades and the inherited `OLLAMA_HOST` alias |
| `cluster_auto_sync` | boolean | `false` | `connection/cluster-auto-sync` | none at runtime; displayed and edited by the TUI settings screen |
| `cluster_id` | string | `""` | `connection/cluster-identity` | broker replay into `nvpair-cluster-manager`; desktop cluster state |
| `cluster_friendly_name` | string | `""` | no | broker replay into `nvpair-cluster-manager`; desktop cluster snapshot |

The TUI settings screen reads and writes all four settings through the broker relay.

`force_ports` is the one product default: the file is decoded *over* `defaultSettings()`, so a missing key enables the recommended managed-port policy while an explicitly persisted `false` remains an opt-out. The other three default to their Go zero values.

`cluster_id` is the identity anything operational keys off; `""` means "not in a cluster". `cluster_friendly_name` is presentation metadata with no operational meaning.

`force_ports` grants **policy consent only**. Consumers must leave a running or unidentified port owner untouched and surface a blocked state instead; the setting never confers generic process-kill authority. When the broker cannot read or decode the policy it treats the managed facade as blocked rather than permitted.

### 7.3 Persistence and the durable-before-acknowledged guarantee

The canonical file is `settings.json` in the per-user data directory resolved by `nvpair-shared/appdir` — `<base>/Nvidia Corporation/Personal AI Router/settings.json`, where `<base>` is `%LocalAppData%` on Windows (deliberately non-roaming, because this is machine-specific state), `$XDG_CONFIG_HOME` or `~/.config` on Linux, and `~/Library/Application Support` on macOS. It sits beside `manual-nodes.json` so all per-user configuration is co-located. `--settings <path>` overrides the location.

Every setter follows **copy, save, then commit**:

1. Take a snapshot of the current settings under a read lock.
2. Apply the candidate value to the snapshot.
3. Persist the whole snapshot to disk.
4. Only on success, take the write lock and commit the snapshot as the new in-memory state.
5. Respond, then emit the push if this setting has one.

A failure at step 3 answers `-32603` and skips steps 4 and 5 entirely, so the acknowledged state and the on-disk state can never disagree. Because the read loop dispatches one frame at a time, no other setter can interleave the snapshot/commit window.

The write itself is the standard atomic replace: create the directory with `0o700`, write `<path>.tmp` with `0o600`, `os.Rename` it over the destination, then `os.Chmod` the destination back to `0o600`. The temporary file is deliberately a sibling of the destination so the rename stays on one filesystem and is therefore atomic. A failed rename removes the stray temporary file. The explicit `Chmod` after rename matters because file creation honors umask on POSIX and ignores mode bits on Windows.

### 7.4 Load outcomes and corrupt-file recovery

`load()` has exactly three outcomes:

- **Missing file** — logged at info, product defaults retained, first run proceeds normally.
- **Well-formed file** — decoded over the defaults; keys the schema does not define are dropped. This is what makes both adding and removing a setting non-breaking on disk with no migration step: a new field is absent-and-defaulted on old files, and a withdrawn field is quietly discarded on the next write.
- **Malformed file** — renamed aside to `<path>.corrupt-<unix-ts>` and startup continues with defaults. The rename preserves a forensic copy the user can recover from while keeping the application usable. If the rename *also* fails, the failure is logged at error level and startup still continues with defaults; the next successful setter overwrites the bad file. Losing a few preferences is preferable to an unresponsive settings surface.

Any other read error — an existing file that cannot be read at all — is fatal at construction and the process exits, because that indicates a storage or permission fault the service cannot paper over.

### 7.5 Push lifecycle

The `connection/*` pushes are strictly **change-only**. `Run()` emits `ready` and nothing else, even when the loaded file already contains non-default cluster values. A client that needs the current value before the first change calls the getter once.

Each push is emitted **after** the setter's response, so a caller's completion callback always observes the notification strictly later than its own response rather than racing it. A push that fails to serialize is logged at warn level and is not retried: the response has already gone out and the file is already written, so the consumer can recover with a getter.

`settings/set-force-ports` and `settings/set-cluster-friendly-name` emit no push, because neither has a live-connection consumer — the former is polled by the broker at the moment it matters, the latter is display metadata a client can fetch on first paint.

### 7.6 Error codes

| Code | Meaning | Raised when |
|------|---------|-------------|
| `-32601` | method not found | any unrecognized method, including the outbound-only `connection/cluster-identity` and `connection/cluster-auto-sync` names, and any withdrawn setting's methods |
| `-32602` | invalid params | `params` will not decode, or `value` is absent or of the wrong JSON type |
| `-32603` | internal error | the settings file could not be persisted; the message carries the underlying storage error |

The `connection/*` names returning `-32601` is a deliberate, tested property: they are notification names, never request methods, and must not acquire request aliases.

### 7.7 Versioning
- `settings/*` method names are namespaced and additive — a new setting is a new `get`/`set` pair, never a change to an existing one.
- The on-disk schema evolves additively in both directions: unknown keys are dropped on load, and withdrawn keys vanish on the next write. Neither requires a migration step or a schema version field.
- Notification payloads grow by adding optional fields. `ClusterIdentityParams` carries only `id` and `ClusterAutoSyncParams` only `value`, so either can gain fields without breaking existing consumers.
- `Version` is stamped at build time via `-ldflags "-X main.Version=…"` from `services/versions.json` and reported in the `ready` payload and by `--version`.

## 8. Dependencies
- **Upstream**: `nvpair-ui-broker` — the parent process that spawns this service, owns its stdio pipe, and is its only peer.
- **Downstream**: none. This service calls no other service and initiates no connections.
- **External**: the per-user filesystem location resolved by `nvpair-shared/appdir`. First-party shared packages only: `nvpair-shared/jsonrpc` (the newline-delimited codec), `nvpair-shared/ipc` (the `--ipc` transport and its platform split), `nvpair-shared/applog` (structured logging and `log/set-level`), and `nvpair-shared/appdir`. No third-party runtime services and no network dependency.

## 9. Data Ownership
- **Owned**: the four settings and the `settings.json` file that holds them. This is one of the few workers that owns durable user state.
- **Source of truth**: yes, for these four values. It is *not* the source of truth for anything derived from them — cluster membership belongs to `nvpair-cluster-manager`, and port ownership decisions belong to the broker and `nvpair-engine-manager`.
- **Storage**: a single JSON file, `settings.json`, in the per-user data directory — `%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `~/.config/Nvidia Corporation/Personal AI Router` on Linux, `~/Library/Application Support/Nvidia Corporation/Personal AI Router` on macOS. Directory mode `0o700`, file mode `0o600`. No database. In-memory state is a single `Settings` value guarded by a `sync.RWMutex`.

## 10. Design Constraints
- **Performance**: getters are in-memory and effectively free. Setters cost one serialize plus one write and rename, and are on interactive settings paths only — there is no hot path here. The broker's `settings/get-force-ports` reads during facade preparation, and its identity reads when it spawns `nvpair-cluster-manager`, are synchronous. Getters never touch the disk, but the read loop is serial, so a getter queued behind a setter waits for that setter's write.
- **Scalability**: one instance per node, one peer, four settings, single-digit calls per session. Nothing here scales with cluster size.
- **Reliability**: durable-before-acknowledged on every write; atomic replace so no crash yields a torn file; corrupt-file recovery keeps the service available. A persist failure is reported to the caller rather than silently swallowed. There is no retry and no write queue — the caller decides whether to retry.
- **Security**: no network surface at all, so nothing here is remotely reachable; the only channel is the parent's pipe. The file is per-user state pinned to `0o600` in a `0o700` directory. This service holds **no** secrets, key material, or credentials, and implements no cryptography — `cluster_id` is an opaque label, not an authenticator, and possession of it proves nothing. Cluster trust is owned entirely by `nvpair-cluster-manager`.
- **Compliance**: no PII. The stored values are a boolean policy flag, a boolean sync preference, an opaque cluster identifier, and a user-chosen cluster label. Logs record the *presence* of `cluster_id` and `cluster_friendly_name` as booleans (`has_cluster_id`, `has_cluster_friendly_name`) rather than their values.

## 11. Assumptions
- The broker is always the parent: it spawns this service, owns the pipe, and is the only caller. On `stdin` EOF the service shuts down cleanly — there is no reconnect and no buffering.
- This service is the only writer of `settings.json`. Nothing detects or reconciles an external edit made while it is running.
- Requests arrive well-formed from the broker, which is itself relaying already-validated client calls; the service still validates every `params` shape rather than trusting that.
- Frames stay well under the codec's 1 MiB cap. Nothing here is large.
- Consumers treat `force_ports` as consent to a policy, never as authority to terminate a process, and treat `cluster_id` as opaque.
- The per-user data directory is writable. If it is not, setters fail loudly and getters continue serving the last known values.

## 12. Failure Modes and Mitigations
- **Malformed `settings.json` at startup**: refusing to start would wedge every settings call in the UI behind a file the user must find and delete by hand. → Rename aside to `<path>.corrupt-<unix-ts>`, start with defaults, stay responsive, and keep a forensic copy.
- **Rename-aside of a corrupt file fails** (a scanner or another process holds a lock): the bad file remains on disk. → Log at error level and continue with defaults anyway; the next successful setter overwrites it.
- **Persist failure during a setter** (full volume, read-only mount, lock contention): a naive implementation would leave memory advertising a value the disk never received. → Copy-then-save-then-commit means the in-memory value is only updated after a successful write; the caller gets `-32603` and the old value stays authoritative.
- **Crash or power loss mid-write**: a torn file would be unparseable on next start. → Write a same-directory temporary and `os.Rename` over the destination, so the file is either wholly old or wholly new; a stray temporary is removed on rename failure.
- **Umask or platform mode differences widen file permissions**: per-user state could become group- or world-readable. → `MkdirAll` at `0o700`, write at `0o600`, and an explicit `os.Chmod` after the rename so POSIX umask and Windows mode-bit indifference both end at the intended mode.
- **Notification emit fails after a successful set**: a consumer keeps showing stale cluster state. → Treat the emit as best-effort and log at warn; the response and the file are already correct, and the consumer recovers via the getter. No replay is attempted.
- **Unreadable managed-port policy when the broker asks**: silently assuming `true` would authorize reconciling a port on unverified policy. → The broker treats an error, an RPC error, or an undecodable result as *blocked* and surfaces that state, rather than proceeding.
- **Recoverable decode error on an inbound frame**: a single bad line could otherwise kill the read loop. → The shared codec reports a per-frame decode failure as recoverable and the loop logs it and continues; only a terminal transport error or EOF ends the loop.
- **Parent exits** (`stdin` EOF): an orphaned datastore would serve nobody. → EOF ends the read loop and the process exits cleanly. `SIGINT` / `SIGTERM` and a `shutdown` request cancel the same context; `shutdown` acknowledges before cancelling.

## 13. Observability
- **Logging**: structured `slog` to `stderr` via `nvpair-shared/applog`, captured by the parent's debug panel. Notable events: transport selection (stdio or IPC path); settings loaded, with `force_ports` and `cluster_auto_sync` values and *boolean presence* of the two strings; settings file missing and defaults used; malformed file renamed aside, with the backup path and parse error; per-setting persist failures; rejected and accepted `log/set-level` changes; ignored inbound notifications; unknown-method responses; shutdown cause. Values of `cluster_id` and `cluster_friendly_name` are not logged.
- **Metrics**: none. There is no metrics endpoint and no in-process counters; the call volume does not justify either. Operational insight comes from the log stream and from the broker's view of call outcomes.
- **Alerts**: none emitted directly. A persist failure surfaces to the user as the setter's `-32603` through the broker and the UI; a blocked managed-port policy surfaces through the broker's errors pipeline, not from here.

## 14. Sample usage
The broker starts and spawns `nvpair-node-settings` as a child on stdio. The service resolves `settings.json` in the per-user data directory, finds no file on a first run, keeps the product defaults (`force_ports: true`, the rest zero-valued), and emits `ready` carrying the version stamped into the binary at build time from `services/versions.json`.

While preparing the managed Ollama compatibility facade, the broker calls `settings/get-force-ports` and receives `{"value": true}`. It combines that consent with `engine:status` from `nvpair-engine-manager` before deciding whether it may reconcile the compatibility port — and if this call had failed, errored, or returned something undecodable, it would have marked the facade blocked instead. It repeats the same check for the LM Studio facade.

The user invites a peer from the UI. Because this node is unclustered, `nvpair-cluster-manager` founds a cluster of one, mints its id, names it after the host, and reports `cluster:identity-changed {"clusterId":"cluster-xyz","clusterFriendlyName":"lab3-desk"}`. The broker persists it with `settings/set-cluster-id {"value":"cluster-xyz"}` and then `settings/set-cluster-friendly-name {"value":"lab3-desk"}`. For each, the service snapshots its settings, applies the candidate, writes the complete file through a sibling temporary and an atomic rename, chmods it to `0o600`, commits the value in memory, and responds `{"ok": true}`. The friendly-name setter stops there. The cluster-id setter then emits `connection/cluster-identity {"id":"cluster-xyz"}`, which the broker re-emits verbatim; the desktop client re-reads the friendly name, refreshes the cluster manager with `cluster:set-identity`, and updates the UI without polling. A client that attached after the change instead calls `settings/get-cluster-id` once and lives on the push from then on.

On the next start, the broker spawns the cluster manager, reads both values back, and replays them with `cluster:set-identity` before reporting `app:ready`.

Later the user leaves the cluster. The cluster manager reports an empty identity, the broker persists `settings/set-cluster-id {"value":""}`, and the service pushes `connection/cluster-identity {"id":""}`. The desktop client drops peers' workloads, and every consumer's local `id != ""` check flips the affordance back off. On teardown the broker sends `shutdown`; the service responds `null`, cancels its context, and exits — or, if the broker simply exits first, `stdin` EOF ends the read loop with the same clean result.

## 15. Process model, CLI, and build wiring

The service is a single-peer child process. `nvpair-ui-broker` spawns the binary named by its `--settings-path` flag, which the desktop application always passes, and otherwise looks for `nvpair-node-settings` in its own working directory. It speaks JSON-RPC over the child's `stdin`/`stdout`.

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--ipc <path>` | *(stdio)* | Use a Unix domain socket or Windows named pipe instead of `stdin`/`stdout`. A dial failure is fatal. |
| `--settings <path>` | `settings.json` in the per-user data dir | Override the settings file location. |
| `--log-level <level>` | `info`, or `$NVPAIR_LOG_LEVEL` | One of `debug`, `info`, `warn`, `error`. Changeable at runtime via `log/set-level`. |
| `--version` | | Print the stamped version and exit. |

With no `--ipc`, the transport is a stdio adapter over `os.Stdin` / `os.Stdout` whose `Close` is a no-op. Shutdown has three equivalent triggers, all cancelling the same context: `stdin` EOF (the parent exited), `SIGINT` / `SIGTERM`, and a `shutdown` request — which responds before cancelling so the caller sees the acknowledgement. There is no reconnect and no state to flush, because every acknowledged write is already durable.

Build and packaging: `build.sh` and `build.bat` read the component version from `services/versions.json`, build with `-ldflags "-X main.Version=<version>"` as step 8 of 12, and stage the binary into `services/build/bin/`. `installer/nvpair-setup.nsi` bundles the Windows executable and removes it on uninstall. Because this service is pure stdio with **no listening port**, it needs no firewall rule — it is one of the few shipped binaries with no network exposure at all.
