<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA Personal AI Router — macOS install

This tarball is the portable backend bundle. The shipped distribution places
its graphical UI alongside this bundle in the same installation directory. The
UI launches `nvpair-ui-broker`, which drives the API and supervises the workers
under `bin/`. See [Unsigned builds](#unsigned-builds) if macOS blocks the
binaries on first launch.

## 1. Extract

```bash
tar xf NVIDIA-Personal-AI-Router-<version>-darwin-arm64.tar.gz
cd NVIDIA-Personal-AI-Router-<version>
```

You should end up with a top-level directory containing `bin/` (the worker
binaries) and this `INSTALL.md`.

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

```bash
rm -rf /path/to/NVIDIA-Personal-AI-Router-<version>
```

The workers store configuration in
`~/Library/Application Support/Nvidia Corporation/Personal AI Router/` (manual-node list,
log-level preference, cluster identity/pins, etc.). Remove that directory too if
you want a fully clean slate:

```bash
rm -rf "$HOME/Library/Application Support/Nvidia Corporation/Personal AI Router"
```

## Requirements

- macOS 12 (Monterey) or newer.
- 64-bit Intel (`amd64`) or Apple Silicon (`arm64`). The tarball is built per
  arch; pick the one that matches your Mac. `uname -m` on the Mac tells you
  which: `x86_64` → amd64, `arm64` → arm64.
- A working mDNS responder on UDP 5353. macOS's built-in `mDNSResponder` is
  fine; the discovery library coexists with it without configuration.

## Unsigned builds

A tarball built with `installer_build.sh` contains **unsigned** binaries.
Gatekeeper may block unsigned downloads on first launch. If it does, clear the
quarantine attribute once:

```bash
xattr -dr com.apple.quarantine /path/to/NVIDIA-Personal-AI-Router-<version>
```

## Troubleshooting

- **No nodes discovered**: confirm UDP 5353 isn't blocked by System Settings →
  Network → Firewall, and that other machines on the LAN are actually
  advertising. Run the broker with `--log-level debug` to see live discovery
  logs on stderr.
- **"Bind: address already in use"**: another process is holding port 11435.
  Find the offender with `lsof -i :11435` and stop it.

Report issues at the project's tracker. Include the broker's `--log-level debug`
output and your macOS version (`sw_vers`) and architecture (`uname -m`).
