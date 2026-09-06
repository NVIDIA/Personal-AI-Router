<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# llama.cpp Engine for NVIDIA PAIR

**Date:** 2026-09-05  
**Updated:** 2026-09-06  
**Status:** Approved  
**Repo:** NVIDIA Personal AI Router (local fork)  
**Approach:** First-class `llamacpp` engine plus a new OpenAI-compatible proxy, adopt-only against an already-running `llama-server`.

## Goal

PAIR routes OpenAI-compatible inference to already-running official llama.cpp `llama-server` instances the same way it routes to Ollama and LM Studio, including across paired Windows, macOS, and Linux nodes on the LAN. PAIR does not install, launch, stop, or load GGUFs. On this Windows machine the AI Playground owns `llama-server` on port 8082. ComfyUI currently holds that GPU, so PAIR must never cause a local model load or swap. Requests may still go to a Mac or laptop that has the model loaded.

## Decisions (locked)

| Decision | Choice |
|---|---|
| Engine surface | Official `llama-server` HTTP API (`/v1/...`) |
| Lifecycle | Adopt already-running server only. No install, no spawn, no kill |
| Default adopt port | **8082** (this machine's AI Playground official llama.cpp / Hermes / DeepSeek Harness) |
| Per-node adopt port | Configurable in Engine settings. Changes which port PAIR **probes**. PAIR never rebinds a foreign llama-server. A Mac on stock `:8080` sets 8080 on that Mac's PAIR |
| PAIR facade port | **8084** (new `llamacpp-proxy`; must not bind the adopt port) |
| Discovery key | `lc` |
| Wire engine id | `llamacpp` (Go, JSON-RPC, scheduler, desktop `EngineType`) |
| Display name | `llama.cpp` |
| Catalog | Full `GET /v1/models` id list |
| Routing eligibility | Only ids with `status.value == "loaded"` **on that node** |
| Unloaded on every node | HTTP **502**, never forwarded, never loaded |
| Unloaded locally, loaded on a peer | Forward to that peer. Not a 502 |
| Preferred model | `ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp` (docs/default label only, not a filter) |
| Cluster | PAIR on each participating Mac/laptop, same LAN, pair with PIN. This fork, not stock NVIDIA 0.1.1 |
| Manual nodes | Probe llama.cpp as well as Ollama/LM Studio (port = that node's adopt port) |
| Other playground ports on this PC | Out of scope as extra engines (Bonsai 8080, ik_llama 8083). 8080 remains valid as another node's adopt port |
| ComfyUI | Not detected. Safety = never trigger a load |

## Architecture

PAIR gains a third engine beside `ollama` and `lmstudio`.

```
App on any PAIR node
    │
    ▼
local llamacpp-proxy :8084
    │
    ├─ GET /v1/models              → merge catalog from every lc node
    └─ chat/completions
            │
            ├─ this node has id loaded?  → loopback llama-server (adopt port)
            ├─ a paired Mac/laptop has it loaded? → that node's PAIR :8084 (mTLS)
            └─ nobody has it loaded?     → 502 (do not load)
```

Two ports per node, same split as LM Studio: apps and peers talk to **8084**. The local `llama-server` stays on that node's adopt port (8082 here, often 8080 on a Mac). Hermes, DeepSeek Harness, and the llama.cpp Web UI on this PC keep talking to **8082** directly.

Engine-manager today can only start an engine by spawning a binary (process mode) or running CLI commands (command mode). llama.cpp needs a third **adopt-only** runtime: Start probes 8082 and never launches a process. If the probe fails, the engine is not running.

Readiness and health probes are `GET http://127.0.0.1:{port}/v1/models` with status 200 (same idea as LM Studio). That stays green when the router is up with an empty loaded set. PAIR does not use `/health` as the probe, because some llama.cpp builds return 503 when no model is loaded, which would hide a running unloaded server.

If a `/v1/models` entry has no `status.value`, treat it as **unloaded**. Missing status must not be treated as loaded.

## Components

### 1. `llamacpp-proxy` (new Go service)

Clone of `lmstudio-proxy`. Own module under `services/llamacpp-proxy/`.

- Default listen port **8084**, persisted like the other proxies, `--ignore-persisted-port` for broker-managed start.
- Subscribe to discovery `lc`.
- Forward `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/embeddings`.
- `GET /v1/models` merges the full catalog across candidate nodes.
- Inference eligibility uses **loaded** ids only. Catalog-only ids are 502 and produce zero upstream requests.
- Cluster mTLS ingress, CORS, failover, `node/set-local-backend`, `node/set-priority`, and activity reports match LM Studio.
- Workloads tagged `llamacpp`.
- Failover only to nodes that advertise the requested id as loaded.

Depends on: `nvpair-shared` (noderec, cors, clustertrust, schedulerwire). Does not own llama-server.

### 2. Engine-manager: manifest + adopt-only runtime

New `services/nvpair-engine-manager/manifests/llamacpp.json`.

- `engine`: `llamacpp`
- `display_name`: `llama.cpp`
- `runtime.port`: 8082 (default adopt probe; overridable per node, not a rebind of llama-server)
- `runtime.bind`: `127.0.0.1` (PAIR talks loopback; playground may still bind `0.0.0.0` itself)
- `runtime.ready` / `runtime.health`: `GET http://127.0.0.1:{port}/v1/models` status 200
- No `install`, `uninstall`, `pull_model`, `load_model`, `unload_model`, `delete_model`
- `list_models`: `GET /v1/models`, result array `data`, field `id`
- `loaded_models`: same endpoint, field `id`, match `status.value` equals `"loaded"`

Engine-manager changes:

- New runtime mode `runtime.mode: "adopt"`. Start probes ready; success marks running with `adopted=true` and no child process. Failure returns an actionable error. No spawn path. Process mode still requires `bin`; command mode still requires `start`; adopt mode requires `ready` and forbids `bin` and `start`.
- Action result `match` must support equality on a nested field (`status.value == "loaded"`). Today LM Studio only has `nonempty`. Add equality rather than a llama.cpp special case in Go.
- Detect: WinGet `ggml.llamacpp` `llama-server.exe`, `llama-server` on `PATH`, AI Playground official binary `vendor\llama.cpp-official\bin\llama-server.exe`, **or** a healthy 8082. Binary present + 8082 down = installed, not running. No binary + 8082 down = not installed.

Stop/restart against a foreign listener stay declined (existing adoption rules). Setting the adopt port is allowed: it only changes which loopback port PAIR probes and hands to `set-local-backend`. It must not send a bind change to llama-server. PAIR never sends load/unload HTTP to llama-server.

### 3. Shared discovery (`nvpair-shared/noderec`)

- `ServiceLlamaCpp ServiceKey = "lc"`
- Include `lc` in `serviceKeyOrder`
- Transport policy same as `ol` / `lm`: advertised proxy port is cluster mTLS ingress; local engine is loopback plaintext

### 4. Manual nodes

`nvpair-manual-nodes` today probes Ollama `:11434` and LM Studio `:1234`. Add a llama.cpp probe (`GET /v1/models`) on the node's adopt port (default 8082). Emit `llamacpp_up` / `llamacpp_port` / `llamacpp_models` (loaded ids for routing, full catalog for listing). The broker bridges a reachable manual llama.cpp node into `llamacpp-proxy` the same way it bridges LM Studio into `lmstudio-proxy`.

### 5. Broker, scheduler, desktop, TUI

**Broker (`nvpair-ui-broker`)**

- Spawn `llamacpp-proxy` via `--llamacpp-proxy-path` (parallel to `--lmstudio-proxy-path`)
- Advertise loop: healthy engine + proxy up + ports differ → register `lc` with **proxy** port; `node/set-local-backend` to the adopt port (8082 here)
- Otherwise unregister `lc` and clear the local backend
- Relay namespace `llamacpp-proxy:`

**Scheduler**

- `schedulerEngines` includes `llamacpp`

**Desktop**

- `EngineTypes` / `EnabledEngineTypes` include `llamacpp`
- Display name `llama.cpp`
- Capabilities: no install, uninstall, delete, load, eject, or engine-hub pull. `hasEnginePort: true`. Endpoints show `http://127.0.0.1:8084/v1`
- Map `llamacpp` through the same proxy-bridge path as LM Studio (`proxyEngineFromManagerId`, `proxyRelayPrefix`, `MODULAR_RUNTIME_BINARIES`)

**TUI**

- Third proxy row (label `llama.cpp`, prefix `llamacpp-proxy`)
- Health list includes `llamacpp-proxy`

**Inference dispatcher**

- `--backend llamacpp` talks to PAIR facade port 8084, not 8082

### 6. Build and packaging

- `services/versions.json`: new `llamacpp-proxy` component version `0.1.0`; bump `nvpair-engine-manager`, `nvpair-ui-broker`, `nvpair-job-scheduler`, `nvpair-tui`, and shared as required by VERSIONING.md
- `desktop/src/shared/constants/modular-binaries.ts`: add `llamacpp-proxy`
- `services/build.bat` / `build.sh`, BOM, service-contract docs (`npm run service-contracts:write`), architecture/engine-lifecycle docs
- Fourteen supervised binaries instead of thirteen

## Data flow

**Bring-up**

1. Electron starts only `nvpair-ui-broker`. Broker starts `llamacpp-proxy` on 8084 and engine-manager with the `llamacpp` manifest.
2. Engine-manager probes `GET http://127.0.0.1:{adoptPort}/v1/models` (default 8082).
   - 200: adopt (installed, running, healthy). No process spawned.
   - down: not running. Proxy stays up. `lc` is not advertised for this node. The proxy can still route to paired peers.
3. If healthy, broker registers `lc` with port 8084 and sets the proxy local backend to `127.0.0.1:{adoptPort}`.
4. Engine-manager fills `modelsByEngine.llamacpp` (all ids) and `loadedByEngine.llamacpp` (loaded ids). Peers read this from engine-manager `em` `/v1/models`, not from mDNS.

**Local inference**

```
App → http://127.0.0.1:8084/v1/chat/completions
        → llamacpp-proxy
            → this node has id loaded? → 127.0.0.1:{adoptPort}
            → a paired node has it loaded? → that node's PAIR :8084 (mTLS)
            → nobody has it loaded? → 502, never forwarded, never loaded
```

`GET /v1/models` on 8084 returns the merged catalog across the cluster. Completions still require the id to be **loaded on the chosen node**.

**Cluster (Mac, laptops, this PC)**

Install **this fork** of PAIR on each machine (stock NVIDIA 0.1.1 has no `llamacpp` engine). Pair with the PIN on the same LAN. On each machine, run `llama-server` yourself and set that node's adopt port if it is not 8082.

A peer sends the OpenAI request over cluster mTLS to the chosen node's advertised 8084. Ingress on that node forwards only to its local llama-server. It does not hop again. Scheduler ranks `llamacpp` using each node's **loaded** inventory.

This Windows box can be a **client only** while ComfyUI has the GPU: local 8082 down or unloaded means this node is not eligible, but 8084 still forwards to a Mac or laptop that has the model loaded.

**ComfyUI holding the local GPU**

Local 8082 is down, or up with an empty loaded set. This node is not advertised as a llama.cpp server (or is advertised with an empty loaded set). Completions for a model loaded on another paired node go there. Completions for a model loaded nowhere are 502. Hermes and the playground Web UI remain the only **local** loaders, and only after ComfyUI is stopped.

**Shutdown**

PAIR unregisters `lc` and clears the proxy backend. It does not stop llama-server.

## Error handling

| Condition | PAIR behavior |
|---|---|
| Local adopt port down | This node is not a llama.cpp server (`lc` unregistered). Proxy stays up. Route to paired nodes that have the id loaded. 502 only if none do. No spawn, no playground script, no install |
| Local up, nothing loaded here | This node is not eligible. Route to a peer with the id loaded. 502 only if none do. No local load |
| Requested id loaded on no node | 502. Never forwarded. Never loaded |
| Stop/restart on foreign listener | Declined. Desired-off may still persist so PAIR does not re-advertise this node; llama-server keeps running |
| Adopt port change | Persist and probe the new port. Do not rebind llama-server |
| 8084 bind failure | Existing bind-failed error. PAIR does not bind the adopt port |
| Upstream 5xx or drop mid-stream | LM Studio-style failover among nodes with the id loaded. If none, 502. No local reload |
| Health flap | Failed probe unregisters `lc` and clears local backend. Next 5s poll re-adopts if the adopt port answers `/v1/models` |
| Logs | Engine, model id, node id, job id only. No prompts, messages, or response bodies |

## Testing

No live GGUF loads. ComfyUI keeps the GPU. Tests use a fake OpenAI `/v1` listener on a free loopback port, never 8082 and never the ISTA Qwen3.8 file.

**Engine-manager**

- Fake `/v1/models` 200 → Start adopts, no child process
- Probe down → not running, Start errors, no spawn
- `list_models` returns every `id`
- `loaded_models` returns only `status.value == "loaded"`
- Missing `status` is treated as unloaded
- Stop against a foreign listener is declined

**llamacpp-proxy**

- `GET /v1/models` merges the full catalog
- Chat for a loaded id is forwarded to the fake backend
- Chat for a catalog-only id is 502 and the fake backend sees **zero** requests
- Failover only considers peers that advertise the id as loaded
- CORS and loopback-plaintext rules match LM Studio

**Broker**

- Healthy engine + proxy up → register `lc` with 8084, local backend = adopt port
- Engine down → unregister `lc`, clear backend; proxy still routes to peers
- Equal proxy/engine ports are refused
- Adopt-port override is probed without spawning or rebinding
- Unloaded locally + loaded on a fake peer → request goes to the peer, not 502

**Desktop**

- `EngineTypes` includes `llamacpp`
- Capabilities: no install, delete, load, or eject
- Endpoint copy is `http://127.0.0.1:8084/v1`

**Commands**

- `go test` in `nvpair-engine-manager`, `llamacpp-proxy`, `nvpair-ui-broker`
- Desktop `npm run test:unit` for engine-type tests
- `npm run service-contracts:check` after JSON-RPC/docs updates

**Not in this work**

Real `llama-server`, ISTA Qwen3.8, Playground launchers, PAIR installer, any VRAM-using load.

## Out of scope

- Installing or compiling llama.cpp
- PAIR starting or stopping playground launchers (`start-llamacpp.ps1`, `connect-dsh.ps1`, …)
- Detecting ComfyUI
- Treating Bonsai 8080 or ik_llama 8083 on **this PC** as extra PAIR engines (a Mac's stock llama-server on 8080 is in scope as that node's adopt port)
- Running stock NVIDIA PAIR 0.1.1 on the Mac/laptops and expecting llama.cpp routing (those machines need this fork)
- Native llama.cpp `/completion` (non-OpenAI)
- PAIR-driven GGUF pull, load, unload, or delete
- Running `NVPAIR-Setup-0.1.1-x64.exe` until the user gives a green light
- Live inference against the 27B GGUF while ComfyUI holds the GPU

## Success criteria

1. With the local adopt port down, PAIR shows llama.cpp as not running on this node and never starts a process. The local 8084 proxy still accepts requests.
2. With a fake loaded model on a peer and nothing loaded locally, PAIR forwards OpenAI chat to the peer, not 502.
3. With a catalog-only (unloaded) id on every node, PAIR returns 502 and no fake backend receives a request.
4. Hermes / dsh on 8082 are untouched: PAIR does not bind 8082 and does not kill llama-server on shutdown.
5. Ollama and LM Studio paths still compile and their existing tests pass.
