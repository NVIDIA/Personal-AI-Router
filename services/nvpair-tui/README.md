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
| **Proxies** | Ollama, LM Studio, llama.cpp and vLLM reverse proxies: status, discovered upstreams, select a node (`enter`/`a`), set the listen port (`p`). |
| **Workloads** | Live cluster workload table for every engine, keyed by origin/engine/run/id. No workload cancellation key or selected-row detail pane. |
| **Engines** | Install (`i`), start (`s`), stop (`x`), restart (`r`), uninstall (`u`); model inventory (`m`), pull (`p`), load (`L`), unload (`e`), delete (`d`), cancel pull (`c`), local GGUF import (`I`). Managed vLLM adds update (`U`) and selecting the exact retained model it serves while stopped (`M`); its pulls take exact `owner/repository@commit` revisions. Managed llama has no update action, and the TUI never substitutes uninstall plus reinstall for one. |
| **Serving** | Managed vLLM groups: create a fresh review (`v`), check participants (`c`), start (`s`), stop (`x`), reconcile (`R`), request exact retained cleanup (`u`), and refresh proof (`r`). Every effect stays bound to the backend-reported review or run. |
| **Setup** | End-to-end device setup plus physical-cable and temporary-fabric workflows. It shows exact candidate, SSH fingerprint, review, operation, hold, port, principal, cleanup, and route bindings; credentials are masked, transient, and never rendered. |
| **Setup history** | Read-only retained onboarding journal inventory. Exact terminal pre-retention records are labelled `history-only`; the tab never exposes retry, access, approval, cleanup, or mutation actions. |
| **Cluster** | Pairing + membership: invite by address (`i`, shows the six-digit PIN — the first invite auto-founds a cluster of one), accept (`a`) / decline (`d`) an inbound invite, remove a member (`r`), leave (`L`). |
| **Manual** | User-added nodes: add by address (`a`), remove (`r`). |
| **Settings** | The node-settings store (force-ports, cluster auto-sync, cluster id/name). |
| **Logs** | The broker's (and workers') stderr, with live log-level control (`d`/`i`/`w`/`e`). |

Retained MPI cleanup reconciliation is currently an explicit Desktop Setup
recovery action. The TUI has no diagnostic-MPI surface, so it does not expose a
second or generic RPC relay for `engine:diagnostic-mpi-reconcile`.

When managed vLLM is unavailable on the current platform, the Engines tab
prints Engine Manager's exact prerequisite/refusal reason instead of offering
an install key.

## Managed vLLM serving workflow

The **Serving** tab is a client of Engine Manager's retained group owner; it
does not build plans or run rank commands itself:

1. Press `v`, enter two or three paired node IDs with the coordinator first,
   enter one exact retained model ID, then choose `tensor`, `pipeline`, or a
   blank PAIR default. The backend returns an expiring immutable review.
2. Press `c` to check every reviewed participant. This step has no effects and
   accepts no administrator input.
3. Press `s` only after the review and participant check both say activation is
   available. The TUI prompts once for each reviewed participant, in review
   order. Input is masked; a blank entry explicitly selects passwordless sudo
   for that participant.
4. Press `r` to refresh retained proof. Start consumes the review before the
   request is sent, so a lost response is reconciled from status and is never
   automatically resent.
5. Press `x` to stop the exact retained run. If an attempted rank remains
   unresolved, press `R` and provide fresh one-use administrator input only for
   the participants the retained run still names. `u` requests the backend's
   exact cleanup-only path; it cannot select a different run, node, or command.

The input value is masked. On submission its bytes enter a mutable buffer for
that one Start or Reconcile request; the TUI clears that buffer on success,
failure, cancellation, or state change and never renders or retains the secret
value. If the review expires or the run generation changes, the TUI discards
the pending administrator input and requires fresh state.

The direct two-node tensor plan is `qualified-direct-socket`: NCCL Socket uses
the reviewed QSFP Ethernet lane while control remains on the management
network, with RDMA disabled. A three-node ordinary pipeline group still uses
the management network even if a ring is active. The separate fixed Qwen3.8
profile is the only current ring-RoCE payload contract; the UI must not infer
that contract from cabling or an active fabric operation alone.

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

## Linux background parent and private setup control

The default remains the interactive terminal interface. Linux also provides a
noninteractive parent using the same `Spawn`/`Supervisor`/RPC implementation:

| Mode | Contract |
| --- | --- |
| `--headless` | Foreground parent for one broker/worker tree; normal signals request ordered shutdown. |
| `--headless-service install\|start\|stop\|status\|uninstall` | Fixed owning-account `nvidia-pair-headless.service`, managed through user systemd; refuses a different executable/configuration owner. |
| `--headless-service upgrade` | Internal reviewed device-setup helper with bounded private JSON stdin. Stops, conditionally replaces, or restores only the exact reviewed old unit; it is not a force-install or general command mode. |
| `--startup-lifetime persistent` | Default service lifetime; requires an active user manager and confirmed background-after-logout configuration. It never silently falls back or invokes `sudo`. |
| `--startup-lifetime session` | Explicit limited lifetime while the account's user session is available. It is not logout/reboot persistence. |
| `--control` | One bounded request from non-terminal stdin and one response to non-terminal stdout, through private same-account local control. It does not start a second broker. |

The private socket lives in the owning account's runtime directory under
`nvidia-pair/control.sock`. Directory/socket permissions and peer UID are checked
on both ends. The control allowlist is `headless:status`, `cluster:get-node-id`,
`nodes:get-initial`, `cluster:invite-node`, `cluster:respond-to-invite`,
`cluster:invite-status`, and `cluster:cancel-invite`. It is not a shell, engine
control endpoint, or network auto-accept service. Windows/macOS reject these new
modes explicitly; their ordinary terminal interface remains unchanged.

Control input is `{ "method": "...", "params": { ... } }`. Output contains
`result` or a nonsecret `error` object (`code`, `message`, optional `rpcCode`).
Requests/responses are bounded and have deadlines. `completion-unknown` means
the caller must reconcile the operation before retrying; closing a connection
does not roll back an accepted pairing. Pairing material belongs only in this
private programmatic channel, never command arguments, terminal transcripts, or
logs. Headless mode drains raw broker streams without writing their frames to
its log sink.

Service installation does not itself pair a device or prove all workers ready.
Persistent startup prerequisites and any authorized configuration change belong
to the product setup review; this command does not grant itself privilege. The
current unit allows 145 seconds for the bounded parent drain. The upgrade helper
recognizes the complete prior 25-second unit and publishes its reviewed longer
drain definition before stopping it. Device setup owns the package, identity,
rollback/retry, and rejoin journal; this helper changes only its fixed unit and
leaves startup enablement, profile, and model directories intact.

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
