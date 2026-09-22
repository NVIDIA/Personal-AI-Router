<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-tui

A terminal UI for running and supervising the NVPAIR fleet on a **headless
machine over SSH**, where the bundled graphical UI cannot run. That is its
purpose: it is an operations tool for hosts without a desktop, not a replacement
for the graphical UI, and it does not cover every operation the desktop does.

It spawns and owns its own `nvpair-ui-broker` child over stdio; the broker in turn
supervises the worker subprocesses, so `nvpair-tui` drives one host on its own.

This file is the component reference. For task-oriented usage instructions, see
[Using the PAIR terminal interface](../../docs/terminal-interface.mdx).

## What it does

`nvpair-tui` is a JSON-RPC 2.0 client of `nvpair-ui-broker` (newline-delimited
JSON over the broker's stdin/stdout). It launches the broker, consumes its
notification stream, and renders a tabbed, keyboard-driven dashboard built
with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

Tabs:

| Tab | Purpose |
| --- | --- |
| **Overview** | Broker liveness/version/uptime (`ping`) and a per-worker health table derived from the broker's `supervisor:subprocess-crashed:*` errors. |
| **Errors** | The service-error datastore (`errors:get-initial` + live `errors:update`); `c` clears the selected entry. |
| **Nodes** | mDNS-discovered Ollama nodes (`discovery:subscribe` / `discovery:nodes-changed`). |
| **Proxies** | Ollama, LM Studio and llama.cpp reverse proxies: status, upstream selection and listen port. |
| **Workloads** | Live cluster workload table for Ollama, LM Studio and llama.cpp, keyed by origin/engine/run/id. No workload cancellation key or selected-row detail pane. |
| **Engines** | Install (`i`), start (`s`), stop (`x`), restart (`r`), uninstall (`u`); model inventory (`m`), pull (`p`), load (`L`), unload (`e`), delete (`d`), cancel pull (`c`), local GGUF import (`I`). No engine update key: managed llama has no update action, and the TUI never substitutes uninstall plus reinstall for one. |
| **Cluster** | Pairing + membership: invite by address (`i`, shows the six-digit PIN — the first invite auto-founds a cluster of one), accept (`a`) / decline (`d`) an inbound invite, remove a member (`r`), leave (`L`). |
| **Manual** | User-added nodes: add by address (`a`), remove (`r`). |
| **Settings** | The node-settings store (force-ports, cluster auto-sync, cluster id/name). |
| **Logs** | The broker's (and workers') stderr, with live log-level control (`d`/`i`/`w`/`e`). |

## Keys

The model view uses arrow keys to scroll and `Esc` to return. Model actions
prefill the selected exact model ID and require Enter. llama downloads accept
`owner/repository:QUANT`; imports take a local GGUF path. Install support and
external ownership come from Engine Manager. Downloaded models are not loaded
until the runtime reports them resident.

The workload table consumes live `workloads:upsert` and `workloads:remove`
events after subscribing. It does not fetch a startup snapshot: requests already
in flight appear on their next event. Arrow keys scroll the table; `c` remains
the cancel-pull command in the Engines tab, not a workload action.

- `tab` / `shift+tab` (or `→` / `←`, `l` / `h`) — switch tabs
- `?` — toggle full help
- `q` / `ctrl+c` — quit (the broker is shut down cleanly on exit)
- Per-tab keys appear in the footer; while editing a field (port, PIN,
  address, setting) all keys go to the field until you press `enter` or
  `esc`.

## Running

`nvpair-tui` resolves `nvpair-ui-broker` next to its own executable (the
installed `bin/` layout). Override with `--broker-path`:

```sh
nvpair-tui                                   # broker is a sibling binary
nvpair-tui --broker-path /opt/nvpair/bin/nvpair-ui-broker
nvpair-tui --log-level debug                 # own logging (to stderr)
nvpair-tui --version
```

Logging goes to stderr (the broker's logs are shown inside the **Logs**
tab, not on the terminal), so it never corrupts the full-screen UI.

## Architecture

```
nvpair-tui (this process)
├── supervisor.go      spawn/own nvpair-ui-broker over stdio, graceful teardown
├── rpc/               JSON-RPC 2.0 codec + id-matching client
└── ui/                Bubble Tea root model + one file per tab
        │ stdio (newline-delimited JSON-RPC 2.0)
        ▼
   nvpair-ui-broker ──► nvpair-node-scanner, nvpair-proxy, nvpair-errors, ... (workers)
```

The supervisor sends `shutdown` and closes the broker's stdin on exit; the
broker tears its own workers down, so quitting leaves no orphans.

## Build & test

Built by the repo's top-level `build.bat` / `build.sh` (stamped via
`-X main.Version` from `versions.json`) and staged in `build/bin/` alongside the
other binaries. Standalone:

```sh
cd nvpair-tui
go build ./...
go test ./...
```
