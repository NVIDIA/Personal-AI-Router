<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-info

A Go service that reports this machine's GPUs, CPU, and physical memory through
`/v1/node-info` and private broker/worker IPC. The broker registers its `ni` port
with the scanner's local service registry.

## Communication

Two surfaces:

- **HTTP(S)** — serves inventory at `/v1/node-info`. With `--cluster-dir`, public HTTP and pinned mTLS share `:14318`. Without it, the default is public HTTP on `:14318`; operator-supplied certificates enable a separate HTTPS listener on `:14319`, with optional CA-based client authentication.
- **stdio JSON-RPC 2.0** — newline-delimited, used for local inventory snapshots, registry metadata, lifecycle/control, and shutdown via stdin EOF. The service is normally launched as a subprocess by the broker.

The scanner advertises `ni` in the node's `_nvpair-node` record. Remote services
read authenticated inventory over pinned mTLS; the owned local scanner receives
private IPC snapshots. Scanner and manual-node workers collect public HTTP
previews for eligible unverified records. Desktop and TUI receive hardware
through broker snapshots and pushes. Public HTTP serves previews in every
membership state.

### Private inventory IPC

`nodeinfo:info-updated {info, capturedAt}` publishes the hardware response at startup and
every two seconds over stdout. The broker relays it as
`discovery:local-node-info {info, capturedAt}` to its owned scanner for local inventory and
scheduling. These owned local measurements work in every membership state.
Scanner emits full local hardware back to the broker for client snapshots and
live updates as well as compact scheduling measurements.
`capturedAt` is Unix milliseconds on the shared local host clock, recorded before
building or writing each private frame. Broker and scanner add elapsed delivery
time to `info.msSince`; broker forwarding and replay also include cache and queue
time. Invalid samples remain invalid, and a missing or future capture time makes
GPU telemetry unavailable. Display receipt time `info.observedAt` stays separate.

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
inventory handler on separate ports.

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
- `clusterUuid` has three wire states: **absent** means unknown, **present and empty** claims unclustered, and a nonempty value claims that node principal. Public preview collectors strip this field before forwarding display hardware.
- With `--cluster-dir`, including under the broker, `clusterUuid` comes from live trust-store membership. Without a cluster directory, a parent may push it over stdin using `nodeinfo:set-cluster-identity`; the field stays absent until that first push. These are separate deployment modes.
- `clusterUuid` is an identity claim. Public HTTP cannot establish membership or change a verified node's identity; service consumers require the protected response and a matching exact certificate pin.
- Dynamic scalar fields and the `cpu` / `memory` objects use `omitempty`. An absent object means its measurements are unavailable. Scalar zero values are also omitted, so a consumer cannot distinguish zero usage from an unavailable scalar measurement. GPU utilization has separate validity and sample-age fields; these do not describe CPU or system-memory freshness.
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
Public HTTP serves hardware previews and omits `peer` in every membership state.

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
