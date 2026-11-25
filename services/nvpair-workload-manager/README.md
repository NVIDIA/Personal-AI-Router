<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-workload-manager

## Purpose

Propagates inference-workload lifecycle events across the cluster, so every node
has the same view of what work is queued, running, or finished — and on which
node. That shared view is what makes the Jobs list meaningful on any member and
what supplies the pending-work side of `nvpair-job-scheduler`'s GPU-aware
ranking.

The manager is a **relay and deduplicator**, not the source of truth. The broker
and proxies decide a workload's state; this component passes `workloadInfo`
through opaquely and forwards it.

Its second job follows the same rule: it **carries another node's cluster model
alias document** to the node that owns it, because its cluster-mTLS listener is
the one port on a node a pinned peer can always reach — see *Peer model alias
writes* below. It neither validates, stores, nor interprets a list on the way.

## Communication

Two channels:

- **Local** — bidirectional newline-delimited JSON-RPC 2.0 with the supervising
  `nvpair-ui-broker`. Stdio by default; `--ipc <path>` switches to a Unix domain
  socket or Windows named pipe.
- **Inter-node** — a cluster-mTLS server on port `14320` that accepts the same
  JSON-RPC frames from pinned peers on `POST /v1/workloads/events`, plus
  `POST /v1/settings/model-aliases` for a peer's alias-document write, and an
  outbound broadcaster that pushes local events to those peers with bounded retry.

Peers are discovered through the broker's node records rather than by this
component browsing the network itself.

## CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--port <n>` | `14320` | Inter-node port to listen on and advertise |
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt` / `node.key` + `trusted/`) supplying the identity and pins the inter-node mTLS channel requires. Without it the node has no cluster identity and exchanges no inter-node traffic |
| `--log-level <level>` | `info` | One of `error` / `warn` / `info` / `debug` |
| `--version` | | Print version and exit |

## Transport security

The inter-node interface is **always cluster mTLS**. There is no plaintext
personality on it. The listener presents this node's cluster certificate,
requires a client certificate, and refuses any caller that is not a pinned
cluster peer with `403`. Pins are re-read per request, so removing a member takes
effect immediately without a restart.

A node that is not a current cluster member has no identity to present, so it
neither serves nor broadcasts inter-node workload traffic at all. The port stays
bound: the certificate is resolved per handshake, so a node converges to serving
after it joins without a rebind or a restart.

Workload state is therefore only ever exchanged between paired members. See the
repository [`SECURITY.md`](../../SECURITY.md) for the surrounding trust
boundaries.

## Lifecycle events

Inbound lifecycle notifications, on either channel:

| Method | Resulting state |
| --- | --- |
| `workload:submitted` | `queued` |
| `workload:started` | `running` |
| `workload:completed` | `completed` |
| `workload:errored` | `failed` |

Each carries `params.workloadInfo`. Removal uses `workloads:remove` with
`params.workloadId` and the origin `params.originatedFrom`.

## Workload shape

Defined in [`workload.go`](workload.go):

| Field | Notes |
| --- | --- |
| `id` | Stable workload identifier |
| `model`, `engine` | What was requested and by which engine |
| `runId` | Producing process's nonce, minted at proxy startup. Part of the dedup key: `id` is a per-facade counter that every engine facade starts at 1 and that resets on restart, so `runId` keeps a reused id from colliding with an older workload |
| `state` | `queued`, `running`, `completed`, `failed`, or `cancelled` |
| `originatedFrom` | Node the request entered the cluster on |
| `scheduledOn` | Node it was routed to; absent until a target is chosen |
| `createdAt`, `startedAt`, `completedAt` | Epoch milliseconds; the last two are nullable |
| `error` | Normalized failure text, nullable |
| `requesterId` | Optional client attribution, nullable |
| `seq` | Producer's event counter, from 1. Part of the dedup key, so a workload that returns to a placement it already had is not mistaken for a redelivery |

Optional and nullable fields use pointers so a peer's payload round-trips without
inventing zero values.

Prompts, messages, and response bodies are **not** part of this contract and must
never be added to it.

## Output to the broker

Translated remote events are forwarded to the broker as:

| Notification | Params |
| --- | --- |
| `workloads:upsert` | `{ workloadInfo }` |
| `workloads:remove` | `{ workloadId, originatedFrom }` |
| `settings/apply-model-aliases` | `{ requestId, callerUuid, aliases }` — a peer's alias write for this node to apply |
| `settings/peer-model-aliases-result` | `{ requestId, applied, error? }` — the owning node's verdict on a write this node asked for |
| `ready` | `{ version }` — startup handshake |

Duplicate events arriving from more than one peer are collapsed before they reach
the broker, so a workload observed over several paths is reported once.

The two `settings/` rows are not workload traffic: they belong to *Peer model alias
writes* below, and they are notifications in both directions because this interface
carries no request/response — only `requestId` ties a verdict to the Save that
asked for it.

## Peer model alias writes

A cluster model alias is a name one node answers to. The document that holds a
node's aliases belongs to **that node**: only its own `nvpair-node-settings`
validates or stores it, and nothing replicates it. So "edit another machine's
aliases" cannot be a shared-registry edit — it is that node's document, handed to
that node, which validates it, applies it, and says whether it did.

This component carries the document because its cluster-mTLS listener is the one
port on a node a pinned peer can always reach: it is bound for the life of the
process, and every peer already knows the address from the node record. A proxy's
facade port would have been a tidier home for a settings route, but that listener
follows an engine and closes whenever the engine is stopped — which is exactly when
an operator may need to fix the alias that was pointing at it.

**The courier is not the authority.** This component never validates, stores, or
interprets an alias: it decodes a document just far enough to know one arrived and
hands it to the local broker, which asks `nvpair-node-settings`. That keeps one
validator for local and remote writes, and it means a node that refuses answers in
its own words — a sentence the caller hears unchanged.

| Direction | Name | Params |
| --- | --- | --- |
| Broker → WM (`stdin`) | `settings/replace-peer-model-aliases` | `{ requestId, nodeUuid, aliases }` — carry this document to that node |
| WM → WM (`POST /v1/settings/model-aliases`) | `settings/replace-model-aliases` | `{ requestId, aliases }` |
| WM → Broker (`stdout`) | `settings/apply-model-aliases` | `{ requestId, callerUuid, aliases }` |
| Broker → WM (`stdin`) | `settings/apply-model-aliases-result` | `{ requestId, applied, error? }` |
| WM → Broker (`stdout`) | `settings/peer-model-aliases-result` | `{ requestId, applied, error? }` |

All five names live in `nvpair-shared/noderec`, which the broker's side of the same
pipe names too: a frame name that changes on one side is a write nobody answers.

- **An absent list is refused, not read as an empty one** (`400`), because the
  write replaces the whole document and guessing here would erase a peer's aliases
  on a malformed frame. `aliases: []` is the only way to ask for none, and a
  `requestId` is required for the same reason — a write nobody can ask about is a
  write nobody can confirm. A second write with an in-flight id gets `409`.
- **Identity comes from the certificate.** The route re-derives membership per
  request, so a peer removed from the cluster stops being served without a restart,
  and the caller it reports to the local broker is the cluster id on that
  certificate, never a field in the body. The broker logs it, so a write that
  surprises an operator is traceable to a machine.
- **The status codes say who spoke**: `200` `{ requestId, applied: true }`; `400`
  a malformed frame, a method other than `settings/replace-model-aliases`, a
  missing `requestId`, or an absent `aliases`; `403` an untrusted caller; `405` a
  non-`POST`; `409` a duplicate in-flight id; `422` the owning node's settings
  service refused the document, with that service's own sentence as the body; `502`
  this node has no settings relay wired; `504` the document was handed over and no
  verdict arrived in time. A `504` is deliberately not called a failure — the
  change is already with the settings service, and "failed" is a sentence an
  operator would act on.
- **One attempt, and a verdict either way.** Unlike lifecycle broadcast, an alias
  write is posted once: retrying a write whose outcome is unknown is a second
  whole-document replace racing the first, so the operator decides by saving again.
  A well-formed request always gets a verdict back to its broker, including "could
  not reach that node" and "this computer is not in a cluster" — a request that
  went unanswered would leave the operator's Save spinning until the broker gave
  up, which is slower and less specific than the answer available here.
- **Timeouts nest, innermost first**: `peerAliasApplyTimeout` (2 s, this node's
  wait for its own settings service) sits under `perPeerTimeout` (3 s, the pooled
  client's hop to a peer), which sits under `peerAliasTripTimeout` (10 s, the
  calling side's whole trip, address selection included). If the inner wait were
  the longer one, the caller's clock would speak first and report an unreachable
  node for a write that may well have landed.

## Testing

```bash
go test ./...
```

## See also

- [`../nvpair-ui-broker/README.md`](../nvpair-ui-broker/README.md) — the
  supervisor and relay
- [`../nvpair-job-scheduler/README.md`](../nvpair-job-scheduler/README.md) — the
  primary consumer of workload counts
- [`../VERSIONING.md`](../VERSIONING.md) — SemVer bump rules
