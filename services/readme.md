<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA Personal AI Router — backend services

These are the Go services behind PAIR. They **discover other PAIR nodes** on the
local network: each node advertises itself over mDNS as one consolidated
`_nvpair-node._tcp` record, and peers learn from that record which services a
node offers and where to reach them.

What a discovered node can actually serve is a separate question, answered after
discovery. A node may be running [Ollama](https://ollama.com/), LM Studio, both,
or neither, and its model inventory is fetched over HTTP from its engine-manager
rather than crammed into mDNS TXT records, which are too small to carry it.

Locally, each node exposes compatibility proxies — Ollama-compatible and
OpenAI-compatible — so an unmodified client on that machine can reach any capable
node in the cluster. Routing is automatic: the proxy picks a node that is online,
has a compatible engine running, and holds the requested model.

Standalone, the Ollama proxy defaults to `http://localhost:11435`; under the
broker it serves the Ollama-compatible `http://localhost:11434` when that port is
free. If an inherited `OLLAMA_HOST` names a different local HTTP port, the broker
can serve that loopback-only address from the same router when it is free —
`localhost` is claimed on IPv4 and IPv6 together, and remote or HTTPS targets are
never intercepted.

PAIR ships a graphical UI alongside these services. The UI launches
**`nvpair-ui-broker`** from the same directory; the broker orchestrates the
workers and exposes a newline-delimited JSON-RPC 2.0 API over stdio (or a Unix
socket / Windows named pipe via `--ipc`). For supported platforms and engines,
see the [root README](../README.md#what-is-supported).

## Architecture

This tree builds thirteen Go binaries. `nvpair-ui-broker` is the parent service and supervises the eleven workers, all spawned at startup — only the scanner is required, and a missing binary for any other leaves the broker running without that capability. `nvpair-tui` is the thirteenth: a terminal client that launches and supervises its own broker rather than being supervised. Processes communicate via newline-delimited JSON-RPC 2.0 over stdio or, optionally, a Unix socket / Windows named pipe.

| Binary | Role |
| --- | --- |
| `nvpair-ui-broker` | Parent service and JSON-RPC API surface used by the bundled UI and other clients. Supervises workers, relays consolidated discovery, and coordinates routing and scheduling. |
| `ollama-proxy` | Ollama-compatible HTTP reverse proxy. Routes only to advertised model owners, with owner failover and scheduler priorities. |
| `lmstudio-proxy` | LM Studio counterpart to `ollama-proxy`, forwarding OpenAI-compatible inference routes with equivalent owner-only routing and failover behavior. |
| `nvpair-node-info` | Local HTTP service on `:14318` exposing GPU, CPU, and memory inventory at `/v1/node-info`. |
| `nvpair-node-scanner` | Consolidated discovery daemon. Advertises and browses `_nvpair-node._tcp`, maintains the node directory, and enriches peers with hardware and model information over HTTP. |
| `nvpair-manual-nodes` | Manages user-added nodes that don't appear via mDNS; probes them every 10 s. |
| `nvpair-workload-manager` | Cluster-wide workload relay. |
| `nvpair-errors` | Service-errors datastore + cross-node sync. |
| `nvpair-engine-manager` | Config-driven control plane for local inference engines, model inventory, and trusted remote engine operations. |
| `nvpair-node-settings` | Typed key-value store for per-node preferences. |
| `nvpair-cluster-manager` | Node identity, PIN pairing, and the trusted-node store. |
| `nvpair-job-scheduler` | Responsive scheduler combining total node queue depth across engines with smoothed GPU pressure. |
| `nvpair-tui` | Terminal interface for headless and SSH operation; launches and supervises its own broker, and serves a per-user control socket so pairing can also be driven by `nvpair-tui invite` / `accept` from a script. |

Shared code lives in the local `shared/` Go module (imported as `nvpair-shared/…`, replaced via `replace nvpair-shared => ../shared`). It provides logging, wire types, JSON-RPC and IPC, discovery records, mDNS, network monitoring, stable node identity, application data paths, and cluster trust helpers.

The mDNS responder is our own rather than the host's, because Windows ships none. It sets `SO_REUSEADDR` so it shares UDP 5353 with sibling PAIR processes and with a system responder — `avahi-daemon` on Linux, Bonjour where present — needing no configuration on either platform.

The broker feeds every accepted local or peer workload transition plus compact
GPU telemetry to the scheduler. Queued and running work is counted by destination
node across Ollama and LM Studio together. Fresh maximum-GPU utilization is
smoothed into pressure 0–3; missing or stale telemetry is neutral. Rankings use
`pending + gpuPressure`, and each proxy adds local reservations before choosing,
so bursts spread without waiting for workload feedback.

## Repository layout

```
nvpair-ui-broker/        Parent service / JSON-RPC API surface
ollama-proxy/            Ollama-compatible routing proxy
lmstudio-proxy/          OpenAI-compatible routing proxy for LM Studio
nvpair-node-info/        Local GPU-inventory HTTP service
nvpair-node-scanner/     Consolidated _nvpair-node._tcp discovery daemon
nvpair-manual-nodes/     Manual-node manager
nvpair-workload-manager/ Cluster workload relay
nvpair-errors/           Service-errors datastore + sync
nvpair-engine-manager/   Local and remote inference-engine control plane
nvpair-node-settings/    Per-node preferences store
nvpair-cluster-manager/  Node pairing / trust service
nvpair-job-scheduler/    Cluster job scheduler
nvpair-tui/              Terminal interface for headless / SSH operation
shared/                   Shared Go module (nvpair-shared/…)
eap-noob/                 EAP-NOOB implementation used by cluster pairing
tests/                    Cross-process integration tests (separate go.mod)
versions.json             Single source of truth for every component version
build.bat                 Builds all thirteen binaries (Windows)
build.sh                  Builds all thirteen binaries (Linux)
VERSIONING.md             SemVer rules and version-bump workflow
```

Every directory above is its own Go module and holds its own tests beside the
source it covers. See [Testing](#testing).

## Requirements

- [Go](https://go.dev/dl/) 1.25 or newer.
- [`jq`](https://jqlang.org/) on `PATH` — the build scripts use it to parse `versions.json`. Install via `winget install jqlang.jq` / `choco install jq` / `scoop install jq` on Windows, `sudo apt install jq` on Debian/Ubuntu, `sudo dnf install jq` on Fedora/RHEL, or `brew install jq` on macOS.
- No C toolchain, GUI, or webkit dependencies. Every module is pure Go (`cgo` is not used).

## Building

Always use the platform-appropriate build script, run from this `services/`
directory.

On Windows:

```powershell
.\build.bat
```

On Linux and macOS:

```bash
./build.sh
```

Both scripts read `versions.json`, build all thirteen Go binaries with `-X main.Version=…` ldflags, and stage them together in `services/build/bin/`.

Do **not** build individual components by hand without also copying their binaries into `build/bin/`: the broker will silently keep using the older binary there.

The staged binaries in `build/bin/` are what you run locally. Installable builds
come from the
[releases page](https://github.com/NVIDIA/Personal-AI-Router/releases).

## Running

Normal installations launch the bundled UI, which starts the broker from the same
installation directory.

To drive a services-only build interactively, run `nvpair-tui` — it launches and
supervises its own broker, so nothing else needs to be running. See
[Using the PAIR terminal interface](../docs/terminal-interface.mdx).

For backend development or direct API access, launch the broker yourself; it
supervises workers that it expects as siblings in the same `build/bin/`
directory:

```powershell
.\build\bin\nvpair-ui-broker.exe                                              # Windows
```

```bash
./build/bin/nvpair-ui-broker                                                  # Linux
```

The bundled UI connects to the broker over its JSON-RPC API (stdio or `--ipc`).
Other clients can use the same contract. See `nvpair-ui-broker/README.md` for
the API surface.

## Debugging

Every binary respects two inputs for its starting log level, resolved before any other work happens. Precedence: `--log-level` flag > `NVPAIR_LOG_LEVEL` env var > default (`info`). The broker passes its own resolved level to every subprocess it spawns (`--log-level <level>`), so setting either before launch turns on debug output from the very first line of all processes.

```powershell
# Windows / PowerShell
$env:NVPAIR_LOG_LEVEL = "debug"
& ".\build\bin\nvpair-ui-broker.exe"
& ".\build\bin\nvpair-ui-broker.exe" --log-level debug
```

```bash
# Linux
NVPAIR_LOG_LEVEL=debug ./build/bin/nvpair-ui-broker
./build/bin/nvpair-ui-broker --log-level debug
```

Accepted values: `debug`, `info`, `warn`, `error`. The level can also be changed live over the broker's `log/set-level` JSON-RPC method (not persisted across restarts), so for launch-time issues use the env var or the flag.

## Testing

There are two layers, and while you are editing a component you want the first
one.

### Per-component tests

Every component keeps its tests beside its source, and each is its own Go module,
so run them from that component's directory:

```bash
cd nvpair-engine-manager
go test ./...
```

These are fast and are what you should run on each edit. Substitute any component
directory — `shared/` and `eap-noob/` have their own tests too:

```bash
cd shared
go test ./...
```

**Every one of the thirteen binaries has tests**, as do `shared/` and
`eap-noob/`. Depth varies with how much behaviour a component carries:
`nvpair-engine-manager` and `nvpair-cluster-manager` have the largest suites,
while a component with one test file may still hold twenty test functions in it.
File count is a poor proxy for coverage, so read the suite rather than counting
files.

### Cross-process tests

`tests/` has its own `go.mod` and exercises real binaries talking to each other.
Its `TestMain` builds the workers it needs into a temp directory first, so you do
not have to build beforehand:

```bash
cd tests
go test ./...
```

Expect this to take a few minutes. Run it before opening a merge request, and
after any change to a JSON-RPC method, payload, or notification.

### Tests that skip themselves

Some tests need something the machine may not have, and skip rather than fail:

- Tests that bind a well-known port (`11434`, `14319`, `14321`) skip when it is
  already in use — so quit PAIR before running them, or expect gaps.
- Tests needing a real mDNS responder skip when one is unavailable.
- Live engine tests only run when you opt in:

```bash
NVPAIR_LIVE_OLLAMA=1 go test ./...        # drives a real Ollama install
NVPAIR_LIVE_LMSTUDIO=1 go test ./...      # drives a real LM Studio install
```

Those two install, start, stop, and pull against the engines actually on your
machine. Run them deliberately, on a machine you do not mind changing.

A skip is not a pass. If you are relying on a test, check it actually ran.

## Versioning

`services/versions.json` is the single source of truth for every component version and the umbrella `product` version. The build scripts read it and stamp each binary via `-ldflags "-X main.Version=..."`. Bump the components your change affects in the same pull request, and describe any user-facing change in the pull-request description so it reaches the release notes. [`VERSIONING.md`](VERSIONING.md) has the bump rules.

You can verify a built binary's stamped version at any time:

```powershell
.\build\bin\ollama-proxy.exe --version                                     # Windows
```

```bash
./build/bin/ollama-proxy --version                                         # Linux
```

## Further reading

- `VERSIONING.md` — SemVer bump rules and how a release is declared.
- `bom.md` — bill of materials for a given release.
- Per-component `README.md` files in each subdirectory.
- [Architecture](../docs/architecture.mdx) — how these services fit together at
  runtime, including node-to-node transport and discovery.
