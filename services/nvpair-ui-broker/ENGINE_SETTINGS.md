<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine settings protocol

The broker owns combined settings operations for every engine in its proxy
table — Ollama, LM Studio and llama.cpp — through one profile-generic path: the
same journal, validation, rebind and recovery code serves each of them, and an
engine's differences live in its `engineProxyProfile` entry rather than in the
settings code. Each node owns its own configuration. `nodeId` selects a
discovered, currently pinned peer; omission or the local host ID selects this
node. Bulk propagation is not part of this API.

| Method | Request | Result |
| --- | --- | --- |
| `engine:get-settings` | `{engine, nodeId?}` | Full snapshot |
| `engine:preview-settings` | `{engine, nodeId?, expectedRevision, settings, resolution?}` | Normalized settings, errors, conflict, restart/rebind summary |
| `engine:apply-settings` | Preview request plus `requestId` | `{revision, phase}` acknowledgement |

`engine` names an entry in `nvpair-shared/engines`: `ollama`, `lmstudio` or
`llamacpp`; any other value is refused as an engine without settings. `settings`
contains all three fields: `serverPort`, `proxyPort`, `launchText`. The last
field contains arguments and leading environment assignments, without the
executable or startup subcommand. The argument grammar is
[`pair-arguments-v1`](../nvpair-engine-manager/LAUNCH_TEXT.md). Preview does not
change component configuration or runtime. A snapshot read may persist the
initial revision baseline or reconcile an external component change.

## Engines

Whether a snapshot is `editable`, and why not, comes from engine-manager's
`engine:get-launch`: an engine whose manifest declares no `editable_launch`
block reports "This engine does not support launch settings.", and the broker
then refuses previews and applies for it — port-only ones included, because the
proxy port travels in the same operation. What the broker adds per engine is
the port choreography an explicit choice has to override:

- **Ollama** is an adopted engine: facade `11434`, managed backend from `11435`.
  Its settings validation also reserves an inherited `OLLAMA_HOST` alias port,
  and an explicit choice clears the pending backend move and re-syncs that alias
  reservation with engine-manager.
- **LM Studio** is a managed engine: facade `1234`, managed backend from `1235`.
  Its standalone proxy once wrote `1235` as its own default, so that value is
  excluded when a saved proxy-port store is migrated (below).
- **llama.cpp** is a managed engine that PAIR assigns both ports for: facade
  `8080`, managed engine port `8081`. An inherited `LLAMA_ARG_PORT` moves the
  facade to the port it names and the engine to one above it. It has no
  editable launch arguments unless its manifest declares `editable_launch`; as
  shipped, engine-manager therefore reports it as not editable and its settings
  are read-only until the manifest gains that block.

For every engine an explicit settings choice disables automatic facade takeover
in the same way: `explicitSettings` is set on the engine's proxy runtime, its
managed-facade claim is dropped, the chosen server and proxy ports become the
backend and startup ports, a bind failure on the chosen proxy port is surfaced
instead of triggering a fallback, and readiness reconciliation opens the
engine's startup gate without moving anything.

Each full snapshot includes desired `settings`, `revision`, `appliedRevision`,
`phase` (`idle`, `applying`, `succeeded`, `failed`), `requestId`, `error`,
`effectiveServerPort`, `effectiveProxyPort`, `running`, `adopted`, `editable`,
`reason`, `format`, `epoch`, and `sequence`. A configured runtime server port
does not imply a listening server: consult `running`. The operation receipt is
not a state update. Consume `engine:settings-changed` or fetch the full snapshot
after a response loss. A renderer must retain dirty drafts and reject stale
baseline revisions, even when only arguments changed.

Declared CORS origin lists, switches and explicit booleans can only change on
the engine's owning node. The broker derives an internal `preserveCORS` preview
guard from the authenticated caller, overriding any client-supplied value. It
rechecks canonical CORS policy under the apply lock before accepting a revision;
the early remote ingress check alone is insufficient when operations overlap.

## Ownership, ordering and failures

The node configuration mutex precedes engine operation locks. Worker dispatch
is asynchronous; neither stdio reader waits for an operation. The broker holds
the node lock across validation, journal acceptance and application. The worker
holds its engine lock across override persistence, stopping with the old launch
context, installing the new context, the narrow proxy rebind callback and one
start/readiness wait. The callback validates an active operation token and
acquires no node configuration lock. The port-only setters — `engine:set-port`
and every engine's `<engine>-proxy:set-port` — enter this same operation and
retain their response shape. Legacy lifecycle port overrides and each engine's
automatic port reconciliation share the node lock and revision reconciliation.

Validate both ports together against the unchanged counterpart, registered
PAIR services, aliases, other configured engines/proxies and occupied listeners.
Only this engine's current running port and its proxy listener can be reused
for a swap. Revalidation happens at Apply; OS bind remains the final arbiter of
a competing process. Adopted process engines are read-only; command-mode
engines require their official stop path.

A normalized no-op changes no runtime. Proxy-only edits do not restart the
engine. A stopped engine remains stopped. Running launch/server changes stop
and start once without rewriting explicit enabled intent. Advertisements and
proxy upstream eligibility are disabled during application and restored only
after readiness. Polling advertisers share the node lock.

Validation, stale revisions and journal-write failures cause no runtime
mutation. Once accepted, desired settings remain saved if the engine or proxy
fails. The failed snapshot exposes actual runtime facts and the prior applied
revision; correction/retry is another explicit Apply. Component writes and OS
listeners are recoverable steps, not an ACID transaction. The failed snapshot
preserves the worker's error message. Settings-driven start failures use this
result instead of also raising global lifecycle errors. Normal start failures
still raise a lifecycle error; the exit watcher begins after readiness to avoid
reporting the same startup failure twice.

`engine-settings-operations.json` in the app data directory holds previous and
desired settings, revision, resume intent and operation receipts. Component
engine overrides and each proxy's saved-port file (`proxy-port.json`,
`lmstudio-proxy-port.json`, `llamacpp-proxy-port.json`, as named by the shared
engine table) remain their owners' configuration. Acceptance is written and
synced before component changes. Interrupted applying records replay before
enabled-engine restoration. Explicit settings override automatic facade defaults
on later startup. Unreadable journal data is preserved and suppresses automatic
component rewrites. On first upgrade, a valid existing proxy-port choice in any
engine's store is preserved as explicit when it differs from the engine port
(except LM Studio's obsolete 1235 default). Old stores lack choice provenance,
so a historical automatic move is conservatively preserved too. Unreadable data
blocks automatic recovery. The journal retains up to 256 terminal receipts
per engine; evicted retries must still pass the original expected revision.

## Paired transport

The existing engine-control listener exposes pinned-mTLS POST endpoints
`/v1/engine-settings/get`, `/preview`, `/apply` and GET `/events`. Requests are
limited to 64 KiB and cannot forward another hop. The authenticated caller ID is
carried through correlated `engine:settings-request` / `engine:settings-reply`
notifications to the local broker. Cancellation and pin removal cancel queued
work; the broker rechecks the caller after acquiring the node lock. Once
durably accepted, the target completes under its own eleven-minute deadline even
if the initiating peer disconnects.

`engine:settings-projection` seeds the worker's subscription hub with full local
snapshots. A subscription registers and receives its baseline under one lock.
Slow readers coalesce full snapshots, avoiding missing-field patches. There are
at most 64 subscribers and 64 outbound peer streams. Each stream checks trust
on every frame/heartbeat, bounds writes to five seconds and emits one-second
heartbeats. Clients reconnect after disconnection and discard older sequences
within an epoch; a fresh authority epoch replaces the baseline. Settings never
enter mDNS/public discovery metadata. Older/offline/unpaired peers are read-only
with an explicit unavailable state in the editor.

## Validation

Tests cover literal argv, parser fuzz/table cases, managed-field validation,
real process no-op/proxy-only/launch changes, ownership rejection, persistence
failure, journal recovery, revision conflicts/deduplication, queued cancellation,
three-node pinned TLS subscriptions, revocation/reconnect, bounded slow readers,
proxy persistence failure preserving its listener, renderer ordering and log
redaction. The rendered editor is also exercised with isolated browser fixtures.
Native vendor and Linux/macOS runtime verification remain release-platform
checks; Windows fixture results do not certify vendor-option compatibility.
