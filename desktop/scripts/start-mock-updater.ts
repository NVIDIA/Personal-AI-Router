// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'

process.env.PAIR_MOCK_UPDATER = process.argv.includes('--error') ? 'error' : '1'

const electronPathFile = join(process.cwd(), 'node_modules', 'electron', 'path.txt')

function run(command: string, args: string[]): void {
    const result = spawnSync(command, args, { stdio: 'inherit', shell: true, env: process.env })
    if (result.status !== 0) {
        process.exit(result.status ?? 1)
    }
}

if (!existsSync(electronPathFile)) {
    console.log('Electron binary missing — running node_modules/electron/install.js …')
    run('node', ['node_modules/electron/install.js'])
}

run('npm', ['run', 'start'])
