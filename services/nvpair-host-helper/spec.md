<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Host Helper Specification

## Purpose

`nvpair-host-helper` provides the smallest local privileged surface needed to
inspect, repair, and verify one target-owned PAIR bootstrap operation. It also
accepts the fixed rank-reconcile identity used by managed serving-group cleanup.
It is owned by the target operating system and is never supervised by Electron
or `nvpair-ui-broker`.

## Non-goals

The helper is not:

- a shell or command runner;
- a network listener;
- a package installer with caller-selected paths;
- a credential, private-key, or privilege-grant store;
- a source of controller-authored observations, plans, or receipts; or
- an alternate broker or worker supervisor.

## Transport

The only server transport is local:

- Windows protected named pipe `\\.\pipe\nvpair-host-helper`;
- macOS root-owned/group-scoped `0660` Unix socket
  `/var/run/nvpair-host-helper.sock`;
- Linux root-owned/group-scoped `0660` Unix socket
  `/run/nvpair-host-helper.sock`, normally inherited from one systemd socket
  descriptor.

Endpoint identity is exact. Symlinks, wrong file types, wrong ACL/mode,
wrong owner/group, unexpected inherited descriptors, active duplicate servers,
and unverifiable stale endpoints fail closed.

Peer identity is obtained from the pipe client process token on Windows,
`LOCAL_PEERCRED` on macOS, or `SO_PEERCRED` on Linux. Admission is restricted to
system/root, administrator, or the reviewed principal identity allowed by the
platform contract.

## Wire contract

Frames use a four-byte big-endian length followed by at most 64 KiB of canonical
JSON. Unknown, duplicate, aliased, incomplete, oversized, or trailing fields are
rejected.

`HelperRequest` contains exactly:

- `schemaVersion`;
- `operationId`; and
- `action`.

`rank-reconcile` additionally requires fixed `runId`, `generation`, `rank`,
`planDigest`, and `nodeId`. Bootstrap actions reject that extra object.

The action enum is closed to:

- `inspect`;
- `apply`;
- `verify`; and
- `rank-reconcile`.

`HelperResponse` is action-specific:

- inspect: one valid inspect `Status`;
- apply: one valid apply or verify `Status`;
- verify: one valid complete `Receipt`;
- rank-reconcile: no status or receipt.

Rejected responses carry only a bounded, nonsecret normalized reason.

## Startup authority

Before listening, the helper loads the target's reviewed principal and validates
the installed helper/service identity against target-local bootstrap state. It
does not accept a principal from the connecting client. Invalid or incomplete
state prevents startup.

## Bootstrap execution

The helper loads the stored request and reviewed plan for the requested
operation. It invokes one fixed bootstrap executable path with one fixed verb
and canonical bounded stdin. The caller cannot influence executable path,
working directory, arguments beyond the action, environment, or output limit.

Inspect is non-mutating. Apply and verify run under the target mutation lock.
Every response must match the request operation, binding, decision, phase, and
action.

## Repair

Helper apply requires the complete receipt and the exact resource marker set
corresponding to receipt-owned state. If no pending effect exists, the helper
re-inspects and derives the repair plan on the target. The decision must be
`no-op` or `repair-owned`, and every repair action must have an owned marker.

A missing receipt, mismatched binding, marker mismatch, unexpected missing
resource, foreign resource, or unmarked action rejects repair. Historical
markers may be adopted only when their validated prior receipt proves the same
target/account/resource identity.

## Windows headless wrapper

`headless-service` is a second fixed SCM role of the same executable, separate
from the helper endpoint. It starts only
`C:\Program Files\NVIDIA Corporation\PAIR\product\nvpair-ui-broker.exe` with no
caller arguments. Stop sends one bounded JSON-RPC shutdown request, closes
stdin, waits the fixed grace period, and then terminates only that child if it
has not exited.

macOS and Linux reject `headless-service`; their fixed headless ownership is
defined by target-installed launchd/systemd files.

## Security invariants

- No TCP/UDP listener and no remote transport.
- No shell, script, arbitrary command, or caller-selected path.
- No password, passphrase, private key, PIN, certificate, or cluster secret on
  the wire or in helper state.
- Peer and endpoint identity are checked by native operating-system mechanisms.
- Target-produced operation state and receipt remain authoritative.
- Bounded request, response, execution time, and public error text.

## Verification

Component tests cover strict framing, peer authorization, startup state,
platform endpoints, fixed execution, response correlation, repair authority,
rank reconciliation, and the Windows broker wrapper. Cross-compilation does not
replace native endpoint and service acceptance testing.
