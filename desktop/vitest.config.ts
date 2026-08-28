// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitest/config'
import * as path from 'path'

/**
 * Two projects:
 *
 * - `unit` — fast, in-process: shared/electron/preload/renderer logic. No
 *   legacy TypeScript CLI backend, no real network.
 *   Run on its own via `npm run test:unit` (or `:unit:watch`).
 *
 * - `e2e` — reserved for Electron + modular subprocess coverage once a
 *   modular harness exists.
 *
 * `npm test` runs both projects (no `--project` filter).
 *
 * Setup files load `tests/fixtures/isolation.ts` once per worker so any leak
 * onto the host (real userData, real $HOME, real LAN, taskkill, axios calls
 * to the public internet) trips a hard fail before a test body runs.
 */
export default defineConfig({
    resolve: {
        alias: {
            '@': path.resolve(__dirname, 'src')
        }
    },
    test: {
        // Default tests: modular-era tests only. E2E is opt-in once rewritten.
        include: ['tests/modular/**/*.test.ts'],
        setupFiles: ['tests/fixtures/isolation.ts'],
        environment: 'node',
        clearMocks: true,
        restoreMocks: true,
        hookTimeout: 15_000,
        testTimeout: 15_000,
        // Code coverage: V8 provider (no instrumentation, fast). Opt in via
        // `--coverage` or one of the `test:coverage*` scripts. Scope is the
        // pure logic the contract tests actually exercise: all of
        // `src/shared/`, the renderer's helper and constant modules, and the
        // handful of Electron-main modules with no Electron dependency.
        // Components, stores, and the rest of Electron main stay out because
        // this migration temporarily keeps only contract tests active.
        coverage: {
            // V8 provider — no instrumentation, fast. Run via the
            // `test:coverage` script (or `--coverage`) and inspect the
            // HTML report at `coverage/index.html` for per-file
            // drilldowns.
            //
            // Known artifact: V8 reports two entries per source file when
            // the file is evaluated via more than one transform chain
            // (e.g. once at setup-time for module resolution and once at
            // test-time). The "real" entry shows the correct percentages;
            // the sibling 0% entry can be ignored. The `All files` total
            // is therefore lower than truth — use per-file numbers, not
            // the total, until this is resolved upstream.
            provider: 'v8',
            reporter: ['text', 'html', 'json-summary', 'lcov'],
            reportsDirectory: 'coverage',
            excludeAfterRemap: true,
            include: [
                'src/shared/**/*.ts',
                'src/ui/utils/**/*.ts',
                'src/ui/constants/**/*.ts',
                'src/electron/csp.ts',
                'src/electron/globals.ts',
                'src/electron/inference-demo-schedule.ts',
                'src/electron/redact-log.ts'
            ],
            exclude: [
                // Generated proto bindings — not authored, not testable.
                // Type-only files / ambient declarations.
                '**/*.d.ts',
                // Tests don't cover themselves.
                '**/*.test.ts',
                'tests/**',
                // Pure type modules (re-checked by tsc, no runtime to cover).
                'src/shared/types/**'
            ]
        },
        projects: [
            {
                extends: true,
                test: {
                    name: 'unit',
                    include: ['tests/modular/**/*.test.ts']
                }
            },
            {
                extends: true,
                test: {
                    name: 'e2e',
                    include: [],
                    // E2E spawns binaries; these need much longer than unit.
                    hookTimeout: 120_000,
                    testTimeout: 60_000,
                    // Run E2E sequentially to avoid port collisions and to
                    // keep the host's resource graph readable when triaging.
                    pool: 'forks',
                    poolOptions: { forks: { singleFork: true } }
                }
            }
        ]
    }
})
