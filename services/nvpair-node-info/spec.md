<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Node Info (`nvpair-node-info`)

## 1. Purpose
The per-node hardware reporter. It detects this host's GPU, CPU, and physical-memory inventory at startup, samples live utilization on a one-second tick, and serves the merged result as one JSON document at `GET /v1/node-info` on TCP `14318`. It is the only place in the fleet where a machine's own accelerator and load numbers are produced; every node card, GPU chart, and capacity readout anywhere in the product is downstream of this route.

It exists as a separate binary because the readout is inherently platform-specific and privileged-adjacent — DXGI COM calls and PDH performance counters on Windows, `/proc` plus `nvidia-smi` on Linux, `ioreg` plus Mach counters on macOS, `ghw` for CPU identity everywhere — and because its consumers are heterogeneous: `nvpair-node-scanner` fetches it to enrich the node directory, `nvpair-manual-nodes` probes it to identify a typed-in address, and the Electron desktop polls each discovered node's advertised port directly every two seconds. It carries no state beyond the current sample and answers exactly one route. It makes no routing decision itself, but its GPU sample is the input `nvpair-job-scheduler` turns into GPU pressure, by way of the scanner and `nvpair-manual-nodes`. It also reports upward which of this host's addresses remote peers have actually reached it on, which the scanner uses to rank the addresses it publishes.

## 2. Scope
**In scope**
- Static hardware detection at startup: GPU adapters (name + total VRAM), CPU (model + physical core count), total physical RAM.
- Live utilization sampling on a one-second cadence: per-GPU VRAM used and busy percent, node CPU busy percent, memory used.
- Reporting whether a usable GPU utilization sample exists and how old it is (`telemetryValid`, `msSince`), so consumers can tell an idle 0 % reading from no reading and a fresh sample from a stale one.
- Serving the merged inventory as JSON over HTTP at `/v1/node-info`.
- Recording which local addresses served non-loopback peers and reporting that set to the parent as `nodeinfo:observed-addresses`.
- Reporting this node's stable per-host UUID (`hostUuid`) so an HTTP-only consumer can key the node by permanent identity.
- Reporting the cluster principal this node holds (`clusterUuid`) so a peer can learn its membership without receiving its mDNS record.
- Optional bring-your-own TLS/mTLS listener, and an optional cluster-gated listener whose personality follows live cluster membership.

**Out of scope**
- mDNS advertisement and node discovery — `nvpair-node-scanner` owns the single `_nvpair-node` record and registers this service's port as `ni`.
- Minting cluster identity, pairing, and pin management — `nvpair-cluster-manager`; this service reads the cluster dir through `nvpair-shared/clustertrust` when it is given one, and is otherwise told the principal by its parent.
- Engine lifecycle, model inventory, and per-engine ports — `nvpair-engine-manager`.
- Routing, scheduling, and workload accounting — `nvpair-proxy`, `nvpair-job-scheduler`, and `nvpair-workload-manager`. This service reports the raw GPU sample; turning it into pressure is the scheduler's job.
- Choosing which address to publish — the scanner daemon ranks addresses; this service only supplies peer-observed evidence.
- Persisting telemetry history, aggregating across nodes, or raising alerts — consumers do that; this service reports only the current sample.

## 3. Key Use Cases
- **Local enrichment**: the node-scanner daemon reads its own node over `127.0.0.1:14318/v1/node-info` and folds `GPUs` / `cpu` / `memory` into this node's `DirectoryNode` before publishing the record.
- **Peer enrichment**: a peer's daemon fetches `/v1/node-info` at the `ni=` port from the neighbor's `_nvpair-node` TXT, over plain HTTP by policy (`noderec.ServiceNodeInfo.Transport()` is `TransportPlain` even when the target node is clustered).
- **Desktop telemetry poll**: the Electron main process polls every discovered node's `http://<host>:<ni>/v1/node-info` every 2000 ms with a 1500 ms per-attempt timeout, and merges the response into its node model. Repeated failures back off to a 30 s cap; for a multi-homed node it remembers the address that answered and asks only that one; and it reads its own machine over `127.0.0.1` rather than its LAN address. This is the product's one sanctioned direct-HTTP exception to going through the broker's JSON-RPC surface, because the broker exposes no rich per-node telemetry.
- **Scheduler GPU pressure**: the scanner daemon and `nvpair-manual-nodes` each read `telemetryValid`, `msSince`, and the highest `utilization_percent` across `GPUs`, and hand that compact sample to the broker, which ages it and relays it to `nvpair-job-scheduler`. A sample with `telemetryValid: false`, or one that has aged past the scheduler's freshness window, counts as neutral pressure.
- **Peer-observed addresses**: every request served to a non-loopback peer records the local address it arrived on. Every 30 s the service sends the parent `nodeinfo:observed-addresses` with the current set; the broker relays it to the scanner daemon as `discovery:set-observed-addresses`, which ranks a peer-proven address above one it only inferred.
- **Manual node identification**: `nvpair-manual-nodes` probes a user-typed address on `14318` (or the entry's `TLSPort` over HTTPS) every ten seconds and reads `hostUuid` so a node added by address is tracked under the same identity the rest of the fleet keys on.
- **Membership convergence**: a peer's node-scanner daemon re-reads `clusterUuid` from every directory entry's `ni` port on its fifteen-second sweep, because membership otherwise reaches the fleet only as the `cluster-uuid=` TXT key on a record whose change it may never receive. A failed fetch, an absent field, or an answer whose `hostUuid` disagrees with the entry all leave the peer's annotation untouched; only an explicit, identity-matched report is acted on.
- **Edge case — no GPU**: `detectGPUs` returns an empty slice; `buildResponse` still allocates the slice, so the body is `{"GPUs":[],"telemetryValid":false,"msSince":0}` plus whatever CPU/memory data exists. The route never 404s or errors for a GPU-less host.
- **Edge case — GPU collection stops working**: a failed pass keeps the last usable sample and its original timestamp, so `telemetryValid` stays `true` and `msSince` keeps growing. Consumers read the growing age as staleness rather than seeing numbers blank out or silently freeze.
- **Edge case — unmatched adapter**: a GPU whose `statsKey` is absent from the current snapshot keeps zero-valued dynamic fields, which `omitempty` drops. Clients see a missing `vram_used_bytes` rather than a misleading literal `0` (`TestBuildResponseOmitsZero`).
- **Edge case — static introspection fails**: a nil `*CPUInfo` or a zero `memTotal` drops the whole `cpu` / `memory` object, never an empty shell like `"cpu":{}` (`TestBuildResponseCPUMemoryMatrix`).
- **Edge case — unified memory (UMA)**: on Grace-Blackwell class hosts `nvidia-smi` returns `[N/A]` for `memory.total` / `memory.used`; `vram_bytes` is backfilled from total system RAM and `vram_used_bytes` from the `/proc/meminfo` used figure, which keeps working even when dynamic `nvidia-smi` collection never succeeds (`TestBuildResponseUnifiedMemoryUsesSystemSnapshot`).
- **Edge case — macOS enumeration fails at startup**: `detectGPUs` returns nothing, but the collector re-reads IORegistry every tick and `mergeGPUInventory` folds the recovered adapters into the response, so the inventory repairs itself without a restart (`TestBuildResponseRecoversDarwinGPUInventory`).
- **Edge case — RDP session on Windows**: DXGI enumerates a phantom clone of the physical adapter plus Microsoft's remoting adapters. Both a name denylist and a `HKLM\SOFTWARE\Microsoft\DirectX` `AdapterLuid` gate suppress them (`TestIsVirtualDisplayAdapter`, `TestKeepPhysicalAdapter`).
- **Edge case — clustered read (`--cluster-dir` only)**: a pinned peer gets `200`; the node reading itself is accepted through self-trust; a peer that completes the handshake but is not pinned here gets `403` (`TestNodeInfoHandler_MTLSGate`).

## 4. Open Questions / Risks
- **Risk — plaintext LAN inventory**: under the broker, `nvpair-node-info` is deliberately spawned *without* `--cluster-dir`, so the GPU/CPU/memory inventory is readable by anything on the local subnet. It is the one documented exception to "the inter-node cluster data plane is always mTLS", accepted because the desktop's two-second poll and baseline discovery hold no cluster identity. The cluster-gated mode is implemented and unit-tested but unused in the shipped topology; enabling it without first solving the desktop read blanks every node card.
- **Risk — one-shot hardware detection**: `detectGPUs`, `detectCPU`, and `detectMemoryTotal` run once in `main`, so the CPU and memory readout is fixed for the process lifetime and a Windows or Linux adapter added, removed, or driver-installed after startup never appears. macOS is the exception: its collector re-enumerates IORegistry every tick and `mergeGPUInventory` adds adapters the startup pass did not see.
- **Risk — latched telemetry unavailability**: Linux latches `nvidiaUnavailable` on the first `nvidia-smi` failure and never retries; Windows adds PDH counters only inside `startStatsCollector`. A driver or counter set that becomes available later is not picked up for the process lifetime. macOS retries `ioreg` on every tick instead.
- **Risk — a stopped collector keeps serving its last GPU numbers**: on every platform a failed GPU pass retains the last usable sample, so a host whose `nvidia-smi` latched, whose PDH query broke, or whose IORegistry read started failing keeps reporting the old VRAM and utilization figures indefinitely. `msSince` growing is the only signal. The scheduler path honors it; a consumer that renders `utilization_percent` without checking `msSince` shows a frozen number.
- **Risk — `--tls-port` default collides**: `14319` is also the broker's fixed `nvpair-errors` port. Nothing binds it today because the broker never passes `--cert`/`--key`, and the cluster-gated listener deliberately reuses `--port` instead. Any future use of the bring-your-own TLS path under the broker must move this port.
- **Risk — unified-memory VRAM is system RAM**: on a unified-memory host `vram_bytes` is the total physical system memory — on Linux `vram_used_bytes` is the whole-system used figure as well, while on Apple Silicon it is the GPU driver's mapped allocation (`Alloc system memory`) rather than either the active working set or system-wide usage — so a consumer that sums "VRAM + RAM" double-counts. Nothing on the wire marks a GPU as unified.
- **Risk — undocumented IORegistry keys**: the macOS GPU numbers come from the `PerformanceStatistics` dictionary of an `IOAccelerator` entry (`Device Utilization %`, `GPU Activity(%)`, `Alloc system memory`, `vramUsedBytes`, `inUseVidMemoryBytes`, `vramFreeBytes`), none of which Apple documents or guarantees. A renamed memory key drops that one metric to zero, where `omitempty` hides it. A renamed utilization key is more visible: a pass with no utilization reading is not a usable sample, so `telemetryValid` stays `false` (or the previous sample ages). Either way a macOS release can degrade the readout without surfacing an error.
- **Risk — macOS VRAM used without a total**: an adapter that exposes neither `VRAM,totalMB` nor a dedicated used counter still reports `Alloc system memory` as `vram_used_bytes`, so a response can carry a used figure with no `vram_bytes` to divide it by (`TestParseIORegistryGPUsPreservesDedicatedCounterPresence`). A consumer rendering a VRAM percentage has to treat the absent total as "no percentage" rather than zero.
- **Risk — the reported cluster principal rides one best-effort push**: under the broker `clusterUuid` comes only from `nodeinfo:set-cluster-identity`, written on spawn and on every membership or pin-set change. A failed write is warned about and not retried, so a dropped frame leaves peers reading a stale principal — or none at all — until the next change or a restart.
- **Risk — fatal listener errors**: a bind failure or a non-`ErrServerClosed` serve error calls `log.Fatalf`, terminating the process. Recovery is the broker's supervisor restart, not in-process.
- **`inference_hardware_ids` (open — needs a producer if the filter is ever to engage)**: the desktop's merge reads an optional `inference_hardware_ids` string array from the response, but no field with that JSON tag exists in `NodeInfoResponse` or anywhere under `services/`, so nothing produces it today. This is a forward-compatibility hook rather than a broken read: the consumer preserves the absent-versus-empty distinction deliberately — absent means "no readiness reported, show every GPU" and `[]` means "no inference-ready hardware" — so the unproduced field degrades to showing all GPUs. The filter is display-only and never influences routing, which stays owned by `nvpair-proxy` and `nvpair-job-scheduler`. Specifying and producing the field here is what would make it engage; until then the response schema in §7.1 is complete as written.
- **Firewall rule surface (open — needs an installer audit)**: `nvpair-setup.nsi` grants this binary an inbound UDP 5353 mDNS rule (`NVPAIR mDNS Node Info`) in addition to the TCP rule it needs. The process opens no multicast socket — discovery belongs to the node-scanner daemon — so that rule widens the binary's inbound surface for no functional reason.
- **Risk — peer-observed addresses are evidence, not proof of identity**: an address is recorded once a request from a non-loopback peer is read, with no check of who the peer is. It says only that some machine reached this host there. The 5-minute TTL bounds how long an address the host stops answering on keeps being reported.
- **Future — per-adapter identity on the wire**: `statsKey` (Windows LUID, Linux GPU UUID, macOS IORegistry entry ID) is `json:"-"` and unexported, so consumers cannot stably key a specific adapter across responses; they match by array position.

## 5. Requirements

**Functional**
- Detect GPU adapters, CPU identity, and total physical memory at startup, per OS, degrading to fewer fields rather than failing.
- Sample dynamic utilization on a background ticker and publish each pass atomically as one `statsSnapshot`.
- Merge static identity with the latest snapshot per request and serve it as `application/json` at `/v1/node-info`.
- Mark a GPU pass usable only when at least one adapter yielded a utilization reading (a parsed 0 % counts); report `telemetryValid` and the usable sample's age as `msSince`, and keep the previous usable sample, timestamp included, when a pass fails.
- Record the local address of every request served to a non-loopback peer, expire each after 5 minutes unseen, and send the full current set, empty included, as `nodeinfo:observed-addresses` every 30 s.
- Report `hostUuid`, taking `--node-id` verbatim when supplied and otherwise resolving the shared local identity.
- Report `clusterUuid` from the live trust store when cluster-gated and from the parent's push otherwise, keeping "unknown" distinct from "belongs to no cluster".
- Honor the bring-your-own TLS contract (`--cert`/`--key`/`--client-ca`) and the cluster-gated contract (`--cluster-dir`) with the documented precedence.
- Handle `log/set-level` and `nodeinfo:set-cluster-identity` on stdin and shut down on stdin EOF, `SIGINT`, or `SIGTERM`.

**Non-functional**
- The request path performs no hardware sampling and takes no lock: one atomic pointer load, the JSON marshal, and — only when cluster-gated — a cluster-dir refresh. It must stay well inside a 1500 ms client timeout under a two-second poll from every peer.
- Compiles and runs on Windows, Linux, and macOS × amd64/arm64; the telemetry paths use direct syscalls, file reads, purego bindings, and short-lived unprivileged helper processes rather than cgo.
- Unknown values are omitted from JSON, never reported as a literal zero.
- Never logs prompts, request bodies, certificates, or key material.
- Testable without real hardware: every parser and the response merge are pure functions with table tests.

## 6. Inputs and Outputs

**Inputs** — CLI flags at startup (§15); the host's own hardware interfaces (DXGI, PDH, `GlobalMemoryStatusEx`, `/proc/stat`, `/proc/meminfo`, `nvidia-smi`, `/usr/sbin/ioreg`, Mach through gopsutil, `ghw`); optionally a cluster dir (`node.crt`, `node.key`, `trusted/*.json`, `admission.json`) read through `nvpair-shared/clustertrust`; the identity store read through `nvpair-shared/nodeid`; and newline-delimited JSON-RPC 2.0 control frames on stdin. There is no request payload — `/v1/node-info` takes no parameters, headers, or query string.

**Outputs** — one JSON body per request, and stderr logs via `nvpair-shared/applog`. On stdout the service emits one notification, `nodeinfo:observed-addresses` every 30 s, and the response to an id-bearing `log/set-level` request; the two share one writer so their frames never interleave. It emits no `ready` frame.

`NodeInfoResponse` (top level):
```json
{
  "GPUs": [GPUInfo],
  "cpu": CPUInfo,
  "memory": MemoryInfo,
  "telemetryValid": false,
  "msSince": 0,
  "hostUuid": "string",
  "clusterUuid": "string"
}
```

`GPUInfo`, `CPUInfo`, `MemoryInfo`:
```json
{ "name": "string", "vram_bytes": 0, "vram_used_bytes": 0, "utilization_percent": 0 }
{ "name": "string", "cores": 0, "utilization_percent": 0 }
{ "total_bytes": 0, "used_bytes": 0 }
```

Example response from a Linux host with one discrete NVIDIA GPU:
```json
{
  "GPUs": [
    {
      "name": "NVIDIA GeForce RTX 4090",
      "vram_bytes": 25757220864,
      "vram_used_bytes": 8589934592,
      "utilization_percent": 37
    }
  ],
  "cpu": { "name": "AMD Ryzen 9 5900X 12-Core Processor", "cores": 12, "utilization_percent": 7 },
  "memory": { "total_bytes": 34359738368, "used_bytes": 12884901888 },
  "telemetryValid": true,
  "msSince": 412,
  "hostUuid": "6b1f5c2a-9e44-4f70-b8d1-2c9a7e3f01aa",
  "clusterUuid": ""
}
```

Minimum response, from a host with no GPU, no `ghw` introspection, and no cluster principal reported yet:
```json
{"GPUs":[],"telemetryValid":false,"msSince":0}
```

`nodeinfo:observed-addresses` params — the complete current set, sorted; an empty list withdraws the last evidence:
```json
{ "addresses": ["10.10.0.1", "192.168.1.10"] }
```

## 7. API / Interface Contract
Two interfaces: an HTTP inventory route toward every consumer (the node-scanner daemon, `nvpair-manual-nodes`, and the Electron desktop's direct poll), and a stdio JSON-RPC control channel toward the parent process that spawned it.

### 7.0 Methods and notifications
HTTP surface — one route, exactly:

| Route | Method | Auth | Success | Failure |
|-------|--------|------|---------|---------|
| `/v1/node-info` | any (the pattern is path-only, so `http.ServeMux` does not filter by method) | none by default; pinned-peer mTLS while `--cluster-dir` is set *and* this node is a cluster member | `200`, `Content-Type: application/json`, `NodeInfoResponse` | `403 forbidden: not a pinned cluster peer` |
| any other path | any | — | — | `404` from `http.ServeMux` |

stdio surface — newline-delimited JSON-RPC 2.0 on stdin, handled by `applog.StdinRPC`:

| Method | Params | Result |
|--------|--------|--------|
| `log/set-level` | `{ level }` (`debug`\|`info`\|`warn`\|`warning`\|`error`) | `{ level }` when the frame carries an `id`; a rejected level answers `-32602` |
| `nodeinfo:set-cluster-identity` | `{ clusterUuid }` (empty means "belongs to no cluster") | none — a notification; the value appears on the next `/v1/node-info` response |

`applog.StdinRPC` dispatches `log/set-level` itself and forwards every other frame to `handleClusterIdentity`, which ignores any method other than `nodeinfo:set-cluster-identity` and drops a malformed payload rather than latching a wrong membership.

Outbound, the service sends one notification to its parent:

| Notification | Params | Cadence |
|--------------|--------|---------|
| `nodeinfo:observed-addresses` | `{ addresses: string[] }` | every 30 s, unconditionally, including an empty list |

It publishes nothing about inventory or telemetry; HTTP consumers poll.

### 7.1 Response schema
`buildResponse` copies the static `[]GPUInfo` detected at startup, folds in any adapters the snapshot's own `GPUInventory` carries (`mergeGPUInventory` enriches a matching `statsKey` in place and appends an unseen one), overlays the latest snapshot by `statsKey`, and marshals. Field semantics:

| JSON tag | Go type | `omitempty` | Source |
|----------|---------|-------------|--------|
| `GPUs` | `[]GPUInfo` | no — always present, `[]` when none | `detectGPUs()` at startup, merged with any adapters the collector re-enumerates |
| `GPUs[].name` | `string` | no | DXGI `Description` / `nvidia-smi` `name` / IORegistry `model` / `ghw` product name, else `"Unknown"` |
| `GPUs[].vram_bytes` | `uint64` | yes | DXGI `DedicatedVideoMemory` / `nvidia-smi` `memory.total` (MiB→bytes) / IORegistry `VRAM,totalMB` (MiB→bytes) / system RAM on a unified-memory host |
| `GPUs[].vram_used_bytes` | `uint64` | yes | per-tick snapshot, or the system memory-used figure for an adapter marked `usesSystemMemoryUsage` |
| `GPUs[].utilization_percent` | `uint32` | yes | per-tick snapshot, 0–100 |
| `cpu` | `*CPUInfo` | yes — object absent when `ghw` failed | `detectCPU()` |
| `cpu.name` | `string` | yes | `ghw` `Processors[0].Model`, else `Vendor` |
| `cpu.cores` | `uint32` | yes | `ghw` `TotalCores` (physical cores across sockets, not SMT threads) |
| `cpu.utilization_percent` | `uint32` | yes | per-tick snapshot, 0–100 |
| `memory` | `*MemoryInfo` | yes — object absent when total is unknown | `detectMemoryTotal()` |
| `memory.total_bytes` | `uint64` | yes | `ghw` `TotalPhysicalBytes` with negatives clamped to 0, or `hw.memsize` through gopsutil on macOS |
| `memory.used_bytes` | `uint64` | yes | per-tick snapshot |
| `telemetryValid` | `bool` | no — always present | `true` once the collector has produced a usable GPU utilization sample (§7.3) |
| `msSince` | `int64` | no — always present | age in ms of that sample at response time; `0` whenever `telemetryValid` is `false` |
| `hostUuid` | `string` | yes | `resolveHostUUID()` |
| `clusterUuid` | `*string` | yes — absent means the node does not know | `mesh.NodeUUID()` when cluster-gated, else the parent's `nodeinfo:set-cluster-identity` push |

`GPUInfo.statsKey` (Windows adapter LUID, Linux GPU UUID, macOS IORegistry entry ID) and `GPUInfo.usesSystemMemoryUsage` are unexported and tagged `json:"-"`: the first joins the static record to the snapshot map, the second marks an adapter whose used bytes come from the system memory sample, and neither reaches the wire. An absent field means "unknown". A genuinely idle GPU also drops `utilization_percent`, and for display the two cases render identically. `telemetryValid` resolves that ambiguity at node level for anything that needs it: a valid sample whose highest reported utilization is 0 % is an idle node, not a missing reading. `telemetryValid` and `msSince` describe the node-wide GPU sample as a whole, not any one adapter, and consumers must ignore `msSince` while `telemetryValid` is `false`. `clusterUuid` is the one field where absent and empty are different answers — absent is "this node does not know its membership", empty is "this node belongs to no cluster" — and a consumer that collapses them clears a correct annotation elsewhere in the fleet, so it is a pointer rather than a plain `omitempty` string (`TestBuildResponseClusterUUIDWireStates`).

### 7.2 Per-OS hardware detection
- **Windows GPU** (`gpu_windows.go`): `CreateDXGIFactory1` → `IDXGIFactory1::EnumAdapters1` → `IDXGIAdapter1::GetDesc1`, walked by hand-rolled COM vtable calls until `DXGI_ERROR_NOT_FOUND`. DXGI is the sole source because it is vendor-agnostic and returns name and VRAM in one struct; `Win32_VideoController.AdapterRAM` is explicitly rejected as a 32-bit field that is wrong for every modern dGPU. Adapters flagged `DXGI_ADAPTER_FLAG_SOFTWARE` are skipped, as are names matching `remote display`, `microsoft basic display`, or `virtual display adapter`, and any adapter whose LUID is absent from `HKLM\SOFTWARE\Microsoft\DirectX\*\AdapterLuid` (the gate is skipped entirely when that key is unreadable or empty). `statsKey` is `luidKey(low, high)` = `luid_0x%08x_0x%08x_phys_0`.
- **Linux GPU** (`gpu_linux.go`): `nvidia-smi --query-gpu=uuid,name,memory.total --format=csv,noheader,nounits` under a 3 s `nvidiaSmiTimeout`. Rows without a uuid or name are skipped; an unparseable VRAM figure yields `vram_bytes: 0` but keeps the row; `[N/A]` / `[Not Supported]` marks the row `usesSystemMemoryUsage` and backfills `vram_bytes` from `detectMemoryTotal()`, so a mixed host can carry discrete and unified adapters in one response. `statsKey` is the GPU UUID. When `nvidia-smi` is absent or yields nothing, `detectGPUsGHW()` returns adapter names with no VRAM and no join key.
- **macOS GPU** (`gpu_darwin.go`, `ioreg_parse.go`): `/usr/sbin/ioreg -a -r -d 1 -c IOAccelerator` under a 3 s `ioRegistryTimeout`, decoded as a plist and walked recursively through `IORegistryEntryChildren`. Each entry with a non-zero `IORegistryEntryID` is named from `model`, then `IORegistryEntryName`, then `IOObjectClass`, and an entry that resolves to no name at all is skipped. `statsKey` is `ioreg:%x` of the entry ID. An `AGXAccelerator`-prefixed class or an `Apple `-prefixed name marks unified memory, whose `vram_bytes` is total system memory; any other adapter takes `VRAM,totalMB`, or `vramUsedBytes + vramFreeBytes` when the total is absent but a dedicated used counter and a free counter are both present. The command is the stock unprivileged one — no elevation and no private framework binding.
- **Other GPU** (`gpu_other.go`, any platform without a specific detector): `ghw.GPU()` graphics cards, name only.
- **CPU** (`cpu_detect.go`): `ghw` on every platform, one implementation, called once. Failure yields `nil` and drops the object.
- **Memory** (`memory_detect.go`, `memory_detect_darwin.go`): `ghw` `TotalPhysicalBytes` everywhere except macOS, where `ghw` has no memory implementation and gopsutil's purego-backed `mem.VirtualMemory().Total` supplies the `hw.memsize` capacity instead. Called once; failure yields `0` and drops the object.

### 7.3 Live utilization sampling
A background collector samples every `statsTickInterval` (1 s) and stores a freshly-allocated `*statsSnapshot` through `atomic.Pointer`. `Snapshot()` is a lock-free load that returns a zero value before the first tick, so the HTTP handler needs no nil check and cannot block on collection. Bundling GPU, CPU, and memory in one struct means a response can never mix tick *N* GPU numbers with tick *N-1* CPU numbers — macOS is the deliberate exception, publishing its `ioreg` reading through a second atomic pointer that `Snapshot()` overlays, so a slow enumeration cannot delay CPU and memory telemetry.

Each snapshot also carries `GPUSampledAt`, the collection time of the latest **usable** GPU sample — one in which at least one adapter produced a utilization reading. A pass that is not usable keeps the previous sample and its timestamp (`applyGPUStats`), so a failure never resets the age. Before the first usable sample a pass may still publish partial dynamic fields such as VRAM used, with a zero timestamp, so they are display-only until utilization appears. `buildResponse` derives `telemetryValid` (timestamp set) and `msSince` (response time minus timestamp, floored at 0) from it.

- **Windows** (`stats_windows.go`): one persistent PDH query opened at startup with `PdhAddEnglishCounterW` (English variant so non-English locales resolve), holding `\GPU Adapter Memory(*)\Dedicated Usage` (`PDH_FMT_LARGE`), `\GPU Engine(*)\Utilization Percentage` (`PDH_FMT_DOUBLE`), and `\Processor(_Total)\% Processor Time` (`PDH_FMT_DOUBLE`). These are rate counters needing two collects, so a priming collect runs at startup and the first real reading lands on the first tick — opening a query per request would add ~1 s to every poll. GPU busy percent follows Task Manager's algorithm in `aggregateUtilization`: sum per-process values within each `(luid, engine-type)` bucket, clamp each bucket to 100, take the max across engine types, round. `memory.used_bytes` bypasses PDH entirely — one `GlobalMemoryStatusEx` call per tick, `TotalPhys - AvailPhys` — so it keeps working when every counter is missing. `PDH_CSTATUS_NO_OBJECT` latches a per-counter unavailable flag to keep the logs quiet. A pass is a usable GPU sample when the engine-utilization counter yields at least one adapter; a failed collect keeps the previous sample. Struct layouts (`dxgiAdapterDesc1` 312 B, `pdhFmtCounterValue` 16 B, `pdhFmtCounterValueItemW` 24 B, `memoryStatusEx` 64 B) are pinned by size assertions so an ABI drift fails a test instead of corrupting a syscall.
- **Linux** (`stats_linux.go`): CPU percent from the delta of two `/proc/stat` aggregate `cpu` line samples (idle counted as `idle + iowait`, baseline primed before the first tick); memory-used from `/proc/meminfo` as `MemTotal - MemAvailable`, falling back to `MemFree` on kernels without `MemAvailable`; GPU from `nvidia-smi --query-gpu=uuid,utilization.gpu,memory.used` joined by UUID, utilization clamped to 100, `memory.used` converted MiB→bytes, an `[N/A]` `memory.used` left at zero for `buildResponse` to fill from the system sample. A pass is a usable GPU sample when at least one row's `utilization.gpu` parses — a `0` counts, `[N/A]` or a malformed value does not — so a unified-memory host reporting only memory rows is not mistaken for fresh utilization. The first memory read happens synchronously inside `startStatsCollector`, before the ticker exists, so a unified-memory host can report `vram_used_bytes` on its very first request. Any counter reset, zero elapsed jiffies, or invalid sample reads as 0 rather than a spike.
- **macOS** (`stats_darwin.go`): two goroutines behind one collector. The system goroutine ticks once a second through gopsutil's purego Mach bindings — CPU percent from the delta of two `cpu.Times(false)` samples over `user + system + idle + nice`, memory-used from `mem.VirtualMemory().Used` — and, like Linux, publishes an initial memory sample synchronously before the ticker starts. The GPU goroutine re-runs the same `ioreg` read the static detector uses and waits one interval *between* passes rather than ticking, so a slow enumeration delays only itself; each pass publishes VRAM-used and utilization keyed by IORegistry entry ID together with the inventory it just enumerated, which is what lets a failed startup enumeration recover. Utilization comes from `Device Utilization %`, falling back to `GPU Activity(%)`, clamped to 100. A pass is a usable GPU sample when at least one adapter carries either key. A pass that finds no adapters is discarded; one that finds adapters but no utilization is published only while no usable sample exists yet, and otherwise the previous usable reading is kept. A failed read warns once through a `sync.Once` and leaves the previous GPU reading in place.
- **Other** (`stats_other.go`, any platform without a collector): the collector is a no-op returning a zero snapshot, so every dynamic field is omitted.

### 7.4 Node identity
`resolveHostUUID(nodeID, clusterDir)` returns `--node-id` verbatim when non-empty. The broker always passes its own already-resolved node UUID, so `/v1/node-info` reports exactly the identity the fleet keys on even when the broker runs on a custom data root. Standalone, it calls `nodeid.Resolve(filepath.Dir(clusterDir))` — the empty string selects the shared per-user data dir — which prefers `<base>/cluster/identity.json`'s `node_uuid` (the cluster-manager's principal), then `<base>/node-id.json`, and otherwise mints and persists a random v4 UUID. Resolution never fails the caller. `TestResolveHostUUIDFlagWins` and `TestResolveHostUUIDFallsBackToClusterRoot` pin both branches.

The cluster principal is resolved separately, and by deployment rather than by a fallback chain. With `--cluster-dir` this process holds the trust store and answers `mesh.NodeUUID()` while `mesh.Clustered()`, so the answer is always known. Without one it can read no membership at all — that is the price of keeping the inventory plain for every LAN peer — so it reports only what the parent pushed and stays absent until the first `nodeinfo:set-cluster-identity` frame arrives (`TestClusterIdentityStartsUnknown`). The two sources are mutually exclusive by construction.

### 7.5 Listeners, TLS, and cluster trust
Three mutually-exclusive listener layouts, decided at startup from the flags:

1. **Plaintext (default, and what the broker runs)**: one HTTP listener on `--port`. `--cluster-dir` unset, no cert.
2. **Bring-your-own TLS**: `--cert` + `--key` build a `tls.Config` pinned to `MinVersion: TLS 1.2` on `--tls-port`; `--client-ca` additionally loads a trust bundle and sets `RequireAndVerifyClientCert`, so an untrusted client is dropped at handshake. `validate()` rejects `--cert` without `--key`, `--key` without `--cert`, and `--client-ca` without both. The plaintext listener is dropped unless `--accept-http` is passed; the two bind separate ports so clients migrate independently.
3. **Cluster-gated**: `--cluster-dir` takes precedence over `--cert`/`--key` (which are logged as ignored) and binds a *single* listener on `--port` — deliberately not `--tls-port`, to avoid colliding with `nvpair-errors`. `nvpair-shared/splitlisten` fans connections by first byte: `0x16` (a TLS handshake record) goes to a `tls.NewListener` wrapping `Mesh.ServerTLSConfig()`, anything else to a plain sub-listener, with the peeked byte restored and a 5 s peek timeout. Both feed the same mux.

Under (3) the gate is per request: `nodeInfoHandler` calls `mesh.Refresh()`, and while `mesh.Clustered()` the caller must satisfy `VerifyClientPin` — an authenticated client cert whose URI-SAN/CN UUID resolves to a byte-for-byte matching pinned peer, or this node's own leaf via self-trust — else `403`. A plaintext caller on the shared port has no client cert and is refused the same way, so joining a cluster closes the plaintext inventory in place. The server leaf is resolved *per handshake*, so an unclustered node presents none and the handshake fails with `ErrNotClustered`; because that decision precedes any handler, a `Mesh.Watch(ctx, nil)` goroutine re-derives membership every `clustertrust.RefreshInterval` (2 s) independently of traffic.

Every listener sets `ReadHeaderTimeout: 5 * time.Second` and the same `ConnState` hook (§7.6). The cluster-gated TLS sub-listener also sets `IdleTimeout` to `clustertrust.PeerListenerIdleTimeout` (105 s), longer than the calling pool's 90 s idle timeout so a peer's client always reaps a pooled connection before this side closes it.

### 7.6 Peer-observed addresses
A multi-homed host cannot tell from its own side which of its addresses a given peer can reach — a direct-connect link works only from the machine on its far end. A peer completing a request is the one fact that settles it, and every peer's inventory poll already arrives here, so the evidence is free and continuous.

- Every listener's `http.Server.ConnState` hook records the connection's local IP when the connection reaches `StateActive`, meaning a request has actually been read. That is what makes it evidence rather than a guess: a bare TCP connection could be a port scan. Loopback peers, and loopback or unspecified local addresses, are ignored, because this machine talking to itself says nothing about what others can reach.
- Each address stays current for `observationTTL` (5 min) after it was last seen, which keeps a quiet peer from retracting its proof while letting an address the host stops answering on drop out within minutes.
- `reportLoop` sends the full current set every `observedReportEvery` (30 s) as `nodeinfo:observed-addresses`, sorted so an unchanged set reports identically. It reports unconditionally — including an empty list — so a restarted parent relearns the set on the next tick and an expired observation is actually withdrawn.
- The broker relays each report to the scanner daemon as `discovery:set-observed-addresses`, off node-info's reader goroutine so the relay cannot stall this process's stdout. The scanner treats the set as peer-proven evidence when ranking the addresses it publishes for this node.

### 7.7 Versioning
- The route is version-prefixed (`/v1/node-info`); a breaking shape change takes a new prefix.
- Response fields grow additively. Every optional field is `omitempty` — `telemetryValid` and `msSince` are deliberately not, because their zero values are meaningful — and consumers decode into their own mirrored structs (`noderec.GPUInfo`/`CPUInfo`/`MemoryInfo`, `nvpair-manual-nodes`, the desktop's parser), so an unknown field is ignored rather than fatal. `GPUs` keeps its capitalized tag; renaming it would break every consumer at once.
- The binary stamps `main.Version` from `services/versions.json` at build time and prints it with `--version`.

## 8. Dependencies
- **Upstream**: `nvpair-ui-broker`, which spawns the process, passes `--log-level` and `--node-id`, owns the stdin pipe, pushes this node's cluster principal over it, reads its `nodeinfo:observed-addresses` reports from stdout, and registers the `ni` service port with the discovery daemon.
- **Downstream**: none — the service calls no other nvpair service. Its HTTP consumers are `nvpair-node-scanner` (`fetchNodeInfo`), `nvpair-manual-nodes` (`probeNodeInfo`), and the Electron desktop's node-info poller; `nvpair-job-scheduler` receives its GPU sample indirectly through the first two, and the scanner receives its observed addresses through the broker.
- **External**: `github.com/jaypipes/ghw` (CPU, non-macOS memory, fallback GPU); `github.com/shirou/gopsutil/v4` (macOS CPU times and memory, purego rather than cgo); `howett.net/plist` (IORegistry decoding); `golang.org/x/sys/windows` (DXGI, PDH, kernel32, registry); the `nvidia-smi` binary on Linux when present and the stock `/usr/sbin/ioreg` on macOS; first-party `nvpair-shared/applog`, `nvpair-shared/nodeid`, `nvpair-shared/noderec`, `nvpair-shared/clustertrust`, and `nvpair-shared/splitlisten`. No third-party runtime services, no network egress.

## 9. Data Ownership
- **Owned**: the static hardware readout captured at startup, the current `statsSnapshot` with its last-usable GPU sample time, and the set of peer-observed local addresses — all in-memory and transient. Nothing is retained across restarts.
- **Source of truth**: yes, for this node's hardware inventory and live utilization. Consumers cache the last successful response (the scanner keeps `lastInfo` keyed by `hostUuid` and reuses it on a transient fetch failure rather than blanking a node card), but this service is where the numbers originate. It is *not* the source of truth for node identity or cluster membership — `nvpair-cluster-manager` owns the cluster principal, and under the broker both the host UUID and that principal are handed to this process rather than derived here.
- **Storage**: no database, no telemetry file. The only possible write is `nodeid.Resolve` minting `<base>/node-id.json` (`0600`, parent `0700`) when it runs standalone with no existing identity; under the broker `--node-id` short-circuits that path. `<base>` is `%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `$XDG_CONFIG_HOME`/`~/.config/Nvidia Corporation/Personal AI Router` on Linux, and `~/Library/Application Support/Nvidia Corporation/Personal AI Router` on macOS. The cluster dir is read-only to this service.

## 10. Design Constraints
- **Performance**: request handling is one atomic load plus a marshal of a few hundred bytes, and the connection hook adds one short mutex-guarded map write per request from a remote peer; all hardware work happens on the collector goroutines, and macOS keeps its once-a-second `ioreg` child process off the path that publishes CPU and memory. The 1 s tick matches the natural interval of the Windows rate counters and keeps the sample fresh for a 2 s poll without being wastefully tight.
- **Scalability**: one process per node, one route, no fan-out. Load scales with the number of peers polling — at ~dozen-node scale that is a handful of requests per second against a cached body.
- **Reliability**: every detection path degrades to omission instead of failure; the collector is non-nil even when PDH initialization fails entirely, and memory-used keeps publishing. A listener bind or serve failure is fatal and relies on the broker's supervisor for restart.
- **Security**: no elevation, no key material generated or held beyond an operator-supplied cert/key or the cluster leaf read from disk. TLS is pinned to 1.2 minimum in both the bring-your-own and the cluster path. The cluster path requires a client cert and enforces a byte-for-byte DER pin match, answering `403` rather than an opaque handshake failure. In the shipped broker topology the route is plaintext on the local subnet by design; the Windows installer scopes its inbound rule to `remoteip=localsubnet` so the port is never open off-link.
- **Compliance**: the payload carries GPU marketing names, a CPU model string, core count, memory and VRAM byte counts, utilization percentages, `hostUuid`, and `clusterUuid`. It carries no hostname, no hardware serial number, no MAC or IP address, no username or path, no GPU UUID, adapter LUID, or IORegistry entry ID (`statsKey` is `json:"-"`), no model names, and no inference content. `hostUuid` is a random v4 UUID (or the cluster principal), not derived from hardware, so it is a per-install pseudonym rather than a hardware identifier — but it is stable and correlatable across sessions, and `clusterUuid` additionally reveals that a set of machines belongs to one cluster, which is exactly why the response must stay on the local subnet.

## 11. Assumptions
- The parent owns the stdio pipe and is the only control peer; stdin EOF means shut down, with no reconnect and no buffering.
- The broker relies on the fixed `--port` default of `14318` and registers that port with the discovery daemon without a handshake, so port negotiation does not apply to this service.
- Hardware does not change while the process runs, with the single exception that macOS re-enumerates its GPU inventory on every tick.
- Windows GPU counters require Windows 10 1709 or newer; the processor counter exists on every supported version.
- `nvidia-smi`, when present, is on `PATH` and returns within 3 s; a wedged driver is bounded by the timeout, not waited out.
- `/usr/sbin/ioreg` exists on every supported macOS release, runs unprivileged, and returns within 3 s; its `IOAccelerator` `PerformanceStatistics` keys are undocumented and may be renamed by a macOS release.
- The parent reports this node's cluster principal; until it does, membership is unknown rather than empty, and a consumer is expected to act only on an explicit report.
- Consumers treat an absent field as unknown and never as zero, ignore `msSince` while `telemetryValid` is `false`, and tolerate a response that carries only `{"GPUs":[],"telemetryValid":false,"msSince":0}`.
- Discovery is somebody else's job: the record advertising this port is published by the node-scanner daemon, and this process never touches mDNS.

## 12. Failure Modes and Mitigations
- **Static CPU or memory introspection fails**: no CPU or memory data. → `detectCPU` returns `nil` and `detectMemoryTotal` returns `0`; the `cpu` / `memory` objects drop out of JSON entirely and the rest of the response still serves.
- **DXGI factory creation fails on Windows**: no GPUs reported. → Logged at warn with the `HRESULT`; `detectGPUs` returns `nil` and the body carries `"GPUs":[]` rather than failing the request.
- **`nvidia-smi` missing or failing on Linux**: no per-GPU VRAM-used or utilization. → Static detection falls back to `ghw` names; the collector latches `nvidiaUnavailable` after one warn so the process does not re-spawn a missing binary every second, and CPU and memory keep reporting.
- **PDH counter set absent or query in an error state**: no GPU/CPU utilization on Windows. → Per-counter unavailable latches keep logs quiet; the per-tick collect failure is logged at debug and skips only the PDH-derived fields, because `GlobalMemoryStatusEx` runs on an independent path.
- **`/proc` read or parse failure on Linux**: CPU percent or memory-used unknown. → An invalid sample, a counter reset, or zero elapsed jiffies all yield `0`, which `omitempty` drops; the next tick recovers.
- **`ioreg` missing, wedged, or reporting renamed keys on macOS**: no GPU inventory or GPU stats. → The 3 s timeout bounds a wedged read and shutdown cancels one in flight, the failure warns once and every following tick retries, a renamed `PerformanceStatistics` key drops only its own metric, and CPU and memory keep publishing from the other goroutine.
- **RDP phantom or virtual display adapters**: the UI would show GPUs the node cannot compute on. → Software-flag skip, name denylist, and the DirectX-registry LUID gate; when the registry is unreadable the gate is skipped with a warn rather than dropping real adapters.
- **Unpinned peer reads a cluster-gated node**: inventory disclosure to a non-member. → `VerifyClientPin` fails and the handler answers `403`; the membership answer is re-read per request and by a 2 s watch, so a peer paired after startup is admitted without a restart.
- **Malformed or undelivered cluster-identity push**: peers cannot learn this node's membership over HTTP. → A malformed payload is dropped with a warn so a wrong principal is never latched, and the parent re-pushes on every membership or pin-set change; until a push lands the field stays absent, which consumers read as "unknown" and answer by leaving their existing annotation alone.
- **Address reuse / wrong host answers a poll**: telemetry attributed to the wrong node. → The response carries `hostUuid`; the desktop skips the merge when it disagrees with the node being polled, and `nvpair-manual-nodes` re-keys the entry to the reported identity.
- **Port already bound**: the process cannot serve. → `log.Fatalf` with the port in the message; the broker's supervisor restarts it and the failure is visible in the shared stderr stream.

## 13. Observability
- **Logging**: structured `slog` to stderr through `nvpair-shared/applog`, prefixed `[nvpair-node-info]` and interleaved into the broker's own stderr. Startup logs the detected GPU count with each adapter's name and VRAM in MiB, the CPU name and core count, total memory in MiB, the chosen listener layout with its port and (when gated) the current `clustered` value, and the served route. Steady state is quiet: counter, `/proc`, and Mach read failures log at debug, unavailability latches log once at warn (including the first failed `ioreg` read, after which retries are silent), an accepted cluster-identity push logs at info carrying only whether a principal is now held, cluster membership flips log at info from the watch goroutine, and a failed `nodeinfo:observed-addresses` write logs at debug. Request bodies are never logged — there are none — and neither are certificates or key material.
- **Metrics**: none. There is no metrics endpoint, no counter export, and no histogram; `/v1/node-info` itself is the telemetry surface, and aggregation is the consumer's job.
- **Alerts**: none raised by this service. It does not participate in the `errors:*` pipeline. A dead process surfaces as the broker's supervisor restart log and, downstream, as a node whose card falls back to its last cached enrichment.

## 14. Sample usage
The broker starts and spawns `nvpair-node-info --log-level info --node-id 6b1f5c2a-…`, deliberately without a cluster dir. The process resolves `hostUuid` from the flag, detects hardware — say one RTX 4090 via `nvidia-smi` with a 24564 MiB total, a 12-core Ryzen and 32 GiB via `ghw` — starts the one-second collector, binds `:14318`, and logs `serving /v1/node-info on port 14318`. The broker pushes `nodeinfo:set-cluster-identity` with this node's principal — empty here, because the node belongs to no cluster — so the process can report membership it holds no trust store to read. The broker then registers `{Service: "ni", Port: 14318}` with the discovery daemon, which folds `ni=14318` into this node's single `_nvpair-node` TXT record alongside `uuid=`, `ip=`, and the other service ports.

One second later the collector's first real tick lands: `/proc/stat` deltas to 7 % busy, `/proc/meminfo` reports 12 GiB used, and `nvidia-smi --query-gpu=uuid,utilization.gpu,memory.used` reports the 4090 at 37 % with 8 GiB resident. Because a utilization value parsed, the pass is usable and stamps `GPUSampledAt`; from now on responses carry `telemetryValid: true` and an `msSince` of at most about a second. The snapshot is published atomically.

A peer's node-scanner daemon sees the record, reads `ni=14318` from TXT (never the SRV port, which is non-authoritative), and issues `GET http://192.168.1.10:14318/v1/node-info` over plain HTTP — plain by static policy, because `noderec.ServiceNodeInfo.Transport()` is `TransportPlain` even against a clustered node. The handler refreshes an empty mesh (a no-op with no cluster dir), merges the static records with the snapshot by GPU UUID, and returns the document shown in §6. The daemon decodes `GPUs` / `cpu` / `memory` into the peer's `DirectoryNode`, caches it under the node's `hostUuid`, and publishes a `discovery:node-updated` event that the broker relays. It also reduces the response to the 4090's 37 % plus `telemetryValid` and `msSince` aged by its own fetch time, and hands that to its broker for the scheduler. On this host, that request's arrival recorded `192.168.1.10` as a peer-observed address, and the next 30 s report carries it to this node's own scanner.

Meanwhile the Electron desktop, having learned the same node from `discovery:get-nodes`, polls that identical URL every 2000 ms with a 1500 ms timeout. Each response is checked against the polled node's id via `hostUuid`; a match with unchanged telemetry emits a `metrics:update` push, and a change upserts the node so the GPU chart and node card re-render. On shutdown the broker closes stdin, `applog.StdinRPC` observes EOF and cancels the context, the collector's ticker stops, and both servers unwind under a 3 s graceful shutdown.

## 15. Process model, CLI, and build wiring

**CLI** (`flag` definitions in `main.go`):

| Flag | Type | Default | Effect |
|------|------|---------|--------|
| `--port` | int | `14318` | HTTP port to listen on; also the port the cluster-gated listener owns |
| `--tls-port` | int | `14319` | HTTPS port, used only with `--cert` and `--key` |
| `--cert` | string | _(none)_ | TLS server certificate (PEM); requires `--key` |
| `--key` | string | _(none)_ | TLS server private key (PEM); requires `--cert` |
| `--client-ca` | string | _(none)_ | PEM bundle of client-cert CAs; enables `RequireAndVerifyClientCert`; requires `--cert` and `--key` |
| `--accept-http` | bool | `false` | Keep the plaintext listener alive alongside HTTPS; inert when no cert is configured |
| `--cluster-dir` | string | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`); binds one cluster-gated listener on `--port` and overrides `--cert`/`--key` |
| `--node-id` | string | _(none)_ | Stable per-host UUID to report; empty resolves the local identity store |
| `--version` | bool | `false` | Print `main.Version` and exit `0` |
| `--log-level` | string | `""` | Registered by `applog.RegisterFlag`; precedence is flag > `NVPAIR_LOG_LEVEL` > `info` |

**Process model**: `nvpair-ui-broker` spawns it as a supervised child (`startNodeInfo`), passing `--log-level` and `--node-id`, hiding the console window on Windows, plumbing the child's stderr to the broker's own, and reading its stdout for `nodeinfo:observed-addresses` notifications. Any other notification is logged at debug and ignored, a frame with no method (such as a `log/set-level` response) is dropped, and an oversized frame is skipped with a warning. The broker writes `log/set-level` and `nodeinfo:set-cluster-identity` notifications to stdin. It shuts down on stdin EOF (the broker's `Stop` closes the pipe), `SIGINT`, or `SIGTERM`; teardown cancels the context, stops the collector, calls `Shutdown` on whichever servers exist under a 3 s timeout, and closes the splitter so both sub-listeners unwind. Running the binary standalone is supported and is what the bring-your-own TLS and `--cluster-dir` paths exist for.

**Build-tag matrix**:

| File | Build tag | Provides |
|------|-----------|----------|
| `main.go`, `stats.go`, `observed.go`, `cpu_detect.go`, `ioreg_parse.go`, `tls.go` | _(none)_ | wire types, `buildResponse`, `telemetryStatus`, `mergeGPUInventory`, `applyGPUStats`, handler, flags, cluster-identity state, the peer-observed address recorder and reporter, `ghw` CPU, IORegistry plist parsing, PDH instance-name parsing, TLS option validation |
| `memory_detect.go` | `!darwin` | `ghw` `detectMemoryTotal` |
| `memory_detect_darwin.go` | `darwin` | gopsutil `detectMemoryTotal` |
| `gpu_windows.go` | `windows` | DXGI `detectGPUs` |
| `gpu_linux.go` | `linux` | `nvidia-smi` `detectGPUs` with `ghw` fallback |
| `gpu_darwin.go` | `darwin` | `ioreg` `detectGPUs` |
| `gpu_other.go` | `!windows && !linux && !darwin` | `ghw` `detectGPUs` |
| `stats_windows.go` | `windows` | PDH + `GlobalMemoryStatusEx` collector, `luidKey` |
| `stats_linux.go` | `linux` | `/proc` + `nvidia-smi` collector |
| `stats_darwin.go` | `darwin` | gopsutil + `ioreg` collector |
| `stats_other.go` | `!windows && !linux && !darwin` | no-op collector, `luidKey` stub |
| `stats_test.go`, `observed_test.go`, `observed_wiring_test.go`, `identity_test.go`, `cluster_mtls_test.go`, `cluster_identity_test.go`, `ioreg_parse_test.go` | _(none)_ | merge/omitempty and telemetry validity, the observed-address recorder and its listener wiring, identity resolution, the mTLS gate, the cluster-principal wire states, and IORegistry parsing — run on every platform |
| `stats_linux_test.go` | `linux` | `/proc` and `nvidia-smi` parsers |
| `gpu_darwin_test.go`, `stats_darwin_test.go`, `memory_detect_darwin_test.go` | `darwin` | IORegistry GPU assembly and its timeout, the two-goroutine collector, and physical-memory detection |
| `stats_windows_test.go`, `gpu_windows_test.go` | `windows` | LUID formatting and Win32 struct-size assertions |

**Build and staging**: `services/build.sh` (Linux/macOS) and `services/build.bat` (Windows) read the component version from `services/versions.json` (`components["nvpair-node-info"]`), build step 2 of 12 with `go build -ldflags "-X main.Version=$V_NINFO"`, and stage the binary into `services/build/bin/`. `services/installer_build.sh` packages it into `dist/*.tar.gz` under `bin/`; `services/installer/nvpair-setup.nsi` bundles `nvpair-node-info.exe`, terminates any running instance on upgrade, deletes it on uninstall, and — because this is an inbound network listener — adds the firewall rule `NVPAIR Node Info` (`dir=in action=allow program="$INSTDIR\bin\nvpair-node-info.exe" profile=any remoteip=localsubnet`). `profile=any` is required so LAN discovery still works when Windows has classified the network as Public; `remoteip=localsubnet` keeps `14318` closed off-link. The desktop's macOS privileged-helper firewall list derives from `needsFirewallAccess` on this binary's entry in `desktop/src/shared/constants/modular-binaries.ts`, which is `true`.
