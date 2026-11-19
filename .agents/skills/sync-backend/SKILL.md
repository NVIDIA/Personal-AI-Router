---
name: sync-backend
description: >-
  Refresh services/ from upstream, resolve JSON-RPC drift, update contract
  docs, rebuild cli-bin, and verify. Use when the backend changed or the user
  asks to sync dhc-modular / services.
disable-model-invocation: true
---
<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Sync services (backend)

In this monorepo the backend is the sibling `services/` tree (authoritative
checked-in source). Never invent a `dhc-modular/` checkout inside `desktop/`.

Follow `.cursor/rules/service-contracts.mdc` and
`desktop/docs/services-backend.md`.

## Workflow

1. Change `services/` in place. It is the authoritative checked-in source, so
   there is no upstream import step.

2. From `desktop/`:

    ```bash
    npm run service-contracts
    ```

4. Review:
    - component version changes;
    - added or removed binaries;
    - missing notification consumers;
    - backend requests the bridge does not call.

5. For each changed component, read its `README.md` / `spec.md` and the Go
   implementation. Go source is the contract.

6. Resolve drift in the bridge and docs, then rebuild:

    ```bash
    npm run build:modular-binaries -- --force
    npm run service-contracts:write
    npm run typecheck
    npm run test:unit
    ```

7. Verify:

    ```bash
    npm run service-contracts:check
    ```

8. Commit `services/` changes together with desktop integration edits.
