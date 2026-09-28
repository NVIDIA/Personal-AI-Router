// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto'
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { spawnSync, type SpawnSyncReturns } from 'node:child_process'
import { afterEach, describe, expect, test } from 'vitest'

type FixturePlatform = 'windows' | 'darwin' | 'linux'
type FixtureArchitecture = 'amd64' | 'arm64'
type FixtureSignatureKind = 'none' | 'authenticode' | 'apple-code-sign' | 'detached-release'

interface FixtureSignature {
    status: 'unsigned' | 'signed'
    kind: FixtureSignatureKind
    identity: string
    notarized: boolean
    signatureFile: string
    checksumFile: string
    expectedContentSHA256?: string
    expectedContentSize?: number
}

interface FixtureTarget {
    platform: FixturePlatform
    architecture: FixtureArchitecture
    productRoot: string
    bootstrapPath: string
    helperPath: string
    versions: {
        bootstrap: string
        helper: string
        product: string
    }
    provenance: 'engineering' | 'official-release'
    signatures: {
        bootstrap: FixtureSignature
        helper: FixtureSignature
        product: FixtureSignature
        combination: FixtureSignature
    }
}

interface FixtureConfig {
    schemaVersion: 1
    outputDirectory: string
    official: boolean
    catalogSignature: FixtureSignature
    targets: FixtureTarget[]
}

interface PayloadManifestFile {
    path: string
    type: 'file' | 'directory'
    size: number
    mode: number
    sha256: string
}

interface PayloadManifestArtifact {
    id: string
    version: string
    fileName: string
    byteCount: number
    sha256: string
    files: PayloadManifestFile[]
}

interface PayloadManifest {
    schemaVersion: number
    platform: FixturePlatform
    architecture: FixtureArchitecture
    artifacts: PayloadManifestArtifact[]
}

interface CatalogArtifact {
    identity: {
        id: string
        version: string
        sha256: string
        path: string
    }
    fileName: string
    size: number
    provenance: string
    signature: FixtureSignature
}

interface CatalogTarget {
    target: {
        platform: FixturePlatform
        architecture: FixtureArchitecture
    }
    roles: string[]
    bootstrap: CatalogArtifact
    helper: CatalogArtifact
    product: CatalogArtifact
    combination: {
        fileName: string
        size: number
        sha256: string
        provenance: string
        signature: FixtureSignature
    }
}

interface BootstrapCatalog {
    schemaVersion: number
    integrity: {
        checksumAlgorithm: string
        checksumFile: string
        signature: FixtureSignature
    }
    targets: CatalogTarget[]
}

interface ZipEntry {
    name: string
    mode: number
    directory: boolean
    body: Buffer
}

const DESKTOP_ROOT = path.resolve(__dirname, '..', '..')
const SCRIPT = path.join(DESKTOP_ROOT, 'scripts', 'package-bootstrap.ts')
const TSX = path.join(DESKTOP_ROOT, 'node_modules', 'tsx', 'dist', 'cli.mjs')
const roots: string[] = []

function sha256(raw: Buffer | string): string {
    return createHash('sha256').update(raw).digest('hex')
}

function engineeringSignature(): FixtureSignature {
    return {
        status: 'unsigned',
        kind: 'none',
        identity: '',
        notarized: false,
        signatureFile: '',
        checksumFile: ''
    }
}

function createRoot(): string {
    const root = mkdtempSync(path.join(tmpdir(), 'nvpair-bootstrap-packaging-'))
    roots.push(root)
    return root
}

function writeExecutable(filePath: string, content: string): void {
    mkdirSync(path.dirname(filePath), { recursive: true })
    writeFileSync(filePath, content)
    chmodSync(filePath, 0o755)
}

function productFixture(
    root: string,
    platform: FixturePlatform,
    architecture: FixtureArchitecture
): string {
    const product = path.join(root, `${platform}-${architecture}-product`)
    const extension = platform === 'windows' ? '.exe' : ''
    const cli = platform === 'darwin' ? 'Contents/Resources/cli-bin' : 'resources/cli-bin'
    const app =
        platform === 'darwin'
            ? 'Contents/MacOS/PAIR'
            : platform === 'windows'
              ? 'PAIR.exe'
              : 'nvpair'
    writeExecutable(path.join(product, app), `app:${platform}:${architecture}`)
    writeExecutable(
        path.join(product, cli, `nvpair-tui${extension}`),
        `tui:${platform}:${architecture}`
    )
    writeExecutable(
        path.join(product, cli, `nvpair-ui-broker${extension}`),
        `broker:${platform}:${architecture}`
    )
    mkdirSync(path.join(product, 'assets', 'empty'), { recursive: true })
    writeFileSync(
        path.join(product, 'assets', 'target.txt'),
        `${platform}/${architecture}\n`,
        'utf8'
    )
    return product
}

function targetFixture(
    root: string,
    platform: FixturePlatform,
    architecture: FixtureArchitecture
): FixtureTarget {
    const extension = platform === 'windows' ? '.exe' : ''
    const bootstrap = path.join(root, `${platform}-${architecture}-bootstrap${extension}`)
    const helper = path.join(root, `${platform}-${architecture}-helper${extension}`)
    writeExecutable(bootstrap, `bootstrap:${platform}:${architecture}`)
    writeExecutable(helper, `helper:${platform}:${architecture}`)
    return {
        platform,
        architecture,
        productRoot: productFixture(root, platform, architecture),
        bootstrapPath: bootstrap,
        helperPath: helper,
        versions: {
            bootstrap: '0.1.0',
            helper: '0.1.0',
            product: '1.2.3'
        },
        provenance: 'engineering',
        signatures: {
            bootstrap: engineeringSignature(),
            helper: engineeringSignature(),
            product: engineeringSignature(),
            combination: engineeringSignature()
        }
    }
}

function matrixConfig(root: string, outputDirectory: string): FixtureConfig {
    const targets: FixtureTarget[] = []
    const platforms: FixturePlatform[] = ['windows', 'darwin', 'linux']
    const architectures: FixtureArchitecture[] = ['amd64', 'arm64']
    for (const platform of platforms) {
        for (const architecture of architectures) {
            targets.push(targetFixture(root, platform, architecture))
        }
    }
    return {
        schemaVersion: 1,
        outputDirectory,
        official: false,
        catalogSignature: engineeringSignature(),
        targets
    }
}

function writeConfig(root: string, config: FixtureConfig, name = 'config.json'): string {
    const file = path.join(root, name)
    writeFileSync(file, `${JSON.stringify(config, null, 2)}\n`, 'utf8')
    return file
}

function runScript(...args: string[]): SpawnSyncReturns<string> {
    return spawnSync(process.execPath, [TSX, SCRIPT, ...args], {
        cwd: DESKTOP_ROOT,
        encoding: 'utf8'
    })
}

function zipEntries(filePath: string): ZipEntry[] {
    const raw = readFileSync(filePath)
    let eocd = -1
    for (let offset = raw.length - 22; offset >= 0; offset -= 1) {
        if (raw.readUInt32LE(offset) === 0x06054b50) {
            eocd = offset
            break
        }
    }
    if (eocd < 0) throw new Error(`ZIP EOCD missing: ${filePath}`)
    const count = raw.readUInt16LE(eocd + 10)
    let central = raw.readUInt32LE(eocd + 16)
    const entries: ZipEntry[] = []
    for (let index = 0; index < count; index += 1) {
        if (raw.readUInt32LE(central) !== 0x02014b50)
            throw new Error(`ZIP central entry missing: ${filePath}`)
        const method = raw.readUInt16LE(central + 10)
        const size = raw.readUInt32LE(central + 24)
        const nameLength = raw.readUInt16LE(central + 28)
        const extraLength = raw.readUInt16LE(central + 30)
        const commentLength = raw.readUInt16LE(central + 32)
        const external = raw.readUInt32LE(central + 38)
        const local = raw.readUInt32LE(central + 42)
        const name = raw.subarray(central + 46, central + 46 + nameLength).toString('utf8')
        if (method !== 0) throw new Error(`Unexpected ZIP compression for ${name}`)
        if (raw.readUInt32LE(local) !== 0x04034b50)
            throw new Error(`ZIP local entry missing: ${name}`)
        const localNameLength = raw.readUInt16LE(local + 26)
        const localExtraLength = raw.readUInt16LE(local + 28)
        const bodyStart = local + 30 + localNameLength + localExtraLength
        entries.push({
            name,
            mode: (external >>> 16) & 0xffff,
            directory: name.endsWith('/'),
            body: raw.subarray(bodyStart, bodyStart + size)
        })
        central += 46 + nameLength + extraLength + commentLength
    }
    return entries
}

function findEntry(entries: ZipEntry[], name: string): ZipEntry {
    const entry = entries.find(candidate => candidate.name === name)
    if (!entry) throw new Error(`ZIP entry missing: ${name}`)
    return entry
}

afterEach(() => {
    for (const root of roots.splice(0)) {
        rmSync(root, { recursive: true, force: true })
    }
})

describe('bootstrap packaging', () => {
    test('emits deterministic target bytes and canonical payload manifests', () => {
        const root = createRoot()
        const target = targetFixture(root, 'linux', 'amd64')
        const firstOutput = path.join(root, 'first')
        const secondOutput = path.join(root, 'second')
        const firstConfig = writeConfig(root, {
            schemaVersion: 1,
            outputDirectory: firstOutput,
            official: false,
            catalogSignature: engineeringSignature(),
            targets: [target]
        })
        const secondConfig = writeConfig(
            root,
            {
                schemaVersion: 1,
                outputDirectory: secondOutput,
                official: false,
                catalogSignature: engineeringSignature(),
                targets: [target]
            },
            'config-second.json'
        )

        expect(runScript('build-target', '--config', firstConfig).status).toBe(0)
        expect(runScript('build-target', '--config', secondConfig).status).toBe(0)

        const archiveName = 'nvpair-bootstrap-linux-amd64.zip'
        const firstArchive = readFileSync(path.join(firstOutput, 'linux-amd64', archiveName))
        const secondArchive = readFileSync(path.join(secondOutput, 'linux-amd64', archiveName))
        expect(firstArchive.equals(secondArchive)).toBe(true)

        const combination = zipEntries(path.join(firstOutput, 'linux-amd64', archiveName))
        expect(combination.map(entry => entry.name)).toEqual([
            'nvpair-host-bootstrap',
            'payload/',
            'payload/manifest.json',
            'payload/nvpair-host-helper',
            'payload/nvpair-product.zip'
        ])
        expect(findEntry(combination, 'nvpair-host-bootstrap').mode & 0o777).toBe(0o755)
        expect(findEntry(combination, 'payload/nvpair-host-helper').mode & 0o777).toBe(0o755)

        const manifest: PayloadManifest = JSON.parse(
            findEntry(combination, 'payload/manifest.json').body.toString('utf8')
        )
        expect(manifest.platform).toBe('linux')
        expect(manifest.architecture).toBe('amd64')
        expect(manifest.artifacts.map(artifact => artifact.id)).toEqual([
            'nvpair',
            'nvpair-host-helper'
        ])
        expect(manifest.artifacts.map(artifact => artifact.version)).toEqual(['1.2.3', '0.1.0'])
        const product = manifest.artifacts[0]
        expect(product?.files.map(file => file.path)).toContain('nvpair-tui')
        expect(product?.files.map(file => file.path)).toContain('nvpair-ui-broker')
        expect(
            product?.files.every(file =>
                file.type === 'directory'
                    ? file.mode === 0o755 && file.size === 0 && file.sha256 === ''
                    : (file.mode === 0o644 || file.mode === 0o755) && file.sha256.length === 64
            )
        ).toBe(true)
    })

    test('builds one strict unique catalog for all six targets and verifies it', () => {
        const root = createRoot()
        const output = path.join(root, 'matrix')
        const config = writeConfig(root, matrixConfig(root, output))
        const build = runScript('build-matrix', '--config', config)
        expect(build.status, `${build.stdout}\n${build.stderr}`).toBe(0)

        const catalog: BootstrapCatalog = JSON.parse(
            readFileSync(path.join(output, 'onboarding-bootstrap-catalog.json'), 'utf8')
        )
        expect(catalog.targets).toHaveLength(6)
        expect(
            catalog.targets.map(target => `${target.target.platform}-${target.target.architecture}`)
        ).toEqual([
            'windows-amd64',
            'windows-arm64',
            'darwin-amd64',
            'darwin-arm64',
            'linux-amd64',
            'linux-arm64'
        ])
        expect(new Set(catalog.targets.map(target => target.combination.fileName)).size).toBe(6)
        expect(new Set(catalog.targets.map(target => target.bootstrap.identity.sha256)).size).toBe(
            6
        )
        expect(catalog.integrity.signature.status).toBe('unsigned')

        const verify = runScript('verify', '--root', output)
        expect(verify.status, `${verify.stdout}\n${verify.stderr}`).toBe(0)
        expect(verify.stdout).toContain('verified 6 bootstrap combinations')
    })

    test('rejects unsafe and case-colliding product paths', () => {
        const root = createRoot()
        const output = path.join(root, 'unsafe')
        const target = targetFixture(root, 'windows', 'amd64')
        writeFileSync(path.join(target.productRoot, 'NVPAIR-TUI.EXE'), 'collision')
        const config = writeConfig(root, {
            schemaVersion: 1,
            outputDirectory: output,
            official: false,
            catalogSignature: engineeringSignature(),
            targets: [target]
        })
        const result = runScript('build-target', '--config', config)
        expect(result.status).not.toBe(0)
        expect(result.stderr).toContain('case-colliding')
    })

    test('rejects final bytes changed after signature metadata was recorded', () => {
        const root = createRoot()
        const output = path.join(root, 'changed')
        const target = targetFixture(root, 'windows', 'amd64')
        const original = readFileSync(target.helperPath)
        target.signatures.helper = {
            status: 'signed',
            kind: 'authenticode',
            identity: 'NVIDIA Corporation',
            notarized: false,
            signatureFile: '',
            checksumFile: '',
            expectedContentSHA256: sha256(original),
            expectedContentSize: original.byteLength
        }
        writeFileSync(target.helperPath, 'changed after signing')
        const config = writeConfig(root, {
            schemaVersion: 1,
            outputDirectory: output,
            official: false,
            catalogSignature: engineeringSignature(),
            targets: [target]
        })
        const result = runScript('build-target', '--config', config)
        expect(result.status).not.toBe(0)
        expect(result.stderr).toContain('changed after signature metadata')
    })

    test('fails closed when official output lacks required signatures', () => {
        const root = createRoot()
        const output = path.join(root, 'official')
        const target = targetFixture(root, 'linux', 'arm64')
        target.provenance = 'official-release'
        const config = writeConfig(root, {
            schemaVersion: 1,
            outputDirectory: output,
            official: true,
            catalogSignature: engineeringSignature(),
            targets: [target]
        })
        const result = runScript('build-target', '--config', config)
        expect(result.status).not.toBe(0)
        expect(result.stderr).toContain('official output requires signed')
    })

    test('stages a target-shaped engineering product fixture from final binaries', () => {
        const root = createRoot()
        const app = path.join(root, 'app')
        const tui = path.join(root, 'nvpair-tui')
        const broker = path.join(root, 'nvpair-ui-broker')
        writeExecutable(app, 'app')
        writeExecutable(tui, 'tui')
        writeExecutable(broker, 'broker')
        const output = path.join(root, 'product')

        const result = runScript(
            'stage-product',
            '--platform',
            'darwin',
            '--app',
            app,
            '--tui',
            tui,
            '--broker',
            broker,
            '--output',
            output
        )
        expect(result.status, result.stderr).toBe(0)
        expect(readFileSync(path.join(output, 'Contents', 'MacOS', 'PAIR'), 'utf8')).toBe('app')
        expect(
            readFileSync(
                path.join(output, 'Contents', 'Resources', 'cli-bin', 'nvpair-tui'),
                'utf8'
            )
        ).toBe('tui')
        expect(
            readFileSync(
                path.join(output, 'Contents', 'Resources', 'cli-bin', 'nvpair-ui-broker'),
                'utf8'
            )
        ).toBe('broker')
    })

    test('plans CGO-free reproducible Go builds for bootstrap and helper', () => {
        const result = runScript('plan-go', '--target', 'windows-amd64')
        expect(result.status, result.stderr).toBe(0)
        const plans: {
            component: string
            env: { CGO_ENABLED: string; GOOS: string; GOARCH: string; GOFLAGS: string }
            args: string[]
        }[] = JSON.parse(result.stdout)
        expect(plans.map(plan => plan.component)).toEqual([
            'nvpair-host-bootstrap',
            'nvpair-host-helper'
        ])
        for (const plan of plans) {
            expect(plan.env).toEqual({
                CGO_ENABLED: '0',
                GOOS: 'windows',
                GOARCH: 'amd64',
                GOFLAGS: '-buildvcs=false'
            })
            expect(plan.args).toContain('-buildvcs=false')
        }
    })

    function writeCliBin(): { cliBin: string; binary: string } {
        const cliBin = path.join(createRoot(), 'cli-bin')
        mkdirSync(cliBin)
        const binary = path.join(cliBin, 'nvpair-host-bootstrap.exe')
        writeFileSync(binary, 'unsigned')
        const before = readFileSync(binary)
        writeFileSync(
            path.join(cliBin, 'manifest.json'),
            `${JSON.stringify({
                source: 'services-build',
                sourceFingerprint: sha256('source'),
                services: '1.2.3',
                platform: 'win32',
                arch: 'x64',
                components: { 'nvpair-host-bootstrap': '1.2.3' },
                files: [
                    {
                        fileName: 'nvpair-host-bootstrap.exe',
                        size: before.byteLength,
                        sha256: sha256(before)
                    }
                ],
                builtAt: '2026-01-01T00:00:00.000Z'
            })}\n`,
            'utf8'
        )
        return { cliBin, binary }
    }

    test('verifies a cli-bin manifest in the modular build shape', () => {
        const { cliBin } = writeCliBin()

        const result = runScript('verify-cli-manifest', '--cli-bin', cliBin)
        expect(result.status).toBe(0)
        expect(result.stdout).toContain('verified final cli-bin manifest')
    })

    test('detects a cli-bin manifest left stale by post-build signing', () => {
        const { cliBin, binary } = writeCliBin()
        writeFileSync(binary, 'signed bytes are different')

        const result = runScript('verify-cli-manifest', '--cli-bin', cliBin)
        expect(result.status).not.toBe(0)
        expect(result.stderr).toContain('stale cli-bin manifest')
    })
})
