<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-settings

> **Status: scaffolding.** This subprocess persists four per-node
> settings (`force_ports`, `cluster_auto_sync`, `cluster_id`,
> `cluster_friendly_name`) and pushes cluster identity / auto-sync
> change notifications to its peer. The **names** of the settings
> reflect a later reshape of the design (auto-join-invites and the
> opaque cluster-secret were dropped; a stable cluster identifier and
> a display label took their place). How — and whether — each
> remaining setting gets used end-to-end depends on a node-joining +
> clustering model that is still being defined. Some of these settings
> may not survive that review. The shape below is in place so consumers
> can be wired up now; semantics will follow.
>
> Concrete uncertainties as of writing:
>
> - `force_ports` (a single boolean policy switch) may be removed if
>   the team decides we shouldn't ever kill processes the user
>   started on our default ports.
> - `cluster_id` semantics — exact format (UUID? hash? human-typed?
>   issued by a join handshake?) and what it actually authorizes
>   are pending the security design. Today it's an opaque stored
>   string.
> - `cluster_friendly_name` is display metadata; it has no
>   operational meaning. Anything that needs to act on cluster
>   membership keys off `cluster_id`, not this label.
> - The `connection/cluster-identity` push payload reports the raw
>   `cluster_id`. Consumers derive "are we clustered?" locally
>   from `id != ""` so the real membership predicate can land in
>   the consumer when the security model is settled, without a
>   wire-format change here.

## Purpose

A typed key-value store for per-node NVPAIR preferences, persisted to a
JSON file in the user's config directory and exposed to the supervising
broker over JSON-RPC. Pure datastore: no business logic, no
auto-generation, no defaulting beyond Go zero-values.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0. Stdio by default; `--ipc <path>` switches to a Unix domain socket or Windows named pipe.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--settings <path>` | `settings.json` in the per-user data dir (`%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `~/.config/Nvidia Corporation/Personal AI Router` on Linux) | Override settings file location |
| `--log-level <level>` | `info` | One of `error`/`warn`/`info`/`debug` |
| `--version` | | Print version and exit |

## Persistence

- File: `settings.json` in the per-user data dir — `%LocalAppData%\Nvidia Corporation\Personal AI Router` (Windows), `~/.config/Nvidia Corporation/Personal AI Router` (Linux) — same directory as `manual-nodes.json`.
- Writes go to `settings.json.tmp` first, then `os.Rename` over the destination. A crash mid-save leaves either the previous file intact or the new one fully written, never a half-file.
- File mode is pinned to `0o600` (parent directory `0o700`) regardless of umask. The previous cluster-secret rationale is gone, but per-user settings still shouldn't be world-readable on shared hosts.
- A malformed file on load is renamed aside as `settings.json.corrupt-<unix-ts>` and the subprocess starts with product defaults — refusing to start used to wedge every UI settings call behind a parse error.
- Unknown JSON keys on load are silently ignored, and missing fields take Go zero-values. Adding or removing a setting is a non-breaking on-disk change: older settings.json files with the removed `auto_join_invites` / `cluster_secret` keys load cleanly under the new schema and those keys are dropped on the next save.

## Settings schema

```json
{
  "force_ports":            true,
  "cluster_auto_sync":      false,
  "cluster_id":             "",
  "cluster_friendly_name":  ""
}
```

The four stored fields:

- `force_ports` (bool) — when true, NVPAIR attempts to reserve the Ollama compatibility port for its proxy. Running or unidentified owners are left untouched and reported as blocked; this setting does not authorize a generic process kill. An explicitly saved `false` is preserved as an opt-out.
- `cluster_auto_sync` (bool) — when true, this node accepts automatic syncs of cluster-managed state from peers.
- `cluster_id` (string) — opaque, stable identifier for the cluster this node belongs to. Empty means "not in a cluster". The wire format is byte-for-byte (no normalization, no trimming) so a future migration of the id format doesn't have to coordinate with the datastore.
- `cluster_friendly_name` (string) — human-presentable label for the cluster (e.g. "Lab 3 desks"). Display only — anything operational keys off `cluster_id`.

## JSON-RPC Notifications (manager → caller)

### `ready`

Emitted once on startup with the binary's version.

```json
{"jsonrpc":"2.0","method":"ready","params":{"version":"1.0.0"}}
```

### `connection/cluster-identity`

Emitted **after every successful `settings/set-cluster-id`**. Push
fires on *change* only; callers that need the current value before
the first change call `settings/get-cluster-id` (this is the
React-init path the getter exists for). Payload:

```json
{
  "jsonrpc": "2.0",
  "method":  "connection/cluster-identity",
  "params":  { "id": "<string>" }
}
```

- `id` is the raw stored `cluster_id`. Empty string carries a "no longer in a cluster" signal — consumers that drive a "Leave cluster" affordance flip it off on `id === ""` and on otherwise.
- Consumers derive "are we clustered?" locally from `id !== ""`. The boolean intentionally is NOT pre-derived in the payload, because the real membership predicate (peer attestation, handshake state, whatever security defines) will land elsewhere and embedding the boolean here today would freeze the wrong answer.

### `connection/cluster-auto-sync`

Same lifecycle as `connection/cluster-identity` — fires on change only, after every successful `settings/set-cluster-auto-sync`. Payload:

```json
{"jsonrpc":"2.0","method":"connection/cluster-auto-sync","params":{"value": <bool>}}
```

## JSON-RPC Methods (caller → manager)

Every stored setting has a symmetric get/set pair. The cluster-id
and cluster-auto-sync setters also trigger the matching
`connection/*` notification above; the request/response side stays
as a simple ack so React-init code can rely on a single round-trip.
`set-force-ports` and `set-cluster-friendly-name` are pure get/set
— they have no live-connection consumers and don't emit a push.

```json
// force-ports (bool)
{"jsonrpc":"2.0","id":1,"method":"settings/get-force-ports"}                                            // -> {"value": false}
{"jsonrpc":"2.0","id":2,"method":"settings/set-force-ports","params":{"value": true}}                   // -> {"ok": true}

// cluster-auto-sync (bool)
{"jsonrpc":"2.0","id":3,"method":"settings/get-cluster-auto-sync"}                                      // -> {"value": false}
{"jsonrpc":"2.0","id":4,"method":"settings/set-cluster-auto-sync","params":{"value": true}}             // -> {"ok": true}
                                                                                                        //    plus a connection/cluster-auto-sync notification

// cluster-id (string)
{"jsonrpc":"2.0","id":5,"method":"settings/get-cluster-id"}                                             // -> {"value": ""}
{"jsonrpc":"2.0","id":6,"method":"settings/set-cluster-id","params":{"value":"cluster-abc-123"}}        // -> {"ok": true}
                                                                                                        //    plus a connection/cluster-identity notification
{"jsonrpc":"2.0","id":7,"method":"settings/get-cluster-id"}                                             // -> {"value": "cluster-abc-123"}

// cluster-friendly-name (string)
{"jsonrpc":"2.0","id":8,"method":"settings/get-cluster-friendly-name"}                                  // -> {"value": ""}
{"jsonrpc":"2.0","id":9,"method":"settings/set-cluster-friendly-name","params":{"value":"Lab 3 desks"}} // -> {"ok": true}
```

`settings/get-cluster-id` returns the raw stored `cluster_id`
string — the same shape every other `settings/get-*` getter uses
(`{"value": <stored value>}`) — so the getter round-trips with
`settings/set-cluster-id`. Empty / unset is `{"value": ""}`;
non-empty is the verbatim id. Live cluster-membership changes
come from `connection/cluster-identity` push notifications, not
from polling this getter.

### Removed methods

The schema update removed four method pairs. They return `-32601`
("method not found") and are NOT redirected to the new equivalents,
so any code path still calling them surfaces loudly instead of
silently dropping writes:

- `settings/get-auto-join-invites`, `settings/set-auto-join-invites` — the auto-accept-invites toggle was removed; cluster invites are now always interactive.
- `settings/get-cluster-secret`, `settings/set-cluster-secret` — the opaque shared secret was replaced by `cluster_id`. The push `connection/cluster-identity` was retained but its payload changed from `{secret, clustered}` to `{id}`; callers parsing the old shape must be updated.

### Standard handlers

- `shutdown` — gracefully shuts the manager down. Response is sent before `cancel()` fires.
- `log/set-level` — accepts both request and notification form. Params: `{"level": "<error|warn|info|debug>"}`.

## Validation errors

All setters validate their `value` field via `json.Unmarshal` and return `-32602` on:

- A missing `value` field (`{}` params).
- A wrong-type `value` (string where bool was expected, etc.).

Unknown methods return `-32601`.

## Shutdown

The manager shuts down on:

- stdin EOF (parent closed the pipe).
- `SIGINT` / `SIGTERM`.
- `shutdown` JSON-RPC request.
