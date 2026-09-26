<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA Personal AI Router — Linux install

This tarball is the portable "extract anywhere" backend bundle. The shipped
distribution places its graphical UI alongside this bundle in the same
installation directory. The UI launches `nvpair-ui-broker`, which drives the
API and supervises the workers under `bin/`.

## 1. Extract

```bash
tar xf NVIDIA-Personal-AI-Router-<version>-linux-amd64.tar.gz
cd NVIDIA-Personal-AI-Router-<version>
```

You should end up with this layout:

```
NVIDIA-Personal-AI-Router-<version>/
├── bin/
│   ├── nvpair-ui-broker              # primary entry point (JSON-RPC over stdio / IPC)
│   ├── nvpair-proxy
│   ├── nvpair-node-info
│   ├── nvpair-node-scanner
│   ├── nvpair-manual-nodes
│   ├── nvpair-workload-manager
│   ├── nvpair-errors
│   ├── nvpair-engine-manager
│   ├── nvpair-node-settings
│   ├── nvpair-cluster-manager
│   ├── nvpair-job-scheduler
│   ├── nvpair-tui
│   ├── nvpair-host-bootstrap
│   └── nvpair-host-helper
├── installer-inputs/
│   ├── nvpair-host-helper.service
│   └── nvpair-host-helper.socket
└── INSTALL.md                     # this file
```

The helper unit/socket files are inert templates. Only the signed bootstrap may
render the reviewed account-bound values and install them under
`/etc/systemd/system`; extracting this archive never registers a service.

## 2. Run

The bundled UI normally launches the broker from the same installation
directory. For backend-only development or direct API access, run it yourself.
It supervises the other workers (which it expects as siblings in the same
`bin/` directory) and speaks newline-delimited JSON-RPC 2.0 over stdio (or a
Unix socket via `--ipc`):

```bash
./bin/nvpair-ui-broker
```

The bundled UI connects to the broker over this contract. Other clients can use
the same API; see the `nvpair-ui-broker` README for its JSON-RPC surface.

## 3. Uninstall

Since nothing was installed by a package manager, removal is just deleting the
extracted directory:

```bash
rm -rf /path/to/NVIDIA-Personal-AI-Router-<version>
```

The workers store configuration in `~/.config/Nvidia Corporation/Personal AI Router/`
(manual-node list, log-level preference, cluster identity/pins, etc.). Remove
that directory too if you want a fully clean slate.

## Requirements

- Debian or Ubuntu on 64-bit `x86_64`/`amd64` or `aarch64`/`arm64`.
- mDNS on UDP 5353. The workers run their own — a custom per-interface responder
  (`nvpair-shared/mdns`) plus a `grandcat/zeroconf` browser (`nvpair-shared/discovery`),
  bound with `SO_REUSEADDR` — and coexist fine with a system responder like
  `avahi-daemon`; no configuration needed.

## Troubleshooting

- **No nodes discovered**: confirm UDP 5353 isn't blocked by your firewall and
  that other machines on the LAN are actually advertising. Run the broker with
  `--log-level debug` to see live discovery logs on stderr.
- **"Bind: address already in use"**: port 11435 is the Ollama proxy port;
  another instance of the proxy or another process is holding it. Find the
  offender with `ss -tlnp | grep 11435` and stop or kill it.

Report issues at the project's tracker. Include the broker's `--log-level debug`
output and your distro / kernel version.
