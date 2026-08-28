// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { chmodSync, existsSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import type { AfterPackContext, BuildResult } from 'electron-builder'
// electron-builder loads this hook through jiti (via electron-builder.config.ts),
// and jiti does not resolve the `@/*` alias, so shared modules are imported by
// relative path here. See the same note at the top of electron-builder.config.ts.
import { INFERENCE_DISPATCHER_RESOURCE_DIR } from '../../../src/shared/constants/inference-dispatcher'

function findAppBundle(appOutDir: string): string {
    const entries = readdirSync(appOutDir, { withFileTypes: true })
    const app = entries.find(e => e.isDirectory() && e.name.endsWith('.app'))
    if (!app) {
        throw new Error(`No .app bundle found in ${appOutDir}`)
    }
    return join(appOutDir, app.name)
}

/**
 * extraResources directories holding loose Go binaries: the services workers in
 * `cli-bin` and the `inference-dispatcher` client in
 * INFERENCE_DISPATCHER_RESOURCE_DIR.
 */
const GO_BINARY_RESOURCE_DIRS = ['cli-bin', INFERENCE_DISPATCHER_RESOURCE_DIR]

/**
 * The `.dmg` has no installer step (unlike the old `.pkg` postinstall that ran
 * `chmod a+rx` as root), so the bundled Go subprocess binaries must carry exec
 * bits in the shipped bundle. The build scripts already chmod them, but we
 * re-assert it here so a copy that dropped the mode bit can never produce a
 * non-executable binary in a release.
 */
function ensureGoBinaryExecBits(appPath: string): void {
    for (const dirName of GO_BINARY_RESOURCE_DIRS) {
        const dir = join(appPath, 'Contents/Resources', dirName)
        if (!existsSync(dir)) continue
        for (const entry of readdirSync(dir, { withFileTypes: true })) {
            if (!entry.isFile() || entry.name.includes('.')) continue
            chmodSync(join(dir, entry.name), 0o755)
        }
    }
}

// The public build is unsigned. These hooks only re-assert executable bits on
// the packaged Go subprocesses. NVIDIA 3S signing + notarization is layered on
// by internal-build/signing/macos/hooks.ts, which the internal electron-builder
// config points afterPack / afterAllArtifactBuild at instead of these.
export async function macAfterPack(context: AfterPackContext): Promise<void> {
    if (context.electronPlatformName !== 'darwin') return
    const appPath = findAppBundle(context.appOutDir)
    ensureGoBinaryExecBits(appPath)
}

export async function macAfterAllArtifactBuild(buildResult: BuildResult): Promise<string[]> {
    return buildResult.artifactPaths
}
