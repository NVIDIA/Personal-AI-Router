<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-host-bootstrap

`nvpair-host-bootstrap` is the privileged, target-local PAIR setup transaction.
It prepares SSH access, installs the fixed helper and product payload, and
selects one Desktop or Headless runtime owner. Electron,
`nvpair-ui-broker`, and `nvpair-engine-manager` never supervise it.

## Supported targets

The canonical matrix has exactly six platform/architecture combinations:

- Windows amd64 and arm64;
- macOS amd64 and arm64;
- Linux amd64 and arm64.

The Linux adapter additionally requires `/etc/os-release` to identify Debian or
Ubuntu. Other Linux distributions and processor architectures fail closed.

Quick Connect and Zero Touch are the operator-reviewed and enterprise lanes.
Both lanes use the same request, ownership rules, phase machine, and receipt.

## Runtime owner

Every request carries both the requested role and an already resolved runtime
owner:

- `auto` may resolve to Desktop or Headless;
- explicit `desktop` must resolve to Desktop;
- explicit `headless` must resolve to Headless.

Desktop and TUI currently resolve Auto to Desktop on Windows/macOS and Headless
on Linux. Desktop records the desktop broker tree as the sole owner and requires
the fixed headless owner to be absent. Headless installs one fixed owner:

- Windows SCM runs `nvpair-host-helper.exe headless-service`, a fixed wrapper
  for the installed broker;
- macOS uses a system launchd job; and
- Linux uses a system systemd service.

A live Desktop broker tree prevents a Headless transition. The bootstrap never
allows simultaneous Desktop and Headless owners.

## Contract and phases

The shared contract is `nvpair-shared/hostbootstrap`. A request binds:

- target, lane, requested role, and runtime owner;
- target account, home, authorized-keys path, and SSH endpoint;
- the controller's canonical SSH public-key material and fingerprint; and
- exact product and helper versions, digests, and fixed install paths.

Passwords, passphrases, private keys, arbitrary commands, and shell fragments
are prohibited.

The legal progression is:

1. `inspect` produces a target-local ownership snapshot.
2. `review` deterministically returns `apply`, `repair-owned`, `no-op`, or
   `refuse-foreign` plus the fixed resource actions.
3. `apply` freshly reproduces the reviewed snapshot, journals each effect, and
   changes only the listed resource.
4. `verify` re-inspects exact native state.
5. A successful verification writes an immutable `complete` receipt.

A no-op review goes directly to verification. Foreign ownership blocks apply.
The target's observations, journal, markers, and receipt are authoritative; a
controller cannot provide substitutes.

## Fixed resources

The transaction reconciles only:

- SSH service;
- platform firewall state where applicable;
- one exact authorized-key line;
- the fixed host helper;
- the exact product tree; and
- the one active runtime owner.

The signed sibling payload contains a strict manifest for helper and product
bytes. Product archives are reopened and checked against their canonical
path/type/mode/size/SHA-256 inventory before installation. Symlinks, hard links,
path aliases, case collisions, unexpected files, and unsafe ownership fail
closed.

## Recovery, repair, and uninstall

Each mutation is journaled before its native effect. A rerun either proves the
effect completed or safely resumes from the exact precondition; unrelated drift
returns an error instead of being guessed away.

Repair accepts only a `repair-owned` or `no-op` plan for the same stored
operation and binding. The prior complete receipt and current ownership markers
must authorize every affected resource. Otherwise the operator must rerun the
signed bootstrap.

`uninstall` requires the original request and immutable complete receipt. It
preflights ownership, removes only proven-owned resources in reverse dependency
order, verifies absence after each effect, and writes an uninstall tombstone
before removing active operation state. It never treats foreign state as PAIR
owned.

## CLI

The executable accepts one fixed verb and one bounded canonical JSON document
on stdin:

```text
nvpair-host-bootstrap inspect
nvpair-host-bootstrap review
nvpair-host-bootstrap apply
nvpair-host-bootstrap verify
nvpair-host-bootstrap uninstall
```

`inspect`, `review`, and `uninstall` consume a request. `apply` and `verify`
consume the exact reviewed plan. JSON results are written to stdout; errors go
to stderr. There is no command execution mode.

Administrator/root authority is required for apply, repair, verify, and
uninstall. Use the signed package's native authorization flow; the binary does
not implement a reusable privilege grant.

## Packaging identity

The package catalog distinguishes `official-release` from `engineering`.
Official mode requires coherent final-content signature metadata, detached
release material where required, and notarized macOS records. Missing or
inconsistent official inputs fail closed. Public source builds are explicitly
unsigned engineering artifacts and are not official releases.

This repository defines and tests the matrix contract and deterministic
packaging. That is not, by itself, native acceptance of all six targets.

## Build and test

```bash
go build ./...
go test ./...
```

The repository build scripts stamp the component version from
`services/versions.json`.
