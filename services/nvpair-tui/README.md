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

The tab set is machine-first: a node is the unit an operator reasons about, so
everything specific to one machine hangs off its row rather than living in a tab
of its own.

| Tab | Purpose |
| --- | --- |
| **Nodes** | Every machine PAIR knows about — discovered, added by hand, or paired into the cluster — merged into one table. Reachability (`STATUS`) and membership (`CLUSTER`) are separate columns because they are independent facts. `enter` opens the node's detail screen; `p` pairs, `n` pairs by address, `f` finds by address, `c` cancels a pairing request you sent, `r` removes — un-pairing a member asks you to confirm, dropping a hand-added entry does not — `a`/`d` answer an inbound pairing request, `l` leaves the cluster (with a confirmation), `/` filters by name or address. The keys follow the words on screen: everything here is "pair", so `p` starts one and `a` accepts one. A filtered table says so, and the cluster summary still counts every node rather than the visible ones. |
| **Jobs** | Inference work across the cluster (`workloads:get-initial` plus the live `workloads:upsert` / `workloads:remove` stream), headed by the proxy endpoints local clients connect to. `FROM` and `RAN ON` are the job's `originatedFrom` and `scheduledOn` nodes. `a` toggles finished work. `t` starts or stops the Inference Demo — a sixty-second burst of synthetic traffic through those endpoints, which is why it lives here rather than with the service controls: the ports it needs are already on this tab and the jobs it produces land in the table below. |
| **Service** | Broker version and uptime (`ping`), a row per supervised worker derived from `supervisor:subprocess-crashed:*` errors, the cluster name, the fleet log level (a picker over the four `applog` levels), and a confirmed data reset. Ports are not here — they live on the node detail screen beside the engine each one serves. `force-ports` and `cluster-auto-sync` are persisted by `nvpair-node-settings` but not offered: nothing currently acts on either. |
| **Errors** | The service-error datastore (`errors:get-initial` plus live `errors:update`); `c` clears the selected entry. Only entries this node reported are clearable: `errors:clear` is delete-by-id on the receiving node and cross-node propagation is unbuilt (`shared/errors` stamps `ClearedBy` for it and ignores it), so clearing a peer's entry is reverted by the next sync. The broker acknowledges the relay rather than the outcome, so the reply cannot be used to detect it — the key is withdrawn for a peer's entry instead, naming the node to clear it from. Node ids are resolved to names, and a line under the table carries the selected entry's engine, operation, model, and suggested action. |
| **Logs** | The broker's and workers' stderr, with a substring filter (`/`), a follow toggle (`t`, for tail — `f` belongs to the viewport's paging), and save-to-file (`s`). |

Diagnostics come last, errors before logs, which is the order you consult them
in. The **Errors** tab carries its active count in its own label (`Errors (2)`),
so the tab bar is the indicator and nothing extra has to be learned to notice a
problem from another tab.

Errors were briefly an overlay on a dedicated key instead. That needed a global
binding, and every candidate was either a letter that shadowed a view's own verb
or a digit that looked like a tab number without being one — so it became the
tab it was already pretending to be.

### Update notice

A newer published release is announced in a row under the tab bar, checked
shortly after startup and every six hours against the public releases feed.

It belongs to the shell rather than to the **Service** tab: the operator this is
for is the one who lives on **Nodes** or **Jobs** and has no reason to open
**Service**. `ctrl+x` dismisses it on every tab at once, and a release newer
than the dismissed one brings it back — that version was never acknowledged.
The key is `ctrl+x` because the views between them bind `a` through `y` and the
table and viewport add the paging keys; the shell handles its own bindings
before the active view sees them, so a global letter would silently shadow a
verb.

Two layout constraints, both regression-tested. The row comes out of
`contentHeight()`, or it is a row the shell then deletes from the bottom of
whichever view is showing — which is where every view keeps its messages. And
the line is assembled longest-first against the real width, dropping the URL and
then the version detail, because the frame is clamped to the terminal and the
rightmost text is the dismiss hint: the only key that closes it.

It compares `ui.ReleaseVersion`, stamped by both build paths from
`desktop/package.json`, against the feed's latest stable tag. That is the
release number users install and the one the tags are named for — this
component's own version and the services suite version describe parts of the
build and mean nothing to the comparison. Drafts and prereleases are ignored.

Awareness only: nothing is downloaded or installed, because this client resolves
the broker beside its own executable and that broker spawns the worker set from
the same directory — replacing "the client" means swapping every binary
atomically while they serve inference, and a partial swap leaves a new client
driving old workers across a JSON-RPC contract that may have changed. Silent on
failure, skipped for an unstamped build, and disabled by
`NVPAIR_NO_UPDATE_CHECK`. A desktop-app install needs none of this: `nvpair-tui`
ships in `cli-bin` and the app's updater replaces it.

### Node detail

`enter` on a node opens a full-screen drill-down with two panes, switched with
`h`/`l`:

- **Engines** — install (`i`), start (`s`), stop (`x`), and, on this machine
  only, restart (`r`), uninstall (`u`, confirmed with `y`), the engine's own port (`e`, via
  `engine:set-port`), and the client-facing port of the proxy fronting it
  (`p`, via `<prefix>:set-port`). Both ports are shown per engine because they
  are easily confused and were previously configured on different tabs.
- **Models** — the inventory per engine with loaded state, plus browse-and-download
  (`p`), download by name (`n`), load (`enter`), eject (`e`), and delete (`d`,
  confirmed with `y`). Every destructive key arms on the first press and acts
  only on `y`, against the target captured at arm time — these lists re-sort
  under the cursor whenever a download finishes or a peer republishes.

`p` opens a catalog browser over the engine's downloadable models, served by the
backend's `engine:catalog`. Filtering (`/`) and sorting (`o`) are local over the
whole fetched list, because the catalog is thousands of entries for Ollama and
there is no server-side search.

Both panes work on remote cluster peers through the engine manager's
`engine:remote-*` methods. Restart, uninstall, and the port change need process
ownership on the target host, so they are hidden on a peer rather than offered
and then failed.

A remote node's models come from the discovery snapshot, which the broker
enriches from each peer's engine manager — no extra request. This machine's come
from `engine:models` and stay live through `engine:models-changed`.

The detail screen also polls the node's own `/v1/node-info` endpoint over HTTP
for GPU, CPU, and memory. That is the one reading the broker's JSON-RPC surface
does not carry, and only the open node is polled.

## Keys

- `tab` / `shift+tab` or the digits `1`-`5` — switch tabs
- `?` — full help
- `ctrl+x` — dismiss the update notice, while one is showing
- `q` / `ctrl+c` — quit (the broker is shut down cleanly on exit)
- Per-tab keys appear in the footer. While editing a field (port, PIN, address,
  model name) every key goes to the field until `enter` or `esc`.

`h` / `l` and the arrows are deliberately **not** bound to tab switching: they
move within content, and the node detail screen needs them for its panes.

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
└── ui/                Bubble Tea root model, one file per tab, plus:
    ├── table.go       shared column layout (accounts for bubbles' cell padding)
    ├── toast.go       transient status lines that expire on their own
    ├── nodesmodel.go  merges the discovery, cluster, and manual feeds
    └── nodedetail.go  the per-node engines + models drill-down
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
