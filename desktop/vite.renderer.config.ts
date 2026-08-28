// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { resolve } from 'path'
import { defineConfig } from 'vite'
import { srcAlias } from './scripts/vite-alias'
import react from '@vitejs/plugin-react'
// @ts-ignore - Tailwind CSS plugin
import tailwindcss from '@tailwindcss/vite'

/**
 * Standalone renderer build for browser/static UI checks.
 * Mirrors the renderer section of electron.vite.config.ts.
 * Output: out/ui/.
 */
export default defineConfig({
    root: resolve(__dirname, 'src/ui'),
    // Relative base so the built `index.html` references assets as
    // `./assets/main-*.js` instead of `/assets/...`. Electron loads the UI
    // via `file://.../out/ui/index.html`, where absolute paths resolve to
    // `file:///assets/...` and fail. Relative paths work in both `file://`
    // (Electron) and HTTP (frontend gateway serving the UI at `/`).
    base: './',
    build: {
        outDir: resolve(__dirname, 'out/ui'),
        emptyOutDir: true,
        // cssCodeSplit: false,
        // assetsInlineLimit: Infinity,
        rollupOptions: {
            input: {
                main: resolve(__dirname, 'src/ui/index.html')
            }
            // output: {
            //     inlineDynamicImports: true
            // }
        }
    },
    resolve: {
        alias: [srcAlias]
    },
    plugins: [react(), tailwindcss()]
})
