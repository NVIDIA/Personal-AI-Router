// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { describe, expect, it } from 'vitest'

describe('Debian runtime dependencies', () => {
    it('preserves the installed builder defaults and supplies GBM and ALSA runtimes', () => {
        const require = createRequire(import.meta.url)
        const builder = readFileSync(
            require.resolve('app-builder-lib/out/targets/FpmTarget.js'),
            'utf8'
        )
        const defaults = builder.match(/case "deb":\s*return (\[[^\n]+\]);/)
        expect(defaults).not.toBeNull()
        const config = readFileSync('electron-builder.config.ts', 'utf8')
        const dependencyBlock = config.match(/\bdeb:\s*\{[\s\S]*?depends:\s*\[([^\]]+)\]/)
        expect(dependencyBlock).not.toBeNull()
        const dependencies = [...dependencyBlock![1].matchAll(/'([^']+)'/g)].map(m => m[1])
        expect(dependencies).toEqual([
            ...JSON.parse(defaults![1]),
            'libgbm1',
            'libasound2t64 | libasound2'
        ])
    })
})
