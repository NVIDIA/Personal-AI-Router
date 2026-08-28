<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-cluster-manager

> This README is the **integrator's guide**: how a parent process drives
> cluster membership and node pairing over this component's JSON-RPC
> protocol. The binary is built by `build.sh` / `build.bat` alongside the
> other twelve components and is spawned by `nvpair-ui-broker` at startup,
> as an optional worker — a missing binary leaves the broker running without
> cluster pairing.

## Purpose

`nvpair-cluster-manager` owns, for one node: its **cluster membership** (who
its peers are), its **cryptographic identity** (a stable UUID + keypair +
self-signed cert), and the **trusted-node store** (the pinned peer certs
that back mTLS on every inter-node interface). You drive it to *found* a
cluster, *invite* other nodes, *respond* to invites, and *remove* nodes.
Trust between nodes is bootstrapped by an **EAP-NOOB pairing authenticated
by a six-digit PIN** the user carries out of band.

You talk to it over **local JSON-RPC** (this README). It talks to other
nodes' cluster managers over **HTTP/mTLS** on TCP `14321` — that part is
internal; you only need to make sure the port is reachable (see
[Networking](#inter-node-networking)).

## Communication

Newline-delimited **JSON-RPC 2.0**, one object per `\n`. Stdio by default
(`stdin` = requests in, `stdout` = results + notifications out); `--ipc
<path>` switches to a Unix domain socket or Windows named pipe. The local
interface is **request/response** plus a few manager → caller
notifications. The manager never sends you a *request*, so you do not need
an inbound-request handler — only a notification handler.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe instead of stdio |
| `--config-dir <path>` | shared per-user config dir | Where the `cluster/` identity + trusted-store subtree lives (§7.4) |
| `--port <n>` | `14321` | Inter-node HTTP listener port |
| `--log-level <level>` | `info` | One of `error`/`warn`/`info`/`debug`; mutable at runtime via `log/set-level` |
| `--version` | | Print version and exit |

No flag carries the node identity (self-generated) or the cluster identity
(supplied via `cluster:set-identity`) — the binary is self-bootstrapping
under any supervisor.

## Lifecycle

1. **Spawn** the binary as a child, wired to its stdio (or `--ipc`). On
   Windows, hide the console window the same way the other subprocesses are
   spawned (`syscall.SysProcAttr{HideWindow: true, CreationFlags:
   0x08000000}`).
2. **Readiness:** there is no `ready` banner. The node identity is minted
   at startup, so a first successful `cluster:get-node-id` is the natural
   readiness probe.
3. **Hand it the cluster identity:** right after spawn, call
   `cluster:set-identity` with the `cluster_id` / `cluster_friendly_name`
   you hold in `nvpair-node-settings` (empty strings if none). Call it again
   whenever node-settings reports a change.
4. **Shutdown:** the manager exits cleanly on `stdin` EOF (close its pipe)
   or `SIGINT`/`SIGTERM`. There is no reconnect or buffering — a fresh
   manager is a fresh spawn.

## Your responsibilities (the integration contract)

The manager owns membership and trust; **you own the human and the
settings store**. Concretely, the parent must:

- **Persist cluster identity.** When you receive `cluster:identity-changed`
  (after a `cluster:create` or a successful join), write the new
  `cluster_id` / `cluster_friendly_name` to `nvpair-node-settings`. On the
  next startup, feed it back via `cluster:set-identity`. The manager
  *reports* the value; it never writes node-settings itself.
- **Display and collect the PIN.** On `cluster:invite-node` you get back a
  six-digit `pin` — show it to the inviting user. On the joining node you
  get `cluster:invite-received` (no PIN) — prompt that user to type the PIN
  shown on the inviting node, then send `cluster:respond-to-invite`. The
  PIN never crosses the network; the human carries it. If the same node
  sends a newer invite before the first is answered, the manager emits
  `cluster:invite-canceled` for the old ID before `cluster:invite-received`
  for the replacement; dismiss the old prompt and keep the newest one.
- **Resolve invite targets to an address.** `cluster:invite-node` takes a
  reachable `address` (and optional `port`/`nodeId`). You pick the target
  (from `nvpair-node-scanner`, manual nodes, etc.) and pass a resolved
  address; the manager does not do UUID→address resolution for you.
- **Don't re-invite an already-clustered node.** If the target is already
  in a cluster it **rejects** the pairing: `cluster:invite-node` returns
  `state:"rejected"` (`reason:"already-clustered"`) with no PIN. Skip the
  invite up front when discovery already shows the target as clustered
  (its `cluster-uuid=`/`trusted`) — surface it as "Connected" / "Already
  paired" — and only allow a re-pair after the existing relationship is
  removed (`nodes:remove` / `cluster:leave`).
- **Drive the join/leave UI** off `nodes:get-initial` (seed) plus
  `nodes:changed` (live updates, if enabled) — see the [flows](#typical-flows).

## Methods (caller → manager)

All requests carry a JSON-RPC `id` and get exactly one `result` or `error`
back. Negative *outcomes* (failed pairing, wrong PIN, non-member removal)
come back as a **successful `result`** with a terminal `state`/flag — not
as a JSON-RPC `error`. See [Errors](#errors).

```jsonc
// --- identity ---
{"jsonrpc":"2.0","id":1,"method":"cluster:get-node-id"}
//   -> {"nodeUuid":"b3d4...","nodeId":"NODE-A","name":"Lab desk A","certFingerprint":"sha256:...","clusterId":""}

{"jsonrpc":"2.0","id":2,"method":"cluster:set-identity","params":{"clusterId":"cluster-xyz","clusterFriendlyName":"Lab 3 desks"}}
//   -> {"clusterId":"cluster-xyz","clusterFriendlyName":"Lab 3 desks"}   (reflects node-settings; no identity-changed push)

// --- found a cluster (only when clusterId == "") ---
{"jsonrpc":"2.0","id":3,"method":"cluster:create","params":{"clusterFriendlyName":"Lab 3 desks"}}
//   -> {"clusterId":"<new-uuid-v4>","clusterFriendlyName":"Lab 3 desks"}  (also emits cluster:identity-changed)
//   The manager ALWAYS mints a fresh, globally-unique clusterId (UUID v4) — you only pass a friendly name.
//   An explicit create is intentional and is never auto-dissolved by a declined invite.
//   To replay a previously-known id (restore/startup) use cluster:set-identity instead, not create.
//   Called while already clustered -> error -32004 {reason:"already-clustered"}.
//   OPTIONAL: you only need create to name the cluster up front. Inviting while
//   unclustered auto-founds a cluster of one for you (see cluster:invite-node).

// --- membership ---
{"jsonrpc":"2.0","id":4,"method":"nodes:get-initial"}
//   -> {"nodes":[ <ClusterNode>, ... ]}   (members + pending invites; empty list is normal)

{"jsonrpc":"2.0","id":5,"method":"nodes:remove","params":{"nodeId":"NODE-B"}}   // nodeUuid also accepted
//   -> {"nodeId":"NODE-B","removed":true}   (removed:false if it wasn't a member)
//   removing self -> error -32602 {field:"nodeUuid"} — use cluster:leave instead.

{"jsonrpc":"2.0","id":10,"method":"cluster:leave"}   // this node unjoins its own cluster
//   -> {"left":true}   (left:false if already unclustered)
//   announces departure to all members, drops every pin/member, resets to unclustered
//   (emits cluster:identity-changed with clusterId:"" and an empty nodes:changed).

// --- pairing (inviter side) ---
{"jsonrpc":"2.0","id":6,"method":"cluster:invite-node","params":{"address":"192.168.1.22","port":14321,"nodeId":"NODE-B"}}
//   -> {"inviteId":"inv-9f3a1c","state":"pending","pin":"402199"}   (show the PIN; "failed" + no pin if unreachable)
//   -> {"inviteId":"inv-9f3a1c","state":"rejected","reason":"already-clustered"}   (no pin; target is already in a cluster - remove/leave first)
//   if this node is unclustered, it auto-founds a cluster of one first (same as
//   cluster:create, emitting cluster:identity-changed) — no separate create needed.

{"jsonrpc":"2.0","id":7,"method":"cluster:invite-status","params":{"inviteId":"inv-9f3a1c"}}
//   -> <Invite>   (poll the inviter's invite until state flips to "paired"/"failed"/"expired")

// --- pairing (joiner side) ---
{"jsonrpc":"2.0","id":8,"method":"cluster:respond-to-invite","params":{"inviteId":"inv-9f3a1c","accept":true,"pin":"402199"}}
//   -> <Invite with state:"paired">   (state:"failed" + reason:"incorrect-pin" on a wrong PIN; state:"declined" if accept:false)
//   On decline the joiner also best-effort notifies the inviter (pairing phase "decline"),
//   so the inviter's invite-status flips to "declined" and it pushes cluster:invite-declined.
//   On a wrong PIN it likewise notifies the inviter (pairing phase "fail"), so the inviter's
//   invite-status flips to "failed" (reason:"incorrect-pin") and it pushes cluster:invite-failed.
//   If the user never responds, the invite times out on both sides after the invite TTL
//   (default 5m): the receiver expires it, clears the PIN prompt, and notifies the inviter
//   (pairing phase "expire"); both push cluster:invite-expired (see below).

// --- logging ---
{"jsonrpc":"2.0","id":9,"method":"log/set-level","params":{"level":"debug"}}
```

`cluster:invite-node` and `cluster:respond-to-invite` block on multi-round
network I/O for up to the pairing timeout. The manager processes requests
concurrently and **responses may return out of order** (match by `id`), so
set your per-request timeout above the pairing timeout for these two.

## Notifications (manager → caller)

No `id`; never carry an error; handle them off your read loop.

```jsonc
// A pairing arrived and this node was invited. Prompt the user for the PIN, then respond-to-invite.
{"jsonrpc":"2.0","method":"cluster:invite-received","params": <Invite with state:"pending", pin:null> }

// Inviter-side: the joiner declined. Clear the PIN prompt. If this cluster was
// auto-created by invite-node and no sibling
// pending outbound invite remains, the manager also leaves (cluster:identity-changed
// with empty id) so both sides end standalone. An intentional solo cluster is kept.
{"jsonrpc":"2.0","method":"cluster:invite-declined","params": <Invite with state:"declined", pin:null> }

// Inviter-side: the Completion Exchange failed (today only a wrong PIN). Show an
// "Incorrect PIN" error keyed on reason:"incorrect-pin" (empty reason for other causes).
// Same provenance-safe cleanup as decline (a throwaway auto-created solo cluster is left).
{"jsonrpc":"2.0","method":"cluster:invite-failed","params": <Invite with state:"failed", reason:"incorrect-pin", pin:null> }

// A pending invite hit its TTL without accept/decline. Emitted on the INVITER
// (outbound: lost decline / abandoned PIN / offline joiner; same provenance-safe
// cleanup as decline; a purely local fallback that does not signal the joiner)
// and on the RECEIVER (inbound: the local user never entered the PIN — clears the
// PIN prompt and drops the tentative pending-inbound member). The receiver
// signals the inviter (pairing phase "expire") for immediate teardown; each
// side's own TTL is the fallback if the signal is lost.
{"jsonrpc":"2.0","method":"cluster:invite-expired","params": <Invite with state:"expired", pin:null> }

// Joiner-side: the inviter canceled this invite, its TTL expired, or a newer
// invite from the same sender superseded it. Dismiss this invite's PIN prompt.
{"jsonrpc":"2.0","method":"cluster:invite-canceled","params": <Invite with state:"canceled", pin:null> }

// The local clusterId originated here — i.e. after cluster:create, or after this node joined on accept.
// Persist {clusterId, clusterFriendlyName} to nvpair-node-settings. NOT emitted for changes you drove via set-identity.
{"jsonrpc":"2.0","method":"cluster:identity-changed","params":{"clusterId":"...","clusterFriendlyName":"..."}}

// (PROPOSED, spec §4) Full membership snapshot on every change, so you can avoid polling nodes:get-initial.
{"jsonrpc":"2.0","method":"nodes:changed","params":{"nodes":[ <ClusterNode>, ... ]}}
```

## Data shapes

`ClusterNode` (a member or pending invitee, from `nodes:get-initial` /
`nodes:changed`):

```jsonc
{
  "id":        "NODE-B",            // logical id (hostname); the membership key, display only
  "nodeUuid":  "7a1c...",           // stable cryptographic id; the trust principal
  "name":      "Lab desk B",        // display name
  "ipAddress": "192.168.1.22",
  "port":      14321,
  "clusterId": "cluster-xyz",
  "state":     "member",            // "member" | "pending-outbound" | "pending-inbound"
  "joinedAt":  1716998460000,       // epoch ms; null while pending
  "lastSeen":  1716998500000        // epoch ms; null if never contacted
}
```

`Invite` (the pairing session, from invite-node/-status/-received/respond):

```jsonc
{
  "inviteId":  "inv-9f3a1c",        // correlation / dedup key
  "fromNodeId":   "NODE-A",
  "fromNodeUuid": "b3d4...",
  "fromNodeName": "Lab desk A",
  "toNodeId":  "NODE-B",            // null if unknown to the inviter
  "clusterId": "cluster-xyz",
  "clusterFriendlyName": "Lab 3 desks",
  "pin":       "402199",            // present only in the inviter's invite-node RESULT; null everywhere else, never logged
  "state":     "pending",           // "pending" | "paired" | "declined" | "canceled" | "expired" | "failed" | "rejected"
  "createdAt": 1716998400000,
  "respondedAt": null
}
```

## Typical flows

**Found a new cluster (first node).** `cluster:get-node-id` → `clusterId:
""` → `cluster:create` (the manager mints a unique UUID v4 `clusterId`) →
persist the `cluster:identity-changed` payload to node-settings. The node is
now a cluster of one and can invite. This explicit step is **optional** — its
only value over the auto-found below is naming the cluster (`clusterFriendlyName`)
before the first invite.

**Invite a node (inviter).** `cluster:invite-node {address}` → show the
returned `pin` → tell the user to read it to the other node's operator →
watch for `state:"paired"` via `nodes:changed` (or poll
`cluster:invite-status`). If this node isn't clustered yet the invite
auto-founds a cluster of one first (same mint-and-record as `cluster:create`,
emitting `cluster:identity-changed`), so a UI need not orchestrate a separate
create step.

**Receive & accept an invite (joiner).** Handle `cluster:invite-received` →
prompt "enter the PIN shown on `fromNodeName`" → `cluster:respond-to-invite
{inviteId, accept:true, pin}`. A `result` with `state:"paired"` means
joined (the manager also pinned the peer and emitted
`cluster:identity-changed` with the adopted cluster — persist it). Decline
with `accept:false`. A replacement invite from the same `fromNodeUuid`
automatically cancels the older ID; handle its `cluster:invite-canceled`
notification before presenting the replacement.

**Remove a node.** `nodes:remove {nodeId}` → `{removed:true}`. The manager
drops the peer locally, deletes its pin, and best-effort notifies the peer;
removal is authoritative locally even if the peer is offline.

## Errors

`result` vs `error` is the "did it apply?" vs "could I even process this?"
signal. Use the standard JSON-RPC `error` object `{code, message, data?}`.

| Code | Meaning |
|---|---|
| `-32700` / `-32600` / `-32601` / `-32602` | parse error / invalid request / method not found / invalid params |
| `-32603` | internal error — the change did **not** take effect |
| `-32001` | unknown `inviteId` (`invite-status`, `respond-to-invite`) |
| `-32002` | invalid invite state — e.g. `respond-to-invite` on an outbound or already-terminal invite (`data:{inviteId,state}`) |
| `-32004` | precondition not met — `create` while clustered (`{reason:"already-clustered"}`). `invite-node` while unclustered no longer errors: it auto-founds a cluster of one instead. |

Note the deliberate splits: a **malformed** PIN (not six digits) is
`-32602` (no attempt spent), but a **well-formed-but-wrong** PIN is a
`result` with `state:"failed"` and `reason:"incorrect-pin"` (the joiner also
mirrors this to the inviter, which pushes `cluster:invite-failed`). An
unreachable peer is likewise a `failed` *result* but with an empty `reason`.
A target that refuses because it is **already clustered** is a `result` with
`state:"rejected"` (`reason:"already-clustered"`), also not an error. Full table
and per-method surface in spec §7.6.

## Identity, certificates & storage

Generated on first run and reused forever after, under
`<config-dir>/cluster/` (dirs `0700`, key/pin files `0600`):

```
<config-dir>/cluster/
  identity.json            # { "node_uuid": "...", "created_at": <epoch ms> }
  node.key                 # this node's private key (PEM) — never leaves the host
  node.crt                 # this node's self-signed leaf (PEM)
  trusted/                 # the trusted-node store: ONE <peerUuid>.json per pinned peer
```

The **node UUID is the trust principal** (cert subject `CN`/URI SAN), not
the hostname — hostnames are mutable, non-unique, and spoofable. Removing a
node deletes its `trusted/<uuid>.json`, which is the only revocation
mechanism (no CRL/OCSP). This keypair + `trusted/` store is the shared
trust fabric other inter-node services (`nvpair-workload-manager`,
`nvpair-errors` peer-sync) are intended to reuse. Full format in spec §7.4.

## Inter-node networking

- One HTTP listener on TCP **`14321`** serves both the plain-HTTP **pairing
  channel** (`POST /v1/cluster/pairing`, used only during a pairing, before
  any pin exists, authenticated by the PIN) and the **mTLS trusted
  endpoints** (`POST /v1/cluster/members/remove` and `POST /v1/cluster/roster`,
  reachable only by already-paired peers). Open `14321/tcp` in the firewall
  / installer rules.
- The manager no longer runs its own `_nvpair-cluster-manager` mDNS
  advertise/browse. Its `cl` port is registered with the
  `nvpair-node-scanner` discovery daemon (carried on this node's one
  `_nvpair-node` record), and it resolves peer addresses from the
  daemon's `discovery:node-*` events (relayed by the broker); a
  Broker-supplied address always wins, so manual-only nodes are still
  invitable and discovery-less networks still work.

## Cluster trust fan-out

Pairing is point-to-point, but trust is **not** left point-to-point: every
member transitively trusts every other member, so pairing A↔B then A↔C also
leaves B and C able to mTLS each other — no manual B↔C pairing needed. On
each pairing both sides sign an **endorsement** of the peer's cert (Ed25519,
stored with the pin); members then exchange **rosters** over the mTLS
`POST /v1/cluster/roster` endpoint and pin any peer endorsed by a node they
already trust, iterating to a fixpoint.

Pairing also assigns each node a durable, monotonic **admission epoch**. A
removal is an admission-targeted proof containing the original remover's
signature/cert plus endorsements from relays. This lets a machine that was
offline verify its removal through an older trusted member even if it never
pinned the remover. Proof is persisted before de-pinning, survives restart,
has no time-based expiry, and is deleted only after a strictly newer
authenticated admission of that machine is pinned. A unanimous `403` without
such proof is not enough to make a machine leave—another member may simply
have left and dropped its pins. A bare authenticated rejector is instead
de-pinned as a departed peer after the local cluster generation is rechecked,
so the surviving machine converges to the correct solo roster.

Proofs are accepted only for a target admission this machine has actually
authenticated; a signer cannot invent a very large epoch to block future
readmission. Endorsements must also name the endorser's current admission, and
legacy timestamp-only tombstones never revoke admission-aware membership.
Pre-v2 pins are deterministically migrated to their first admission so an
offline machine from an older install remains removable, including older files
whose cluster id was absent. Pairing grants a peer's mTLS pin only after its
membership is durable, and create/join activates admission only after the
provisional self/member/pin set is durable; startup rolls back a crash-left
provisional set. Local teardown is journaled and completed on restart before
stale settings can be applied; while that journal is pending, old pins cannot
authenticate and no new admission can activate. Startup fails closed if removal
replay or cleanup cannot be persisted. Unauthenticated pre-invite sessions must
begin with a valid Type-1 EAP message, are capped at 64, and expire on the invite
TTL when they never reach a user-visible invite.

Reconciles fire on pair success and removal, with a ~30 s heartbeat backstop.
The human PIN stays the only root of trust — nothing is pinned without an
endorsement chain leading back to a real pairing. Full model in spec §7.7.

## Security note

The six-digit pairing PIN is **temporary, low-entropy security debt**: it
protects against passive eavesdroppers and accidental joins but **not an
active man-in-the-middle** on the LAN, and must be replaced by a
high-entropy pairing code before cluster trust is relied on in production.
The repository [`SECURITY.md`](../../SECURITY.md) records the same boundary.

Trust is also **transitive** now (see fan-out above): a compromised member
can endorse certs the whole cluster will pin, widening the blast radius of
one bad node. This deliberate trade-off is why pairing should occur only on
a controlled network.

## See also

- [`../../SECURITY.md`](../../SECURITY.md) — pairing, trust, and mTLS
  boundaries as documented for the product.
- [`../VERSIONING.md`](../VERSIONING.md) — SemVer bump rules and
  version-stamp workflow.
- [`../readme.md`](../readme.md) — product overview and repository layout.
