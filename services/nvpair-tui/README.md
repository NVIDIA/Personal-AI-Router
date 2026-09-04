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

While it runs it also serves a **local control socket**, so pairing can be
driven from a script instead of a keyboard — see
[Pairing without a keyboard](#pairing-without-a-keyboard).

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
| **Proxies** | Ollama and LM Studio reverse proxies: status, discovered upstreams, select a node (`enter`/`a`), set the listen port (`p`). |
| **Workloads** | Live cluster workloads (`workloads:subscribe` / `workloads:upsert` / `workloads:remove`). |
| **Engines** | Local inference engines: install (`i`), start (`s`), stop (`x`), restart (`r`), uninstall (`u`). |
| **Cluster** | Pairing + membership: invite by address (`i`, shows the six-digit PIN — the first invite auto-founds a cluster of one), accept (`a`) / decline (`d`) an inbound invite, remove a member (`r`), leave (`L`). |
| **Manual** | User-added nodes: add by address (`a`), remove (`r`). |
| **Settings** | The node-settings store (force-ports, cluster auto-sync, cluster id/name). |
| **Logs** | The broker's (and workers') stderr, with live log-level control (`d`/`i`/`w`/`e`). |

## Keys

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
nvpair-tui --control-socket /run/pair.sock   # serve the endpoint somewhere else
nvpair-tui --no-control-socket               # serve no endpoint at all
nvpair-tui --version
```

Logging goes to stderr (the broker's logs are shown inside the **Logs**
tab, not on the terminal), so it never corrupts the full-screen UI.

## Pairing without a keyboard

A running `nvpair-tui` listens on a per-user JSON-RPC control socket, and the
same binary invoked with a subcommand connects to it. That is how an operator
pairs a headless box over SSH, or from a script, without pressing keys in the
Cluster tab.

The subcommands reach a running **`nvpair-tui`**, not the desktop application's
broker. On a machine where only the desktop application is running, they report
that nothing is listening — which is correct: the two must never run at once.

### Subcommands

```sh
nvpair-tui invite <address> [--port N] [--wait]   # prints the PIN on stdout
nvpair-tui pending                                # inbound invitations
nvpair-tui accept --pin 123456 [--invite <id>]    # answer one
nvpair-tui accept --pin 123456 --wait 2m          # wait for one, then answer it
nvpair-tui decline [--invite <id>]
nvpair-tui members                                # cluster id and members
```

Every subcommand takes `--json`, which prints the endpoint's raw result instead
of a human line, and `--control-socket <path>` to reach an instance started
with a non-default endpoint. `invite --wait --json` prints two JSON objects,
one per line: the invite as created, which carries the PIN, and then its final
state (an invite in a terminal state carries no PIN, so one document cannot
serve both).

`--invite` is only needed when more than one invitation is waiting; with
exactly one, the endpoint resolves it, and with none or several it says so.

**Pass the PIN in `NVPAIR_PIN`, not `--pin`, in a script.** An argument is
visible to every process on the machine in `ps` and is written to shell
history; the environment form is neither. `--pin` exists for typing at an
interactive prompt.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | The pairing reached the state asked for: `paired` for `accept` and `invite --wait`, `declined` for `decline`. |
| `1` | The other side refused: an incorrect PIN, or a declined invitation. |
| `2` | Anything else: no running `nvpair-tui`, a bad argument, an unreachable peer, an expired or rejected invite, a transport failure. |

### The control socket

| | |
| --- | --- |
| **Path** | `$XDG_RUNTIME_DIR/nvpair/tui.sock` when the OS provides a runtime directory; otherwise `run/tui.sock` under the shared per-user data directory (`nvpair-shared/appdir`). On Windows, the named pipe `\\.\pipe\nvpair-tui-<sid>`. |
| **Protocol** | Newline-delimited JSON-RPC 2.0, the same as every other PAIR surface. Request/response only; the endpoint pushes nothing. |
| **Access** | The directory is `0700` and the socket `0600`, so the file permissions are the check. On Windows the pipe's default DACL grants the creating user. Anything that can open it can pair this machine, which is the authority the operator at the TUI already has. |
| **Clients** | Any number, served concurrently, each issuing any number of sequential requests. |
| **Overrides** | `--control-socket <path>`, or `--no-control-socket` to serve none. |

A socket file left behind by an instance that did not exit cleanly is removed
and reclaimed, but only after confirming nothing answers on it — a live socket
means another `nvpair-tui` is running, and this one refuses to steal it. If the
endpoint cannot be opened at all, `nvpair-tui` logs a warning and runs the
interactive UI anyway: the UI is its primary job.

On Unix the path has to fit the platform's `sun_path` limit (104 bytes on
macOS). A default path that would not fit is reported with that limit named,
rather than being silently truncated by the kernel; `--control-socket` with a
shorter path is the fix.

### Methods

Each is relayed through the TUI's one broker connection, so the broker keeps a
single client. Where the answer is a cluster-manager `Invite`, the manager's own
JSON is relayed untouched.

| Method | Params | Result |
| --- | --- | --- |
| `ping` | — | `{version, brokerReady}` |
| `pair:invite` | `{address, port?, nodeId?}` | the `Invite`, including its `pin` |
| `pair:invite-status` | `{inviteId}` | the current `Invite` |
| `pair:pending` | — | `{invites: [...]}` — inbound invitations not yet answered, each with the `pin` member removed and a `receivedAt` (this node's clock, epoch ms) added so a caller can age it |
| `pair:respond` | `{inviteId?, accept, pin?}` | the resulting `Invite` |
| `pair:members` | — | `{clusterId, clusterFriendlyName, nodeId, nodeUuid, name, members: [ClusterNode]}` |

`Invite` and `ClusterNode` are `nvpair-cluster-manager`'s own shapes; see its
[README](../nvpair-cluster-manager/README.md).

Errors relay the cluster manager's code and message verbatim, so `-32001`
(unknown invite) and `-32002` (invalid invite state) mean there what they mean
in the manager. Two codes originate here:

| Code | Meaning |
| --- | --- |
| `-32010` | `pair:respond` named no invite and none is waiting |
| `-32011` | `pair:respond` named no invite and several are waiting; `data.invites` lists them |

### What it does not do

**The pending set is session state.** `nvpair-cluster-manager` has no "list the
invitations you are holding" call, and `nodes:get-initial` reports a
`pending-inbound` peer without the invite id needed to answer it. So an
`nvpair-tui` restarted while an invitation was in flight reports nothing
pending even though the manager still holds a live invite; the inviting machine
has to send a new one. Nothing here reconstructs it.

**The PIN never reaches a log.** It goes to the terminal the operator asked for
and into the `pair:invite` result, and nowhere else: `pair:pending` strips it,
and no error message repeats one back.

## Architecture

```
nvpair-tui (this process)
├── supervisor.go      spawn/own nvpair-ui-broker over stdio, graceful teardown
├── cli.go             the subcommands: connect to a running instance's socket
├── rpc/               JSON-RPC 2.0 codec + id-matching client
├── pairing/           the one pairing implementation both drivers call
├── control/           the local control socket: path, listener, server, client
└── ui/                Bubble Tea root model + one file per tab
        │ stdio (newline-delimited JSON-RPC 2.0)
        ▼
   nvpair-ui-broker ──► nvpair-node-scanner, ollama-proxy, nvpair-errors, ... (workers)
```

The supervisor sends `shutdown` and closes the broker's stdin on exit; the
broker tears its own workers down, so quitting leaves no orphans.

`pairing.Service` is deliberately the only place pairing happens. The Cluster
tab and the control socket both call it, so they cannot disagree about which
invitation is waiting: an invite created from a script shows its PIN on the
tab's status line exactly as one created with `i`, and an accept made from a
script clears a PIN prompt the tab left open. Broker notifications are fanned
out in `main.go` — to the service first, then the UI — so the service sees an
invitation arrive whether or not the UI is keeping up.

## Build & test

Built by the repo's top-level `build.bat` / `build.sh` (stamped via
`-X main.Version` from `versions.json`) and staged in `build/bin/` alongside the
other binaries. Standalone:

```sh
cd nvpair-tui
go build ./...
go test ./...
```
