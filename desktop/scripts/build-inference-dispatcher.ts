// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Build the `inference-dispatcher` Go client from the monorepo's
 * `scripts/inference-dispatcher` module into `tools/`.
 *
 * The dispatcher is not a PAIR service: it speaks no JSON-RPC, is not listed in
 * `services/versions.json`, and is never supervised by the broker. It is a
 * conventional third-party HTTP client that the Inference Demo spawns once per
 * scheduled request (`src/electron/inference-demo.ts`). That is why it ships in
 * its own `tools/` resource directory rather than in `cli-bin/`, whose contents
 * are asserted against the services binary inventory.
 *
 * Like the services binaries it is pure Go with no cgo, so any host can
 * cross-compile any target with `CGO_ENABLED=0 GOOS=… GOARCH=…`. The version is
 * stamped from the desktop `package.json` because the tool now versions with
 * the app that ships it.
 *
 * Usage:
 *   tsx scripts/build-inference-dispatcher.ts [--platform=win32|linux|darwin]
 *                                             [--arch=x64|arm64] [--force]
 *
 * A `tools/manifest.json` records the target and source fingerprint so repeat
 * runs (e.g. `npm start`) skip the rebuild when nothing changed. CI passes
 * `--force`.
 */

import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
    chmodSync,
    existsSync,
    mkdirSync,
    readdirSync,
    readFileSync,
    rmSync,
    writeFileSync
} from 'node:fs'
import path from 'node:path'
import {
    INFERENCE_DISPATCHER_BASE_NAME,
    INFERENCE_DISPATCHER_RESOURCE_DIR,
    inferenceDispatcherFileName
} from '@/shared/constants/inference-dispatcher'
import type { ModularPackageArch } from '@/shared/constants/modular-binaries'
import type { SupportedPlatform } from '@/shared/types/platform'
import { currentPlatform } from '@/shared/utils/platform'
import { version as appVersion } from '../package.json'

const DESKTOP_ROOT = path.resolve(__dirname, '..')
const TOOLS_DIR = path.join(DESKTOP_ROOT, INFERENCE_DISPATCHER_RESOURCE_DIR)
const MANIFEST_PATH = path.join(TOOLS_DIR, 'manifest.json')
const MODULE_DIR = path.resolve(DESKTOP_ROOT, '..', 'scripts', INFERENCE_DISPATCHER_BASE_NAME)

interface BuildOptions {
    platform: SupportedPlatform
    arch: ModularPackageArch
    force: boolean
}

interface ToolsManifest {
    sourceFingerprint: string
    version: string
    platform: SupportedPlatform
    arch: ModularPackageArch
    fileName: string
    size: number
    sha256: string
    builtAt: string
}

function argValue(name: string): string | null {
    const prefix = `--${name}=`
    const arg = process.argv.find(entry => entry.startsWith(prefix))
    return arg ? arg.slice(prefix.length) : null
}

function normalizePlatform(value: string): SupportedPlatform {
    if (value === 'win32' || value === 'darwin' || value === 'linux') return value
    throw new Error(`Unsupported inference-dispatcher platform: ${value}`)
}

function normalizeArch(value: string): ModularPackageArch {
    if (value === 'x64' || value === 'arm64') return value
    throw new Error(`Unsupported inference-dispatcher arch: ${value}`)
}

function readOptions(): BuildOptions {
    const platform = normalizePlatform(
        argValue('platform') ?? process.env.DHC_MODULAR_PLATFORM ?? currentPlatform()
    )
    const arch = normalizeArch(argValue('arch') ?? process.env.DHC_MODULAR_ARCH ?? process.arch)
    const force =
        process.argv.includes('--force') ||
        process.env.DHC_MODULAR_FORCE_FETCH === '1' ||
        process.env.CI === 'true'
    return { platform, arch, force }
}

function goos(platform: SupportedPlatform): string {
    return platform === 'win32' ? 'windows' : platform
}

function goarch(arch: ModularPackageArch): string {
    return arch === 'x64' ? 'amd64' : 'arm64'
}

function moduleDir(): string {
    if (!existsSync(path.join(MODULE_DIR, 'go.mod'))) {
        throw new Error(
            `inference-dispatcher module not found at ${MODULE_DIR} (expected go.mod).\n` +
                'Run from the monorepo with scripts/ checked out beside desktop/.'
        )
    }
    return MODULE_DIR
}

function ensureGoToolchain(): void {
    const res = spawnSync('go', ['version'], { encoding: 'utf8' })
    if (res.status !== 0) {
        throw new Error(
            'Go toolchain not found on PATH.\n' +
                'Install Go 1.25+ from https://go.dev/dl/ and reopen your terminal so PATH updates.'
        )
    }
    console.log(`[tools-build] ${res.stdout.trim()}`)
}

/** Content hash of the module's Go sources — not monorepo git HEAD. */
function sourceFingerprint(dir: string): string {
    const hash = createHash('sha256')
    const files = readdirSync(dir)
        .filter(entry => entry.endsWith('.go') || entry === 'go.mod' || entry === 'go.sum')
        .sort()
    for (const file of files) {
        hash.update(file)
        hash.update('\0')
        hash.update(readFileSync(path.join(dir, file)))
        hash.update('\0')
    }
    return hash.digest('hex')
}

function sha256(filePath: string): { size: number; sha256: string } {
    const data = readFileSync(filePath)
    return { size: data.byteLength, sha256: createHash('sha256').update(data).digest('hex') }
}

function readManifest(): ToolsManifest | null {
    if (!existsSync(MANIFEST_PATH)) return null
    const parsed: unknown = JSON.parse(readFileSync(MANIFEST_PATH, 'utf8'))
    if (typeof parsed !== 'object' || parsed === null) return null
    if (!('sourceFingerprint' in parsed) || typeof parsed.sourceFingerprint !== 'string') {
        return null
    }
    if (!('version' in parsed) || typeof parsed.version !== 'string') return null
    if (!('platform' in parsed) || typeof parsed.platform !== 'string') return null
    if (!('arch' in parsed) || typeof parsed.arch !== 'string') return null
    if (!('fileName' in parsed) || typeof parsed.fileName !== 'string') return null
    if (!('size' in parsed) || typeof parsed.size !== 'number') return null
    if (!('sha256' in parsed) || typeof parsed.sha256 !== 'string') return null
    if (
        parsed.platform !== 'win32' &&
        parsed.platform !== 'darwin' &&
        parsed.platform !== 'linux'
    ) {
        return null
    }
    if (parsed.arch !== 'x64' && parsed.arch !== 'arm64') return null
    return {
        sourceFingerprint: parsed.sourceFingerprint,
        version: parsed.version,
        platform: parsed.platform,
        arch: parsed.arch,
        fileName: parsed.fileName,
        size: parsed.size,
        sha256: parsed.sha256,
        builtAt: 'builtAt' in parsed && typeof parsed.builtAt === 'string' ? parsed.builtAt : ''
    }
}

function manifestIsCurrent(options: BuildOptions, fingerprint: string): boolean {
    const manifest = readManifest()
    if (!manifest) return false
    if (manifest.sourceFingerprint !== fingerprint || !fingerprint) return false
    if (manifest.version !== appVersion) return false
    if (manifest.platform !== options.platform || manifest.arch !== options.arch) return false

    const fileName = inferenceDispatcherFileName(options.platform)
    if (manifest.fileName !== fileName) return false
    // A stray file means an earlier build targeted a different platform; the
    // packaging assertion would reject it, so rebuild rather than skip.
    const entries = readdirSync(TOOLS_DIR, { withFileTypes: true })
    const expected = new Set([fileName, 'manifest.json'])
    if (entries.length !== expected.size) return false
    if (!entries.every(entry => entry.isFile() && expected.has(entry.name))) return false

    const actual = sha256(path.join(TOOLS_DIR, fileName))
    return actual.size === manifest.size && actual.sha256 === manifest.sha256
}

function clearToolsDir(): void {
    if (!existsSync(TOOLS_DIR)) return
    for (const entry of readdirSync(TOOLS_DIR)) {
        rmSync(path.join(TOOLS_DIR, entry), { recursive: true, force: true })
    }
}

/**
 * The linker splits `-ldflags` on whitespace, so a version carrying a space (or
 * other linker syntax) would inject extra directives into the build. Restrict
 * the stamped value before it reaches the command line.
 */
const SAFE_VERSION = /^[0-9A-Za-z][0-9A-Za-z.+-]*$/

function assertSafeVersion(version: string): void {
    if (!SAFE_VERSION.test(version)) {
        throw new Error(
            `Refusing to build ${INFERENCE_DISPATCHER_BASE_NAME}: unsafe version string ${JSON.stringify(version)} ` +
                'from package.json (must match /^[0-9A-Za-z][0-9A-Za-z.+-]*$/, no whitespace or ' +
                'leading dash).'
        )
    }
}

function main(): void {
    const options = readOptions()
    const dir = moduleDir()
    const fingerprint = sourceFingerprint(dir)

    if (!options.force && existsSync(TOOLS_DIR) && manifestIsCurrent(options, fingerprint)) {
        console.log(
            `[tools-build] tools/ is current for fingerprint ${fingerprint.slice(0, 12)} ${options.platform}/${options.arch}; skipping build.`
        )
        return
    }

    assertSafeVersion(appVersion)
    ensureGoToolchain()

    clearToolsDir()
    mkdirSync(TOOLS_DIR, { recursive: true })

    const fileName = inferenceDispatcherFileName(options.platform)
    const outFile = path.join(TOOLS_DIR, fileName)
    const res = spawnSync(
        'go',
        [
            'build',
            '-trimpath',
            '-ldflags',
            `-s -w -X main.Version=${appVersion}`,
            '-o',
            outFile,
            '.'
        ],
        {
            cwd: dir,
            env: {
                ...process.env,
                CGO_ENABLED: '0',
                GOOS: goos(options.platform),
                GOARCH: goarch(options.arch)
            },
            stdio: 'inherit'
        }
    )
    if (res.status !== 0) {
        throw new Error(
            `go build failed for ${INFERENCE_DISPATCHER_BASE_NAME} (exit ${res.status ?? 'signal'})`
        )
    }
    if (options.platform !== 'win32') chmodSync(outFile, 0o755)

    const hash = sha256(outFile)
    const manifest: ToolsManifest = {
        sourceFingerprint: fingerprint,
        version: appVersion,
        platform: options.platform,
        arch: options.arch,
        fileName,
        size: hash.size,
        sha256: hash.sha256,
        builtAt: new Date().toISOString()
    }
    writeFileSync(MANIFEST_PATH, `${JSON.stringify(manifest, null, 2)}\n`, 'utf8')
    console.log(
        `[tools-build] built ${fileName} (v${appVersion}) for ${options.platform}/${options.arch}`
    )
}

main()
