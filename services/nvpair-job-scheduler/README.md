<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-job-scheduler

## Purpose

Ranks the cluster's nodes least-loaded-first and publishes that ordering to the
routing proxies. It is the **priority** input to proxy auto-routing: the proxies
own the actual per-request decision, and this component only tells them what
order to prefer.

The ranking combines node-wide pending workload with a coarse, smoothed GPU
pressure signal. It is not VRAM-capacity- or latency-aware.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0, one object per `\n`. Stdio by
default; `--ipc <path>` switches to a Unix domain socket or Windows named pipe.

The scheduler is supervised by `nvpair-ui-broker`. It holds no listening socket
of its own and never talks to another node — it consumes the broker's cluster
view and hands rankings back for relay to the proxies.

## CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--interval <duration>` | `1s` | Reconciliation cadence; floor is `200ms` |
| `--log-level <level>` | `info` | One of `error` / `warn` / `info` / `debug` |
| `--version` | | Print version and exit |

## Inputs

The broker forwards these notifications:

| Notification | Effect |
| --- | --- |
| `discovery:nodes-changed` | Replaces the eligible node set |
| `workloads:upsert` | Adds or updates a workload in the catalog |
| `workloads:remove` | Drops a workload from the catalog |
| `scheduler:telemetry` | Updates one node's maximum GPU utilization and sample age |

A workload counts as pending when its state is `queued` or `running`. Pending
work is attributed to the node in its `scheduledOn` field; work that has not been
placed yet counts toward no node.

## Ranking

Fresh maximum-GPU utilization is smoothed with an EWMA (`alpha = 0.35`) and
mapped to pressure units: 0 below 40%, 1 from 40–69%, 2 from 70–84%, and 3 at
85% or above. Downward transitions use 35%, 65%, and 80% thresholds to avoid
rank thrash. Invalid, missing, or older-than-10-second telemetry contributes a
neutral pressure of 1.

Nodes are sorted by `pending + gpuPressure`, then lower GPU pressure, then
stable node ID. Pending counts include **every** engine together, so Ollama load
affects the LM Studio and vLLM ordering and vice versa.

Rankings are recomputed when the node set, catalog, or effective pressure
changes, and reconciled on the interval timer. A ranking is only emitted when
the order, pending counts, or pressure actually changed.

## Output

One `schedule:priority` notification per engine (`ollama`, `lmstudio`, `vllm`):

```json
{
  "jsonrpc": "2.0",
  "method": "schedule:priority",
  "params": {
    "engine": "ollama",
    "nodes": ["node-a-uuid", "node-b-uuid"],
    "ranks": [
      { "id": "node-a-uuid", "pending": 0, "gpuPressure": 0, "rank": 0 },
      { "id": "node-b-uuid", "pending": 3, "gpuPressure": 2, "rank": 1 }
    ]
  }
}
```

The broker relays each snapshot to the matching proxy as `node/set-priority`;
`lmstudio` and `vllm` both resolve to the one OpenAI-compatible proxy. Every
engine currently receives the same node-wide ordering; the per-engine envelope
exists so the routing contract can diverge later without a wire change.

Each proxy then adds its own reservations for in-flight requests whose workload
feedback has not arrived yet, so a burst of concurrent requests does not all
pile onto the node that was least-loaded at ranking time.

## Requests

| Method | Result |
| --- | --- |
| `scheduler:get-status` | `{ interval_ms, engines: { <engine>: { engine, emitted, lastEmittedAt } } }` |
| `scheduler:get-interval` | `{ interval_ms }` |
| `scheduler:set-interval` | `{ interval_ms }` — values below the `200ms` floor are clamped |
| `scheduler:tick` | `{ ticked: true }` — forces an immediate recompute |
| `shutdown` | `null`, then exits |

`applog`'s log-level method is also handled, so verbosity can be changed at
runtime without a restart.

The desktop application does not call these methods; it consumes the routing
behavior they produce. They exist for the broker and for diagnostics.

## Notifications emitted

| Notification | Meaning |
| --- | --- |
| `ready` | `{ version }` — startup handshake for the supervisor |
| `schedule:priority` | A changed ranking for one engine |

## Testing

```bash
go test ./...
```

## See also

- [`../nvpair-ui-broker/README.md`](../nvpair-ui-broker/README.md) — the
  supervisor that feeds this component and relays its output
- [`../ollama-proxy/README.md`](../ollama-proxy/README.md) and
  [`../lmstudio-proxy/README.md`](../lmstudio-proxy/README.md) — how the
  ordering is consumed at request time
- [`../nvpair-workload-manager/README.md`](../nvpair-workload-manager/README.md)
  — where workload state originates
- [`../VERSIONING.md`](../VERSIONING.md) — SemVer bump rules
