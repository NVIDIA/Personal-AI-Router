<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-host-helper

`nvpair-host-helper` is the narrow local companion to
`nvpair-host-bootstrap`. It exposes fixed target-owned actions to an authorized
local peer and invokes the bootstrap executable at one fixed path. It is not a
broker worker, shell, remote agent, or network listener.

## Local endpoints

The endpoint is fixed per platform:

- Windows: `\\.\pipe\nvpair-host-helper`;
- macOS: `/var/run/nvpair-host-helper.sock`;
- Linux: `/run/nvpair-host-helper.sock`.

Windows creates a protected named pipe for LocalSystem, Administrators, and the
reviewed account SID. macOS and Linux create a root-owned `0660` Unix socket for
the reviewed target group. Linux normally receives the socket from systemd
socket activation. Existing endpoints are accepted or removed only after exact
type, owner, group, mode, and liveness checks.

The server also verifies the connecting process identity. Root/LocalSystem,
Administrators, the reviewed user, or the reviewed group may connect as allowed
by the platform contract. Network connections are impossible.

## Closed protocol

Messages are canonical, length-prefixed JSON frames capped at 64 KiB. A request
contains:

- schema version;
- a 32-hex operation ID; and
- one action: `inspect`, `apply`, `verify`, or `rank-reconcile`.

There is no executable, argument vector, path, environment, script, shell
fragment, observation, plan, or receipt field.

For bootstrap actions, the helper loads the target's stored request and plan:

- `inspect` returns target-produced inspect status;
- `apply` validates receipt/marker repair authority, derives any current
  repair-owned plan on the target, and returns apply or verify status;
- `verify` returns the target-produced complete receipt.

`rank-reconcile` accepts only the fixed bounded run/generation/rank/plan/node
identity used by managed serving-group cleanup. It does not expand the
bootstrap command surface.

Responses are strictly correlated to operation ID and action. Public failures
return a small normalized reason and never expose raw command output.

## Fixed bootstrap execution

The helper invokes only:

- Windows:
  `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-bootstrap.exe`;
- macOS: `/Library/PrivilegedHelperTools/nvpair-host-bootstrap`;
- Linux: `/usr/libexec/nvpair-host-bootstrap`.

Only `inspect`, `apply`, and `verify` are selected, each with bounded canonical
JSON on stdin and bounded output. No caller-controlled command is ever
constructed.

## Repair authority

Apply is not a generic repair primitive. The helper requires:

- the same stored request and operation;
- a valid complete receipt;
- matching ownership markers for every receipt-owned resource; and
- either an exact no-op state or a deterministic `repair-owned` plan whose
  actions all have markers.

Missing or foreign authority is rejected. The signed target bootstrap must be
rerun when repair cannot be proved.

## Process modes

The executable accepts one fixed mode:

- `service`: run the platform helper service;
- `request`: send one framed request to the fixed local endpoint and print one
  response;
- `serve-console`: run the same local server for controlled service/test
  environments; and
- `headless-service`: Windows SCM only, where the helper acts as a fixed wrapper
  for the installed `nvpair-ui-broker.exe`.

The Windows headless wrapper starts no shell and accepts no path or arguments.
It launches the one fixed broker path, discards broker output, sends JSON-RPC
`shutdown` on service stop, waits a bounded grace period, then terminates the
child if required. macOS and Linux headless owners launch `nvpair-tui
--headless` through their fixed launchd/systemd definitions instead.

## Supervision and lifecycle

`nvpair-host-bootstrap` installs the helper and exact platform service
definition after review. The operating system then owns the helper service.
Electron, the desktop broker, and the TUI broker never spawn or supervise it.

The helper state directory and reviewed principal are target-produced,
root-owned, and validated before the endpoint starts. Startup fails closed when
the operation, receipt, principal, helper identity, service definition, or
endpoint does not match.

## Build and test

```bash
go build ./...
go test ./...
```

The repository build scripts stamp the component version from
`services/versions.json`.
