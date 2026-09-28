// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Deterministically packages target-local bootstrap combinations.
 *
 * The public path emits truthful unsigned engineering artifacts. Official
 * metadata is accepted only when every final input is attested after signing,
 * macOS notarization is recorded, and detached release signatures are present
 * where the platform has no embedded code-signing primitive.
 *
 * Commands:
 *   build-target --config <json>
 *   build-matrix --config <json>
 *   verify --root <directory> [--official]
 *   verify-cli-manifest --cli-bin <directory>
 *   refresh-cli-manifest --cli-bin <directory>
 */

import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import {
    chmodSync,
    copyFileSync,
    lstatSync,
    mkdirSync,
    readFileSync,
    readdirSync,
    rmSync,
    writeFileSync
} from 'node:fs'
import path from 'node:path'
import type { JsonValue } from '@/shared/types/json'

type BootstrapPlatform = 'windows' | 'darwin' | 'linux'
type BootstrapArchitecture = 'amd64' | 'arm64'
type Provenance = 'engineering' | 'official-release'
type SignatureStatus = 'unsigned' | 'signed'
type SignatureKind = 'none' | 'authenticode' | 'apple-code-sign' | 'detached-release'
type ProductEntryType = 'file' | 'directory'

interface SignatureConfig {
    status: SignatureStatus
    kind: SignatureKind
    identity: string
    notarized: boolean
    signatureFile: string
    checksumFile: string
    expectedContentSHA256?: string
    expectedContentSize?: number
}

interface SignatureRecord {
    status: SignatureStatus
    kind: SignatureKind
    identity: string
    notarized: boolean
    signatureFile: string
    checksumFile: string
    contentSHA256: string
    contentSize: number
}

interface TargetSignatures {
    bootstrap: SignatureConfig
    helper: SignatureConfig
    product: SignatureConfig
    combination: SignatureConfig
}

interface TargetVersions {
    bootstrap: string
    helper: string
    product: string
}

interface TargetConfig {
    platform: BootstrapPlatform
    architecture: BootstrapArchitecture
    productRoot: string
    bootstrapPath: string
    helperPath: string
    versions: TargetVersions
    provenance: Provenance
    signatures: TargetSignatures
}

interface PackagingConfig {
    schemaVersion: 1
    outputDirectory: string
    official: boolean
    catalogSignature: SignatureConfig
    targets: TargetConfig[]
}

interface ProductEntry {
    path: string
    type: ProductEntryType
    size: number
    mode: number
    sha256: string
    body: Buffer
}

interface ZipEntry {
    name: string
    mode: number
    type: ProductEntryType
    body: Buffer
}

interface PayloadArtifact {
    id: string
    version: string
    fileName: string
    byteCount: number
    sha256: string
    files: ProductManifestEntry[]
}

interface ProductManifestEntry {
    path: string
    type: ProductEntryType
    size: number
    mode: number
    sha256: string
}

interface PayloadManifest {
    schemaVersion: 1
    platform: BootstrapPlatform
    architecture: BootstrapArchitecture
    artifacts: PayloadArtifact[]
}

interface ArtifactIdentity {
    id: string
    version: string
    sha256: string
    path: string
}

interface CatalogArtifact {
    identity: ArtifactIdentity
    fileName: string
    size: number
    provenance: Provenance
    signature: SignatureRecord
}

interface CatalogCombination {
    fileName: string
    size: number
    sha256: string
    provenance: Provenance
    signature: SignatureRecord
}

interface CatalogTarget {
    target: {
        platform: BootstrapPlatform
        architecture: BootstrapArchitecture
    }
    roles: ['desktop', 'headless']
    bootstrap: CatalogArtifact
    helper: CatalogArtifact
    product: CatalogArtifact
    combination: CatalogCombination
}

interface BootstrapCatalog {
    schemaVersion: 1
    integrity: {
        checksumAlgorithm: 'sha256'
        checksumFile: string
        signature: SignatureRecord
    }
    targets: CatalogTarget[]
}

interface FileDigest {
    size: number
    sha256: string
}

interface ProductDigest extends FileDigest {
    entries: ProductEntry[]
}

interface VerifiedProductArchive {
    archive: FileDigest
    tree: FileDigest
}

interface ParsedZipEntry extends ZipEntry {
    crc32: number
}

interface CliManifestFile {
    fileName: string
    size: number
    sha256: string
}

interface CliManifest {
    source: string
    sourceFingerprint: string
    services: string
    platform: string
    arch: string
    components: { [name: string]: string }
    files: CliManifestFile[]
    builtAt: string
}

interface GoBuildPlan {
    component: 'nvpair-host-bootstrap' | 'nvpair-host-helper' | 'nvpair-tui' | 'nvpair-ui-broker'
    cwd: string
    output: string
    env: {
        CGO_ENABLED: '0'
        GOOS: BootstrapPlatform | 'windows'
        GOARCH: BootstrapArchitecture
        GOFLAGS: '-buildvcs=false'
    }
    args: string[]
}

const MAX_ARTIFACT_BYTES = 8 * 1024 * 1024 * 1024
const MAX_ZIP32_BYTES = 0xffffffff
const MAX_ZIP_ENTRIES = 0xffff
const SHA256_PATTERN = /^[0-9a-f]{64}$/
const VERSION_PATTERN =
    /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/
const SAFE_FILE_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/
const WINDOWS_RESERVED_NAMES = new Set([
    'CON',
    'PRN',
    'AUX',
    'NUL',
    'COM1',
    'COM2',
    'COM3',
    'COM4',
    'COM5',
    'COM6',
    'COM7',
    'COM8',
    'COM9',
    'LPT1',
    'LPT2',
    'LPT3',
    'LPT4',
    'LPT5',
    'LPT6',
    'LPT7',
    'LPT8',
    'LPT9'
])
const TARGET_ORDER: ReadonlyArray<{
    platform: BootstrapPlatform
    architecture: BootstrapArchitecture
}> = [
    { platform: 'windows', architecture: 'amd64' },
    { platform: 'windows', architecture: 'arm64' },
    { platform: 'darwin', architecture: 'amd64' },
    { platform: 'darwin', architecture: 'arm64' },
    { platform: 'linux', architecture: 'amd64' },
    { platform: 'linux', architecture: 'arm64' }
]
const CATALOG_FILE = 'onboarding-bootstrap-catalog.json'
const CATALOG_CHECKSUM_FILE = `${CATALOG_FILE}.sha256`
const CRC32_TABLE = buildCRC32Table()
const DESKTOP_ROOT = path.resolve(__dirname, '..')
const SERVICES_ROOT = path.resolve(DESKTOP_ROOT, '..', 'services')

function fail(message: string): never {
    throw new Error(message)
}

function jsonObject(value: JsonValue, label: string): { [key: string]: JsonValue } {
    if (value === null || typeof value !== 'object' || Array.isArray(value)) {
        fail(`${label} must be a JSON object`)
    }
    return value
}

function exactKeys(
    value: { [key: string]: JsonValue },
    label: string,
    required: string[],
    optional: string[] = []
): void {
    const allowed = new Set([...required, ...optional])
    const actual = Object.keys(value)
    for (const key of actual) {
        if (!allowed.has(key)) fail(`${label} has unknown field ${JSON.stringify(key)}`)
    }
    for (const key of required) {
        if (!Object.hasOwn(value, key)) fail(`${label} is missing ${JSON.stringify(key)}`)
    }
}

function jsonString(value: JsonValue | undefined, label: string): string {
    if (typeof value !== 'string') fail(`${label} must be a string`)
    return value
}

function jsonBoolean(value: JsonValue | undefined, label: string): boolean {
    if (typeof value !== 'boolean') fail(`${label} must be a boolean`)
    return value
}

function jsonInteger(value: JsonValue | undefined, label: string): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value)) {
        fail(`${label} must be a safe integer`)
    }
    return value
}

function jsonArray(value: JsonValue | undefined, label: string): JsonValue[] {
    if (!Array.isArray(value)) fail(`${label} must be an array`)
    return value
}

function parsePlatform(value: JsonValue | undefined, label: string): BootstrapPlatform {
    if (value === 'windows' || value === 'darwin' || value === 'linux') return value
    fail(`${label} must be windows, darwin, or linux`)
}

function parseArchitecture(value: JsonValue | undefined, label: string): BootstrapArchitecture {
    if (value === 'amd64' || value === 'arm64') return value
    fail(`${label} must be amd64 or arm64`)
}

function parseProvenance(value: JsonValue | undefined, label: string): Provenance {
    if (value === 'engineering' || value === 'official-release') return value
    fail(`${label} must be engineering or official-release`)
}

function parseSignatureStatus(value: JsonValue | undefined, label: string): SignatureStatus {
    if (value === 'unsigned' || value === 'signed') return value
    fail(`${label} must be unsigned or signed`)
}

function parseSignatureKind(value: JsonValue | undefined, label: string): SignatureKind {
    if (
        value === 'none' ||
        value === 'authenticode' ||
        value === 'apple-code-sign' ||
        value === 'detached-release'
    ) {
        return value
    }
    fail(`${label} is unsupported`)
}

function parseSignature(value: JsonValue | undefined, label: string): SignatureConfig {
    const object = jsonObject(value ?? null, label)
    exactKeys(
        object,
        label,
        ['status', 'kind', 'identity', 'notarized', 'signatureFile', 'checksumFile'],
        ['expectedContentSHA256', 'expectedContentSize']
    )
    const expectedSHA = object['expectedContentSHA256']
    const expectedSize = object['expectedContentSize']
    if ((expectedSHA === undefined) !== (expectedSize === undefined)) {
        fail(`${label} must provide expected content SHA-256 and size together`)
    }
    if (
        expectedSHA !== undefined &&
        (typeof expectedSHA !== 'string' || !SHA256_PATTERN.test(expectedSHA))
    ) {
        fail(`${label} expected content SHA-256 is invalid`)
    }
    if (
        expectedSize !== undefined &&
        (typeof expectedSize !== 'number' ||
            !Number.isSafeInteger(expectedSize) ||
            expectedSize < 0 ||
            expectedSize > MAX_ARTIFACT_BYTES)
    ) {
        fail(`${label} expected content size is invalid`)
    }
    const signature: SignatureConfig = {
        status: parseSignatureStatus(object['status'], `${label}.status`),
        kind: parseSignatureKind(object['kind'], `${label}.kind`),
        identity: jsonString(object['identity'], `${label}.identity`),
        notarized: jsonBoolean(object['notarized'], `${label}.notarized`),
        signatureFile: jsonString(object['signatureFile'], `${label}.signatureFile`),
        checksumFile: jsonString(object['checksumFile'], `${label}.checksumFile`)
    }
    if (typeof expectedSHA === 'string' && typeof expectedSize === 'number') {
        signature.expectedContentSHA256 = expectedSHA
        signature.expectedContentSize = expectedSize
    }
    return signature
}

function parseTarget(value: JsonValue, index: number): TargetConfig {
    const label = `targets[${index}]`
    const object = jsonObject(value, label)
    exactKeys(object, label, [
        'platform',
        'architecture',
        'productRoot',
        'bootstrapPath',
        'helperPath',
        'versions',
        'provenance',
        'signatures'
    ])
    const versionsObject = jsonObject(object['versions'] ?? null, `${label}.versions`)
    exactKeys(versionsObject, `${label}.versions`, ['bootstrap', 'helper', 'product'])
    const versions: TargetVersions = {
        bootstrap: jsonString(versionsObject['bootstrap'], `${label}.versions.bootstrap`),
        helper: jsonString(versionsObject['helper'], `${label}.versions.helper`),
        product: jsonString(versionsObject['product'], `${label}.versions.product`)
    }
    for (const [component, version] of Object.entries(versions)) {
        if (!VERSION_PATTERN.test(version)) {
            fail(`${label}.versions.${component} is not canonical SemVer`)
        }
    }
    const signaturesObject = jsonObject(object['signatures'] ?? null, `${label}.signatures`)
    exactKeys(signaturesObject, `${label}.signatures`, [
        'bootstrap',
        'helper',
        'product',
        'combination'
    ])
    return {
        platform: parsePlatform(object['platform'], `${label}.platform`),
        architecture: parseArchitecture(object['architecture'], `${label}.architecture`),
        productRoot: path.resolve(jsonString(object['productRoot'], `${label}.productRoot`)),
        bootstrapPath: path.resolve(jsonString(object['bootstrapPath'], `${label}.bootstrapPath`)),
        helperPath: path.resolve(jsonString(object['helperPath'], `${label}.helperPath`)),
        versions,
        provenance: parseProvenance(object['provenance'], `${label}.provenance`),
        signatures: {
            bootstrap: parseSignature(
                signaturesObject['bootstrap'],
                `${label}.signatures.bootstrap`
            ),
            helper: parseSignature(signaturesObject['helper'], `${label}.signatures.helper`),
            product: parseSignature(signaturesObject['product'], `${label}.signatures.product`),
            combination: parseSignature(
                signaturesObject['combination'],
                `${label}.signatures.combination`
            )
        }
    }
}

function parsePackagingConfig(configPath: string): PackagingConfig {
    const raw: JsonValue = JSON.parse(readFileSync(configPath, 'utf8'))
    const object = jsonObject(raw, 'packaging config')
    exactKeys(object, 'packaging config', [
        'schemaVersion',
        'outputDirectory',
        'official',
        'catalogSignature',
        'targets'
    ])
    if (object['schemaVersion'] !== 1) fail('packaging config schemaVersion must be 1')
    const targets = jsonArray(object['targets'], 'packaging config targets').map(parseTarget)
    if (targets.length === 0 || targets.length > TARGET_ORDER.length) {
        fail('packaging config must contain between one and six targets')
    }
    const config: PackagingConfig = {
        schemaVersion: 1,
        outputDirectory: path.resolve(
            jsonString(object['outputDirectory'], 'packaging config outputDirectory')
        ),
        official: jsonBoolean(object['official'], 'packaging config official'),
        catalogSignature: parseSignature(
            object['catalogSignature'],
            'packaging config catalogSignature'
        ),
        targets
    }
    validateTargetUniqueness(config.targets)
    return config
}

function validateTargetUniqueness(targets: TargetConfig[]): void {
    const seen = new Set<string>()
    for (const target of targets) {
        const key = targetKey(target.platform, target.architecture)
        if (seen.has(key)) fail(`duplicate bootstrap target ${key}`)
        seen.add(key)
    }
}

function targetKey(platform: BootstrapPlatform, architecture: BootstrapArchitecture): string {
    return `${platform}-${architecture}`
}

function compareCanonical(left: string, right: string): number {
    if (left < right) return -1
    if (left > right) return 1
    return 0
}

function sha256Buffer(raw: Buffer): string {
    return createHash('sha256').update(raw).digest('hex')
}

function digestFile(filePath: string): FileDigest {
    const info = lstatSync(filePath)
    if (!info.isFile() || info.isSymbolicLink()) fail(`artifact is not a regular file: ${filePath}`)
    if (info.size < 1 || info.size > MAX_ARTIFACT_BYTES) {
        fail(`artifact size is outside the supported range: ${filePath}`)
    }
    const raw = readFileSync(filePath)
    return { size: raw.byteLength, sha256: sha256Buffer(raw) }
}

function canonicalProductPath(platform: BootstrapPlatform, value: string): boolean {
    if (
        value.length === 0 ||
        value.startsWith('/') ||
        value.startsWith('../') ||
        value.includes('\\') ||
        value.split('/').some(segment => segment === '' || segment === '.' || segment === '..')
    ) {
        return false
    }
    for (const character of value) {
        const point = character.codePointAt(0)
        if (point === undefined || point < 0x20 || (point >= 0x7f && point <= 0x9f)) {
            return false
        }
    }
    if (platform !== 'windows') return true
    for (const segment of value.split('/')) {
        const base = segment.split('.')[0]?.toUpperCase() ?? ''
        if (
            segment.endsWith('.') ||
            segment.endsWith(' ') ||
            /[<>:"|?*]/.test(segment) ||
            WINDOWS_RESERVED_NAMES.has(base)
        ) {
            return false
        }
    }
    return true
}

function normalizedFileMode(platform: BootstrapPlatform, relative: string, mode: number): number {
    if (platform === 'windows') {
        return relative.toLowerCase().endsWith('.exe') ? 0o755 : 0o644
    }
    if (
        (platform === 'darwin' &&
            (relative.startsWith('Contents/MacOS/') ||
                (relative.startsWith('Contents/Resources/cli-bin/') &&
                    !path.posix.basename(relative).includes('.')))) ||
        (platform === 'linux' &&
            (relative === 'nvpair' ||
                (relative.startsWith('resources/cli-bin/') &&
                    !path.posix.basename(relative).includes('.'))))
    ) {
        return 0o755
    }
    return (mode & 0o111) !== 0 ? 0o755 : 0o644
}

function collectProductEntries(target: TargetConfig): ProductDigest {
    const rootInfo = lstatSync(target.productRoot)
    if (!rootInfo.isDirectory() || rootInfo.isSymbolicLink()) {
        fail(`product root is not a regular directory: ${target.productRoot}`)
    }
    const entries: ProductEntry[] = []
    const walk = (directory: string, prefix: string): void => {
        const children = readdirSync(directory, { withFileTypes: true }).sort((left, right) =>
            compareCanonical(left.name, right.name)
        )
        for (const child of children) {
            const relative = prefix ? `${prefix}/${child.name}` : child.name
            if (!canonicalProductPath(target.platform, relative)) {
                fail(`unsafe product path ${JSON.stringify(relative)}`)
            }
            const absolute = path.join(directory, child.name)
            const info = lstatSync(absolute)
            if (info.isSymbolicLink()) fail(`product links are forbidden: ${relative}`)
            if (info.isDirectory()) {
                entries.push({
                    path: relative,
                    type: 'directory',
                    size: 0,
                    mode: 0o755,
                    sha256: '',
                    body: Buffer.alloc(0)
                })
                walk(absolute, relative)
                continue
            }
            if (!info.isFile()) fail(`unsupported product entry type: ${relative}`)
            const body = readFileSync(absolute)
            entries.push({
                path: relative,
                type: 'file',
                size: body.byteLength,
                mode: normalizedFileMode(target.platform, relative, info.mode),
                sha256: sha256Buffer(body),
                body
            })
        }
    }
    walk(target.productRoot, '')
    addRequiredRootBinaries(target, entries)
    entries.sort((left, right) => compareCanonical(left.path, right.path))
    validateProductEntries(target.platform, entries)
    const digest = digestProductEntries(entries)
    return { ...digest, entries }
}

function digestProductEntries(entries: ProductEntry[]): FileDigest {
    const digest = createHash('sha256')
    let totalSize = 0
    for (const entry of entries) {
        digest.update(entry.path)
        digest.update('\0')
        digest.update(entry.type)
        digest.update('\0')
        digest.update(entry.mode.toString(8))
        digest.update('\0')
        digest.update(entry.size.toString(10))
        digest.update('\0')
        digest.update(entry.sha256)
        digest.update('\0')
        if (entry.type === 'file') totalSize += entry.size
    }
    return { size: totalSize, sha256: digest.digest('hex') }
}

function addRequiredRootBinaries(target: TargetConfig, entries: ProductEntry[]): void {
    if (target.platform === 'darwin') return
    const extension = target.platform === 'windows' ? '.exe' : ''
    for (const base of ['nvpair-tui', 'nvpair-ui-broker']) {
        const rootName = `${base}${extension}`
        if (entries.some(entry => entry.path === rootName)) continue
        const sourcePath = `resources/cli-bin/${rootName}`
        const source = entries.find(entry => entry.path === sourcePath && entry.type === 'file')
        if (!source) {
            fail(`product tree is missing ${rootName} and ${sourcePath}`)
        }
        entries.push({
            path: rootName,
            type: 'file',
            size: source.size,
            mode: 0o755,
            sha256: source.sha256,
            body: source.body
        })
    }
}

function requiredProductPaths(platform: BootstrapPlatform): string[] {
    if (platform === 'windows') return ['nvpair-tui.exe', 'nvpair-ui-broker.exe']
    if (platform === 'darwin') {
        return [
            'Contents/Resources/cli-bin/nvpair-tui',
            'Contents/Resources/cli-bin/nvpair-ui-broker'
        ]
    }
    return ['nvpair-tui', 'nvpair-ui-broker']
}

function validateProductEntries(platform: BootstrapPlatform, entries: ProductEntry[]): void {
    if (entries.length === 0 || entries.length > MAX_ZIP_ENTRIES) {
        fail('product tree has an unsupported number of entries')
    }
    const exact = new Set<string>()
    const folded = new Set<string>()
    let previous = ''
    for (const entry of entries) {
        const lower = entry.path.toLowerCase()
        if (!canonicalProductPath(platform, entry.path)) fail(`unsafe product path ${entry.path}`)
        if (exact.has(entry.path)) fail(`duplicate product path ${entry.path}`)
        if (folded.has(lower)) fail(`case-colliding product path ${entry.path}`)
        if (previous && entry.path <= previous) fail('product entries are not canonically sorted')
        exact.add(entry.path)
        folded.add(lower)
        previous = entry.path
        if (entry.type === 'directory') {
            if (entry.size !== 0 || entry.mode !== 0o755 || entry.sha256 !== '') {
                fail(`invalid product directory metadata: ${entry.path}`)
            }
        } else if (
            entry.size < 0 ||
            (entry.mode !== 0o644 && entry.mode !== 0o755) ||
            !SHA256_PATTERN.test(entry.sha256)
        ) {
            fail(`invalid product file metadata: ${entry.path}`)
        }
        for (
            let ancestor = path.posix.dirname(entry.path);
            ancestor !== '.';
            ancestor = path.posix.dirname(ancestor)
        ) {
            const parent = entries.find(
                candidate => candidate.path.toLowerCase() === ancestor.toLowerCase()
            )
            if (parent?.type === 'file') fail(`product file is an ancestor: ${parent.path}`)
        }
    }
    for (const required of requiredProductPaths(platform)) {
        const entry = entries.find(candidate => candidate.path === required)
        if (!entry || entry.type !== 'file' || entry.mode !== 0o755) {
            fail(
                `product tree is missing required executable ${required}; entries: ${entries
                    .map(
                        candidate =>
                            `${candidate.path}:${candidate.type}:${candidate.mode.toString(8)}`
                    )
                    .join(', ')}`
            )
        }
    }
}

function buildCRC32Table(): Uint32Array {
    const table = new Uint32Array(256)
    for (let index = 0; index < table.length; index += 1) {
        let value = index
        for (let bit = 0; bit < 8; bit += 1) {
            value = (value & 1) !== 0 ? 0xedb88320 ^ (value >>> 1) : value >>> 1
        }
        table[index] = value >>> 0
    }
    return table
}

function crc32(raw: Buffer): number {
    let value = 0xffffffff
    for (const byte of raw) {
        const tableValue = CRC32_TABLE[(value ^ byte) & 0xff]
        if (tableValue === undefined) fail('CRC32 lookup failed')
        value = tableValue ^ (value >>> 8)
    }
    return (value ^ 0xffffffff) >>> 0
}

function zipUnixMode(entry: ZipEntry): number {
    return entry.type === 'directory' ? 0o040000 | entry.mode : 0o100000 | entry.mode
}

function buildDeterministicZip(entries: ZipEntry[]): Buffer {
    if (entries.length === 0 || entries.length > MAX_ZIP_ENTRIES) {
        fail('ZIP entry count is outside the supported range')
    }
    const sorted = [...entries].sort((left, right) => compareCanonical(left.name, right.name))
    const names = new Set<string>()
    const folded = new Set<string>()
    const localParts: Buffer[] = []
    const centralParts: Buffer[] = []
    let offset = 0
    for (const entry of sorted) {
        const canonicalName =
            entry.type === 'directory' ? `${entry.name.replace(/\/+$/, '')}/` : entry.name
        const foldedName = canonicalName.toLowerCase()
        if (names.has(canonicalName) || folded.has(foldedName)) {
            fail(`duplicate or case-colliding ZIP entry ${canonicalName}`)
        }
        names.add(canonicalName)
        folded.add(foldedName)
        const name = Buffer.from(canonicalName, 'utf8')
        const size = entry.body.byteLength
        if (size > MAX_ZIP32_BYTES || offset > MAX_ZIP32_BYTES) {
            fail('ZIP64 output is not supported')
        }
        const checksum = crc32(entry.body)
        const local = Buffer.alloc(30)
        local.writeUInt32LE(0x04034b50, 0)
        local.writeUInt16LE(20, 4)
        local.writeUInt16LE(0x0800, 6)
        local.writeUInt16LE(0, 8)
        local.writeUInt16LE(0, 10)
        local.writeUInt16LE(0x21, 12)
        local.writeUInt32LE(checksum, 14)
        local.writeUInt32LE(size, 18)
        local.writeUInt32LE(size, 22)
        local.writeUInt16LE(name.byteLength, 26)
        local.writeUInt16LE(0, 28)
        localParts.push(local, name, entry.body)

        const central = Buffer.alloc(46)
        central.writeUInt32LE(0x02014b50, 0)
        central.writeUInt16LE(0x0314, 4)
        central.writeUInt16LE(20, 6)
        central.writeUInt16LE(0x0800, 8)
        central.writeUInt16LE(0, 10)
        central.writeUInt16LE(0, 12)
        central.writeUInt16LE(0x21, 14)
        central.writeUInt32LE(checksum, 16)
        central.writeUInt32LE(size, 20)
        central.writeUInt32LE(size, 24)
        central.writeUInt16LE(name.byteLength, 28)
        central.writeUInt16LE(0, 30)
        central.writeUInt16LE(0, 32)
        central.writeUInt16LE(0, 34)
        central.writeUInt16LE(0, 36)
        central.writeUInt32LE((zipUnixMode(entry) << 16) >>> 0, 38)
        central.writeUInt32LE(offset, 42)
        centralParts.push(central, name)
        offset += local.byteLength + name.byteLength + size
    }
    const centralOffset = offset
    const centralSize = centralParts.reduce((total, part) => total + part.byteLength, 0)
    if (centralOffset > MAX_ZIP32_BYTES || centralSize > MAX_ZIP32_BYTES) {
        fail('ZIP64 output is not supported')
    }
    const end = Buffer.alloc(22)
    end.writeUInt32LE(0x06054b50, 0)
    end.writeUInt16LE(0, 4)
    end.writeUInt16LE(0, 6)
    end.writeUInt16LE(sorted.length, 8)
    end.writeUInt16LE(sorted.length, 10)
    end.writeUInt32LE(centralSize, 12)
    end.writeUInt32LE(centralOffset, 16)
    end.writeUInt16LE(0, 20)
    return Buffer.concat([...localParts, ...centralParts, end])
}

function parseDeterministicZip(raw: Buffer, label: string): ParsedZipEntry[] {
    let endOffset = -1
    for (let offset = raw.length - 22; offset >= 0; offset -= 1) {
        if (raw.readUInt32LE(offset) === 0x06054b50) {
            endOffset = offset
            break
        }
    }
    if (endOffset < 0 || endOffset + 22 !== raw.length) fail(`${label} has no canonical EOCD`)
    const disk = raw.readUInt16LE(endOffset + 4)
    const centralDisk = raw.readUInt16LE(endOffset + 6)
    const count = raw.readUInt16LE(endOffset + 10)
    const diskCount = raw.readUInt16LE(endOffset + 8)
    const centralSize = raw.readUInt32LE(endOffset + 12)
    const centralOffset = raw.readUInt32LE(endOffset + 16)
    const commentLength = raw.readUInt16LE(endOffset + 20)
    if (
        disk !== 0 ||
        centralDisk !== 0 ||
        count !== diskCount ||
        count === 0 ||
        commentLength !== 0 ||
        centralOffset + centralSize !== endOffset
    ) {
        fail(`${label} has a noncanonical ZIP directory`)
    }
    let cursor = centralOffset
    let previous = ''
    const names = new Set<string>()
    const folded = new Set<string>()
    const entries: ParsedZipEntry[] = []
    for (let index = 0; index < count; index += 1) {
        if (cursor + 46 > endOffset || raw.readUInt32LE(cursor) !== 0x02014b50) {
            fail(`${label} central entry is invalid`)
        }
        const flags = raw.readUInt16LE(cursor + 8)
        const method = raw.readUInt16LE(cursor + 10)
        const checksum = raw.readUInt32LE(cursor + 16)
        const compressedSize = raw.readUInt32LE(cursor + 20)
        const size = raw.readUInt32LE(cursor + 24)
        const nameLength = raw.readUInt16LE(cursor + 28)
        const extraLength = raw.readUInt16LE(cursor + 30)
        const entryCommentLength = raw.readUInt16LE(cursor + 32)
        const external = raw.readUInt32LE(cursor + 38)
        const localOffset = raw.readUInt32LE(cursor + 42)
        const nameStart = cursor + 46
        const nameEnd = nameStart + nameLength
        if (
            flags !== 0x0800 ||
            method !== 0 ||
            compressedSize !== size ||
            extraLength !== 0 ||
            entryCommentLength !== 0 ||
            nameEnd > endOffset
        ) {
            fail(`${label} contains an unsupported ZIP entry`)
        }
        const name = raw.subarray(nameStart, nameEnd).toString('utf8')
        const lower = name.toLowerCase()
        if (
            name.length === 0 ||
            names.has(name) ||
            folded.has(lower) ||
            (previous && name <= previous)
        ) {
            fail(`${label} contains duplicate, case-colliding, or unsorted paths`)
        }
        names.add(name)
        folded.add(lower)
        previous = name
        if (localOffset + 30 > centralOffset || raw.readUInt32LE(localOffset) !== 0x04034b50) {
            fail(`${label} local entry is invalid`)
        }
        const localFlags = raw.readUInt16LE(localOffset + 6)
        const localMethod = raw.readUInt16LE(localOffset + 8)
        const localCRC = raw.readUInt32LE(localOffset + 14)
        const localCompressed = raw.readUInt32LE(localOffset + 18)
        const localSize = raw.readUInt32LE(localOffset + 22)
        const localNameLength = raw.readUInt16LE(localOffset + 26)
        const localExtraLength = raw.readUInt16LE(localOffset + 28)
        const localNameStart = localOffset + 30
        const localNameEnd = localNameStart + localNameLength
        const bodyStart = localNameEnd + localExtraLength
        const bodyEnd = bodyStart + size
        if (
            localFlags !== flags ||
            localMethod !== method ||
            localCRC !== checksum ||
            localCompressed !== size ||
            localSize !== size ||
            localExtraLength !== 0 ||
            localNameEnd > centralOffset ||
            raw.subarray(localNameStart, localNameEnd).toString('utf8') !== name ||
            bodyEnd > centralOffset
        ) {
            fail(`${label} local and central ZIP metadata differ`)
        }
        const body = raw.subarray(bodyStart, bodyEnd)
        if (crc32(body) !== checksum) fail(`${label} entry CRC32 differs: ${name}`)
        const unixMode = (external >>> 16) & 0xffff
        const directory = name.endsWith('/')
        const typeBits = unixMode & 0o170000
        if (
            (directory && typeBits !== 0o040000) ||
            (!directory && typeBits !== 0o100000) ||
            (unixMode & 0o777) !== (directory ? 0o755 : unixMode & 0o777)
        ) {
            fail(`${label} entry type or mode is invalid: ${name}`)
        }
        entries.push({
            name: directory ? name.slice(0, -1) : name,
            mode: unixMode & 0o777,
            type: directory ? 'directory' : 'file',
            body,
            crc32: checksum
        })
        cursor = nameEnd
    }
    if (cursor !== endOffset) fail(`${label} central directory has trailing data`)
    return entries
}

function signatureExpectedKind(
    platform: BootstrapPlatform,
    artifact: 'bootstrap' | 'helper' | 'product' | 'combination'
): SignatureKind {
    if (artifact === 'combination' || platform === 'linux') return 'detached-release'
    return platform === 'windows' ? 'authenticode' : 'apple-code-sign'
}

function validateSignaturePolicy(
    target: TargetConfig,
    artifact: 'bootstrap' | 'helper' | 'product' | 'combination',
    signature: SignatureConfig,
    official: boolean
): void {
    if (signature.status === 'unsigned') {
        if (
            signature.kind !== 'none' ||
            signature.identity !== '' ||
            signature.notarized ||
            signature.signatureFile !== '' ||
            signature.checksumFile !== ''
        ) {
            fail(
                `${targetKey(target.platform, target.architecture)} ${artifact} unsigned metadata is inconsistent`
            )
        }
        if (official) fail(`official output requires signed ${artifact} metadata`)
        return
    }
    if (signature.kind === 'none' || signature.identity.length === 0) {
        fail(`signed ${artifact} metadata is incomplete`)
    }
    if (official) {
        const expected = signatureExpectedKind(target.platform, artifact)
        if (signature.kind !== expected) {
            fail(`official output requires ${expected} ${artifact} metadata`)
        }
        if (target.platform === 'darwin' && artifact !== 'combination' && !signature.notarized) {
            fail(`official macOS ${artifact} metadata must record notarization`)
        }
        if (signature.kind === 'detached-release' && signature.signatureFile.length === 0) {
            fail(`official output requires signed detached ${artifact} metadata`)
        }
    }
}

function validateCatalogSignature(signature: SignatureConfig, official: boolean): void {
    if (!official) {
        if (
            signature.status !== 'unsigned' ||
            signature.kind !== 'none' ||
            signature.identity !== '' ||
            signature.notarized ||
            signature.signatureFile !== '' ||
            signature.checksumFile !== ''
        ) {
            fail('engineering catalog signature metadata must be unsigned')
        }
        return
    }
    if (
        signature.status !== 'signed' ||
        signature.kind !== 'detached-release' ||
        signature.identity.length === 0 ||
        signature.signatureFile.length === 0
    ) {
        fail('official output requires signed detached catalog metadata')
    }
}

function assertFinalContent(label: string, actual: FileDigest, signature: SignatureConfig): void {
    if (
        signature.expectedContentSHA256 !== undefined &&
        signature.expectedContentSize !== undefined &&
        (actual.sha256 !== signature.expectedContentSHA256 ||
            actual.size !== signature.expectedContentSize)
    ) {
        fail(`${label} changed after signature metadata was recorded`)
    }
    if (
        signature.status === 'signed' &&
        (signature.expectedContentSHA256 === undefined ||
            signature.expectedContentSize === undefined)
    ) {
        fail(`${label} signed metadata must attest final content SHA-256 and size`)
    }
}

function signatureRecord(
    config: SignatureConfig,
    content: FileDigest,
    outputDirectory: string,
    artifactFileName: string,
    checksumContent: FileDigest = content
): SignatureRecord {
    let signatureFile = ''
    let checksumFile = ''
    if (config.status === 'signed') {
        checksumFile = `${artifactFileName}.sha256`
        writeChecksum(outputDirectory, checksumFile, artifactFileName, checksumContent.sha256)
        if (config.signatureFile) {
            const source = path.resolve(config.signatureFile)
            const info = lstatSync(source)
            if (!info.isFile() || info.isSymbolicLink() || info.size < 1) {
                fail(`signature file is not a nonempty regular file: ${source}`)
            }
            signatureFile = path.basename(source)
            copyFileSync(source, path.join(outputDirectory, signatureFile))
        }
    }
    return {
        status: config.status,
        kind: config.kind,
        identity: config.identity,
        notarized: config.notarized,
        signatureFile,
        checksumFile,
        contentSHA256: content.sha256,
        contentSize: content.size
    }
}

function writeChecksum(
    directory: string,
    checksumFile: string,
    artifactFile: string,
    digest: string
): void {
    writeFileSync(path.join(directory, checksumFile), `${digest}  ${artifactFile}\n`, 'utf8')
}

function fileNames(platform: BootstrapPlatform): {
    bootstrap: string
    helper: string
    product: 'nvpair-product.zip'
    combination: string
} {
    const extension = platform === 'windows' ? '.exe' : ''
    return {
        bootstrap: `nvpair-host-bootstrap${extension}`,
        helper: `nvpair-host-helper${extension}`,
        product: 'nvpair-product.zip',
        combination: ''
    }
}

function fixedPaths(platform: BootstrapPlatform): {
    bootstrap: string
    helper: string
    product: string
} {
    if (platform === 'windows') {
        return {
            bootstrap: 'C:\\Program Files\\NVIDIA Corporation\\PAIR\\nvpair-host-bootstrap.exe',
            helper: 'C:\\Program Files\\NVIDIA Corporation\\PAIR\\nvpair-host-helper.exe',
            product: 'C:\\Program Files\\NVIDIA Corporation\\PAIR\\product'
        }
    }
    if (platform === 'darwin') {
        return {
            bootstrap: '/Library/PrivilegedHelperTools/nvpair-host-bootstrap',
            helper: '/Library/PrivilegedHelperTools/nvpair-host-helper',
            product: '/Applications/NVPAIR.app'
        }
    }
    return {
        bootstrap: '/usr/libexec/nvpair-host-bootstrap',
        helper: '/usr/libexec/nvpair-host-helper',
        product: '/opt/nvpair/product'
    }
}

function manifestEntries(entries: ProductEntry[]): ProductManifestEntry[] {
    return entries.map(entry => ({
        path: entry.path,
        type: entry.type,
        size: entry.size,
        mode: entry.mode,
        sha256: entry.sha256
    }))
}

function jsonBytes(value: JsonValue | object): Buffer {
    return Buffer.from(`${JSON.stringify(value, null, 2)}\n`, 'utf8')
}

function productZip(entries: ProductEntry[]): Buffer {
    return buildDeterministicZip(
        entries.map(entry => ({
            name: entry.path,
            mode: entry.mode,
            type: entry.type,
            body: entry.body
        }))
    )
}

function safeTargetOutput(root: string, key: string): string {
    const target = path.resolve(root, key)
    const relative = path.relative(root, target)
    if (
        relative === '' ||
        relative.startsWith('..') ||
        path.isAbsolute(relative) ||
        path.parse(root).root === root
    ) {
        fail(`unsafe packaging output path ${target}`)
    }
    return target
}

function prepareTargetDirectory(root: string, key: string): string {
    mkdirSync(root, { recursive: true })
    const target = safeTargetOutput(root, key)
    rmSync(target, { recursive: true, force: true })
    mkdirSync(path.join(target, 'payload'), { recursive: true })
    chmodSync(target, 0o755)
    chmodSync(path.join(target, 'payload'), 0o755)
    return target
}

function buildTarget(config: PackagingConfig, target: TargetConfig): CatalogTarget {
    const key = targetKey(target.platform, target.architecture)
    if (config.official && target.provenance !== 'official-release') {
        fail(`official output requires official-release provenance for ${key}`)
    }
    if (!config.official && target.provenance !== 'engineering') {
        fail(`engineering output requires engineering provenance for ${key}`)
    }
    validateSignaturePolicy(target, 'bootstrap', target.signatures.bootstrap, config.official)
    validateSignaturePolicy(target, 'helper', target.signatures.helper, config.official)
    validateSignaturePolicy(target, 'product', target.signatures.product, config.official)
    validateSignaturePolicy(target, 'combination', target.signatures.combination, config.official)

    const names = fileNames(target.platform)
    names.combination = `nvpair-bootstrap-${key}.zip`
    const paths = fixedPaths(target.platform)
    if (path.basename(target.bootstrapPath) !== names.bootstrap && config.official) {
        fail(`official bootstrap input must use fixed filename ${names.bootstrap}`)
    }
    if (path.basename(target.helperPath) !== names.helper && config.official) {
        fail(`official helper input must use fixed filename ${names.helper}`)
    }
    const bootstrapDigest = digestFile(target.bootstrapPath)
    const helperDigest = digestFile(target.helperPath)
    const product = collectProductEntries(target)
    assertFinalContent(`${key} bootstrap`, bootstrapDigest, target.signatures.bootstrap)
    assertFinalContent(`${key} helper`, helperDigest, target.signatures.helper)
    assertFinalContent(
        `${key} product tree`,
        { size: product.size, sha256: product.sha256 },
        target.signatures.product
    )

    const archive = productZip(product.entries)
    const productDigest = { size: archive.byteLength, sha256: sha256Buffer(archive) }
    const payload: PayloadManifest = {
        schemaVersion: 1,
        platform: target.platform,
        architecture: target.architecture,
        artifacts: [
            {
                id: 'nvpair',
                version: target.versions.product,
                fileName: names.product,
                byteCount: productDigest.size,
                sha256: productDigest.sha256,
                files: manifestEntries(product.entries)
            },
            {
                id: 'nvpair-host-helper',
                version: target.versions.helper,
                fileName: names.helper,
                byteCount: helperDigest.size,
                sha256: helperDigest.sha256,
                files: []
            }
        ]
    }
    const manifest = jsonBytes(payload)
    const output = prepareTargetDirectory(config.outputDirectory, key)
    const bootstrap = readFileSync(target.bootstrapPath)
    const helper = readFileSync(target.helperPath)
    writeFileSync(path.join(output, names.bootstrap), bootstrap)
    writeFileSync(path.join(output, 'payload', 'manifest.json'), manifest)
    writeFileSync(path.join(output, 'payload', names.helper), helper)
    writeFileSync(path.join(output, 'payload', names.product), archive)
    chmodSync(path.join(output, names.bootstrap), 0o755)
    chmodSync(path.join(output, 'payload', names.helper), 0o755)
    chmodSync(path.join(output, 'payload', names.product), 0o644)
    chmodSync(path.join(output, 'payload', 'manifest.json'), 0o644)

    const combination = buildDeterministicZip([
        { name: names.bootstrap, mode: 0o755, type: 'file', body: bootstrap },
        { name: 'payload', mode: 0o755, type: 'directory', body: Buffer.alloc(0) },
        { name: 'payload/manifest.json', mode: 0o644, type: 'file', body: manifest },
        { name: `payload/${names.helper}`, mode: 0o755, type: 'file', body: helper },
        { name: `payload/${names.product}`, mode: 0o644, type: 'file', body: archive }
    ])
    const combinationDigest = {
        size: combination.byteLength,
        sha256: sha256Buffer(combination)
    }
    assertFinalContent(`${key} combination`, combinationDigest, target.signatures.combination)
    writeFileSync(path.join(output, names.combination), combination)
    chmodSync(path.join(output, names.combination), 0o644)

    const bootstrapSignature = signatureRecord(
        target.signatures.bootstrap,
        bootstrapDigest,
        output,
        names.bootstrap
    )
    const helperSignature = signatureRecord(
        target.signatures.helper,
        helperDigest,
        output,
        names.helper
    )
    const productSignature = signatureRecord(
        target.signatures.product,
        { size: product.size, sha256: product.sha256 },
        output,
        names.product,
        productDigest
    )
    const combinationSignature = signatureRecord(
        target.signatures.combination,
        combinationDigest,
        output,
        names.combination
    )
    const catalogTarget: CatalogTarget = {
        target: {
            platform: target.platform,
            architecture: target.architecture
        },
        roles: ['desktop', 'headless'],
        bootstrap: {
            identity: {
                id: 'nvpair-host-bootstrap',
                version: target.versions.bootstrap,
                sha256: bootstrapDigest.sha256,
                path: paths.bootstrap
            },
            fileName: names.bootstrap,
            size: bootstrapDigest.size,
            provenance: target.provenance,
            signature: bootstrapSignature
        },
        helper: {
            identity: {
                id: 'nvpair-host-helper',
                version: target.versions.helper,
                sha256: helperDigest.sha256,
                path: paths.helper
            },
            fileName: names.helper,
            size: helperDigest.size,
            provenance: target.provenance,
            signature: helperSignature
        },
        product: {
            identity: {
                id: 'nvpair',
                version: target.versions.product,
                sha256: productDigest.sha256,
                path: paths.product
            },
            fileName: names.product,
            size: productDigest.size,
            provenance: target.provenance,
            signature: productSignature
        },
        combination: {
            fileName: names.combination,
            size: combinationDigest.size,
            sha256: combinationDigest.sha256,
            provenance: target.provenance,
            signature: combinationSignature
        }
    }
    writeFileSync(path.join(output, 'target-catalog-entry.json'), jsonBytes(catalogTarget), 'utf8')
    return catalogTarget
}

function sortedMatrixTargets(targets: TargetConfig[]): TargetConfig[] {
    const byKey = new Map(
        targets.map(target => [targetKey(target.platform, target.architecture), target])
    )
    const ordered: TargetConfig[] = []
    for (const expected of TARGET_ORDER) {
        const target = byKey.get(targetKey(expected.platform, expected.architecture))
        if (!target)
            fail(`matrix is missing ${targetKey(expected.platform, expected.architecture)}`)
        ordered.push(target)
    }
    if (byKey.size !== TARGET_ORDER.length) fail('matrix must contain exactly six targets')
    return ordered
}

function catalogSignatureRecord(config: SignatureConfig, outputDirectory: string): SignatureRecord {
    let signatureFile = ''
    if (config.status === 'signed') {
        const source = path.resolve(config.signatureFile)
        const info = lstatSync(source)
        if (!info.isFile() || info.isSymbolicLink() || info.size < 1) {
            fail(`catalog signature file is not a nonempty regular file: ${source}`)
        }
        signatureFile = path.basename(source)
        copyFileSync(source, path.join(outputDirectory, signatureFile))
    }
    return {
        status: config.status,
        kind: config.kind,
        identity: config.identity,
        notarized: config.notarized,
        signatureFile,
        checksumFile: CATALOG_CHECKSUM_FILE,
        contentSHA256: '',
        contentSize: 0
    }
}

function buildMatrix(config: PackagingConfig): BootstrapCatalog {
    validateCatalogSignature(config.catalogSignature, config.official)
    mkdirSync(config.outputDirectory, { recursive: true })
    rmSync(path.join(config.outputDirectory, CATALOG_FILE), { force: true })
    rmSync(path.join(config.outputDirectory, CATALOG_CHECKSUM_FILE), { force: true })
    const targets = sortedMatrixTargets(config.targets).map(target => buildTarget(config, target))
    const catalog: BootstrapCatalog = {
        schemaVersion: 1,
        integrity: {
            checksumAlgorithm: 'sha256',
            checksumFile: CATALOG_CHECKSUM_FILE,
            signature: catalogSignatureRecord(config.catalogSignature, config.outputDirectory)
        },
        targets
    }
    const raw = jsonBytes(catalog)
    writeFileSync(path.join(config.outputDirectory, CATALOG_FILE), raw)
    writeChecksum(config.outputDirectory, CATALOG_CHECKSUM_FILE, CATALOG_FILE, sha256Buffer(raw))
    return catalog
}

function parseSignatureRecord(value: JsonValue | undefined, label: string): SignatureRecord {
    const object = jsonObject(value ?? null, label)
    exactKeys(object, label, [
        'status',
        'kind',
        'identity',
        'notarized',
        'signatureFile',
        'checksumFile',
        'contentSHA256',
        'contentSize'
    ])
    const contentSHA256 = jsonString(object['contentSHA256'], `${label}.contentSHA256`)
    const contentSize = jsonInteger(object['contentSize'], `${label}.contentSize`)
    if (contentSHA256 !== '' && !SHA256_PATTERN.test(contentSHA256)) {
        fail(`${label}.contentSHA256 is invalid`)
    }
    if (contentSize < 0 || contentSize > MAX_ARTIFACT_BYTES) {
        fail(`${label}.contentSize is invalid`)
    }
    return {
        status: parseSignatureStatus(object['status'], `${label}.status`),
        kind: parseSignatureKind(object['kind'], `${label}.kind`),
        identity: jsonString(object['identity'], `${label}.identity`),
        notarized: jsonBoolean(object['notarized'], `${label}.notarized`),
        signatureFile: jsonString(object['signatureFile'], `${label}.signatureFile`),
        checksumFile: jsonString(object['checksumFile'], `${label}.checksumFile`),
        contentSHA256,
        contentSize
    }
}

function parseIdentity(value: JsonValue | undefined, label: string): ArtifactIdentity {
    const object = jsonObject(value ?? null, label)
    exactKeys(object, label, ['id', 'version', 'sha256', 'path'])
    const version = jsonString(object['version'], `${label}.version`)
    const sha256 = jsonString(object['sha256'], `${label}.sha256`)
    if (!VERSION_PATTERN.test(version) || !SHA256_PATTERN.test(sha256)) {
        fail(`${label} version or SHA-256 is invalid`)
    }
    return {
        id: jsonString(object['id'], `${label}.id`),
        version,
        sha256,
        path: jsonString(object['path'], `${label}.path`)
    }
}

function parseCatalogArtifact(value: JsonValue | undefined, label: string): CatalogArtifact {
    const object = jsonObject(value ?? null, label)
    exactKeys(object, label, ['identity', 'fileName', 'size', 'provenance', 'signature'])
    const size = jsonInteger(object['size'], `${label}.size`)
    if (size < 1 || size > MAX_ARTIFACT_BYTES) fail(`${label}.size is invalid`)
    return {
        identity: parseIdentity(object['identity'], `${label}.identity`),
        fileName: jsonString(object['fileName'], `${label}.fileName`),
        size,
        provenance: parseProvenance(object['provenance'], `${label}.provenance`),
        signature: parseSignatureRecord(object['signature'], `${label}.signature`)
    }
}

function parseCatalogTarget(value: JsonValue, index: number): CatalogTarget {
    const label = `catalog.targets[${index}]`
    const object = jsonObject(value, label)
    exactKeys(object, label, ['target', 'roles', 'bootstrap', 'helper', 'product', 'combination'])
    const targetObject = jsonObject(object['target'] ?? null, `${label}.target`)
    exactKeys(targetObject, `${label}.target`, ['platform', 'architecture'])
    const roles = jsonArray(object['roles'], `${label}.roles`)
    if (roles.length !== 2 || roles[0] !== 'desktop' || roles[1] !== 'headless') {
        fail(`${label}.roles is invalid`)
    }
    const combinationObject = jsonObject(object['combination'] ?? null, `${label}.combination`)
    exactKeys(combinationObject, `${label}.combination`, [
        'fileName',
        'size',
        'sha256',
        'provenance',
        'signature'
    ])
    const combinationSize = jsonInteger(combinationObject['size'], `${label}.combination.size`)
    const combinationSHA = jsonString(combinationObject['sha256'], `${label}.combination.sha256`)
    if (
        combinationSize < 1 ||
        combinationSize > MAX_ARTIFACT_BYTES ||
        !SHA256_PATTERN.test(combinationSHA)
    ) {
        fail(`${label}.combination identity is invalid`)
    }
    return {
        target: {
            platform: parsePlatform(targetObject['platform'], `${label}.target.platform`),
            architecture: parseArchitecture(
                targetObject['architecture'],
                `${label}.target.architecture`
            )
        },
        roles: ['desktop', 'headless'],
        bootstrap: parseCatalogArtifact(object['bootstrap'], `${label}.bootstrap`),
        helper: parseCatalogArtifact(object['helper'], `${label}.helper`),
        product: parseCatalogArtifact(object['product'], `${label}.product`),
        combination: {
            fileName: jsonString(combinationObject['fileName'], `${label}.combination.fileName`),
            size: combinationSize,
            sha256: combinationSHA,
            provenance: parseProvenance(
                combinationObject['provenance'],
                `${label}.combination.provenance`
            ),
            signature: parseSignatureRecord(
                combinationObject['signature'],
                `${label}.combination.signature`
            )
        }
    }
}

function parseCatalog(raw: Buffer): BootstrapCatalog {
    const value: JsonValue = JSON.parse(raw.toString('utf8'))
    const object = jsonObject(value, 'catalog')
    exactKeys(object, 'catalog', ['schemaVersion', 'integrity', 'targets'])
    if (object['schemaVersion'] !== 1) fail('catalog schemaVersion must be 1')
    const integrity = jsonObject(object['integrity'] ?? null, 'catalog.integrity')
    exactKeys(integrity, 'catalog.integrity', ['checksumAlgorithm', 'checksumFile', 'signature'])
    if (integrity['checksumAlgorithm'] !== 'sha256') {
        fail('catalog checksum algorithm must be sha256')
    }
    const targets = jsonArray(object['targets'], 'catalog.targets').map(parseCatalogTarget)
    if (targets.length !== TARGET_ORDER.length) fail('catalog must contain six targets')
    for (let index = 0; index < TARGET_ORDER.length; index += 1) {
        const expected = TARGET_ORDER[index]
        const actual = targets[index]
        if (
            !expected ||
            !actual ||
            actual.target.platform !== expected.platform ||
            actual.target.architecture !== expected.architecture
        ) {
            fail('catalog target order is invalid')
        }
    }
    return {
        schemaVersion: 1,
        integrity: {
            checksumAlgorithm: 'sha256',
            checksumFile: jsonString(integrity['checksumFile'], 'catalog.integrity.checksumFile'),
            signature: parseSignatureRecord(integrity['signature'], 'catalog.integrity.signature')
        },
        targets
    }
}

function parsePayloadManifest(raw: Buffer): PayloadManifest {
    const value: JsonValue = JSON.parse(raw.toString('utf8'))
    const object = jsonObject(value, 'payload manifest')
    exactKeys(object, 'payload manifest', [
        'schemaVersion',
        'platform',
        'architecture',
        'artifacts'
    ])
    if (object['schemaVersion'] !== 1) fail('payload manifest schemaVersion must be 1')
    const artifacts = jsonArray(object['artifacts'], 'payload manifest artifacts')
    if (artifacts.length !== 2) fail('payload manifest must contain two artifacts')
    const parsedArtifacts: PayloadArtifact[] = artifacts.map((artifactValue, artifactIndex) => {
        const label = `payload manifest artifacts[${artifactIndex}]`
        const artifact = jsonObject(artifactValue, label)
        exactKeys(artifact, label, ['id', 'version', 'fileName', 'byteCount', 'sha256', 'files'])
        const byteCount = jsonInteger(artifact['byteCount'], `${label}.byteCount`)
        const digest = jsonString(artifact['sha256'], `${label}.sha256`)
        if (byteCount < 1 || byteCount > MAX_ARTIFACT_BYTES || !SHA256_PATTERN.test(digest)) {
            fail(`${label} byte count or SHA-256 is invalid`)
        }
        const files = jsonArray(artifact['files'], `${label}.files`).map(
            (fileValue, fileIndex): ProductManifestEntry => {
                const fileLabel = `${label}.files[${fileIndex}]`
                const file = jsonObject(fileValue, fileLabel)
                exactKeys(file, fileLabel, ['path', 'type', 'size', 'mode', 'sha256'])
                const type = file['type']
                if (type !== 'file' && type !== 'directory') {
                    fail(`${fileLabel}.type is invalid`)
                }
                return {
                    path: jsonString(file['path'], `${fileLabel}.path`),
                    type,
                    size: jsonInteger(file['size'], `${fileLabel}.size`),
                    mode: jsonInteger(file['mode'], `${fileLabel}.mode`),
                    sha256: jsonString(file['sha256'], `${fileLabel}.sha256`)
                }
            }
        )
        return {
            id: jsonString(artifact['id'], `${label}.id`),
            version: jsonString(artifact['version'], `${label}.version`),
            fileName: jsonString(artifact['fileName'], `${label}.fileName`),
            byteCount,
            sha256: digest,
            files
        }
    })
    return {
        schemaVersion: 1,
        platform: parsePlatform(object['platform'], 'payload manifest platform'),
        architecture: parseArchitecture(object['architecture'], 'payload manifest architecture'),
        artifacts: parsedArtifacts
    }
}

function entryByName(entries: ParsedZipEntry[], name: string): ParsedZipEntry {
    const entry = entries.find(candidate => candidate.name === name)
    if (!entry) fail(`combination is missing ${name}`)
    return entry
}

function verifySignatureRecord(
    directory: string,
    artifactFileName: string,
    signature: SignatureRecord,
    expectedContent: FileDigest,
    official: boolean,
    expectedKind: SignatureKind,
    checksumContent: FileDigest = expectedContent
): void {
    if (
        signature.contentSHA256 !== expectedContent.sha256 ||
        signature.contentSize !== expectedContent.size
    ) {
        fail(`catalog signature content identity differs for ${artifactFileName}`)
    }
    if (signature.status === 'unsigned') {
        if (official) fail(`official verification requires signed ${artifactFileName}`)
        if (
            signature.kind !== 'none' ||
            signature.identity !== '' ||
            signature.notarized ||
            signature.signatureFile !== '' ||
            signature.checksumFile !== ''
        ) {
            fail(`unsigned signature metadata is inconsistent for ${artifactFileName}`)
        }
        return
    }
    if (signature.kind === 'none' || signature.identity.length === 0) {
        fail(`signed signature metadata is incomplete for ${artifactFileName}`)
    }
    if (official && signature.kind !== expectedKind) {
        fail(`official signature kind differs for ${artifactFileName}`)
    }
    if (signature.checksumFile.length === 0) {
        fail(`signed artifact has no checksum metadata: ${artifactFileName}`)
    }
    verifyChecksumFile(
        path.join(directory, signature.checksumFile),
        artifactFileName,
        checksumContent.sha256
    )
    if (signature.kind === 'detached-release') {
        if (signature.signatureFile.length === 0) {
            fail(`detached signature is missing for ${artifactFileName}`)
        }
        const signaturePath = path.join(directory, signature.signatureFile)
        const info = lstatSync(signaturePath)
        if (!info.isFile() || info.isSymbolicLink() || info.size < 1) {
            fail(`detached signature is invalid for ${artifactFileName}`)
        }
    }
}

function verifyChecksumFile(filePath: string, artifact: string, digest: string): void {
    const expected = `${digest}  ${artifact}\n`
    if (readFileSync(filePath, 'utf8') !== expected) {
        fail(`checksum metadata differs for ${artifact}`)
    }
}

function verifyProductArchive(
    raw: Buffer,
    manifest: ProductManifestEntry[],
    platform: BootstrapPlatform
): VerifiedProductArchive {
    const zip = parseDeterministicZip(raw, 'product ZIP')
    if (zip.length !== manifest.length) fail('product ZIP and manifest entry counts differ')
    const productEntries: ProductEntry[] = []
    for (let index = 0; index < manifest.length; index += 1) {
        const expected = manifest[index]
        const actual = zip[index]
        if (!expected || !actual) fail('product ZIP and manifest ordering differs')
        if (
            actual.name !== expected.path ||
            actual.type !== expected.type ||
            actual.mode !== expected.mode ||
            actual.body.byteLength !== expected.size
        ) {
            fail(`product ZIP metadata differs at ${expected.path}`)
        }
        const digest = actual.type === 'file' ? sha256Buffer(actual.body) : ''
        if (digest !== expected.sha256) fail(`product ZIP SHA-256 differs at ${expected.path}`)
        productEntries.push({
            path: expected.path,
            type: expected.type,
            size: expected.size,
            mode: expected.mode,
            sha256: expected.sha256,
            body: actual.body
        })
    }
    validateProductEntries(platform, productEntries)
    return {
        archive: { size: raw.byteLength, sha256: sha256Buffer(raw) },
        tree: digestProductEntries(productEntries)
    }
}

function verifyTarget(root: string, target: CatalogTarget, official: boolean): void {
    const key = targetKey(target.target.platform, target.target.architecture)
    const directory = path.join(root, key)
    const archivePath = path.join(directory, target.combination.fileName)
    const combinationRaw = readFileSync(archivePath)
    const combinationDigest = {
        size: combinationRaw.byteLength,
        sha256: sha256Buffer(combinationRaw)
    }
    if (
        combinationDigest.size !== target.combination.size ||
        combinationDigest.sha256 !== target.combination.sha256
    ) {
        fail(`combination digest differs for ${key}`)
    }
    const entries = parseDeterministicZip(combinationRaw, `${key} combination`)
    const names = fileNames(target.target.platform)
    names.combination = target.combination.fileName
    const expectedNames = [
        names.bootstrap,
        'payload',
        'payload/manifest.json',
        `payload/${names.helper}`,
        `payload/${names.product}`
    ]
    if (
        entries.length !== expectedNames.length ||
        entries.some((entry, index) => entry.name !== expectedNames[index])
    ) {
        fail(`${key} combination contents are not canonical`)
    }
    const bootstrap = entryByName(entries, names.bootstrap)
    const helper = entryByName(entries, `payload/${names.helper}`)
    const product = entryByName(entries, `payload/${names.product}`)
    const manifestEntry = entryByName(entries, 'payload/manifest.json')
    if (
        bootstrap.type !== 'file' ||
        bootstrap.mode !== 0o755 ||
        helper.type !== 'file' ||
        helper.mode !== 0o755 ||
        product.type !== 'file' ||
        product.mode !== 0o644
    ) {
        fail(`${key} executable or payload modes are invalid`)
    }
    const manifest = parsePayloadManifest(manifestEntry.body)
    if (
        manifest.platform !== target.target.platform ||
        manifest.architecture !== target.target.architecture
    ) {
        fail(`${key} payload target differs`)
    }
    const productManifest = manifest.artifacts[0]
    const helperManifest = manifest.artifacts[1]
    if (
        !productManifest ||
        !helperManifest ||
        productManifest.id !== 'nvpair' ||
        helperManifest.id !== 'nvpair-host-helper' ||
        productManifest.fileName !== names.product ||
        helperManifest.fileName !== names.helper ||
        helperManifest.files.length !== 0
    ) {
        fail(`${key} payload artifact identities differ`)
    }
    const bootstrapDigest = {
        size: bootstrap.body.byteLength,
        sha256: sha256Buffer(bootstrap.body)
    }
    const helperDigest = { size: helper.body.byteLength, sha256: sha256Buffer(helper.body) }
    const verifiedProduct = verifyProductArchive(
        product.body,
        productManifest.files,
        target.target.platform
    )
    const productDigest = verifiedProduct.archive
    if (
        bootstrapDigest.size !== target.bootstrap.size ||
        bootstrapDigest.sha256 !== target.bootstrap.identity.sha256 ||
        helperDigest.size !== target.helper.size ||
        helperDigest.sha256 !== target.helper.identity.sha256 ||
        productDigest.size !== target.product.size ||
        productDigest.sha256 !== target.product.identity.sha256 ||
        productDigest.size !== productManifest.byteCount ||
        productDigest.sha256 !== productManifest.sha256 ||
        helperDigest.size !== helperManifest.byteCount ||
        helperDigest.sha256 !== helperManifest.sha256
    ) {
        fail(`${key} catalog, payload, and archive digests differ`)
    }
    const paths = fixedPaths(target.target.platform)
    if (
        target.bootstrap.identity.id !== 'nvpair-host-bootstrap' ||
        target.bootstrap.identity.path !== paths.bootstrap ||
        target.helper.identity.id !== 'nvpair-host-helper' ||
        target.helper.identity.path !== paths.helper ||
        target.product.identity.id !== 'nvpair' ||
        target.product.identity.path !== paths.product ||
        target.bootstrap.fileName !== names.bootstrap ||
        target.helper.fileName !== names.helper ||
        target.product.fileName !== names.product
    ) {
        fail(`${key} fixed catalog identity differs`)
    }
    verifySignatureRecord(
        directory,
        names.bootstrap,
        target.bootstrap.signature,
        bootstrapDigest,
        official,
        signatureExpectedKind(target.target.platform, 'bootstrap')
    )
    verifySignatureRecord(
        directory,
        names.helper,
        target.helper.signature,
        helperDigest,
        official,
        signatureExpectedKind(target.target.platform, 'helper')
    )
    verifySignatureRecord(
        directory,
        names.product,
        target.product.signature,
        verifiedProduct.tree,
        official,
        signatureExpectedKind(target.target.platform, 'product'),
        productDigest
    )
    verifySignatureRecord(
        directory,
        target.combination.fileName,
        target.combination.signature,
        combinationDigest,
        official,
        signatureExpectedKind(target.target.platform, 'combination')
    )
}

function verifyCatalog(root: string, officialOverride: boolean): void {
    const catalogPath = path.join(root, CATALOG_FILE)
    const raw = readFileSync(catalogPath)
    const catalog = parseCatalog(raw)
    if (catalog.integrity.checksumFile !== CATALOG_CHECKSUM_FILE) {
        fail('catalog checksum filename differs')
    }
    verifyChecksumFile(path.join(root, CATALOG_CHECKSUM_FILE), CATALOG_FILE, sha256Buffer(raw))
    const official =
        officialOverride ||
        catalog.targets.every(target => target.bootstrap.provenance === 'official-release')
    if (official) {
        if (
            catalog.integrity.signature.status !== 'signed' ||
            catalog.integrity.signature.kind !== 'detached-release' ||
            catalog.integrity.signature.identity.length === 0 ||
            catalog.integrity.signature.signatureFile.length === 0
        ) {
            fail('official verification requires a signed detached catalog identity')
        }
        const signaturePath = path.join(root, catalog.integrity.signature.signatureFile)
        const info = lstatSync(signaturePath)
        if (!info.isFile() || info.isSymbolicLink() || info.size < 1) {
            fail('catalog detached signature is missing')
        }
    } else if (
        catalog.integrity.signature.status !== 'unsigned' ||
        catalog.integrity.signature.kind !== 'none'
    ) {
        fail('engineering catalog signature metadata is inconsistent')
    }
    for (const target of catalog.targets) verifyTarget(root, target, official)
    process.stdout.write(
        `[bootstrap-package] verified ${catalog.targets.length} bootstrap combinations in ${root}\n`
    )
}

function parseCliManifest(filePath: string): CliManifest {
    const raw: JsonValue = JSON.parse(readFileSync(filePath, 'utf8'))
    const object = jsonObject(raw, 'cli-bin manifest')
    exactKeys(object, 'cli-bin manifest', [
        'source',
        'sourceFingerprint',
        'services',
        'platform',
        'arch',
        'components',
        'files',
        'builtAt'
    ])
    const componentsObject = jsonObject(object['components'] ?? null, 'cli-bin components')
    const components: { [name: string]: string } = {}
    for (const [name, value] of Object.entries(componentsObject)) {
        components[name] = jsonString(value, `cli-bin component ${name}`)
    }
    const files = jsonArray(object['files'], 'cli-bin files').map(
        (fileValue, index): CliManifestFile => {
            const label = `cli-bin files[${index}]`
            const file = jsonObject(fileValue, label)
            exactKeys(file, label, ['fileName', 'size', 'sha256'])
            const fileName = jsonString(file['fileName'], `${label}.fileName`)
            const size = jsonInteger(file['size'], `${label}.size`)
            const digest = jsonString(file['sha256'], `${label}.sha256`)
            if (!SAFE_FILE_NAME.test(fileName) || size < 1 || !SHA256_PATTERN.test(digest)) {
                fail(`${label} is invalid`)
            }
            return { fileName, size, sha256: digest }
        }
    )
    return {
        source: jsonString(object['source'], 'cli-bin source'),
        sourceFingerprint: jsonString(object['sourceFingerprint'], 'cli-bin sourceFingerprint'),
        services: jsonString(object['services'], 'cli-bin services'),
        platform: jsonString(object['platform'], 'cli-bin platform'),
        arch: jsonString(object['arch'], 'cli-bin arch'),
        components,
        files,
        builtAt: jsonString(object['builtAt'], 'cli-bin builtAt')
    }
}

function cliBinFiles(directory: string): CliManifestFile[] {
    return readdirSync(directory, { withFileTypes: true })
        .filter(entry => entry.isFile() && entry.name !== 'manifest.json')
        .map(entry => {
            if (!SAFE_FILE_NAME.test(entry.name)) fail(`invalid cli-bin filename ${entry.name}`)
            const digest = digestFile(path.join(directory, entry.name))
            return { fileName: entry.name, size: digest.size, sha256: digest.sha256 }
        })
        .sort((left, right) => compareCanonical(left.fileName, right.fileName))
}

function verifyCliManifest(directory: string): void {
    const manifest = parseCliManifest(path.join(directory, 'manifest.json'))
    const actual = cliBinFiles(directory)
    const expected = [...manifest.files].sort((left, right) =>
        compareCanonical(left.fileName, right.fileName)
    )
    if (
        actual.length !== expected.length ||
        actual.some((file, index) => {
            const recorded = expected[index]
            return (
                !recorded ||
                file.fileName !== recorded.fileName ||
                file.size !== recorded.size ||
                file.sha256 !== recorded.sha256
            )
        })
    ) {
        fail('stale cli-bin manifest: final signed bytes differ from recorded hashes')
    }
    process.stdout.write(`[bootstrap-package] verified final cli-bin manifest in ${directory}\n`)
}

function refreshCliManifest(directory: string): void {
    const manifestPath = path.join(directory, 'manifest.json')
    const manifest = parseCliManifest(manifestPath)
    const refreshed: CliManifest = {
        ...manifest,
        files: cliBinFiles(directory)
    }
    writeFileSync(manifestPath, jsonBytes(refreshed))
    verifyCliManifest(directory)
}

function stageProduct(
    platform: BootstrapPlatform,
    appPath: string,
    tuiPath: string,
    brokerPath: string,
    output: string
): void {
    for (const input of [appPath, tuiPath, brokerPath]) digestFile(input)
    const resolvedOutput = path.resolve(output)
    if (path.parse(resolvedOutput).root === resolvedOutput) {
        fail(`unsafe product staging output ${resolvedOutput}`)
    }
    rmSync(resolvedOutput, { recursive: true, force: true })
    const extension = platform === 'windows' ? '.exe' : ''
    const appRelative =
        platform === 'darwin'
            ? 'Contents/MacOS/PAIR'
            : platform === 'windows'
              ? 'PAIR.exe'
              : 'nvpair'
    const cliRelative = platform === 'darwin' ? 'Contents/Resources/cli-bin' : 'resources/cli-bin'
    const files = [
        { source: appPath, relative: appRelative },
        { source: tuiPath, relative: `${cliRelative}/nvpair-tui${extension}` },
        { source: brokerPath, relative: `${cliRelative}/nvpair-ui-broker${extension}` }
    ]
    for (const file of files) {
        const destination = path.join(resolvedOutput, ...file.relative.split('/'))
        mkdirSync(path.dirname(destination), { recursive: true })
        copyFileSync(file.source, destination)
        chmodSync(destination, 0o755)
    }
    process.stdout.write(
        `[bootstrap-package] staged ${platform} engineering product at ${resolvedOutput}\n`
    )
}

function componentVersions(): { [name: string]: string } {
    const versionsPath = path.join(SERVICES_ROOT, 'versions.json')
    const raw: JsonValue = JSON.parse(readFileSync(versionsPath, 'utf8'))
    const root = jsonObject(raw, 'services versions')
    const components = jsonObject(root['components'] ?? null, 'services versions components')
    const result: { [name: string]: string } = {}
    for (const [name, value] of Object.entries(components)) {
        const version = jsonString(value, `services version ${name}`)
        if (!VERSION_PATTERN.test(version)) fail(`services version ${name} is invalid`)
        result[name] = version
    }
    return result
}

function selectedBuildTargets(value: string | null): ReadonlyArray<{
    platform: BootstrapPlatform
    architecture: BootstrapArchitecture
}> {
    if (value === null) return TARGET_ORDER
    const selected = TARGET_ORDER.find(
        target => targetKey(target.platform, target.architecture) === value
    )
    if (!selected) fail(`unsupported bootstrap build target ${value}`)
    return [selected]
}

function goBuildPlans(
    targetSelector: string | null,
    outputRoot: string,
    includeProductTools: boolean
): GoBuildPlan[] {
    const versions = componentVersions()
    const plans: GoBuildPlan[] = []
    const components: GoBuildPlan['component'][] = ['nvpair-host-bootstrap', 'nvpair-host-helper']
    if (includeProductTools) components.push('nvpair-tui', 'nvpair-ui-broker')
    for (const target of selectedBuildTargets(targetSelector)) {
        const key = targetKey(target.platform, target.architecture)
        const extension = target.platform === 'windows' ? '.exe' : ''
        const goos = target.platform === 'windows' ? 'windows' : target.platform
        for (const component of components) {
            const version = versions[component]
            if (!version) fail(`services versions is missing ${component}`)
            const output = path.join(outputRoot, key, `${component}${extension}`)
            plans.push({
                component,
                cwd: path.join(SERVICES_ROOT, component),
                output,
                env: {
                    CGO_ENABLED: '0',
                    GOOS: goos,
                    GOARCH: target.architecture,
                    GOFLAGS: '-buildvcs=false'
                },
                args: [
                    'build',
                    '-buildvcs=false',
                    '-trimpath',
                    '-ldflags',
                    `-s -w -X main.Version=${version}`,
                    '-o',
                    output,
                    '.'
                ]
            })
        }
    }
    return plans
}

function executeGoBuilds(plans: GoBuildPlan[]): void {
    for (const plan of plans) {
        mkdirSync(path.dirname(plan.output), { recursive: true })
        const result = spawnSync('go', plan.args, {
            cwd: plan.cwd,
            env: { ...process.env, ...plan.env },
            stdio: 'inherit'
        })
        if (result.status !== 0) {
            fail(`go build failed for ${plan.component} (${plan.env.GOOS}/${plan.env.GOARCH})`)
        }
        if (plan.env.GOOS !== 'windows') chmodSync(plan.output, 0o755)
        process.stdout.write(
            `[bootstrap-package] built ${plan.component} for ${plan.env.GOOS}/${plan.env.GOARCH}\n`
        )
    }
}

function optionalArgument(name: string): string | null {
    const equals = process.argv.find(item => item.startsWith(`--${name}=`))
    if (equals) return equals.slice(name.length + 3)
    const index = process.argv.indexOf(`--${name}`)
    const value = index >= 0 ? process.argv[index + 1] : undefined
    if (!value || value.startsWith('--')) return null
    return value
}

function argument(name: string): string {
    const value = optionalArgument(name)
    if (value === null) fail(`missing --${name}`)
    return value
}

function main(): void {
    const command = process.argv[2]
    if (command === 'build-target') {
        const config = parsePackagingConfig(path.resolve(argument('config')))
        if (config.targets.length !== 1) fail('build-target requires exactly one target')
        validateCatalogSignature(config.catalogSignature, false)
        const target = config.targets[0]
        if (!target) fail('build-target target is missing')
        buildTarget(config, target)
        process.stdout.write(
            `[bootstrap-package] built ${targetKey(target.platform, target.architecture)} in ${config.outputDirectory}\n`
        )
        return
    }
    if (command === 'build-matrix') {
        const config = parsePackagingConfig(path.resolve(argument('config')))
        buildMatrix(config)
        verifyCatalog(config.outputDirectory, config.official)
        return
    }
    if (command === 'verify') {
        verifyCatalog(path.resolve(argument('root')), process.argv.includes('--official'))
        return
    }
    if (command === 'verify-cli-manifest') {
        verifyCliManifest(path.resolve(argument('cli-bin')))
        return
    }
    if (command === 'refresh-cli-manifest') {
        refreshCliManifest(path.resolve(argument('cli-bin')))
        return
    }
    if (command === 'plan-go') {
        const output = path.resolve(
            optionalArgument('output') ?? path.join(DESKTOP_ROOT, 'bootstrap-bin')
        )
        process.stdout.write(
            `${JSON.stringify(
                goBuildPlans(
                    optionalArgument('target'),
                    output,
                    process.argv.includes('--include-product-tools')
                ),
                null,
                2
            )}\n`
        )
        return
    }
    if (command === 'build-go') {
        const output = path.resolve(
            optionalArgument('output') ?? path.join(DESKTOP_ROOT, 'bootstrap-bin')
        )
        executeGoBuilds(
            goBuildPlans(
                optionalArgument('target'),
                output,
                process.argv.includes('--include-product-tools')
            )
        )
        return
    }
    if (command === 'stage-product') {
        stageProduct(
            parsePlatform(argument('platform'), 'stage product platform'),
            path.resolve(argument('app')),
            path.resolve(argument('tui')),
            path.resolve(argument('broker')),
            path.resolve(argument('output'))
        )
        return
    }
    fail(
        'usage: package-bootstrap.ts build-target|build-matrix|verify|verify-cli-manifest|refresh-cli-manifest|plan-go|build-go|stage-product'
    )
}

try {
    main()
} catch (error) {
    process.stderr.write(
        `[bootstrap-package] ${error instanceof Error ? error.message : 'unexpected failure'}\n`
    )
    process.exitCode = 1
}
