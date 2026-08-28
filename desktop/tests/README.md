<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tests

Vitest runs the active modular unit project by default.

```
tests/
  fixtures/        # Reusable, typed test infrastructure (no test files here)
  modular/         # Active unit tests
```

## Commands

| Command                   | What it runs                            |
| ------------------------- | --------------------------------------- |
| `npm test`                | Active modular unit suite               |
| `npm run test:unit`       | `tests/modular/**/*.test.ts`            |
| `npm run test:unit:watch` | Modular unit tests in watch mode        |
| `npm run typecheck:test`  | `tsc --noEmit -p tsconfig.test.json`    |
| `npm run typecheck`       | All three projects: web, node, and test |
| `npm run test:clean`      | Wipe Vitest cache and leftover tmpdirs  |

## Avoiding mock hell

Three rules:

1. **Use typed mocks.** Parameterize `vi.fn` with the real function type.
2. **Mock at module boundaries.** Keep production source free of test-only
   switches.
3. **Prefer shared wire/types coverage.** Tests should validate the current UI
   contract and shared utilities.

## Isolation invariants

Setup-files in `tests/fixtures/isolation.ts` enforce the following at every
test load:

- **Network**: An axios request interceptor blocks any request whose host
  is not `127.0.0.1` / `localhost` / `::1`. Tests that
  need HTTP must spin up a local fake (`tests/fixtures/release-server.ts`).
- **`PAIR_USER_DATA`** is set to a per-worker tmpdir so unit/contract tests
  that inject a `PathProvider` (via `initPlatform`) never write to the dev's
  real userData. Tests that want a different path can override it explicitly.
- **Sandboxing of destructive subsystems** is the responsibility of the test
  runtime. In-process tests use `vi.mock` at module boundaries. There are no
  `PAIR_TEST_DISABLE_*` env vars in production source.
- **`assertIsolated()`** verifies `PAIR_USER_DATA` is set and lives under
  `os.tmpdir()`. Call from any test that touches the file-config-store.
- **Tmpdir tracking**: `createTmpUserData()` registers an `afterEach`
  cleanup. A teardown leak fails `afterAll` with the leaked paths.

## Adding a new test

Add unit coverage under `tests/modular/<area>/<file>.test.ts`. Use direct
imports for pure code and module-boundary mocks for process, network, or
filesystem effects.

## Type discipline

The same discipline as production source applies:

- No `as any` / `as unknown` / `as Type` / `: any`. Same as production.
- `vi.fn<typeof realFn>()` for every mocked function.
- `implements` for every reusable fake.
- Fixtures are typed `.ts` modules, not raw JSON.
- `vi.mocked(realFn)` for assertion access — never cast.
