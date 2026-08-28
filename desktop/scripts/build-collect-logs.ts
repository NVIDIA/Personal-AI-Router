// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Compile the log sanitizer (`scripts/collectlogs`) for a packaging target.
 *
 * The sanitizer is what turns a raw log directory into something shareable, and
 * support asks people to attach its output. Without a compiled copy the wrapper
 * scripts fall back to `go build`, which means an installed user needs the Go
 * toolchain and the module source to file a good bug report. Building it here
 * lets the packaged app carry a runnable binary instead.
 *
 * Output goes to `out/collect-logs/`, not `cli-bin/`. `cli-bin` is the broker's
 * supervised runtime inventory and packaging rejects anything unexpected in it;
 * the sanitizer is an operator tool that no service launches.
 *
 * Pure Go with `CGO_ENABLED=0`, so any host cross-compiles for any target.
 */

import { spawnSync } from 'child_process'
import { chmodSync, existsSync, mkdirSync, rmSync } from 'fs'
import path from 'path'
import { fileURLToPath } from 'url'
import type { SupportedPlatform } from '@/shared/types/platform'
import { currentPlatform } from '@/shared/utils/platform'

const DESKTOP_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const REPO_ROOT = path.resolve(DESKTOP_DIR, '..')
const MODULE_DIR = path.join(REPO_ROOT, 'scripts', 'collectlogs')
const OUT_DIR = path.join(DESKTOP_DIR, 'out', 'collect-logs')

type Arch = 'x64' | 'arm64'

function readFlag(name: string): string | undefined {
    const prefix = `--${name}=`
    return process.argv.find(entry => entry.startsWith(prefix))?.slice(prefix.length)
}

function readPlatform(): SupportedPlatform {
    const value = readFlag('platform')
    if (value === undefined) return currentPlatform()
    if (value === 'win32' || value === 'darwin' || value === 'linux') return value
    throw new Error(`Unsupported --platform: ${value}`)
}

function readArch(): Arch {
    const value = readFlag('arch')
    if (value === undefined) return process.arch === 'arm64' ? 'arm64' : 'x64'
    if (value === 'x64' || value === 'arm64') return value
    throw new Error(`Unsupported --arch: ${value}`)
}

function goos(platform: SupportedPlatform): string {
    return platform === 'win32' ? 'windows' : platform
}

function goarch(arch: Arch): string {
    return arch === 'x64' ? 'amd64' : 'arm64'
}

function main(): void {
    const platform = readPlatform()
    const arch = readArch()

    if (!existsSync(path.join(MODULE_DIR, 'go.mod'))) {
        throw new Error(`Missing collector module at ${MODULE_DIR}`)
    }

    rmSync(OUT_DIR, { recursive: true, force: true })
    mkdirSync(OUT_DIR, { recursive: true })

    const fileName = platform === 'win32' ? 'collectlogs.exe' : 'collectlogs'
    const outFile = path.join(OUT_DIR, fileName)

    const result = spawnSync(
        'go',
        ['build', '-trimpath', '-ldflags', '-s -w', '-o', outFile, '.'],
        {
            cwd: MODULE_DIR,
            env: { ...process.env, CGO_ENABLED: '0', GOOS: goos(platform), GOARCH: goarch(arch) },
            stdio: 'inherit'
        }
    )
    if (result.status !== 0) {
        throw new Error(`go build failed for collectlogs (exit ${result.status ?? 'signal'})`)
    }
    if (platform !== 'win32') chmodSync(outFile, 0o755)

    console.log(`[collect-logs] built ${fileName} for ${platform}/${arch}`)
}

main()
