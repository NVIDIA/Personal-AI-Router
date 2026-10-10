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

## Communication

Two channels:

- **Local** — bidirectional newline-delimited JSON-RPC 2.0 with the supervising
  `nvpair-ui-broker`. Stdio by default; `--ipc <path>` switches to a Unix domain
  socket or Windows named pipe.
- **Inter-node** — a cluster-mTLS server on port `14320`
  (`POST /v1/workloads/events`) that accepts the same JSON-RPC frames from pinned
  peers, plus an outbound broadcaster that pushes local events to those peers with
  bounded retry.

Peers are discovered through the broker's node records rather than by this
component browsing the network itself.

## CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--port <n>` | `14320` | Inter-node port to listen on and advertise |
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt` / `node.key` + `trusted/`) supplying the identity and pins the inter-node mTLS channel requires. Without it the node has no cluster identity and exchanges no inter-node traffic |
| `--local-ingress <host:port>` | _(off)_ | Loopback address of the optional [local ingress](#local-ingress-optional-loopback-only); a non-loopback address is refused. Without the flag, `<appdir>/workload-ingress.json` can enable it |
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

## Local ingress (optional, loopback only)

A third-party producer on the same machine — an external scheduler, a local
inference harness that routes around the proxies — can report its workloads so
they appear in every member's Jobs list and count in the scheduler's pending
work for the node that runs them. The ingress is **off by default** and
**never leaves loopback**:

- Enable it with `--local-ingress 127.0.0.1:14324`, or, when the broker
  launches this worker without the flag, with `<appdir>/workload-ingress.json`
  containing `{"listen": "127.0.0.1:14324"}` (file-registered, like an engine
  manifest under `<appdir>/engines/`). A non-loopback address is refused at
  startup; a port already in use fails startup loudly.
- `POST /v1/workloads/events` takes the same JSON-RPC 2.0 frames as the
  inter-node port: `workload:submitted` / `workload:started` /
  `workload:completed` / `workload:errored` with `params.workloadInfo`, and
  `workloads:remove` with `params.workloadId`. `originatedFrom` must be empty
  (it is then stamped with this node's UUID) or this node's UUID; any other
  value is `400`, because the ingress reports only workloads that run here.
  `state` must be one of `queued`, `running`, `completed`, `failed` or
  `cancelled`, and a workload id (`workloadInfo.id`, or `workloadId` on a
  removal) is at most 256 bytes.
- Field names must be spelt exactly as documented, and once. JSON decoders
  match names without regard to case, so a variant such as `originatedfrom` or
  `WorkloadInfo`, or two names that differ only in case, is `400` rather than
  being read as the field. A `resync` name in any spelling is `400` too: it is
  the peers' own marker for a re-assertion, and a frame carrying it would
  bypass their dedup.
- A web page in the user's browser can reach loopback, so a request is refused
  before its body is read when it carries an `Origin` header (`403`), when its
  `Host` is not `localhost`, a `127.0.0.0/8` address or `::1` with the ingress
  port (`421`), or when its `Content-Type` is not `application/json` (`415`). A
  body over 1 MiB is `413`. Other producer mistakes are `400`, any method other
  than `POST` is `405`, and a broker that cannot be written is `500`. Request
  header, read and idle timeouts are set. There is no write timeout: it would
  also cover the wait for the broker, and cut off a producer whose frame was
  in fact accepted.
- A frame may carry `workloadInfo.seq`, the producer's event counter for that
  workload, from 1. It is part of the key peers deduplicate on, so a producer
  should stamp it: without it, peers drop an event that repeats a `state` and
  `scheduledOn` the workload already had.
- `cancelled` is a valid `state`, carried on `workload:errored`.
- An accepted frame is treated as **local origin**. It is first emitted to the
  broker as `workloads:upsert` / `workloads:remove` — the same translation a
  peer-origin event receives, so the local store, the Jobs list, the persisted
  history and the scheduler all update — and only then tracked for re-sync and
  queued for broadcast to pinned peers. If the broker write fails the frame is
  neither tracked nor queued, so the producer can retry it.
- `200` means the frame was written to the broker and queued for peers. The
  peer side is best-effort like every broadcast: a frame is dropped with a
  warning when the outbound queue is full, and a dropped removal is not
  re-synced.
- A workload removed through the ingress is remembered for a minute, up to
  4096 removals (past that the oldest is forgotten first). In that window a
  replay of the broker's store (sent to a restarted worker) that carries the
  same workload is ignored instead of tracking it again; a new ingress frame
  for it clears the memory.
- Choose ingress ids that cannot collide with the ids of the built-in engine
  proxies, which count from 1: a removal matches on `(originatedFrom, id)`, so
  it would also drop a proxy workload with the same id, and that id is then
  ignored from the broker for the minute above.
- The ingress writes to the broker while holding a lock, so a broker that stops
  reading its pipe blocks ingress requests, as it blocks every other path that
  writes to the broker.
- Ordering: for one workload, the broker and the peers see the ingress frames in
  the same order. That holds among ingress frames only. A `workloads:remove`
  the broker itself sends for the same workload (a delete from the Jobs list)
  while an ingress update for it is being written can reach the peers before
  that update; the producer's next frame or its removal makes them converge.

Example, a producer reporting one job that has started:

```bash
curl -X POST http://127.0.0.1:14324/v1/workloads/events \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{
        "id":"job-17","model":"my-model","engine":"my-engine","runId":"9f3c1a7b",
        "seq":1,"state":"running","createdAt":1716998400000,
        "startedAt":1716998401000,"completedAt":null,"error":null,"requesterId":null}}}'
```

The trust boundary is the one the proxies' plaintext loopback personality
already documents: a process that can reach this machine's loopback may report
work, just as it may already submit it. The frame carries workload metadata
only, and a producer must send nothing else: the ingress checks the fields it
knows, but passes any other field inside `workloadInfo` (and `params`) through
unchanged to the peers and keeps it in the re-sync set. Prompts,
messages and response bodies must never be put in a frame.

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
| `ready` | `{ version }` — startup handshake |

Duplicate events arriving from more than one peer are collapsed before they reach
the broker, so a workload observed over several paths is reported once.

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
