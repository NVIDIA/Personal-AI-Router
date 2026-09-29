<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-info

A Go service that exposes this machine's hardware inventory (GPUs, CPU, physical memory) over a small HTTP API at `/v1/node-info`. It advertises nothing over mDNS itself — its parent (the broker) registers its `ni` port with the `nvpair-node-scanner` discovery daemon, which folds it into this node's single `_nvpair-node` record and fetches `/v1/node-info` to enrich the node for peers.

## Communication

Two surfaces:

- **HTTP(S)** — serves the node inventory at `/v1/node-info`. Plaintext HTTP on `:14318` by default; optional HTTPS (with optional mTLS) on `:14319` when a cert/key pair is supplied.
- **stdio JSON-RPC 2.0** — newline-delimited, used for lifecycle/control (`log/set-level`, `nodeinfo:set-cluster-identity`) and shutdown via stdin EOF. The service is normally launched as a subprocess by the broker.

It does **not** advertise itself over mDNS. Discovery is centralized in the `nvpair-node-scanner` daemon: the broker registers this service's `ni` port with the daemon, which carries it on the node's one `_nvpair-node` record and fetches `/v1/node-info` over plain HTTP to enrich each node.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--port <n>` | `14318` | HTTP port to listen on |
| `--tls-port <n>` | `14319` | HTTPS port (only used when `--cert` and `--key` are set) |
| `--cert <path>` | _(none)_ | TLS server certificate (PEM); enables HTTPS when set together with `--key` |
| `--key <path>` | _(none)_ | TLS server private key (PEM); enables HTTPS when set together with `--cert` |
| `--client-ca <path>` | _(none)_ | PEM bundle of CAs trusted to sign client certificates; enables mTLS when set |
| `--accept-http` | `false` | Keep the plaintext HTTP listener alive even when TLS is enabled (default: HTTPS-only once `--cert` is set) |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`); when set, the primary port carries one cluster-gated listener. While this node is a cluster member, `/v1/node-info` is served **only** over cluster-scoped pin-based mTLS, to pinned cluster peers — or this node itself via self-trust — and a plaintext caller is refused with `403`; while it is not a member the plaintext inventory is served as usual. Membership is re-read per request. Takes precedence over the `--cert` path |
| `--version` | | Print version and exit |

`--log-level` is also accepted (registered by `nvpair-shared/applog`).

### TLS / mTLS configuration

The cert flags follow a bring-your-own contract, validated at startup:

- `--cert` and `--key` must be supplied together; either alone is rejected.
- `--client-ca` requires `--cert` and `--key`. When set, the server requires and verifies a client certificate (`RequireAndVerifyClientCert`); connections without a trusted client cert are dropped at handshake time.
- TLS minimum version is pinned to TLS 1.2.

When TLS is enabled, the HTTP listener is dropped unless `--accept-http` is passed. Both listeners share the same handler, but bind separate ports so clients can migrate independently.

`--cluster-dir` replaces both with a single cluster-gated listener on the primary port (see the flag table): one bound port, both personalities, and the choice between them follows live cluster membership rather than being fixed when the listener was bound.

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
- Under the broker, `clusterUuid` is pushed in over stdin (`nodeinfo:set-cluster-identity`) because node-info is spawned with no cluster dir and so cannot read membership itself; the field stays absent until the first push arrives. Standalone with `--cluster-dir`, it reads membership from the trust store per request instead and is therefore always known. The two sources are mutually exclusive by deployment, not a fallback chain.
- `clusterUuid` exists so a peer can learn this node's membership without its mDNS record. Membership otherwise travels only as the `cluster-uuid=` TXT key, which a consumer reads once per record *change*; a consumer that misses that change keeps the previous value indefinitely, and one still holding a departed node's principal will suppress the invite that would bring it back.
- All dynamic fields and the `cpu` / `memory` objects use `omitempty`: a value the service couldn't read is dropped from the JSON entirely rather than reported as a misleading literal zero. A genuinely idle CPU renders the same as "unknown" — that ambiguity is intentional and benign.
- `vram_bytes` is reported through DXGI on Windows, `nvidia-smi` on Linux, and IORegistry on macOS. On a unified-memory NVIDIA GPU such as DGX Spark, Linux uses total physical system memory for `vram_bytes` and the independently sampled system-memory usage for `vram_used_bytes`. On Apple Silicon, `vram_bytes` is total physical unified memory and `vram_used_bytes` is the GPU driver's mapped allocation (`Alloc system memory`), not whole-system RAM usage or the momentarily active subset.

## Discovery

The service no longer advertises itself over mDNS. Its parent (the broker) registers the `ni` port with the `nvpair-node-scanner` discovery daemon, which carries it on this node's single `_nvpair-node` record; a peer's daemon then fetches `/v1/node-info` over plain HTTP to enrich the node. Transport is derived from the shared per-service policy (`nvpair-shared/noderec`): node-info is served plain on the broker path. The standalone BYO-TLS / `--cluster-dir` mTLS serving above is retained for running node-info on its own, but is unused under the broker.

## Platform Notes

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes VRAM). Dynamic CPU / VRAM-used / utilization / memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, and utilization come from `nvidia-smi`; CPU and system-memory usage come from `/proc`. Unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable. Non-NVIDIA adapters fall back to names from `ghw` without dynamic GPU stats.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

### Linux AMD (amdgpu sysfs)

AMD ships no `nvidia-smi` equivalent in a default install, so a Radeon host used to fall through to `ghw` and publish the PCI database's codename and nothing else (`{"name":"Barcelo"}`). The amdgpu driver already exposes everything needed under `/sys/class/drm/card<N>/device/`, world-readable, so the inventory and the per-tick sample are plain file reads — no daemon, no root, no cgo, no new dependency.

What is read, per card:

| attribute | use |
| --- | --- |
| `vendor`, `device` | vendor `0x1002` selects the card; the device id picks the marketing name |
| `uevent` (`PCI_SLOT_NAME`) | join-key fallback when the `device` symlink cannot be resolved |
| `mem_info_vram_total` | dedicated VRAM, or an APU's firmware carve-out |
| `mem_info_gtt_total` | the GTT aperture — system memory the GPU may map |
| `mem_info_vram_used`, `mem_info_gtt_used` | `vram_used_bytes`, sampled every collector tick |
| `gpu_busy_percent` | `utilization_percent`, already the 0..100 figure `nvidia-smi`'s `utilization.gpu` reports |

Only `card<N>` directories are enumerated. The DRM class also carries one `card<N>-<CONNECTOR>` entry per output (`card1-DP-1`, `card1-HDMI-A-1`, …) and `renderD<N>` nodes, all pointing back at the same device; listing them would publish the same GPU several times.

**Unified-pool capacity rule.** On an APU `mem_info_vram_total` is only the carve-out the firmware reserved — 512 MiB on a Ryzen 5 5625U — while the real ceiling for a model is that carve-out plus the GTT aperture. So:

- **APU** (a device id in the built-in table, or `mem_info_vram_total` under 1 GiB alongside a `mem_info_gtt_total`): `vram_bytes = mem_info_vram_total + mem_info_gtt_total`, and `vram_used_bytes = mem_info_vram_used + mem_info_gtt_used`.
- **Discrete card**: `vram_bytes = mem_info_vram_total` and `vram_used_bytes = mem_info_vram_used` — a discrete card also exposes a GTT aperture, and counting it would inflate a 16 GiB card to 24 GiB.

Capacity and usage always come from the same side of that rule, so `vram_used_bytes` can never exceed `vram_bytes`. A unified AMD row is *not* the unified-memory NVIDIA case: that one substitutes whole-system RAM usage for a GPU whose driver cannot report its own, while amdgpu reports its own usage precisely and is always believed.

**Join key.** Each row's internal `statsKey` is `amd:<pci address>` — `amd:0000:04:00.0` — taken from resolving the `device` symlink into `/sys/bus/pci/devices`, or from `uevent`'s `PCI_SLOT_NAME`, or, as a last resort, the DRM node name (`amd:card1`). The key never reaches the wire; it exists so the static row and the collector's sample meet. The `amd:<pci>` spelling matches the upstream amdgpu inventory work, so both implementations key identically.

**Names.** A small table maps the device id to a marketing name (`0x15e7` → `AMD Radeon Graphics (Vega 7, Barcelo)`; Cezanne, Lucienne, Renoir, Picasso, Raven, Rembrandt, Phoenix and Strix are listed too). An unlisted id publishes `AMD Radeon Graphics (0x<id>)` — never a bare codename, and never a dropped row.

**Ceilings and caveats.**

- Requires the `amdgpu` kernel driver. The older `radeon` driver and pre-Vega parts expose none of these attributes; such a card keeps the `ghw` name-only behavior.
- `gpu_busy_percent` is the driver's own coarse busy figure, not per-engine and not per-process, and a few ASICs do not export it at all. A missing or out-of-range value is dropped rather than published as `0`, so a driver that cannot answer never looks idle — and never marks the node's telemetry fresh.
- The GTT half of an APU's capacity is an aperture the GPU may map, not memory reserved for it: the CPU is using most of it, and `memory.total_bytes` counts the same RAM. Treat an APU's `vram_bytes` as a ceiling, not as free memory.
- Temperature, power and clocks are exposed by the card's hwmon (`temp*_input`, `power1_input`) but are not part of this wire contract.
- A host with no AMD card costs one `os.ReadDir` of `/sys/class/drm` per tick and logs nothing; a host without `/sys/class/drm` at all logs one `Debug` line for the process lifetime.

## Shutdown

The service shuts down on:

- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`

On shutdown it gracefully stops the HTTP/HTTPS listeners (3 s timeout).
