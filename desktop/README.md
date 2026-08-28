<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PAIR desktop application

The Electron + React desktop client for NVIDIA Personal AI Router.

For what PAIR is, how to install it, and how to set it up, start at the
repository root [README](../README.md) and the
[Getting started guide](../docs/getting-started.mdx). This file covers only what
you need to work in this directory.

## Layout

```text
src/electron/     Main process: broker supervision, service bridge, IPC
src/preload/      Typed preload bridge (window.pairApi, window.windowApi)
src/ui/           React renderer
src/shared/       Types and utilities shared across the three above
src/declarations/ Ambient type declarations
scripts/          Build, license, and contract tooling
tests/            Vitest unit tests
docs/             Desktop-specific architecture and contract documentation
```

The renderer reaches services only through the preload bridge; it never talks to
a Go worker directly. Electron starts `nvpair-ui-broker` and the broker
supervises every other worker.

## Develop

```bash
npm install
npm start
```

`npm start` compiles the Go binaries from the sibling `../services` directory
into `cli-bin/`, then launches Electron. Prerequisites, with versions and download
links, are listed under
[Prerequisites](../docs/building.mdx#prerequisites).

## Testing and checks

Four checks, all runnable from this directory:

```bash
npm run lint                     # ESLint, including Prettier formatting
npm run typecheck                # main, renderer, and test tsconfigs
npm test                         # unit tests
npm run service-contracts:check  # this app still agrees with ../services
```

Run all four before opening a merge request. `npm run format` applies formatting
rather than just reporting it.

### While you are editing

```bash
npm run test:unit:watch                # re-runs on save
npm run test:unit tests/modular/x.test.ts   # one file
npm run test:unit:coverage             # coverage plus a summary
npm run test:clean                     # clears the Vitest cache and stray tmpdirs
```

Tests live in `tests/`, not beside the source. `tests/fixtures/isolation.ts` is
loaded for every test and enforces two things worth knowing: outbound HTTP to
anything other than loopback is blocked, and `PAIR_USER_DATA` points at a
per-worker temp directory, so a test can never write to your real application
data. A test that needs HTTP should stand up a local fake.

Coverage is scoped to `src/shared/**`. The Electron main process and the renderer
are excluded, so a green coverage number says less than it appears to — most of
`src/electron/` and the renderer stores have no unit tests at all.

`vitest.config.ts` also declares an `e2e` project, but it selects no files. There
is no end-to-end suite yet; nothing exercises the app together with its service
processes.

### When you change the service contract

`service-contracts:check` compares this app's bridge against the JSON-RPC surface
of `../services` and fails on drift. Run it whenever you touch a method, payload,
or push event. If the Go surface changed legitimately, regenerate the record:

```bash
npm run service-contracts        # report drift
npm run service-contracts:write  # regenerate docs/services-api.md
```

Changing the services themselves also means running their own tests — see
[services testing](../services/readme.md#testing).

## Build

```bash
npm run build              # renderer, main, preload, and CLI bundles
```

This produces the bundles the application runs from, for use on the machine that
built them. Installable builds come from the
[releases page](https://github.com/NVIDIA/Personal-AI-Router/releases).

## Documentation

Everything specific to the application is in [`docs/`](docs/):

- [Architecture](docs/architecture.md) — processes, IPC, and state flow
- [Frontend API](docs/frontend-api.md) — preload surface and push channels
- [Services backend integration](docs/services-backend.md) — service contract
  and update procedure
- [Services parity](docs/services-parity.md) — current capability status
- [Services API](docs/services-api.md) — generated JSON-RPC method surface

Contribution workflow, security policy, and support live at the repository root:
[CONTRIBUTING.md](../CONTRIBUTING.md), [SECURITY.md](../SECURITY.md), and
[SUPPORT.md](../SUPPORT.md).
