<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Node Info (`nvpair-node-info`)

## 1. Purpose
The per-node hardware reporter. It detects this host's GPU, CPU, and physical-memory inventory at startup, samples live utilization on a one-second tick, and serves the merged result as one JSON document at `GET /v1/node-info` on TCP `14318`. It also publishes private inventory snapshots to its owning broker. Public HTTP supplies frontend displays; services use authenticated remote reads or locally owned IPC measurements.

It exists as a separate binary because the readout is inherently platform-specific and privileged-adjacent — DXGI COM calls and PDH performance counters on Windows, `/proc` plus `nvidia-smi` on Linux, `ioreg` plus Mach counters on macOS, `ghw` for CPU identity everywhere — and because its consumers are heterogeneous: remote scanner and manual-node workers read protected inventory over pinned mTLS, the local scanner receives owned IPC snapshots through the broker, and desktop and TUI poll public HTTP for display. It keeps the current sample, local registry metadata, and address observations in memory and makes no routing decision. Its accepted GPU samples reach scheduler through scanner or manual-node workers and the broker. It also reports which of this host's own addresses served remote requests, which scanner uses as reachability evidence.

## 2. Scope
**In scope**
- Static hardware detection at startup: GPU adapters (name + total VRAM), CPU (model + physical core count), total physical RAM.
- Live utilization sampling on a one-second cadence: per-GPU VRAM used and busy percent, node CPU busy percent, memory used.
- Reporting whether a usable GPU utilization sample exists and how old it is (`telemetryValid`, `msSince`), so consumers can tell an idle 0 % reading from no reading and a fresh sample from a stale one.
- Serving public display inventory over HTTP and protected peer inventory over pinned mTLS at `/v1/node-info` on the same registered port.
- Publishing local hardware snapshots over private broker/worker IPC before membership and after departure.
- Receiving owned local registry metadata over IPC and exposing it only in authenticated peer inventory.
- Recording which local addresses served non-loopback peers and reporting that set to the parent as `nodeinfo:observed-addresses`.
- Reporting `hostUuid` and `clusterUuid`; public values are display claims and cannot establish identity or membership.
- Optional bring-your-own TLS/mTLS listener, and a cluster listener whose protected handler follows live membership while public HTTP remains available.

**Out of scope**
- mDNS advertisement and node discovery — `nvpair-node-scanner` owns the single `_nvpair-node` record and registers this service's port as `ni`.
- Minting cluster identity, pairing, and pin management — `nvpair-cluster-manager`; this service reads the cluster dir through `nvpair-shared/clustertrust` when it is given one, and is otherwise told the principal by its parent.
- Engine lifecycle, model inventory, and per-engine ports — `nvpair-engine-manager`.
- Routing, scheduling, and workload accounting — `nvpair-proxy`, `nvpair-job-scheduler`, and `nvpair-workload-manager`. This service reports the raw GPU sample; turning it into pressure is the scheduler's job.
- Choosing which address to publish — the scanner daemon ranks addresses; this service only supplies peer-observed evidence.
- Persisting telemetry history, aggregating across nodes, or raising alerts — consumers do that; this service reports only the current sample.

## 3. Key Use Cases
- **Local enrichment**: startup and two-second `nodeinfo:info-updated` snapshots travel through the broker as `discovery:local-node-info`. Scanner attaches them to its owned registry record, without a loopback HTTP service read.
- **Peer enrichment**: a remote scanner authenticates the node-info endpoint over exact-pin mTLS before promoting an announcement. Protected metadata supplies the peer's name, addresses, service ports, and hardware. Failed authentication never triggers public HTTP fallback.
- **Desktop telemetry poll**: Electron polls public HTTP every 2000 ms with a 1500 ms per-attempt timeout, existing address failover, and failure backoff capped at 30 s. It updates hardware display fields only, independently of verified broker snapshots. It reads this host over loopback. Public responses cannot change identity, trust, addresses, service ports, or models.
- **Terminal telemetry poll**: TUI polls the open node detail screen over public HTTP at the same cadence and timeout. It retains its response-size limit and refuses redirects. Both clients require a matching response `hostUuid` for verified rows, including the local row; a missing or different UUID is rejected. An unverified row's display preview may omit `hostUuid` and never turns the response's UUID into verified identity.
- **Scheduler GPU pressure**: scanner uses owned IPC locally and pinned mTLS remotely; manual-node hardware also requires pinned mTLS. They supply maximum GPU utilization with validity and elapsed age through the broker. Unknown or stale data follows the scheduler's existing freshness and pressure rules, and never becomes freshly idle through a failed read or replay.
- **Observed local addresses**: a non-loopback request records the real local address it reached. Every 30 s the service sends the current set as `nodeinfo:observed-addresses`; broker relays it to scanner. This is reachability evidence, including public requests, and does not authenticate the requester.
- **Manual node identification**: the manual worker probes `14318`, or the configured node-info port, over pinned mTLS every ten seconds. Matching response host/principal and certificate identities allow the broker to join that source to the same verified host. Explicit engine routes remain independent of hardware authentication.
- **Membership convergence**: live cluster state gates every protected request, including reused connections. Departure or pin replacement revokes authorization; public inventory remains available. Protected responses return `503` until owned metadata agrees with the serving node's current identity.
- **Edge case — no GPU**: `detectGPUs` returns an empty slice; `buildResponse` still allocates the slice, so the body is `{"GPUs":[],"telemetryValid":false,"msSince":0}` plus whatever CPU/memory data exists. The route never 404s or errors for a GPU-less host.
- **Edge case — GPU collection stops working**: a failed pass keeps the last usable sample and its original timestamp, so `telemetryValid` stays `true` and `msSince` keeps growing. Consumers read the growing age as staleness rather than seeing numbers blank out or silently freeze.
- **Edge case — unmatched adapter**: a GPU whose `statsKey` is absent from the current snapshot keeps zero-valued dynamic fields, which `omitempty` drops. Clients see a missing `vram_used_bytes` rather than a misleading literal `0`.
- **Edge case — static introspection fails**: a nil `*CPUInfo` or a zero `memTotal` drops the whole `cpu` / `memory` object, never an empty shell like `"cpu":{}`.
- **Edge case — unified memory (UMA)**: on Grace-Blackwell class hosts `nvidia-smi` returns `[N/A]` for `memory.total` / `memory.used`; `vram_bytes` is backfilled from total system RAM and `vram_used_bytes` from the `/proc/meminfo` used figure, which keeps working even when dynamic `nvidia-smi` collection never succeeds.
- **Edge case — macOS enumeration fails at startup**: `detectGPUs` returns nothing, but the collector re-reads IORegistry every tick and `mergeGPUInventory` folds the recovered adapters into the response, so the inventory repairs itself without a restart.
- **Edge case — RDP session on Windows**: DXGI enumerates a phantom clone of the physical adapter plus Microsoft's remoting adapters. Both a name denylist and a `HKLM\SOFTWARE\Microsoft\DirectX` `AdapterLuid` gate suppress them.
- **Edge case — clustered read (`--cluster-dir` only)**: a pinned peer with matching owned metadata gets `200`; an unpinned client gets `403`. Shared TLS self-trust remains supported, while local service measurements use IPC.

## 4. Open Questions / Risks
- **Public LAN inventory**: public HTTP intentionally exposes hardware measurements for desktop and TUI, including while clustered. Those responses cannot authenticate the endpoint or supply service decisions. Broker supplies `--cluster-dir` so pinned mTLS is independently available on the same port.
- **Risk — one-shot hardware detection**: `detectGPUs`, `detectCPU`, and `detectMemoryTotal` run once in `main`, so the CPU and memory readout is fixed for the process lifetime and a Windows or Linux adapter added, removed, or driver-installed after startup never appears. macOS is the exception: its collector re-enumerates IORegistry every tick and `mergeGPUInventory` adds adapters the startup pass did not see.
- **Risk — latched telemetry unavailability**: Linux latches `nvidiaUnavailable` on the first `nvidia-smi` failure and never retries; Windows adds PDH counters only inside `startStatsCollector`. A driver or counter set that becomes available later is not picked up for the process lifetime. macOS retries `ioreg` on every tick instead.
- **Risk — a stopped collector keeps serving its last GPU numbers**: on every platform a failed GPU pass retains the last usable sample, so a host whose `nvidia-smi` latched, whose PDH query broke, or whose IORegistry read started failing keeps reporting the old VRAM and utilization figures indefinitely. `msSince` growing is the only signal. The scheduler path honors it; a consumer that renders `utilization_percent` without checking `msSince` shows a frozen number.
- **Risk — `--tls-port` default collides**: `14319` is also the broker's fixed `nvpair-errors` port. Nothing binds it today because the broker never passes `--cert`/`--key`, and the cluster-gated listener deliberately reuses `--port` instead. Any future use of the bring-your-own TLS path under the broker must move this port.
- **Risk — unified-memory VRAM is system RAM**: on a unified-memory host `vram_bytes` is the total physical system memory — on Linux `vram_used_bytes` is the whole-system used figure as well, while on Apple Silicon it is the GPU driver's mapped allocation (`Alloc system memory`) rather than either the active working set or system-wide usage — so a consumer that sums "VRAM + RAM" double-counts. Nothing on the wire marks a GPU as unified.
- **Risk — undocumented IORegistry keys**: the macOS GPU numbers come from the `PerformanceStatistics` dictionary of an `IOAccelerator` entry (`Device Utilization %`, `GPU Activity(%)`, `Alloc system memory`, `vramUsedBytes`, `inUseVidMemoryBytes`, `vramFreeBytes`), none of which Apple documents or guarantees. A renamed memory key drops that one metric to zero, where `omitempty` hides it. A renamed utilization key is more visible: a pass with no utilization reading is not a usable sample, so `telemetryValid` stays `false` (or the previous sample ages). Either way a macOS release can degrade the readout without surfacing an error.
- **Risk — macOS VRAM used without a total**: an adapter that exposes neither `VRAM,totalMB` nor a dedicated used counter still reports `Alloc system memory` as `vram_used_bytes`, so a response can carry a used figure with no `vram_bytes` to divide it by. A consumer rendering a VRAM percentage has to treat the absent total as "no percentage" rather than zero.
- **Registry metadata is initially unavailable**: the broker supplies complete local metadata over IPC and resends it after worker restart. Protected reads return `503` while it is absent or disagrees with current identity. Public display and owned local measurements remain independent.
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
- Publish `nodeinfo:info-updated {info}` over owned stdio at startup and every two seconds. Broker forwarding and replay preserve validity and source age plus elapsed in-process time; time buffered in worker stdio before receipt is an accepted approximation.
- Accept `nodeinfo:set-peer-metadata {hostUuid, clusterUuid, peer}` from the owning broker, replace the complete snapshot, and require it to match current local identity before serving protected inventory.
- Keep public HTTP available in every membership state. Service reads require exact-current-pin mTLS remotely and owned IPC locally; public data cannot supply scheduling, routing, availability, or verified metadata.
- Mark a GPU pass usable only when at least one adapter yielded a utilization reading (a parsed 0 % counts); report `telemetryValid` and the usable sample's age as `msSince`, and keep the previous usable sample, timestamp included, when a pass fails.
- Record the local address of every request served to a non-loopback peer, expire each after 5 minutes unseen, and send the full current set, empty included, as `nodeinfo:observed-addresses` every 30 s.
- Report `hostUuid`, taking `--node-id` verbatim when supplied and otherwise resolving the shared local identity.
- Report `clusterUuid` from the live trust store when cluster-gated and from the parent's push otherwise, keeping "unknown" distinct from "belongs to no cluster".
- Honor the bring-your-own TLS contract (`--cert`/`--key`/`--client-ca`) and the cluster-gated contract (`--cluster-dir`) with the documented precedence.
- Handle `log/set-level`, `nodeinfo:set-cluster-identity`, and `nodeinfo:set-peer-metadata` on stdin and shut down on stdin EOF, `SIGINT`, or `SIGTERM`.

**Non-functional**
- The request path performs no hardware sampling. It reads the atomic collector snapshot and serializes it; cluster mode also refreshes live trust, and protected reads take short metadata locks. It must stay well inside the existing client timeout under two-second polling.
- Compiles and runs on Windows, Linux, and macOS × amd64/arm64; the telemetry paths use direct syscalls, file reads, purego bindings, and short-lived unprivileged helper processes rather than cgo.
- Unknown values are omitted from JSON, never reported as a literal zero.
- Never logs prompts, request bodies, certificates, or key material.
- Testable without real hardware: every parser and the response merge are pure functions with table tests.

## 6. Inputs and Outputs

**Inputs** — CLI flags at startup (§15); the host's own hardware interfaces (DXGI, PDH, `GlobalMemoryStatusEx`, `/proc/stat`, `/proc/meminfo`, `nvidia-smi`, `/usr/sbin/ioreg`, Mach through gopsutil, `ghw`); optionally a cluster dir (`node.crt`, `node.key`, `trusted/*.json`, `admission.json`) read through `nvpair-shared/clustertrust`; the identity store read through `nvpair-shared/nodeid`; and newline-delimited JSON-RPC 2.0 control frames on stdin. There is no request payload — `/v1/node-info` takes no parameters, headers, or query string.

**Outputs** — one JSON body per request, and stderr logs via `nvpair-shared/applog`. Serialized stdout carries `nodeinfo:info-updated` at startup and every two seconds, `nodeinfo:observed-addresses` every 30 s, and replies to id-bearing control requests. It emits no `ready` frame.

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
Two interfaces: the inventory route with independent public HTTP and protected mTLS handlers, and owned stdio JSON-RPC for local snapshots, metadata, and control.

### 7.0 Methods and notifications
HTTP surface — one route, exactly:

| Route | Method | Auth | Success | Failure |
|-------|--------|------|---------|---------|
| `/v1/node-info` over public HTTP | any (the pattern is path-only) | none, including while clustered | `200`, `Content-Type: application/json`, public `NodeInfoResponse` | — |
| `/v1/node-info` over cluster mTLS | any (the pattern is path-only) | live membership and the client's exact current pin | `200`, `NodeInfoResponse` plus protected `peer` metadata | handshake refused while unclustered, `403` for an unpinned client, `503` while local metadata is incomplete or inconsistent |
| any other path | any | — | — | `404` from `http.ServeMux` |

stdio surface — newline-delimited JSON-RPC 2.0 on stdin, handled by `applog.StdinRPC`:

| Method | Params | Result |
|--------|--------|--------|
| `log/set-level` | `{ level }` (`debug`\|`info`\|`warn`\|`warning`\|`error`) | `{ level }` when the frame carries an `id`; a rejected level answers `-32602` |
| `nodeinfo:set-cluster-identity` | `{ clusterUuid }` (empty means "belongs to no cluster") | none — a notification; the value appears on the next `/v1/node-info` response |
| `nodeinfo:set-peer-metadata` | `{hostUuid, clusterUuid, peer: {name, addresses, services}}` | `{ok:true}` for an id-bearing request; malformed or mismatched host returns `-32602` |

`applog.StdinRPC` dispatches `log/set-level` itself. Identity and peer-metadata handlers accept their corresponding frames and ignore unrelated methods. A malformed identity push is dropped rather than changing the reported claim. With `--cluster-dir`, live trust-store state supplies `clusterUuid`; the identity push is used only without that directory.

Outbound, the service sends these notifications to its owning parent:

| Notification | Params | Cadence |
|--------------|--------|---------|
| `nodeinfo:observed-addresses` | `{ addresses: string[] }` | every 30 s, unconditionally, including an empty list |
| `nodeinfo:info-updated` | `{info: NodeInfoResponse}` | startup and every two seconds |

Local snapshots have the public hardware shape and omit protected `peer` metadata. Broker forwards them privately to scanner; they are not frontend HTTP readings.

### 7.1 Response schema
`buildResponse` copies the static `[]GPUInfo` detected at startup, folds in any adapters the snapshot's own `GPUInventory` carries (`mergeGPUInventory` enriches a matching `statsKey` in place and appends an unseen one), and overlays the latest snapshot by `statsKey`. The resulting typed snapshot is encoded at the HTTP and IPC boundaries. Field semantics:

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
| `peer` | `*noderec.PeerMetadata` | yes — protected response only | owned local registry snapshot: `{name, addresses, services}`, with service keys mapped to ports |

`GPUInfo.statsKey` (Windows adapter LUID, Linux GPU UUID, macOS IORegistry entry ID) and `GPUInfo.usesSystemMemoryUsage` are unexported and tagged `json:"-"`: the first joins the static record to the snapshot map, the second marks an adapter whose used bytes come from the system memory sample, and neither reaches the wire. An absent field means "unknown". A genuinely idle GPU also drops `utilization_percent`, and for display the two cases render identically. `telemetryValid` distinguishes a valid node-wide GPU sample whose highest utilization is 0 % from a missing reading. Consumers ignore `msSince` while validity is false. `clusterUuid` preserves three wire states: absent means unknown, empty claims unclustered, and a nonempty value claims that node principal. Public claims cannot change verified membership.

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
`resolveHostUUID(nodeID, clusterDir)` returns `--node-id` verbatim when non-empty. The broker always passes its own already-resolved node UUID, so `/v1/node-info` reports exactly the identity the fleet keys on even when the broker runs on a custom data root. Standalone, it calls `nodeid.Resolve(filepath.Dir(clusterDir))` — the empty string selects the shared per-user data dir — which prefers `<base>/cluster/identity.json`'s `node_uuid` (the cluster-manager's principal), then `<base>/node-id.json`, and otherwise mints and persists a random v4 UUID. Resolution never fails the caller.

The cluster principal is resolved separately by deployment mode. With `--cluster-dir`, including under the broker, the field is the live mesh principal while clustered and an empty string otherwise. Without that directory it reports the parent's identity push, remaining absent until the first push. Public HTTP carries an unverified claim in either mode; a consumer cannot use it to clear or establish membership. Protected responses require `hostUuid`, `clusterUuid`, and the owned registry identities to equal the serving certificate principal.

### 7.5 Listeners, TLS, and cluster trust
Three mutually-exclusive listener layouts, decided at startup from the flags:

1. **Plaintext standalone default**: one public HTTP listener on `--port`, with no cluster directory or operator cert.
2. **Bring-your-own TLS**: `--cert` + `--key` build a `tls.Config` pinned to `MinVersion: TLS 1.2` on `--tls-port`; `--client-ca` additionally loads a trust bundle and sets `RequireAndVerifyClientCert`, so an untrusted client is dropped at handshake. `validate()` rejects `--cert` without `--key`, `--key` without `--cert`, and `--client-ca` without both. The plaintext listener is dropped unless `--accept-http` is passed; the two bind separate ports so clients migrate independently.
3. **Public HTTP and cluster mTLS (broker topology)**: `--cluster-dir` selects one split listener on `--port`, avoiding a second advertised port and the `--tls-port` collision with `nvpair-errors`. TLS connections go to `Mesh.ServerTLSConfig()` and the protected inventory handler; plain connections go to the independent public handler. Operator certificates do not replace the cluster pins.

Under (3), protected requests refresh membership and require an authenticated client whose certificate matches its current exact pin, or the existing shared self-trust rule. The handler rechecks authorization after collecting the response, including on reused TLS connections, and returns `503` until owned metadata matches its current identity and contains a name, addresses, and a valid `ni` port. Departure disables this protected surface even when the keypair remains. The server leaf is chosen per handshake; the two-second mesh watcher follows changes without depending on an HTTP request. The separate public handler remains available before joining, while clustered, and after leaving. Local service measurements use owned IPC rather than the self-trust network path.

Every listener sets `ReadHeaderTimeout: 5 * time.Second` and the same `ConnState` hook (§7.6). Both cluster-mode sub-listeners use `clustertrust.PeerListenerIdleTimeout` (105 s), longer than the calling pool's 90 s idle timeout so the client reaps its idle connection first.

### 7.6 Peer-observed addresses
A multi-homed host cannot tell from its own side which of its addresses a given peer can reach — a direct-connect link works only from the machine on its far end. A peer completing a request is the one fact that settles it, and every peer's inventory poll already arrives here, so the evidence is free and continuous.

- Every listener's `http.Server.ConnState` hook records the connection's local IP when the connection reaches `StateActive`, meaning a request has actually been read. That is what makes it evidence rather than a guess: a bare TCP connection could be a port scan. Loopback peers, and loopback or unspecified local addresses, are ignored, because this machine talking to itself says nothing about what others can reach.
- Each address stays current for `observationTTL` (5 min) after it was last seen, which keeps a quiet peer from retracting its proof while letting an address the host stops answering on drop out within minutes.
- `reportLoop` sends the full current set every `observedReportEvery` (30 s) as `nodeinfo:observed-addresses`, sorted so an unchanged set reports identically. It reports unconditionally — including an empty list — so a restarted parent relearns the set on the next tick and an expired observation is actually withdrawn.
- The broker relays each report to scanner as `discovery:set-observed-addresses`, which ranks the host's own reachable addresses. Public requests also contribute; this evidence authenticates neither the requester nor any remotely advertised address.

### 7.7 Versioning
- The route is version-prefixed (`/v1/node-info`); a breaking shape change takes a new prefix.
- Response fields grow additively. Every optional field is `omitempty` — `telemetryValid` and `msSince` are deliberately not, because their zero values are meaningful — and consumers decode into their own mirrored structs (`noderec.GPUInfo`/`CPUInfo`/`MemoryInfo`, `nvpair-manual-nodes`, the desktop's parser), so an unknown field is ignored rather than fatal. `GPUs` keeps its capitalized tag; renaming it would break every consumer at once.
- The binary stamps `main.Version` from `services/versions.json` at build time and prints it with `--version`.

## 8. Dependencies
- **Upstream**: `nvpair-ui-broker` owns the process and stdio, supplies node identity and the cluster directory, pushes owned registry metadata, reads local snapshots and address observations, and registers the `ni` service port with scanner.
- **Downstream**: remote scanner and manual-node workers use protected mTLS inventory; desktop and TUI use public HTTP display inventory; the local scanner receives snapshots through the broker. Scheduler receives accepted GPU samples indirectly. This worker makes no outbound service calls.
- **External**: `github.com/jaypipes/ghw` (CPU, non-macOS memory, fallback GPU); `github.com/shirou/gopsutil/v4` (macOS CPU times and memory, purego rather than cgo); `howett.net/plist` (IORegistry decoding); `golang.org/x/sys/windows` (DXGI, PDH, kernel32, registry); the `nvidia-smi` binary on Linux when present and the stock `/usr/sbin/ioreg` on macOS; first-party `nvpair-shared/applog`, `nvpair-shared/nodeid`, `nvpair-shared/noderec`, `nvpair-shared/clustertrust`, and `nvpair-shared/splitlisten`. No third-party runtime services, no network egress.

## 9. Data Ownership
- **Owned**: static hardware, current measurements and their sample time, observed local addresses, and the latest broker-supplied registry metadata, all in memory. Broker resends metadata after restart and retains local sample age when replaying accepted snapshots.
- **Source of truth**: this host's hardware inventory and utilization originate here. Cluster-manager owns identity, membership, and pins; scanner's local registry owns the protected name, addresses, and service ports. Under the broker, `--node-id` supplies host identity and the read-only live mesh supplies membership.
- **Storage**: no database, no telemetry file. The only possible write is `nodeid.Resolve` minting `<base>/node-id.json` (`0600`, parent `0700`) when it runs standalone with no existing identity; under the broker `--node-id` short-circuits that path. `<base>` is `%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows, `$XDG_CONFIG_HOME`/`~/.config/Nvidia Corporation/Personal AI Router` on Linux, and `~/Library/Application Support/Nvidia Corporation/Personal AI Router` on macOS. The cluster dir is read-only to this service.

## 10. Design Constraints
- **Performance**: request handling is one atomic load plus a marshal of a few hundred bytes, and the connection hook adds one short mutex-guarded map write per request from a remote peer; all hardware work happens on the collector goroutines, and macOS keeps its once-a-second `ioreg` child process off the path that publishes CPU and memory. The 1 s tick matches the natural interval of the Windows rate counters and keeps the sample fresh for a 2 s poll without being wastefully tight.
- **Scalability**: one process per node, one route, no fan-out. Load scales with the number of peers polling — at ~dozen-node scale that is a handful of requests per second against a cached body.
- **Reliability**: every detection path degrades to omission instead of failure; the collector is non-nil even when PDH initialization fails entirely, and memory-used keeps publishing. A listener bind or serve failure is fatal and relies on the broker's supervisor for restart.
- **Security**: no elevation or generated key material. Public HTTP is a supported display interface. Service measurements use owned local IPC or exact-pin remote mTLS with TLS 1.2 minimum; operator CAs cannot substitute for cluster pins. Live membership and pin checks revoke reused connections and requests still collecting a response. The Windows installer scopes the node-info inbound rule to the local subnet.
- **Compliance**: public inventory contains hardware marketing names, capacities, utilization, and identity claims. Protected inventory additionally includes the registered host name, addresses, and service ports. Neither contains hardware serial numbers, MAC addresses, usernames, paths, GPU UUIDs, adapter LUIDs, IORegistry entry IDs, model names, or inference content. Host identity is stable and correlatable across sessions; a cluster principal identifies the node rather than a shared cluster-wide identifier.

## 11. Assumptions
- The parent owns the stdio pipe and is the only control peer; stdin EOF means shut down, with no reconnect and no buffering.
- The broker relies on the fixed `--port` default of `14318` and registers that port with the discovery daemon without a handshake, so port negotiation does not apply to this service.
- Hardware does not change while the process runs, with the single exception that macOS re-enumerates its GPU inventory on every tick.
- Windows GPU counters require Windows 10 1709 or newer; the processor counter exists on every supported version.
- `nvidia-smi`, when present, is on `PATH` and returns within 3 s; a wedged driver is bounded by the timeout, not waited out.
- `/usr/sbin/ioreg` exists on every supported macOS release, runs unprivileged, and returns within 3 s; its `IOAccelerator` `PerformanceStatistics` keys are undocumented and may be renamed by a macOS release.
- With a cluster directory, live membership supplies the cluster principal. Standalone without that directory, the parent's identity push is unknown until first supplied. Public claims never override established identity or trust.
- Consumers treat an absent measurement field as unknown and never as zero, and ignore `msSince` while `telemetryValid` is `false`. An unverified frontend preview may accept a response containing only `{"GPUs":[],"telemetryValid":false,"msSince":0}`. A verified frontend row additionally requires its matching `hostUuid`; protected service readers also require the authenticated identity and peer metadata described above.
- Discovery is somebody else's job: the record advertising this port is published by the node-scanner daemon, and this process never touches mDNS.

## 12. Failure Modes and Mitigations
- **Static CPU or memory introspection fails**: no CPU or memory data. → `detectCPU` returns `nil` and `detectMemoryTotal` returns `0`; the `cpu` / `memory` objects drop out of JSON entirely and the rest of the response still serves.
- **DXGI factory creation fails on Windows**: no GPUs reported. → Logged at warn with the `HRESULT`; `detectGPUs` returns `nil` and the body carries `"GPUs":[]` rather than failing the request.
- **`nvidia-smi` missing or failing on Linux**: no per-GPU VRAM-used or utilization. → Static detection falls back to `ghw` names; the collector latches `nvidiaUnavailable` after one warn so the process does not re-spawn a missing binary every second, and CPU and memory keep reporting.
- **PDH counter set absent or query in an error state**: no GPU/CPU utilization on Windows. → Per-counter unavailable latches keep logs quiet; the per-tick collect failure is logged at debug and skips only the PDH-derived fields, because `GlobalMemoryStatusEx` runs on an independent path.
- **`/proc` read or parse failure on Linux**: CPU percent or memory-used unknown. → An invalid sample, a counter reset, or zero elapsed jiffies all yield `0`, which `omitempty` drops; the next tick recovers.
- **`ioreg` missing, wedged, or reporting renamed keys on macOS**: no GPU inventory or GPU stats. → The 3 s timeout bounds a wedged read and shutdown cancels one in flight, the failure warns once and every following tick retries, a renamed `PerformanceStatistics` key drops only its own metric, and CPU and memory keep publishing from the other goroutine.
- **RDP phantom or virtual display adapters**: the UI would show GPUs the node cannot compute on. → Software-flag skip, name denylist, and the DirectX-registry LUID gate; when the registry is unreadable the gate is skipped with a warn rather than dropping real adapters.
- **Unpinned peer reads protected inventory**: the TLS handler returns `403` unless the client has a current matching pin. Public HTTP hardware remains available and omits registry metadata.
- **Owned metadata is absent or inconsistent**: protected inventory returns `503` until broker metadata agrees with the current host and cluster principal. Public display and private local snapshots remain available.
- **Malformed cluster-identity push in standalone mode**: the payload is dropped with a warning. An absent field stays unknown; public responses do not authorize service membership changes.
- **Address reuse or wrong host answers a poll**: remote service readers reject certificate or response-identity mismatches and never retry public HTTP. Frontend public reads remain hardware display data, and a verified row rejects a missing or mismatched response `hostUuid` while trying its remaining addresses. An unverified preview cannot promote a claim.
- **Port already bound**: the process cannot serve. → `log.Fatalf` with the port in the message; the broker's supervisor restarts it and the failure is visible in the shared stderr stream.

## 13. Observability
- **Logging**: structured `slog` to stderr through `nvpair-shared/applog`, prefixed `[nvpair-node-info]` and interleaved into the broker's own stderr. Startup logs the detected GPU count with each adapter's name and VRAM in MiB, the CPU name and core count, total memory in MiB, the chosen listener layout with its port and (when gated) the current `clustered` value, and the served route. Steady state is quiet: counter, `/proc`, and Mach read failures log at debug, unavailability latches log once at warn (including the first failed `ioreg` read, after which retries are silent), an accepted cluster-identity push logs at info carrying only whether a principal is now held, cluster membership flips log at info from the watch goroutine, and a failed `nodeinfo:observed-addresses` write logs at debug. Request bodies are never logged — there are none — and neither are certificates or key material.
- **Metrics**: none. There is no metrics endpoint, no counter export, and no histogram; `/v1/node-info` itself is the telemetry surface, and aggregation is the consumer's job.
- **Alerts**: none raised by this service. It does not participate in the `errors:*` pipeline. A dead process surfaces as the broker's supervisor restart log and, downstream, as a node whose card falls back to its last cached enrichment.

## 14. Sample usage
The broker starts `nvpair-node-info --log-level info --node-id 6b1f5c2a-… --cluster-dir <dir>`. The worker resolves the supplied host identity, detects hardware, starts the one-second collector, and opens the shared public HTTP / protected mTLS port at `14318`. Broker registers `ni=14318` with scanner and supplies the complete local registry snapshot as `nodeinfo:set-peer-metadata`.

One second later the collector's first real tick lands: `/proc/stat` deltas to 7 % busy, `/proc/meminfo` reports 12 GiB used, and `nvidia-smi --query-gpu=uuid,utilization.gpu,memory.used` reports the 4090 at 37 % with 8 GiB resident. Because a utilization value parsed, the pass is usable and stamps `GPUSampledAt`; from now on responses carry `telemetryValid: true` and an `msSince` of at most about a second. The snapshot is published atomically.

The worker publishes the current hardware response privately at startup and every two seconds. Broker forwards it as `discovery:local-node-info` to the local scanner, including elapsed cache time. These local measurements work before joining and after leaving a cluster.

An approved remote scanner treats mDNS addresses and `ni` port as connection candidates and sends `GET https://192.168.1.10:14318/v1/node-info` over pinned mTLS. The handler checks current client authorization, gathers hardware, checks authorization again, and adds matching owned metadata. The reader verifies the actual server certificate and both response identities before promoting the peer or caching its measurements. Failed authentication cannot replace an established peer or trigger an HTTP fallback.

Desktop and TUI independently use public `http://192.168.1.10:14318/v1/node-info` for hardware display, including while clustered. Those responses omit `peer` and cannot modify service identity, trust, routing, or scheduling. On shutdown, stdin EOF cancels the worker, stops sampling, and unwinds both servers under the graceful-shutdown timeout.

## 15. Process model, CLI, and build wiring

**CLI** (`flag` definitions in `main.go`):

| Flag | Type | Default | Effect |
|------|------|---------|--------|
| `--port` | int | `14318` | Public HTTP port; with a cluster directory it also serves pinned mTLS through the split listener |
| `--tls-port` | int | `14319` | HTTPS port, used only with `--cert` and `--key` |
| `--cert` | string | _(none)_ | TLS server certificate (PEM); requires `--key` |
| `--key` | string | _(none)_ | TLS server private key (PEM); requires `--cert` |
| `--client-ca` | string | _(none)_ | PEM bundle of client-cert CAs; enables `RequireAndVerifyClientCert`; requires `--cert` and `--key` |
| `--accept-http` | bool | `false` | Keep public HTTP alongside operator HTTPS; cluster mode always keeps public HTTP available |
| `--cluster-dir` | string | _(none)_ | Read-only cluster state (`node.crt`/`node.key`, admission, and pins); serves independent public HTTP and protected exact-pin mTLS handlers on the primary port |
| `--node-id` | string | _(none)_ | Stable per-host UUID to report; empty resolves the local identity store |
| `--version` | bool | `false` | Print `main.Version` and exit `0` |
| `--log-level` | string | `""` | Registered by `applog.RegisterFlag`; precedence is flag > `NVPAIR_LOG_LEVEL` > `info` |

**Process model**: broker supervises node-info, supplies `--log-level`, `--node-id`, and the cluster directory, owns stdio, and inherits stderr. It reads local inventory and address notifications and writes registry metadata and control frames. Restart rejects retired callbacks, retains the original receipt time of accepted local samples, and resends current metadata. The child exits on stdin EOF, `SIGINT`, or `SIGTERM`, stops the collector, gracefully shuts down listeners, and closes the splitter. Standalone public HTTP and operator HTTPS remain supported deployment modes.

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
| Matching `*_test.go` files beside their primary implementation | _(none)_ | hardware response fields, sample validity and age, observed addresses, identity resolution, public HTTP and protected mTLS behavior, owned IPC, and IORegistry parsing |
| `stats_linux_test.go` | `linux` | `/proc` and `nvidia-smi` parsers |
| `gpu_darwin_test.go`, `stats_darwin_test.go`, `memory_detect_darwin_test.go` | `darwin` | IORegistry GPU assembly and its timeout, the two-goroutine collector, and physical-memory detection |
| `stats_windows_test.go`, `gpu_windows_test.go` | `windows` | LUID formatting and Win32 struct-size assertions |

**Build and staging**: `services/build.sh` (Linux/macOS) and `services/build.bat` (Windows) read the component version from `services/versions.json` (`components["nvpair-node-info"]`), build step 2 of 13 with `go build -ldflags "-X main.Version=$V_NINFO"`, and stage the binary into `services/build/bin/`. `services/installer_build.sh` packages it into `dist/*.tar.gz` under `bin/`; `services/installer/nvpair-setup.nsi` bundles `nvpair-node-info.exe`, terminates any running instance on upgrade, deletes it on uninstall, and — because this is an inbound network listener — adds the firewall rule `NVPAIR Node Info` (`dir=in action=allow program="$INSTDIR\bin\nvpair-node-info.exe" profile=any remoteip=localsubnet`). `profile=any` is required so LAN discovery still works when Windows has classified the network as Public; `remoteip=localsubnet` keeps `14318` closed off-link. The desktop's macOS privileged-helper firewall list derives from `needsFirewallAccess` on this binary's entry in `desktop/src/shared/constants/modular-binaries.ts`, which is `true`.
