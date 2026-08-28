// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Build the modular Go subprocess binaries from sibling `services/` into
 * `cli-bin/`.
 *
 * Every backend binary is pure Go (no cgo), so we cross-compile for any target
 * from any host with `CGO_ENABLED=0 GOOS=… GOARCH=…` — no MinGW, no per-OS
 * runners. Each component resolves `replace nvpair-shared => ../shared`
 * relatively and ships its own `go.sum`, so a plain per-directory `go build
 * is enough.
 *
 * Version stamping mirrors services' own `build.sh`/`build.bat`: each binary
 * is built with `-ldflags "-X main.Version=<component version>"`, read from
 * `services/versions.json`.
 *
 * Usage:
 *   tsx scripts/build-modular-binaries.ts [--platform=win32|linux|darwin]
 *                                         [--arch=x64|arm64] [--force]
 *
 * Env overrides: PAIR_SERVICES_PLATFORM, PAIR_SERVICES_ARCH,
 * PAIR_SERVICES_FORCE_FETCH=1 (or CI=true) forces a rebuild.
 *
 * A `cli-bin/manifest.json` records the services source fingerprint, target,
 * component versions, and per-file hashes so repeat runs (e.g. `npm start`)
 * skip the rebuild when nothing changed. CI passes `--force`.
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
    statSync,
    writeFileSync
} from 'node:fs'
import path from 'node:path'
import {
    MODULAR_BUNDLED_BINARIES,
    MODULAR_RUNTIME_BINARIES,
    modularBinaryFileName,
    modularShippedBinaryBaseNames
} from '@/shared/constants/modular-binaries'
import type { ModularPackageArch } from '@/shared/constants/modular-binaries'
import type { SupportedPlatform } from '@/shared/types/platform'
import { currentPlatform } from '@/shared/utils/platform'

type JsonPrimitive = string | number | boolean | null
type JsonValue = JsonPrimitive | JsonObject | JsonValue[]
interface JsonObject {
    [key: string]: JsonValue
}

interface BuildOptions {
    platform: SupportedPlatform
    arch: ModularPackageArch
    force: boolean
}

interface ManifestFile {
    fileName: string
    size: number
    sha256: string
}

interface BuildManifest {
    source: 'services-build'
    sourceFingerprint: string
    product: string
    platform: SupportedPlatform
    arch: ModularPackageArch
    components: Record<string, string>
    files: ManifestFile[]
    builtAt: string
}

const REPO_ROOT = path.resolve(__dirname, '..')
const CLI_BIN_DIR = path.join(REPO_ROOT, 'cli-bin')
const MANIFEST_PATH = path.join(CLI_BIN_DIR, 'manifest.json')
const SERVICES_ROOT = path.resolve(REPO_ROOT, '..', 'services')

function servicesDir(): string {
    if (!existsSync(path.join(SERVICES_ROOT, 'versions.json'))) {
        throw new Error(
            `services/ tree not found at ${SERVICES_ROOT} (expected versions.json).\n` +
                'Run from the monorepo with services/ checked out beside desktop/.'
        )
    }
    return SERVICES_ROOT
}

function argValue(name: string): string | null {
    const prefix = `--${name}=`
    const arg = process.argv.find(entry => entry.startsWith(prefix))
    return arg ? arg.slice(prefix.length) : null
}

function normalizePlatform(value: string): SupportedPlatform {
    if (value === 'win32' || value === 'darwin' || value === 'linux') return value
    throw new Error(`Unsupported modular binary platform: ${value}`)
}

function normalizeArch(value: string): ModularPackageArch {
    if (value === 'x64' || value === 'arm64') return value
    throw new Error(`Unsupported modular binary arch: ${value}`)
}

function readOptions(): BuildOptions {
    const platform = normalizePlatform(
        argValue('platform') ?? process.env.PAIR_SERVICES_PLATFORM ?? currentPlatform()
    )
    const arch = normalizeArch(argValue('arch') ?? process.env.PAIR_SERVICES_ARCH ?? process.arch)
    const force =
        process.argv.includes('--force') ||
        process.env.PAIR_SERVICES_FORCE_FETCH === '1' ||
        process.env.CI === 'true'
    return { platform, arch, force }
}

function goos(platform: SupportedPlatform): string {
    return platform === 'win32' ? 'windows' : platform
}

function goarch(arch: ModularPackageArch): string {
    return arch === 'x64' ? 'amd64' : 'arm64'
}

function isJsonObject(value: JsonValue): value is JsonObject {
    return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function stringRecord(value: JsonValue | undefined): Record<string, string> {
    const out: Record<string, string> = {}
    if (value && isJsonObject(value)) {
        for (const [key, v] of Object.entries(value)) {
            if (typeof v === 'string') out[key] = v
        }
    }
    return out
}

interface ParsedVersions {
    product: string
    components: Record<string, string>
}

/**
 * Surface new backend binaries at build time. If services' `versions.json`
 * lists a subprocess component that is not in `MODULAR_RUNTIME_BINARIES`, the
 * build cannot produce it — warn loudly (non-fatal) and point at the same fix
 * `npm run service-contracts:check` enforces in CI.
 */
function warnUnbuiltComponents(versions: ParsedVersions): void {
    const built = new Set(modularShippedBinaryBaseNames())
    const missing = Object.keys(versions.components)
        .filter(name => !built.has(name))
        .sort()
    if (missing.length === 0) return
    console.warn(
        `[modular-build] WARNING: ${missing.length} services component(s) in versions.json are not in ` +
            'MODULAR_RUNTIME_BINARIES and will NOT be built:\n' +
            missing.map(name => `  - ${name}`).join('\n') +
            '\n  Add them to src/shared/constants/modular-binaries.ts (see `npm run service-contracts`).'
    )
}

function readVersions(repo: string): ParsedVersions {
    const versionsPath = path.join(repo, 'versions.json')
    const parsed: JsonValue = JSON.parse(readFileSync(versionsPath, 'utf8'))
    if (!isJsonObject(parsed)) throw new Error(`${versionsPath} is not a JSON object`)
    const product = typeof parsed['product'] === 'string' ? parsed['product'] : ''
    return { product, components: stringRecord(parsed['components']) }
}

function ensureGoToolchain(): void {
    const res = spawnSync('go', ['version'], { encoding: 'utf8' })
    if (res.status !== 0) {
        throw new Error(
            'Go toolchain not found on PATH.\n' +
                'Install Go 1.25+ from https://go.dev/dl/ and reopen your terminal so PATH updates.'
        )
    }
    console.log(`[modular-build] ${res.stdout.trim()}`)
}

function listFingerprintFiles(repo: string): string[] {
    const out: string[] = []
    const versionsPath = path.join(repo, 'versions.json')
    if (existsSync(versionsPath)) out.push(versionsPath)

    const walk = (dir: string): void => {
        for (const entry of readdirSync(dir)) {
            const full = path.join(dir, entry)
            const st = statSync(full)
            if (st.isDirectory()) {
                if (
                    entry === 'testdata' ||
                    entry === '.git' ||
                    entry === 'build' ||
                    entry === 'dist'
                ) {
                    continue
                }
                walk(full)
            } else if (entry.endsWith('.go') && !entry.endsWith('_test.go')) {
                out.push(full)
            } else if (entry === 'go.mod' || entry === 'go.sum') {
                out.push(full)
            }
        }
    }
    walk(repo)
    return out.sort()
}

/** Content hash of services Go sources + module files — not monorepo git HEAD. */
function servicesSourceFingerprint(repo: string): string {
    const hash = createHash('sha256')
    for (const file of listFingerprintFiles(repo)) {
        hash.update(file.slice(repo.length + 1))
        hash.update('\0')
        hash.update(readFileSync(file))
        hash.update('\0')
    }
    return hash.digest('hex')
}

function sha256(filePath: string): { size: number; sha256: string } {
    const data = readFileSync(filePath)
    return { size: data.byteLength, sha256: createHash('sha256').update(data).digest('hex') }
}

function manifestFingerprint(parsed: JsonObject): string {
    const fingerprint = parsed['sourceFingerprint']
    if (typeof fingerprint === 'string' && fingerprint) return fingerprint
    const legacyCommit = parsed['commit']
    return typeof legacyCommit === 'string' ? legacyCommit : ''
}

function parseManifest(text: string): BuildManifest | null {
    const parsed: JsonValue = JSON.parse(text)
    if (!isJsonObject(parsed)) return null
    const filesValue = parsed['files']
    if (!Array.isArray(filesValue)) return null
    const files: ManifestFile[] = []
    for (const fileValue of filesValue) {
        if (!isJsonObject(fileValue)) return null
        const fileName = fileValue['fileName']
        const size = fileValue['size']
        const hash = fileValue['sha256']
        if (typeof fileName !== 'string' || typeof size !== 'number' || typeof hash !== 'string') {
            return null
        }
        files.push({ fileName, size, sha256: hash })
    }
    const platform = parsed['platform']
    const arch = parsed['arch']
    const sourceFingerprint = manifestFingerprint(parsed)
    if (platform !== 'win32' && platform !== 'darwin' && platform !== 'linux') return null
    if (arch !== 'x64' && arch !== 'arm64') return null
    if (!sourceFingerprint) return null
    return {
        source: 'services-build',
        sourceFingerprint,
        product: typeof parsed['product'] === 'string' ? parsed['product'] : '',
        platform,
        arch,
        components: stringRecord(parsed['components']),
        files,
        builtAt: typeof parsed['builtAt'] === 'string' ? parsed['builtAt'] : ''
    }
}

function expectedFileNames(platform: SupportedPlatform): string[] {
    return modularShippedBinaryBaseNames().map(baseName =>
        modularBinaryFileName(baseName, platform)
    )
}

function cliBinHasOnlyExpectedFiles(platform: SupportedPlatform): boolean {
    if (!existsSync(CLI_BIN_DIR)) return false
    const expected = new Set([...expectedFileNames(platform), 'manifest.json'])
    const entries = readdirSync(CLI_BIN_DIR, { withFileTypes: true })
    return (
        entries.length === expected.size &&
        entries.every(entry => entry.isFile() && expected.has(entry.name))
    )
}

function clearCliBin(): void {
    if (!existsSync(CLI_BIN_DIR)) return
    for (const entry of readdirSync(CLI_BIN_DIR)) {
        rmSync(path.join(CLI_BIN_DIR, entry), { recursive: true, force: true })
    }
}

function manifestIsCurrent(
    options: BuildOptions,
    sourceFingerprint: string,
    versions: ParsedVersions
): boolean {
    if (!cliBinHasOnlyExpectedFiles(options.platform)) return false
    if (!existsSync(MANIFEST_PATH)) return false
    const manifest = parseManifest(readFileSync(MANIFEST_PATH, 'utf8'))
    if (!manifest) return false
    if (manifest.sourceFingerprint !== sourceFingerprint || !sourceFingerprint) return false
    if (manifest.platform !== options.platform || manifest.arch !== options.arch) return false
    if (JSON.stringify(manifest.components) !== JSON.stringify(versions.components)) return false

    const expected = new Set(expectedFileNames(options.platform))
    if (
        manifest.files.length !== expected.size ||
        manifest.files.some(file => !expected.has(file.fileName))
    ) {
        return false
    }
    for (const fileName of expected) {
        const filePath = path.join(CLI_BIN_DIR, fileName)
        if (!existsSync(filePath)) return false
        const recorded = manifest.files.find(file => file.fileName === fileName)
        if (!recorded) return false
        const actual = sha256(filePath)
        if (actual.size !== recorded.size || actual.sha256 !== recorded.sha256) return false
    }
    return true
}

/**
 * Version strings are stamped into the linker flags (`-X main.Version=<v>`).
 * The linker splits the `-ldflags` value on whitespace, so a version containing
 * a space (or other linker-flag syntax) from a tampered `versions.json` would
 * inject extra linker directives into every build. Restrict to a conservative
 * semver-ish charset and forbid whitespace / a leading dash before it is ever
 * placed on the `go build` command line.
 */
const SAFE_VERSION = /^[0-9A-Za-z][0-9A-Za-z.+-]*$/

function assertSafeVersion(baseName: string, version: string): void {
    if (!SAFE_VERSION.test(version)) {
        throw new Error(
            `Refusing to build ${baseName}: unsafe version string ${JSON.stringify(version)} ` +
                'from versions.json (must match /^[0-9A-Za-z][0-9A-Za-z.+-]*$/, no whitespace or ' +
                'leading dash).'
        )
    }
}

function buildBinary(
    repo: string,
    options: BuildOptions,
    baseName: string,
    version: string
): ManifestFile {
    assertSafeVersion(baseName, version)
    const componentDir = path.join(repo, baseName)
    if (!existsSync(componentDir)) {
        throw new Error(`Missing component source directory: ${componentDir}`)
    }
    const fileName = modularBinaryFileName(baseName, options.platform)
    const outFile = path.join(CLI_BIN_DIR, fileName)
    const res = spawnSync(
        'go',
        ['build', '-trimpath', '-ldflags', `-s -w -X main.Version=${version}`, '-o', outFile, '.'],
        {
            cwd: componentDir,
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
        throw new Error(`go build failed for ${baseName} (exit ${res.status ?? 'signal'})`)
    }
    if (options.platform !== 'win32') chmodSync(outFile, 0o755)
    const hash = sha256(outFile)
    console.log(`[modular-build] built ${fileName} (v${version})`)
    return { fileName, size: hash.size, sha256: hash.sha256 }
}

function main(): void {
    const options = readOptions()
    const repo = servicesDir()

    ensureGoToolchain()

    const versions = readVersions(repo)
    warnUnbuiltComponents(versions)
    const sourceFingerprint = servicesSourceFingerprint(repo)

    if (!options.force && manifestIsCurrent(options, sourceFingerprint, versions)) {
        console.log(
            `[modular-build] cli-bin is current for fingerprint ${sourceFingerprint.slice(0, 12)} ${options.platform}/${options.arch}; skipping build.`
        )
        return
    }

    clearCliBin()
    mkdirSync(CLI_BIN_DIR, { recursive: true })
    const shipped = modularShippedBinaryBaseNames()
    console.log(
        `[modular-build] building ${shipped.length} binaries for ${options.platform}/${options.arch} (fingerprint ${sourceFingerprint.slice(0, 12)})`
    )

    const files: ManifestFile[] = []
    for (const binary of MODULAR_RUNTIME_BINARIES) {
        const version = versions.components[binary.baseName] ?? '0.0.0'
        files.push(buildBinary(repo, options, binary.baseName, version))
    }
    for (const binary of MODULAR_BUNDLED_BINARIES) {
        const version = versions.components[binary.baseName] ?? '0.0.0'
        files.push(buildBinary(repo, options, binary.baseName, version))
    }

    const manifest: BuildManifest = {
        source: 'services-build',
        sourceFingerprint,
        product: versions.product,
        platform: options.platform,
        arch: options.arch,
        components: versions.components,
        files,
        builtAt: new Date().toISOString()
    }
    writeFileSync(MANIFEST_PATH, `${JSON.stringify(manifest, null, 2)}\n`, 'utf8')
    console.log(`[modular-build] wrote ${MANIFEST_PATH}`)
}

main()
