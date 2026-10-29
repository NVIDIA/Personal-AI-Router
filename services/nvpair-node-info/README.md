<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-info

A Go service that exposes this machine's hardware inventory (GPUs, CPU, physical memory) over a small HTTP API at `/v1/node-info`. It advertises nothing over mDNS itself — its parent (the broker) registers its `ni` port with the `nvpair-node-scanner` discovery daemon, which folds it into this node's single `_nvpair-node` record.

## Communication

Two surfaces:

- **HTTP(S)** — serves inventory at `/v1/node-info`. With `--cluster-dir`, public HTTP and pinned mTLS share `:14318`. Without it, the default is public HTTP on `:14318`; operator-supplied certificates enable a separate HTTPS listener on `:14319`, with optional CA-based client authentication.
- **stdio JSON-RPC 2.0** — newline-delimited, used for local inventory snapshots, registry metadata, lifecycle/control, and shutdown via stdin EOF. The service is normally launched as a subprocess by the broker.

It does **not** advertise itself over mDNS. Discovery is centralized in the
`nvpair-node-scanner` daemon: the broker registers this service's `ni` port on
the node's one `_nvpair-node` record. Remote services read inventory over pinned
mTLS; the owned local scanner receives private IPC snapshots. Desktop and TUI
continue to read public HTTP hardware data for display only.

### Private inventory IPC

`nodeinfo:info-updated {info}` publishes the hardware response at startup and
every two seconds over stdout. The broker relays it as
`discovery:local-node-info {info}` to its owned scanner for local inventory and
scheduling. Local service measurements work before joining and after leaving a
cluster, without a loopback HTTP read. Public HTTP hardware displays are unchanged.
The broker adds cache and queue time to `info.msSince` on forwarding or replay;
invalid samples remain invalid.

`nodeinfo:set-peer-metadata {hostUuid, clusterUuid, peer}` accepts the local
scanner registry snapshot over stdin, with `{ok:true}` for an id-bearing request.
`peer` contains `{name, addresses, services}`; `services` maps service keys to
ports. A mismatched host identity or malformed payload returns invalid params
(`-32602`). Each accepted snapshot replaces the previous one completely, so a
removed service or address is cleared. An empty `clusterUuid` clears the
membership claim in this snapshot; with `--cluster-dir`, actual membership still
comes from the trust store. The broker refreshes this metadata on local registry
changes and resends it after node-info restarts. Metadata is served only by the
peer node inventory handler; it is not added to public HTTP or local hardware
snapshots.

Without `--cluster-dir`, `nodeinfo:set-cluster-identity {clusterUuid}` updates
the public membership field. With a cluster directory, live trust-store state
supplies that field instead. `log/set-level` is also accepted.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--port <n>` | `14318` | Primary port: public HTTP and, with `--cluster-dir`, pinned mTLS for peer node inventory |
| `--tls-port <n>` | `14319` | HTTPS port (only used when `--cert` and `--key` are set) |
| `--cert <path>` | _(none)_ | TLS server certificate (PEM); enables HTTPS when set together with `--key` |
| `--key <path>` | _(none)_ | TLS server private key (PEM); enables HTTPS when set together with `--cert` |
| `--client-ca <path>` | _(none)_ | PEM bundle of CAs trusted to sign client certificates for operator-configured HTTPS; requires `--cert` and `--key` |
| `--accept-http` | `false` | Keep plaintext HTTP alongside operator-configured HTTPS. With `--cluster-dir`, public HTTP is always available |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`). The primary port serves public HTTP hardware displays alongside peer node inventory over pinned mTLS. Public HTTP stays available while clustered. Peer node inventory reads require live membership and matching local registry metadata. Takes precedence over the operator TLS flags |
| `--version` | | Print version and exit |

`--log-level` is also accepted (registered by `nvpair-shared/applog`).

### TLS / mTLS configuration

The cert flags follow a bring-your-own contract, validated at startup:

- `--cert` and `--key` must be supplied together; either alone is rejected.
- `--client-ca` requires `--cert` and `--key`. When set, the server requires and verifies a client certificate (`RequireAndVerifyClientCert`); connections without a trusted client cert are dropped at handshake time.
- TLS minimum version is pinned to TLS 1.2.

With operator-supplied `--cert`/`--key` and no `--cluster-dir`, the HTTP listener
is dropped unless `--accept-http` is passed. Both listeners share the public
inventory handler, but bind separate ports so clients can migrate independently.

`--cluster-dir` replaces both with one split listener on the primary port. Its
public HTTP handler is independent of membership. Its mTLS handler verifies the
client's exact current pin on every request, including reused connections. A
retained keypair after departure does not keep peer node inventory available.
Operator certificates or CA settings cannot replace those cluster pin checks.
Without a cluster directory, the operator HTTPS listener serves the public
inventory shape and omits peer registry metadata.

## HTTP API

### `GET /v1/node-info`

Returns the merged static identity (collected once at startup) and the latest dynamic stats snapshot.

```json
{
  "GPUs": [
    {
      "name": "NVIDIA GeForce RTX 3080",
      "vram_bytes": 10737418240,
      "vram_used_bytes": 2147483648,
      "utilization_percent": 42
    }
  ],
  "telemetryValid": true,
  "msSince": 137,
  "cpu": {
    "name": "AMD Ryzen 9 5900X 12-Core Processor",
    "cores": 12,
    "utilization_percent": 7
  },
  "memory": {
    "total_bytes": 34359738368,
    "used_bytes": 12884901888
  },
  "hostUuid": "8661676a-0d1c-4bd3-ac5e-4d370e6f1a9c",
  "clusterUuid": ""
}
```

Field notes:

- Dynamic fields (`vram_used_bytes`, `utilization_percent`, `cpu.utilization_percent`, `memory.used_bytes`) are populated by the Windows, Linux, and macOS per-tick stats collectors.
- `telemetryValid` and `msSince` describe the node-wide GPU utilization snapshot. `telemetryValid` is `true` after the collector has produced a usable GPU sample; `msSince` is that sample's age in milliseconds at response time. A failed collection retains the last usable sample and lets its age increase. Before the first usable sample, and on platforms without dynamic GPU telemetry, the response reports `telemetryValid:false` and `msSince:0`; consumers must ignore the age while validity is false.
- `clusterUuid` is the cluster principal this node currently holds. It has three distinct states on the wire: **absent** means unknown, **present and empty** means this node belongs to no cluster, and a value is that principal. A consumer must not read absent as unclustered — that is how a node too old to report the field answers, and also how this node answers before its parent has told it anything, so acting on it would clear a correct annotation elsewhere in the fleet.
- With `--cluster-dir`, including under the broker, `clusterUuid` comes from live trust-store membership. Without a cluster directory, a parent may push it over stdin using `nodeinfo:set-cluster-identity`; the field stays absent until that first push. These are separate deployment modes.
- `clusterUuid` is an identity claim. Public HTTP cannot establish membership or change a verified node's identity; service consumers require the protected response and a matching exact certificate pin.
- All dynamic fields and the `cpu` / `memory` objects use `omitempty`: a value the service couldn't read is dropped from the JSON entirely rather than reported as a misleading literal zero. A genuinely idle CPU renders the same as "unknown" — that ambiguity is intentional and benign.
- `vram_bytes` is reported through DXGI on Windows, `nvidia-smi` on Linux, and IORegistry on macOS. On a unified-memory NVIDIA GPU such as DGX Spark, Linux uses total physical system memory for `vram_bytes` and the independently sampled system-memory usage for `vram_used_bytes`. On Apple Silicon, `vram_bytes` is total physical unified memory and `vram_used_bytes` is the GPU driver's mapped allocation (`Alloc system memory`), not whole-system RAM usage or the momentarily active subset.

### Peer node inventory on the same port

With `--cluster-dir`, pinned peers read the same route over mTLS. The response
adds `peer: {name, addresses, services}` from the owned scanner registry, where
`services` maps service keys to ports. Some of these details are also advertised
over mDNS; mTLS supplies an authenticated registry snapshot.
`hostUuid`, `clusterUuid`, and registry identity must agree with this node's
current certificate identity. The handler returns `503` until matching metadata,
a name, addresses, and the `ni` service registration are available.

The serving node must still be a member, and the client's presented certificate
must match its exact current pin. Revoked or replaced pins return `403`, including
on reused TLS connections and when trust changes during response collection.
Departure disables peer node inventory even if the keypair remains.
Public HTTP remains available and omits `peer` in every membership state.

## Discovery

The broker registers the `ni` port with scanner, which carries it on this host's
single `_nvpair-node` record. The broker supplies the cluster directory so this
port supports both public display HTTP and peer node inventory over pinned
mTLS. Local service measurements continue to use the owned worker's private IPC
snapshots.

## Platform Notes

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes VRAM). Dynamic CPU / VRAM-used / utilization / memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, and utilization come from `nvidia-smi`; CPU and system-memory usage come from `/proc`. Unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable. Non-NVIDIA adapters fall back to names from `ghw` without dynamic GPU stats.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

## Shutdown

The service shuts down on:

- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`

On shutdown it gracefully stops the HTTP/HTTPS listeners (3 s timeout).
