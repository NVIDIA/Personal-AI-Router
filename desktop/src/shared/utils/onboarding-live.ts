// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { JsonValue } from '@/shared/types/json'
import type {
    OnboardingAcceptedHostKey,
    OnboardingAccessRequest,
    OnboardingAccessResult,
    OnboardingArtifact,
    OnboardingCandidate,
    OnboardingCandidates,
    OnboardingDiscoveryScope,
    OnboardingEnrollment,
    OnboardingInstallationSummary,
    OnboardingOperation,
    OnboardingOperationRequest,
    OnboardingReview,
    OnboardingReviewRequest,
    OnboardingReviewTarget,
    OnboardingScopes,
    OnboardingStartupLifetime,
    OnboardingTargetState,
    BootstrapAccountIdentity,
    BootstrapArchitecture,
    BootstrapArtifactIdentity,
    BootstrapArtifactObservation,
    BootstrapAuthorizedKeyObservation,
    BootstrapBinding,
    BootstrapCatalog,
    BootstrapCatalogArtifact,
    BootstrapCatalogTarget,
    BootstrapControllerKeys,
    BootstrapDecision,
    BootstrapEndpointObservation,
    BootstrapHelperResponse,
    BootstrapObservations,
    BootstrapOperationInvoke,
    BootstrapOwnership,
    BootstrapPlan,
    BootstrapPlanInvoke,
    BootstrapPlatform,
    BootstrapPublicKeyIdentity,
    BootstrapReceipt,
    BootstrapRequest,
    BootstrapRequestInvoke,
    BootstrapResourceKind,
    BootstrapRuntimeOwner,
    BootstrapRuntimeOwnerObservation,
    BootstrapSshEndpoint,
    BootstrapStatus,
    BootstrapTarget,
    BootstrapTargetReference
} from '@/shared/types/onboarding-live'

const OPERATION_ID = /^[a-f0-9]{32}$/
const SHA256 = /^[a-fA-F0-9]{64}$/
const USERNAME = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$/

function objectValue(value: JsonValue | undefined, label: string): { [key: string]: JsonValue } {
    if (value === null || value === undefined || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid ${label}`)
    return value
}

function exactKeys(
    value: object,
    label: string,
    required: readonly string[],
    optional: readonly string[] = []
): void {
    const allowed = new Set([...required, ...optional])
    const keys = Object.keys(value)
    const unknown = keys.find(key => !allowed.has(key))
    if (unknown) throw new Error(`Unknown ${label} field: ${unknown}`)
    const missing = required.find(key => !keys.includes(key))
    if (missing) throw new Error(`Missing ${label} field: ${missing}`)
}

function text(value: JsonValue | undefined, label: string, optional = false): string {
    if (optional && value === undefined) return ''
    if (typeof value !== 'string' || (!optional && value.length === 0))
        throw new Error(`Invalid ${label}`)
    return value
}

function flag(value: JsonValue | undefined, label: string): boolean {
    if (typeof value !== 'boolean') throw new Error(`Invalid ${label}`)
    return value
}

function integer(value: JsonValue | undefined, label: string, minimum = 0): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum)
        throw new Error(`Invalid ${label}`)
    return value
}

function operationId(value: JsonValue | undefined, label: string): string {
    const result = text(value, label)
    if (!OPERATION_ID.test(result)) throw new Error(`Invalid ${label}`)
    return result
}

function optionalText(value: JsonValue | undefined): string | undefined {
    return value === undefined ? undefined : text(value, 'optional setup text', true) || undefined
}

function lifetime(value: JsonValue | undefined): OnboardingStartupLifetime {
    if (value === 'session' || value === 'persistent') return value
    throw new Error('Invalid setup startup lifetime')
}

function parseArtifact(value: JsonValue): OnboardingArtifact {
    const artifact = objectValue(value, 'setup artifact')
    const sha256 = text(artifact.sha256, 'setup artifact digest')
    if (!SHA256.test(sha256)) throw new Error('Invalid setup artifact digest')
    return {
        sourceFingerprint: optionalText(artifact.sourceFingerprint),
        artifactId: text(artifact.artifactId, 'setup artifact id'),
        version: text(artifact.version, 'setup artifact version'),
        platform: text(artifact.platform, 'setup artifact platform'),
        arch: text(artifact.arch, 'setup artifact architecture'),
        sha256,
        provenance: text(artifact.provenance, 'setup artifact provenance')
    }
}

export function parseOnboardingCandidate(value: JsonValue): OnboardingCandidate {
    const candidate = objectValue(value, 'setup candidate')
    exactKeys(
        candidate,
        'setup candidate',
        [
            'candidateId',
            'label',
            'address',
            'port',
            'accessId',
            'accessLabel',
            'accessAvailable',
            'hostKeyTrusted',
            'bootstrapSource',
            'bootstrapState'
        ],
        ['hostKeySha256', 'bootstrapPlatform', 'bootstrapArchitecture', 'reason']
    )
    const hostKeySha256 = optionalText(candidate.hostKeySha256)
    if (hostKeySha256 && !hostKeySha256.startsWith('SHA256:'))
        throw new Error('Invalid setup host-key fingerprint')
    const bootstrapState = enrollment(candidate.bootstrapState)
    if (!bootstrapState) throw new Error('Invalid setup bootstrap state')
    return {
        candidateId: operationId(candidate.candidateId, 'setup candidate id'),
        label: text(candidate.label, 'setup candidate label'),
        address: text(candidate.address, 'setup candidate address'),
        port: integer(candidate.port, 'setup candidate port', 1),
        accessId:
            candidate.accessId === undefined || candidate.accessId === ''
                ? undefined
                : operationId(candidate.accessId, 'setup access id'),
        accessLabel: optionalText(candidate.accessLabel),
        accessAvailable: flag(candidate.accessAvailable, 'setup access availability'),
        hostKeySha256,
        hostKeyTrusted: flag(candidate.hostKeyTrusted, 'setup host-key trust'),
        bootstrapSource: text(candidate.bootstrapSource, 'setup bootstrap source'),
        bootstrapState,
        bootstrapPlatform:
            candidate.bootstrapPlatform === undefined
                ? undefined
                : bootstrapPlatform(candidate.bootstrapPlatform),
        bootstrapArchitecture:
            candidate.bootstrapArchitecture === undefined
                ? undefined
                : bootstrapArchitecture(candidate.bootstrapArchitecture),
        reason: optionalText(candidate.reason)
    }
}

function enrollment(value: JsonValue | undefined): OnboardingEnrollment | undefined {
    if (value === undefined) return undefined
    if (value === 'ssh-ready' || value === 'bootstrap-required') return value
    throw new Error('Invalid setup enrollment')
}

export function partitionOnboardingCandidates(candidates: OnboardingCandidate[]): {
    sshReady: OnboardingCandidate[]
    bootstrapRequired: OnboardingCandidate[]
} {
    return {
        sshReady: candidates.filter(candidate => candidate.bootstrapState === 'ssh-ready'),
        bootstrapRequired: candidates.filter(
            candidate => candidate.bootstrapState === 'bootstrap-required'
        )
    }
}

export function parseOnboardingCandidates(value: JsonValue): OnboardingCandidates {
    const result = objectValue(value, 'setup candidate response')
    if (!Array.isArray(result.candidates) || !Array.isArray(result.artifacts))
        throw new Error('Invalid setup candidate response')
    const candidates = result.candidates.map(parseOnboardingCandidate)
    const ids = new Set(candidates.map(candidate => candidate.candidateId))
    if (ids.size !== candidates.length) throw new Error('Duplicate setup candidate id')
    return { candidates, artifacts: result.artifacts.map(parseArtifact) }
}

export function parseOnboardingAccessResult(value: JsonValue): OnboardingAccessResult {
    const result = objectValue(value, 'setup access response')
    if (!Array.isArray(result.candidates)) throw new Error('Invalid setup access response')
    const candidates = result.candidates.map(parseOnboardingCandidate)
    if (new Set(candidates.map(candidate => candidate.candidateId)).size !== candidates.length)
        throw new Error('Duplicate setup access candidate id')
    return { candidates }
}

function parseInstallation(value: JsonValue): OnboardingInstallationSummary {
    const installation = objectValue(value, 'installed PAIR summary')
    const legacyRollbackSha256 = optionalText(installation.legacyRollbackSha256)
    if (legacyRollbackSha256 && !SHA256.test(legacyRollbackSha256))
        throw new Error('Invalid installed PAIR rollback digest')
    return {
        nodeId: text(installation.nodeId, 'installed PAIR node id'),
        clusterId: text(installation.clusterId, 'installed PAIR cluster id'),
        version: text(installation.version, 'installed PAIR version'),
        sourceFingerprint: text(
            installation.sourceFingerprint,
            'installed PAIR source fingerprint'
        ),
        unit: text(installation.unit, 'installed PAIR unit'),
        bundle: text(installation.bundle, 'installed PAIR bundle'),
        legacyRollbackSha256
    }
}

function parseReviewTarget(value: JsonValue): OnboardingReviewTarget {
    const target = objectValue(value, 'setup review target')
    const action = target.action === undefined || target.action === '' ? undefined : target.action
    if (action !== undefined && action !== 'upgrade') throw new Error('Invalid setup action')
    const status = target.status
    if (status !== 'ready' && status !== 'blocked') throw new Error('Invalid setup review status')
    const artifact = target.artifact === undefined ? undefined : parseArtifact(target.artifact)
    return {
        action,
        existingInstallation:
            target.existingInstallation === undefined
                ? undefined
                : parseInstallation(target.existingInstallation),
        startupLifetime: lifetime(target.startupLifetime),
        needsLinger: flag(target.needsLinger, 'setup linger requirement'),
        candidateId: operationId(target.candidateId, 'setup review candidate id'),
        label: text(target.label, 'setup review label'),
        address: text(target.address, 'setup review address'),
        port: integer(target.port, 'setup review port', 1),
        accessId: operationId(target.accessId, 'setup review access id'),
        accessLabel: text(target.accessLabel, 'setup review access label'),
        hostname: optionalText(target.hostname),
        platform: optionalText(target.platform),
        arch: optionalText(target.arch),
        hostKeySha256: optionalText(target.hostKeySha256),
        artifact,
        status,
        reason: optionalText(target.reason)
    }
}

export function parseOnboardingReview(value: JsonValue): OnboardingReview {
    const review = objectValue(value, 'setup review')
    if (!Array.isArray(review.targets)) throw new Error('Invalid setup review targets')
    const result: OnboardingReview = {
        reviewId: operationId(review.reviewId, 'setup review id'),
        expiresAt: integer(review.expiresAt, 'setup review expiry', 1),
        controllerNodeId: text(review.controllerNodeId, 'setup controller node id'),
        targetClusterId: text(review.targetClusterId, 'setup target cluster id', true),
        targets: review.targets.map(parseReviewTarget),
        canApprove: flag(review.canApprove, 'setup review approval')
    }
    if (result.targets.length < 1 || result.targets.length > 4)
        throw new Error('Invalid setup review target count')
    if (result.canApprove !== result.targets.every(target => target.status === 'ready'))
        throw new Error('Setup approval does not match target readiness')
    return result
}

function parseTargetState(value: JsonValue): OnboardingTargetState {
    const target = objectValue(value, 'setup operation target')
    return {
        candidateId: operationId(target.candidateId, 'setup operation candidate id'),
        stage: text(target.stage, 'setup operation stage'),
        message: optionalText(target.message),
        nodeId: optionalText(target.nodeId),
        canRetry: flag(target.canRetry, 'setup retry authority'),
        canCancel: flag(target.canCancel, 'setup cancel authority'),
        cleanupConfirmed: flag(target.cleanupConfirmed, 'setup cleanup proof')
    }
}

export function parseOnboardingOperation(value: JsonValue): OnboardingOperation {
    const operation = objectValue(value, 'setup operation')
    if (!Array.isArray(operation.targets)) throw new Error('Invalid setup operation targets')
    const targets = operation.targets.map(parseTargetState)
    if (targets.length < 1 || targets.length > 4) throw new Error('Invalid setup operation count')
    return {
        operationId: operationId(operation.operationId, 'setup operation id'),
        reviewId: operationId(operation.reviewId, 'setup operation review id'),
        targetClusterId: text(operation.targetClusterId, 'setup operation cluster id', true),
        revision: integer(operation.revision, 'setup operation revision', 1),
        state: text(operation.state, 'setup operation state'),
        targets,
        startedAt: integer(operation.startedAt, 'setup operation start time', 1),
        finishedAt:
            operation.finishedAt === undefined
                ? undefined
                : integer(operation.finishedAt, 'setup operation finish time', 1)
    }
}

function validateCandidateIds(candidateIds: string[]): string[] {
    if (candidateIds.length < 1 || candidateIds.length > 4)
        throw new Error('Select one to four setup targets')
    const ids = new Set(candidateIds)
    if (ids.size !== candidateIds.length || candidateIds.some(id => !OPERATION_ID.test(id)))
        throw new Error('Invalid setup target selection')
    return candidateIds
}

export function parseOnboardingAccessRequest(
    request: OnboardingAccessRequest
): OnboardingAccessRequest {
    validateCandidateIds(request.candidateIds)
    if (request.purpose !== undefined && request.purpose !== 'cable')
        throw new Error('Invalid setup access purpose')
    if (request.purpose === 'cable' && request.startupLifetime !== 'session')
        throw new Error('Cable access must be session scoped')
    if (!USERNAME.test(request.username)) throw new Error('Invalid setup account')
    if (request.auth !== 'password' && request.auth !== 'existing-key')
        throw new Error('Invalid setup authentication mode')
    if (
        (request.password?.length ?? 0) > 4096 ||
        (request.passphrase?.length ?? 0) > 4096 ||
        (request.elevationPassword?.length ?? 0) > 4096 ||
        (request.privateKeyBase64?.length ?? 0) > 48 * 1024
    )
        throw new Error('Setup access input exceeds its limit')
    const provider = request.privateKeyBase64 !== undefined
    if (
        provider &&
        (request.auth !== 'existing-key' ||
            request.keyPath !== undefined ||
            request.password !== undefined ||
            request.elevationPassword !== undefined ||
            request.credentialProvider !== 'controller-ssh-config' ||
            request.credentialPurpose !== 'enrolled-peer-upgrade' ||
            !request.publicKeySha256?.startsWith('SHA256:') ||
            !Number.isSafeInteger(request.credentialExpiresAt) ||
            (request.credentialExpiresAt ?? 0) <= Date.now())
    )
        throw new Error('Invalid volatile controller credential binding')
    if (request.auth === 'password' && !request.password)
        throw new Error('Enter the device account password')
    if (
        request.auth === 'existing-key' &&
        !provider &&
        (!request.keyPath || !request.keyPath.trim())
    )
        throw new Error('Select an existing absolute SSH key path')
    return {
        purpose: request.purpose,
        candidateIds: [...request.candidateIds],
        username: request.username,
        auth: request.auth,
        startupLifetime: lifetime(request.startupLifetime),
        keyPath: request.keyPath,
        password: request.password,
        passphrase: request.passphrase,
        elevationPassword: request.elevationPassword,
        privateKeyBase64: request.privateKeyBase64,
        credentialProvider: request.credentialProvider,
        credentialPurpose: request.credentialPurpose,
        publicKeySha256: request.publicKeySha256,
        credentialExpiresAt: request.credentialExpiresAt
    }
}

function parseAcceptedKey(value: OnboardingAcceptedHostKey): OnboardingAcceptedHostKey {
    if (!OPERATION_ID.test(value.candidateId) || !value.sha256.startsWith('SHA256:'))
        throw new Error('Invalid accepted setup host key')
    return { candidateId: value.candidateId, sha256: value.sha256 }
}

export function parseOnboardingReviewRequest(
    request: OnboardingReviewRequest
): OnboardingReviewRequest {
    validateCandidateIds(request.candidateIds)
    const acceptedHostKeys = request.acceptedHostKeys.map(parseAcceptedKey)
    if (new Set(acceptedHostKeys.map(key => key.candidateId)).size !== acceptedHostKeys.length)
        throw new Error('Duplicate accepted setup host key')
    return {
        candidateIds: [...request.candidateIds],
        artifactId: request.artifactId?.trim() || undefined,
        acceptedHostKeys
    }
}

export function parseOnboardingOperationRequest(
    request: OnboardingOperationRequest
): OnboardingOperationRequest {
    if (!OPERATION_ID.test(request.operationId)) throw new Error('Invalid setup operation id')
    if (request.candidateId !== undefined && !OPERATION_ID.test(request.candidateId))
        throw new Error('Invalid setup operation candidate id')
    return { operationId: request.operationId, candidateId: request.candidateId }
}

export function parseOnboardingScopes(value: JsonValue): OnboardingScopes {
    const result = objectValue(value, 'setup scope response')
    if (!Array.isArray(result.scopes)) throw new Error('Invalid setup scope response')
    const scopes = result.scopes.map(parseScope)
    if (new Set(scopes.map(scope => scope.scopeId)).size !== scopes.length)
        throw new Error('Duplicate setup scope id')
    return { scopes }
}

function parseScope(value: JsonValue): OnboardingDiscoveryScope {
    const scope = objectValue(value, 'setup scope')
    return {
        scopeId: operationId(scope.scopeId, 'setup scope id'),
        interface: text(scope.interface, 'setup scope interface'),
        localAddress: text(scope.localAddress, 'setup scope address'),
        cidr: text(scope.cidr, 'setup scope network'),
        eligible: flag(scope.eligible, 'setup scope eligibility'),
        reason: optionalText(scope.reason)
    }
}

const HEX64 = /^[a-f0-9]{64}$/
const ACCOUNT_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/
const ARTIFACT_ID = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$/
const VERSION =
    /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/

const SUPPORTED_BOOTSTRAP_TARGETS: BootstrapTarget[] = [
    { platform: 'windows', architecture: 'amd64' },
    { platform: 'windows', architecture: 'arm64' },
    { platform: 'darwin', architecture: 'amd64' },
    { platform: 'darwin', architecture: 'arm64' },
    { platform: 'linux', architecture: 'amd64' },
    { platform: 'linux', architecture: 'arm64' }
]

function digest(value: JsonValue | undefined, label: string): string {
    const result = text(value, label)
    if (!HEX64.test(result)) throw new Error(`Invalid ${label}`)
    return result
}

function bootstrapPlatform(value: JsonValue | undefined): BootstrapPlatform {
    if (value === 'windows' || value === 'darwin' || value === 'linux') return value
    throw new Error('Invalid bootstrap platform')
}

function bootstrapArchitecture(value: JsonValue | undefined): BootstrapArchitecture {
    if (value === 'amd64' || value === 'arm64') return value
    throw new Error('Invalid bootstrap architecture')
}

function bootstrapRuntimeOwner(value: JsonValue | undefined): BootstrapRuntimeOwner {
    if (value === 'desktop' || value === 'headless') return value
    throw new Error('Invalid bootstrap runtime owner')
}

function decisionValue(value: JsonValue | undefined): BootstrapDecision | undefined {
    if (value === undefined) return undefined
    if (
        value === 'apply' ||
        value === 'repair-owned' ||
        value === 'no-op' ||
        value === 'refuse-foreign'
    )
        return value
    throw new Error('Invalid bootstrap decision')
}

function ownershipValue(value: JsonValue | undefined): BootstrapOwnership {
    if (
        value === 'absent' ||
        value === 'owned' ||
        value === 'foreign' ||
        value === 'not-applicable' ||
        value === 'unavailable'
    )
        return value
    throw new Error('Invalid bootstrap ownership')
}

function hasControlCharacter(value: string): boolean {
    for (const character of value) {
        const code = character.codePointAt(0)
        if (code !== undefined && (code < 32 || code === 127)) return true
    }
    return false
}

function canonicalPath(value: JsonValue | undefined, platform: BootstrapPlatform, label: string) {
    const result = text(value, label)
    if (result.length > 512 || hasControlCharacter(result)) throw new Error(`Invalid ${label}`)
    if (platform === 'windows') {
        if (!/^[A-Z]:\\/.test(result) || result.includes('/')) throw new Error(`Invalid ${label}`)
        const segments = result.slice(3).split('\\')
        if (
            segments.some(
                segment =>
                    !segment ||
                    segment === '.' ||
                    segment === '..' ||
                    segment.endsWith(' ') ||
                    segment.endsWith('.') ||
                    /[<>:"|?*]/u.test(segment)
            )
        )
            throw new Error(`Invalid ${label}`)
        return result
    }
    if (
        result === '/' ||
        !result.startsWith('/') ||
        result.startsWith('//') ||
        result.endsWith('/') ||
        result
            .split('/')
            .some(
                (segment, index) => index > 0 && (!segment || segment === '.' || segment === '..')
            )
    )
        throw new Error(`Invalid ${label}`)
    return result
}

function canonicalBase64(value: JsonValue | undefined, label: string): string {
    const result = text(value, label)
    if (
        result.length > 16 * 1024 ||
        result.length % 4 !== 0 ||
        !/^[A-Za-z0-9+/]+={0,2}$/u.test(result)
    )
        throw new Error(`Invalid ${label}`)
    try {
        const decoded = atob(result)
        if (btoa(decoded) !== result) throw new Error('noncanonical')
    } catch {
        throw new Error(`Invalid ${label}`)
    }
    return result
}

const SHA256_CONSTANTS = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
]

function rotateRight(value: number, places: number): number {
    return (value >>> places) | (value << (32 - places))
}

function sha256Base64(value: string): string {
    const binary = atob(value)
    const bytes: number[] = []
    for (let index = 0; index < binary.length; index += 1) bytes.push(binary.charCodeAt(index))
    const bitLength = bytes.length * 8
    bytes.push(0x80)
    while (bytes.length % 64 !== 56) bytes.push(0)
    for (let index = 7; index >= 0; index -= 1)
        bytes.push(index < 4 ? (bitLength >>> (index * 8)) & 0xff : 0)

    const hash = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
        0x5be0cd19
    ]
    const words = new Array<number>(64).fill(0)
    for (let offset = 0; offset < bytes.length; offset += 64) {
        for (let index = 0; index < 16; index += 1) {
            const start = offset + index * 4
            words[index] =
                ((bytes[start] ?? 0) << 24) |
                ((bytes[start + 1] ?? 0) << 16) |
                ((bytes[start + 2] ?? 0) << 8) |
                (bytes[start + 3] ?? 0)
        }
        for (let index = 16; index < 64; index += 1) {
            const left = words[index - 15] ?? 0
            const right = words[index - 2] ?? 0
            const sigma0 = rotateRight(left, 7) ^ rotateRight(left, 18) ^ (left >>> 3)
            const sigma1 = rotateRight(right, 17) ^ rotateRight(right, 19) ^ (right >>> 10)
            words[index] =
                ((words[index - 16] ?? 0) + sigma0 + (words[index - 7] ?? 0) + sigma1) | 0
        }

        let [a, b, c, d, e, f, g, h] = hash
        for (let index = 0; index < 64; index += 1) {
            const sum1 = rotateRight(e ?? 0, 6) ^ rotateRight(e ?? 0, 11) ^ rotateRight(e ?? 0, 25)
            const choice = ((e ?? 0) & (f ?? 0)) ^ (~(e ?? 0) & (g ?? 0))
            const temporary1 =
                ((h ?? 0) + sum1 + choice + (SHA256_CONSTANTS[index] ?? 0) + (words[index] ?? 0)) |
                0
            const sum0 = rotateRight(a ?? 0, 2) ^ rotateRight(a ?? 0, 13) ^ rotateRight(a ?? 0, 22)
            const majority = ((a ?? 0) & (b ?? 0)) ^ ((a ?? 0) & (c ?? 0)) ^ ((b ?? 0) & (c ?? 0))
            const temporary2 = (sum0 + majority) | 0
            h = g
            g = f
            f = e
            e = ((d ?? 0) + temporary1) | 0
            d = c
            c = b
            b = a
            a = (temporary1 + temporary2) | 0
        }
        hash[0] = ((hash[0] ?? 0) + (a ?? 0)) | 0
        hash[1] = ((hash[1] ?? 0) + (b ?? 0)) | 0
        hash[2] = ((hash[2] ?? 0) + (c ?? 0)) | 0
        hash[3] = ((hash[3] ?? 0) + (d ?? 0)) | 0
        hash[4] = ((hash[4] ?? 0) + (e ?? 0)) | 0
        hash[5] = ((hash[5] ?? 0) + (f ?? 0)) | 0
        hash[6] = ((hash[6] ?? 0) + (g ?? 0)) | 0
        hash[7] = ((hash[7] ?? 0) + (h ?? 0)) | 0
    }
    return hash.map(word => (word >>> 0).toString(16).padStart(8, '0')).join('')
}

interface SshString {
    value: string
    next: number
}

function readSshString(binary: string, offset: number): SshString | null {
    if (offset < 0 || binary.length - offset < 4) return null
    const length =
        binary.charCodeAt(offset) * 0x1000000 +
        binary.charCodeAt(offset + 1) * 0x10000 +
        binary.charCodeAt(offset + 2) * 0x100 +
        binary.charCodeAt(offset + 3)
    const start = offset + 4
    const next = start + length
    if (length < 0 || next > binary.length) return null
    return { value: binary.slice(start, next), next }
}

function positiveMpint(value: string): boolean {
    if (value.length === 0 || (value.charCodeAt(0) & 0x80) !== 0) return false
    if (value.length > 1 && value.charCodeAt(0) === 0 && (value.charCodeAt(1) & 0x80) === 0)
        return false
    return [...value].some(character => character.charCodeAt(0) !== 0)
}

function binaryBigInt(value: string): bigint {
    let result = 0n
    for (const character of value) result = (result << 8n) | BigInt(character.charCodeAt(0))
    return result
}

function modulo(value: bigint, modulus: bigint): bigint {
    const result = value % modulus
    return result < 0n ? result + modulus : result
}

function validNistP256Point(value: string): boolean {
    if (value.length !== 65 || value.charCodeAt(0) !== 4) return false
    const x = binaryBigInt(value.slice(1, 33))
    const y = binaryBigInt(value.slice(33))
    const prime = (1n << 256n) - (1n << 224n) + (1n << 192n) + (1n << 96n) - 1n
    const curveB = BigInt('0x5ac635d8aa3a93e7b3ebbd55769886bc651d06b0cc53b0f63bce3c3e27d2604b')
    if (x >= prime || y >= prime) return false
    return modulo(y * y, prime) === modulo(x * x * x - 3n * x + curveB, prime)
}

function validPublicKeyMaterial(
    algorithm: BootstrapPublicKeyIdentity['algorithm'],
    material: string
): boolean {
    const decoded = atob(material)
    const wireAlgorithm = readSshString(decoded, 0)
    if (!wireAlgorithm || wireAlgorithm.value !== algorithm) return false
    if (algorithm === 'ssh-ed25519') {
        const key = readSshString(decoded, wireAlgorithm.next)
        return key !== null && key.value.length === 32 && key.next === decoded.length
    }
    if (algorithm === 'ssh-rsa') {
        const exponent = readSshString(decoded, wireAlgorithm.next)
        const modulus = exponent ? readSshString(decoded, exponent.next) : null
        if (
            !exponent ||
            !modulus ||
            modulus.next !== decoded.length ||
            !positiveMpint(exponent.value) ||
            !positiveMpint(modulus.value) ||
            exponent.value.length > 5 ||
            (modulus.value.charCodeAt(modulus.value.length - 1) & 1) === 0
        )
            return false
        const exponentValue = Number(binaryBigInt(exponent.value))
        return (
            Number.isSafeInteger(exponentValue) &&
            exponentValue >= 3 &&
            exponentValue <= 0x7fffffff &&
            exponentValue % 2 === 1
        )
    }
    const curve = readSshString(decoded, wireAlgorithm.next)
    const point = curve ? readSshString(decoded, curve.next) : null
    return (
        curve?.value === 'nistp256' &&
        point !== null &&
        point.next === decoded.length &&
        validNistP256Point(point.value)
    )
}

function bootstrapTargetEqual(left: BootstrapTarget, right: BootstrapTarget): boolean {
    return left.platform === right.platform && left.architecture === right.architecture
}

function bootstrapValueEqual(left: object, right: object): boolean {
    return JSON.stringify(left) === JSON.stringify(right)
}

function parseBootstrapTarget(
    value: JsonValue | BootstrapTarget,
    label = 'bootstrap target'
): BootstrapTarget {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid ${label}`)
    exactKeys(value, label, ['platform', 'architecture'])
    return {
        platform: bootstrapPlatform(value.platform),
        architecture: bootstrapArchitecture(value.architecture)
    }
}

function parseBootstrapAccount(
    value: JsonValue | BootstrapAccountIdentity,
    platform: BootstrapPlatform
): BootstrapAccountIdentity {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap account')
    exactKeys(value, 'bootstrap account', ['name', 'homePath', 'authorizedKeysPath'])
    const name = text(value.name, 'bootstrap account name')
    if (!ACCOUNT_NAME.test(name) || name === '.' || name === '..')
        throw new Error('Invalid bootstrap account name')
    const homePath = canonicalPath(value.homePath, platform, 'bootstrap account home path')
    const authorizedKeysPath = canonicalPath(
        value.authorizedKeysPath,
        platform,
        'bootstrap authorized-keys path'
    )
    const separator = platform === 'windows' ? '\\' : '/'
    if (!authorizedKeysPath.startsWith(`${homePath}${separator}`))
        throw new Error('Invalid bootstrap authorized-keys path')
    return { name, homePath, authorizedKeysPath }
}

export function buildBootstrapAccountIdentity(
    target: BootstrapTarget,
    name: string
): BootstrapAccountIdentity {
    if (!ACCOUNT_NAME.test(name) || name === '.' || name === '..')
        throw new Error('Invalid bootstrap account name')
    if (target.platform === 'windows')
        return {
            name,
            homePath: `C:\\Users\\${name}`,
            authorizedKeysPath: `C:\\Users\\${name}\\.ssh\\authorized_keys`
        }
    if (target.platform === 'darwin')
        return {
            name,
            homePath: `/Users/${name}`,
            authorizedKeysPath: `/Users/${name}/.ssh/authorized_keys`
        }
    return {
        name,
        homePath: `/home/${name}`,
        authorizedKeysPath: `/home/${name}/.ssh/authorized_keys`
    }
}

function parseBootstrapPublicKey(
    value: JsonValue | BootstrapPublicKeyIdentity
): BootstrapPublicKeyIdentity {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap public key')
    exactKeys(value, 'bootstrap public key', ['algorithm', 'material', 'fingerprintSha256'])
    const algorithm = value.algorithm
    if (
        algorithm !== 'ssh-ed25519' &&
        algorithm !== 'ssh-rsa' &&
        algorithm !== 'ecdsa-sha2-nistp256'
    )
        throw new Error('Invalid bootstrap public key algorithm')
    const material = canonicalBase64(value.material, 'bootstrap public key material')
    if (!validPublicKeyMaterial(algorithm, material))
        throw new Error('Invalid bootstrap public key material')
    const fingerprintSha256 = digest(value.fingerprintSha256, 'bootstrap public-key fingerprint')
    if (sha256Base64(material) !== fingerprintSha256)
        throw new Error('Bootstrap public-key fingerprint does not match its material')
    return {
        algorithm,
        material,
        fingerprintSha256
    }
}

function parseBootstrapEndpoint(value: JsonValue | BootstrapSshEndpoint): BootstrapSshEndpoint {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap SSH endpoint')
    exactKeys(value, 'bootstrap SSH endpoint', ['address', 'port'])
    const address = text(value.address, 'bootstrap SSH address')
    if (address.length > 253 || address.trim() !== address || /\s/u.test(address))
        throw new Error('Invalid bootstrap SSH address')
    const port = integer(value.port, 'bootstrap SSH port', 1)
    if (port > 65535) throw new Error('Invalid bootstrap SSH port')
    return { address, port }
}

function parseBootstrapArtifactIdentity(
    value: JsonValue | BootstrapArtifactIdentity,
    platform: BootstrapPlatform,
    label: string
): BootstrapArtifactIdentity {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid ${label}`)
    exactKeys(value, label, ['id', 'version', 'sha256', 'path'])
    const id = text(value.id, `${label} id`)
    const version = text(value.version, `${label} version`)
    if (!ARTIFACT_ID.test(id) || !VERSION.test(version)) throw new Error(`Invalid ${label}`)
    return {
        id,
        version,
        sha256: digest(value.sha256, `${label} digest`),
        path: canonicalPath(value.path, platform, `${label} path`)
    }
}

function parseBootstrapBinding(value: JsonValue | BootstrapBinding): BootstrapBinding {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap binding')
    exactKeys(value, 'bootstrap binding', [
        'target',
        'lane',
        'role',
        'runtimeOwner',
        'account',
        'controllerKey',
        'endpoint',
        'product',
        'helper'
    ])
    const target = parseBootstrapTarget(value.target)
    const lane = value.lane
    if (lane !== 'quick-connect' && lane !== 'zero-touch') throw new Error('Invalid bootstrap lane')
    const role = value.role
    if (role !== 'auto' && role !== 'desktop' && role !== 'headless')
        throw new Error('Invalid bootstrap role')
    const runtimeOwner = bootstrapRuntimeOwner(value.runtimeOwner)
    if (role !== 'auto' && role !== runtimeOwner)
        throw new Error('Bootstrap role does not match its runtime owner')
    const product = parseBootstrapArtifactIdentity(
        value.product,
        target.platform,
        'bootstrap product artifact'
    )
    const helper = parseBootstrapArtifactIdentity(
        value.helper,
        target.platform,
        'bootstrap helper artifact'
    )
    if (product.id === helper.id || product.path === helper.path)
        throw new Error('Bootstrap product and helper identities must differ')
    return {
        target,
        lane,
        role,
        runtimeOwner,
        account: parseBootstrapAccount(value.account, target.platform),
        controllerKey: parseBootstrapPublicKey(value.controllerKey),
        endpoint: parseBootstrapEndpoint(value.endpoint),
        product,
        helper
    }
}

export function parseBootstrapRequest(value: JsonValue | BootstrapRequest): BootstrapRequest {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap request')
    exactKeys(value, 'bootstrap request', ['schemaVersion', 'operationId', 'binding'])
    if (value.schemaVersion !== 1) throw new Error('Invalid bootstrap schema')
    return {
        schemaVersion: 1,
        operationId: operationId(value.operationId, 'bootstrap operation id'),
        binding: parseBootstrapBinding(value.binding)
    }
}

function parseEndpointObservation(
    value: JsonValue | BootstrapEndpointObservation,
    managed: boolean
): BootstrapEndpointObservation {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap endpoint observation')
    exactKeys(value, 'bootstrap endpoint observation', ['ownership', 'endpoint'])
    const ownership = ownershipValue(value.ownership)
    const endpoint = value.endpoint === null ? null : parseBootstrapEndpoint(value.endpoint)
    if (
        ((ownership === 'owned' || ownership === 'foreign') && endpoint === null) ||
        ((ownership === 'absent' ||
            ownership === 'not-applicable' ||
            ownership === 'unavailable') &&
            endpoint !== null) ||
        (managed && (ownership === 'not-applicable' || ownership === 'unavailable'))
    )
        throw new Error('Invalid bootstrap endpoint observation')
    return { ownership, endpoint }
}

function parseAuthorizedKeyObservation(
    value: JsonValue | BootstrapAuthorizedKeyObservation,
    platform: BootstrapPlatform
): BootstrapAuthorizedKeyObservation {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap authorized-key observation')
    exactKeys(value, 'bootstrap authorized-key observation', ['ownership', 'identity'])
    const ownership = ownershipValue(value.ownership)
    if (ownership !== 'absent' && ownership !== 'owned' && ownership !== 'foreign')
        throw new Error('Invalid bootstrap authorized-key ownership')
    if (value.identity === null) {
        if (ownership !== 'absent') throw new Error('Invalid bootstrap authorized-key observation')
        return { ownership, identity: null }
    }
    if (
        ownership === 'absent' ||
        typeof value.identity !== 'object' ||
        Array.isArray(value.identity)
    )
        throw new Error('Invalid bootstrap authorized-key observation')
    exactKeys(value.identity, 'bootstrap authorized-key identity', [
        'accountName',
        'path',
        'fingerprintSha256'
    ])
    const accountName = text(value.identity.accountName, 'bootstrap authorized-key account')
    if (!ACCOUNT_NAME.test(accountName) || accountName === '.' || accountName === '..')
        throw new Error('Invalid bootstrap authorized-key account')
    return {
        ownership,
        identity: {
            accountName,
            path: canonicalPath(
                value.identity.path,
                platform,
                'bootstrap authorized-key identity path'
            ),
            fingerprintSha256: digest(
                value.identity.fingerprintSha256,
                'bootstrap authorized-key fingerprint'
            )
        }
    }
}

function parseArtifactObservation(
    value: JsonValue | BootstrapArtifactObservation,
    platform: BootstrapPlatform,
    label: string
): BootstrapArtifactObservation {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid ${label}`)
    exactKeys(value, label, ['ownership', 'identity'])
    const ownership = ownershipValue(value.ownership)
    if (ownership !== 'absent' && ownership !== 'owned' && ownership !== 'foreign')
        throw new Error(`Invalid ${label}`)
    if (value.identity === null) {
        if (ownership !== 'absent') throw new Error(`Invalid ${label}`)
        return { ownership, identity: null }
    }
    if (ownership === 'absent') throw new Error(`Invalid ${label}`)
    return {
        ownership,
        identity: parseBootstrapArtifactIdentity(value.identity, platform, label)
    }
}

function parseRuntimeOwnerObservation(
    value: JsonValue | BootstrapRuntimeOwnerObservation
): BootstrapRuntimeOwnerObservation {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap runtime-owner observation')
    exactKeys(value, 'bootstrap runtime-owner observation', ['ownership', 'owner'])
    const ownership = ownershipValue(value.ownership)
    if (ownership !== 'owned' && ownership !== 'foreign')
        throw new Error('Invalid bootstrap runtime-owner observation')
    return { ownership, owner: bootstrapRuntimeOwner(value.owner) }
}

function parseBootstrapObservations(
    value: JsonValue | BootstrapObservations,
    target: BootstrapTarget
): BootstrapObservations {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap observations')
    exactKeys(value, 'bootstrap observations', [
        'sshService',
        'firewall',
        'authorizedKey',
        'helper',
        'product',
        'runtimeOwners'
    ])
    if (!Array.isArray(value.runtimeOwners) || value.runtimeOwners.length > 1)
        throw new Error('Invalid bootstrap runtime owners')
    const sshService = parseEndpointObservation(value.sshService, true)
    const firewall = parseEndpointObservation(value.firewall, false)
    if (
        (target.platform === 'windows' && firewall.ownership === 'not-applicable') ||
        (target.platform === 'darwin' && firewall.ownership !== 'not-applicable')
    )
        throw new Error('Invalid bootstrap firewall observation')
    const product = parseArtifactObservation(
        value.product,
        target.platform,
        'bootstrap product observation'
    )
    const helper = parseArtifactObservation(
        value.helper,
        target.platform,
        'bootstrap helper observation'
    )
    if (
        product.identity &&
        helper.identity &&
        (product.identity.id === helper.identity.id ||
            product.identity.path === helper.identity.path)
    )
        throw new Error('Invalid bootstrap artifact observations')
    return {
        sshService,
        firewall,
        authorizedKey: parseAuthorizedKeyObservation(value.authorizedKey, target.platform),
        helper,
        product,
        runtimeOwners: value.runtimeOwners.map(parseRuntimeOwnerObservation)
    }
}

function endpointExact(
    observation: BootstrapEndpointObservation,
    endpoint: BootstrapSshEndpoint
): boolean {
    return (
        observation.ownership === 'owned' &&
        observation.endpoint !== null &&
        bootstrapValueEqual(observation.endpoint, endpoint)
    )
}

function firewallExact(binding: BootstrapBinding, observation: BootstrapEndpointObservation) {
    if (binding.target.platform === 'windows') return endpointExact(observation, binding.endpoint)
    if (binding.target.platform === 'darwin')
        return observation.ownership === 'not-applicable' && observation.endpoint === null
    return (
        (observation.ownership === 'not-applicable' && observation.endpoint === null) ||
        endpointExact(observation, binding.endpoint)
    )
}

function authorizedKeyExact(
    observation: BootstrapAuthorizedKeyObservation,
    binding: BootstrapBinding
) {
    return (
        observation.ownership === 'owned' &&
        observation.identity?.accountName === binding.account.name &&
        observation.identity.path === binding.account.authorizedKeysPath &&
        observation.identity.fingerprintSha256 === binding.controllerKey.fingerprintSha256
    )
}

function artifactExact(
    observation: BootstrapArtifactObservation,
    artifact: BootstrapArtifactIdentity
) {
    return (
        observation.ownership === 'owned' &&
        observation.identity !== null &&
        bootstrapValueEqual(observation.identity, artifact)
    )
}

function runtimeOwnerExact(
    observations: BootstrapRuntimeOwnerObservation[],
    owner: BootstrapRuntimeOwner
) {
    return (
        observations.length === 1 &&
        observations[0]?.ownership === 'owned' &&
        observations[0].owner === owner
    )
}

function observationsExact(binding: BootstrapBinding, observed: BootstrapObservations) {
    return (
        endpointExact(observed.sshService, binding.endpoint) &&
        firewallExact(binding, observed.firewall) &&
        authorizedKeyExact(observed.authorizedKey, binding) &&
        artifactExact(observed.helper, binding.helper) &&
        artifactExact(observed.product, binding.product) &&
        runtimeOwnerExact(observed.runtimeOwners, binding.runtimeOwner)
    )
}

function hasForeign(observed: BootstrapObservations) {
    return (
        observed.sshService.ownership === 'foreign' ||
        observed.firewall.ownership === 'foreign' ||
        observed.firewall.ownership === 'unavailable' ||
        observed.authorizedKey.ownership === 'foreign' ||
        observed.helper.ownership === 'foreign' ||
        observed.product.ownership === 'foreign' ||
        observed.runtimeOwners[0]?.ownership === 'foreign'
    )
}

function hasOwned(observed: BootstrapObservations) {
    return (
        observed.sshService.ownership === 'owned' ||
        observed.firewall.ownership === 'owned' ||
        observed.authorizedKey.ownership === 'owned' ||
        observed.helper.ownership === 'owned' ||
        observed.product.ownership === 'owned' ||
        observed.runtimeOwners[0]?.ownership === 'owned'
    )
}

function deriveBootstrapPlan(
    binding: BootstrapBinding,
    observed: BootstrapObservations
): {
    decision: BootstrapDecision
    actions: BootstrapResourceKind[]
} {
    if (hasForeign(observed)) return { decision: 'refuse-foreign', actions: [] }
    const actions: BootstrapResourceKind[] = []
    if (!endpointExact(observed.sshService, binding.endpoint)) actions.push('ssh-service')
    if (!firewallExact(binding, observed.firewall)) actions.push('firewall')
    if (!authorizedKeyExact(observed.authorizedKey, binding)) actions.push('authorized-key')
    if (!artifactExact(observed.helper, binding.helper)) actions.push('helper')
    if (!artifactExact(observed.product, binding.product)) actions.push('pair-artifact')
    if (!runtimeOwnerExact(observed.runtimeOwners, binding.runtimeOwner))
        actions.push('active-role')
    if (actions.length === 0) return { decision: 'no-op', actions }
    return { decision: hasOwned(observed) ? 'repair-owned' : 'apply', actions }
}

function parseResourceKind(value: JsonValue): BootstrapResourceKind {
    if (
        value === 'ssh-service' ||
        value === 'firewall' ||
        value === 'authorized-key' ||
        value === 'helper' ||
        value === 'pair-artifact' ||
        value === 'active-role'
    )
        return value
    throw new Error('Invalid bootstrap action')
}

export function parseBootstrapPlan(value: JsonValue | BootstrapPlan): BootstrapPlan {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap plan')
    exactKeys(value, 'bootstrap plan', [
        'schemaVersion',
        'operationId',
        'phase',
        'decision',
        'binding',
        'observed',
        'actions'
    ])
    if (value.schemaVersion !== 1) throw new Error('Invalid bootstrap schema')
    if (value.phase !== 'review') throw new Error('Invalid bootstrap plan phase')
    const decision = decisionValue(value.decision)
    if (!decision) throw new Error('Invalid bootstrap decision')
    const binding = parseBootstrapBinding(value.binding)
    const observed = parseBootstrapObservations(value.observed, binding.target)
    if (!Array.isArray(value.actions)) throw new Error('Invalid bootstrap actions')
    const actions = value.actions.map(parseResourceKind)
    const derived = deriveBootstrapPlan(binding, observed)
    if (decision !== derived.decision || !bootstrapValueEqual(actions, derived.actions))
        throw new Error('Bootstrap plan is not the deterministic reconciliation result')
    return {
        schemaVersion: 1,
        operationId: operationId(value.operationId, 'bootstrap operation id'),
        phase: 'review',
        decision,
        binding,
        observed,
        actions
    }
}

export function parseBootstrapStatus(value: JsonValue | BootstrapStatus): BootstrapStatus {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap status')
    exactKeys(
        value,
        'bootstrap status',
        ['schemaVersion', 'operationId', 'phase', 'binding', 'observed'],
        ['decision']
    )
    if (value.schemaVersion !== 1) throw new Error('Invalid bootstrap schema')
    const phase = value.phase
    if (
        phase !== 'inspect' &&
        phase !== 'review' &&
        phase !== 'apply' &&
        phase !== 'verify' &&
        phase !== 'complete' &&
        phase !== 'blocked'
    )
        throw new Error('Invalid bootstrap status phase')
    const decision = decisionValue(value.decision)
    const binding = parseBootstrapBinding(value.binding)
    const observed = parseBootstrapObservations(value.observed, binding.target)
    const derived = deriveBootstrapPlan(binding, observed)
    switch (phase) {
        case 'inspect':
            if (decision !== undefined) throw new Error('Invalid bootstrap inspect decision')
            break
        case 'review':
            if (decision !== derived.decision) throw new Error('Invalid bootstrap review decision')
            break
        case 'apply':
            if (
                decision !== derived.decision ||
                (decision !== 'apply' && decision !== 'repair-owned')
            )
                throw new Error('Invalid bootstrap apply decision')
            break
        case 'verify':
        case 'complete':
            if (
                !observationsExact(binding, observed) ||
                (decision !== 'apply' && decision !== 'repair-owned' && decision !== 'no-op')
            )
                throw new Error('Invalid verified bootstrap status')
            break
        case 'blocked':
            if (decision !== 'refuse-foreign' || !hasForeign(observed))
                throw new Error('Invalid blocked bootstrap status')
            break
    }
    return {
        schemaVersion: 1,
        operationId: operationId(value.operationId, 'bootstrap operation id'),
        phase,
        ...(decision ? { decision } : {}),
        binding,
        observed
    }
}

export function parseBootstrapReceipt(value: JsonValue | BootstrapReceipt): BootstrapReceipt {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap receipt')
    exactKeys(value, 'bootstrap receipt', [
        'schemaVersion',
        'operationId',
        'phase',
        'decision',
        'binding',
        'verified'
    ])
    if (value.schemaVersion !== 1) throw new Error('Invalid bootstrap schema')
    if (value.phase !== 'complete') throw new Error('Invalid bootstrap receipt phase')
    const decision = decisionValue(value.decision)
    if (decision !== 'apply' && decision !== 'repair-owned' && decision !== 'no-op')
        throw new Error('Invalid bootstrap receipt decision')
    const binding = parseBootstrapBinding(value.binding)
    const verified = parseBootstrapObservations(value.verified, binding.target)
    if (!observationsExact(binding, verified))
        throw new Error('Bootstrap receipt does not contain exact verified state')
    return {
        schemaVersion: 1,
        operationId: operationId(value.operationId, 'bootstrap operation id'),
        phase: 'complete',
        decision,
        binding,
        verified
    }
}

function catalogArtifactExpected(
    target: BootstrapTarget,
    kind: 'bootstrap' | 'helper' | 'product'
): { id: string; path: string; fileName: string } {
    if (kind === 'bootstrap') {
        if (target.platform === 'windows')
            return {
                id: 'nvpair-host-bootstrap',
                path: 'C:\\Program Files\\NVIDIA Corporation\\PAIR\\nvpair-host-bootstrap.exe',
                fileName: 'nvpair-host-bootstrap.exe'
            }
        return {
            id: 'nvpair-host-bootstrap',
            path:
                target.platform === 'darwin'
                    ? '/Library/PrivilegedHelperTools/nvpair-host-bootstrap'
                    : '/usr/libexec/nvpair-host-bootstrap',
            fileName: 'nvpair-host-bootstrap'
        }
    }
    if (kind === 'helper') {
        if (target.platform === 'windows')
            return {
                id: 'nvpair-host-helper',
                path: 'C:\\Program Files\\NVIDIA Corporation\\PAIR\\nvpair-host-helper.exe',
                fileName: 'nvpair-host-helper.exe'
            }
        return {
            id: 'nvpair-host-helper',
            path:
                target.platform === 'darwin'
                    ? '/Library/PrivilegedHelperTools/nvpair-host-helper'
                    : '/usr/libexec/nvpair-host-helper',
            fileName: 'nvpair-host-helper'
        }
    }
    return {
        id: 'nvpair',
        path:
            target.platform === 'windows'
                ? 'C:\\Program Files\\NVIDIA Corporation\\PAIR\\product'
                : target.platform === 'darwin'
                  ? '/Applications/NVPAIR.app'
                  : '/opt/nvpair/product',
        fileName: 'nvpair-product.zip'
    }
}

function parseCatalogSignature(
    value: JsonValue | BootstrapCatalogArtifact['signature'],
    label: string,
    allowEmptyContent = false
): BootstrapCatalogArtifact['signature'] {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid ${label}`)
    exactKeys(value, label, [
        'status',
        'kind',
        'identity',
        'notarized',
        'signatureFile',
        'checksumFile',
        'contentSHA256',
        'contentSize'
    ])
    const status = value.status
    if (status !== 'unsigned' && status !== 'signed') throw new Error(`Invalid ${label} status`)
    const kind = value.kind
    if (
        kind !== 'none' &&
        kind !== 'authenticode' &&
        kind !== 'apple-code-sign' &&
        kind !== 'detached-release'
    )
        throw new Error(`Invalid ${label} kind`)
    const identity = text(value.identity, `${label} identity`, true)
    if (typeof value.notarized !== 'boolean') throw new Error(`Invalid ${label} notarization`)
    const signatureFile = text(value.signatureFile, `${label} signature file`, true)
    const checksumFile = text(value.checksumFile, `${label} checksum file`, true)
    const contentSHA256 = text(value.contentSHA256, `${label} content SHA-256`, true)
    const contentSize = integer(value.contentSize, `${label} content size`, 0)
    if (
        (!allowEmptyContent && !HEX64.test(contentSHA256)) ||
        (allowEmptyContent && contentSHA256 !== '') ||
        (!allowEmptyContent && contentSize < 1) ||
        (allowEmptyContent && contentSize !== 0) ||
        contentSize > 8 * 1024 * 1024 * 1024
    )
        throw new Error(`Invalid ${label} content identity`)
    return {
        status,
        kind,
        identity,
        notarized: value.notarized,
        signatureFile,
        checksumFile,
        contentSHA256,
        contentSize
    }
}

function validateCatalogSignaturePolicy(
    signature: BootstrapCatalogArtifact['signature'],
    target: BootstrapTarget,
    artifact: 'bootstrap' | 'helper' | 'product' | 'combination',
    official: boolean
): void {
    if (!official) {
        if (
            signature.status !== 'unsigned' ||
            signature.kind !== 'none' ||
            signature.identity !== '' ||
            signature.notarized ||
            signature.signatureFile !== '' ||
            signature.checksumFile !== ''
        )
            throw new Error('Invalid engineering bootstrap signature metadata')
        return
    }
    const expectedKind =
        artifact === 'combination' || target.platform === 'linux'
            ? 'detached-release'
            : target.platform === 'windows'
              ? 'authenticode'
              : 'apple-code-sign'
    if (
        signature.status !== 'signed' ||
        signature.kind !== expectedKind ||
        signature.identity === '' ||
        signature.checksumFile === '' ||
        (expectedKind === 'detached-release' && signature.signatureFile === '') ||
        (target.platform === 'darwin' && artifact !== 'combination' && !signature.notarized)
    )
        throw new Error('Invalid official bootstrap signature metadata')
}

function parseCatalogArtifact(
    value: JsonValue | BootstrapCatalogArtifact,
    target: BootstrapTarget,
    kind: 'bootstrap' | 'helper' | 'product'
): BootstrapCatalogArtifact {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap catalog artifact')
    exactKeys(value, 'bootstrap catalog artifact', [
        'identity',
        'fileName',
        'size',
        'provenance',
        'signature'
    ])
    const identity = parseBootstrapArtifactIdentity(
        value.identity,
        target.platform,
        `bootstrap catalog ${kind}`
    )
    const fileName = text(value.fileName, 'bootstrap catalog file name')
    const size = integer(value.size, 'bootstrap catalog artifact size', 1)
    if (size > 8 * 1024 * 1024 * 1024) throw new Error('Invalid bootstrap catalog artifact size')
    const provenance = value.provenance
    if (provenance !== 'official-release' && provenance !== 'engineering')
        throw new Error('Invalid bootstrap catalog provenance')
    const expected = catalogArtifactExpected(target, kind)
    if (
        identity.id !== expected.id ||
        identity.path !== expected.path ||
        fileName !== expected.fileName
    )
        throw new Error('Bootstrap catalog identity is not fixed')
    const signature = parseCatalogSignature(value.signature, `bootstrap catalog ${kind} signature`)
    if (
        kind !== 'product' &&
        (signature.contentSHA256 !== identity.sha256 || signature.contentSize !== size)
    )
        throw new Error('Bootstrap catalog signed content identity differs')
    return { identity, fileName, size, provenance, signature }
}

function parseCatalogCombination(
    value: JsonValue | BootstrapCatalogTarget['combination'],
    target: BootstrapTarget
): BootstrapCatalogTarget['combination'] {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap catalog combination')
    exactKeys(value, 'bootstrap catalog combination', [
        'fileName',
        'size',
        'sha256',
        'provenance',
        'signature'
    ])
    const fileName = text(value.fileName, 'bootstrap catalog combination file name')
    const expectedFileName = `nvpair-bootstrap-${target.platform}-${target.architecture}.zip`
    const size = integer(value.size, 'bootstrap catalog combination size', 1)
    const sha256 = digest(value.sha256, 'bootstrap catalog combination SHA-256')
    const provenance = value.provenance
    if (
        fileName !== expectedFileName ||
        size > 8 * 1024 * 1024 * 1024 ||
        (provenance !== 'official-release' && provenance !== 'engineering')
    )
        throw new Error('Invalid bootstrap catalog combination identity')
    const signature = parseCatalogSignature(
        value.signature,
        'bootstrap catalog combination signature'
    )
    if (signature.contentSHA256 !== sha256 || signature.contentSize !== size)
        throw new Error('Bootstrap catalog combination signed content identity differs')
    return { fileName, size, sha256, provenance, signature }
}

function parseCatalogTarget(
    value: JsonValue | BootstrapCatalogTarget,
    expected: BootstrapTarget
): BootstrapCatalogTarget {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap catalog target')
    exactKeys(value, 'bootstrap catalog target', [
        'target',
        'roles',
        'bootstrap',
        'helper',
        'product',
        'combination'
    ])
    const target = parseBootstrapTarget(value.target)
    if (!bootstrapTargetEqual(target, expected))
        throw new Error('Bootstrap catalog target order is invalid')
    if (
        !Array.isArray(value.roles) ||
        value.roles.length !== 2 ||
        value.roles[0] !== 'desktop' ||
        value.roles[1] !== 'headless'
    )
        throw new Error('Bootstrap catalog roles are invalid')
    const bootstrap = parseCatalogArtifact(value.bootstrap, target, 'bootstrap')
    const helper = parseCatalogArtifact(value.helper, target, 'helper')
    const product = parseCatalogArtifact(value.product, target, 'product')
    const combination = parseCatalogCombination(value.combination, target)
    const official = bootstrap.provenance === 'official-release'
    if (
        bootstrap.provenance !== helper.provenance ||
        bootstrap.provenance !== product.provenance ||
        bootstrap.provenance !== combination.provenance
    )
        throw new Error('Bootstrap catalog artifact release identities differ')
    validateCatalogSignaturePolicy(bootstrap.signature, target, 'bootstrap', official)
    validateCatalogSignaturePolicy(helper.signature, target, 'helper', official)
    validateCatalogSignaturePolicy(product.signature, target, 'product', official)
    validateCatalogSignaturePolicy(combination.signature, target, 'combination', official)
    return {
        target,
        roles: ['desktop', 'headless'],
        bootstrap,
        helper,
        product,
        combination
    }
}

export function parseBootstrapCatalog(value: JsonValue | BootstrapCatalog): BootstrapCatalog {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap catalog')
    exactKeys(value, 'bootstrap catalog', ['schemaVersion', 'integrity', 'targets'])
    if (value.schemaVersion !== 1 || !Array.isArray(value.targets) || value.targets.length !== 6)
        throw new Error('Bootstrap catalog must contain the six supported targets')
    if (
        value.integrity === null ||
        typeof value.integrity !== 'object' ||
        Array.isArray(value.integrity)
    )
        throw new Error('Invalid bootstrap catalog integrity')
    exactKeys(value.integrity, 'bootstrap catalog integrity', [
        'checksumAlgorithm',
        'checksumFile',
        'signature'
    ])
    if (
        value.integrity.checksumAlgorithm !== 'sha256' ||
        value.integrity.checksumFile !== 'onboarding-bootstrap-catalog.json.sha256'
    )
        throw new Error('Invalid bootstrap catalog checksum metadata')
    const integritySignature = parseCatalogSignature(
        value.integrity.signature,
        'bootstrap catalog integrity signature',
        true
    )
    if (integritySignature.checksumFile !== value.integrity.checksumFile)
        throw new Error('Bootstrap catalog checksum identities differ')
    const targets: BootstrapCatalogTarget[] = []
    for (let index = 0; index < value.targets.length; index += 1) {
        const expected = SUPPORTED_BOOTSTRAP_TARGETS[index]
        const entry = value.targets[index]
        if (!expected || entry === undefined)
            throw new Error('Bootstrap catalog target order is invalid')
        targets.push(parseCatalogTarget(entry, expected))
    }
    const official = targets[0]?.bootstrap.provenance === 'official-release'
    if (targets.some(target => (target.bootstrap.provenance === 'official-release') !== official))
        throw new Error('Bootstrap catalog provenance differs between targets')
    if (official) {
        if (
            integritySignature.status !== 'signed' ||
            integritySignature.kind !== 'detached-release' ||
            integritySignature.identity === '' ||
            integritySignature.signatureFile === ''
        )
            throw new Error('Invalid official bootstrap catalog identity')
    } else if (
        integritySignature.status !== 'unsigned' ||
        integritySignature.kind !== 'none' ||
        integritySignature.identity !== '' ||
        integritySignature.notarized ||
        integritySignature.signatureFile !== ''
    )
        throw new Error('Invalid engineering bootstrap catalog identity')
    return {
        schemaVersion: 1,
        integrity: {
            checksumAlgorithm: 'sha256',
            checksumFile: 'onboarding-bootstrap-catalog.json.sha256',
            signature: integritySignature
        },
        targets
    }
}

export function parseBootstrapControllerKeys(
    value: JsonValue | BootstrapControllerKeys
): BootstrapControllerKeys {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap controller keys')
    exactKeys(value, 'bootstrap controller keys', ['schemaVersion', 'keys'])
    if (value.schemaVersion !== 1 || !Array.isArray(value.keys))
        throw new Error('Invalid bootstrap controller keys')
    const keys = value.keys.map(parseBootstrapPublicKey)
    if (new Set(keys.map(key => key.fingerprintSha256)).size !== keys.length)
        throw new Error('Duplicate bootstrap controller public key')
    return { schemaVersion: 1, keys }
}

export function parseBootstrapHelperResponse(
    value: JsonValue | BootstrapHelperResponse
): BootstrapHelperResponse {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap helper response')
    exactKeys(
        value,
        'bootstrap helper response',
        ['schemaVersion', 'accepted'],
        ['operationId', 'action', 'status', 'receipt', 'reason']
    )
    if (value.schemaVersion !== 1 || typeof value.accepted !== 'boolean')
        throw new Error('Invalid bootstrap helper response')
    const operation =
        value.operationId === undefined
            ? undefined
            : operationId(value.operationId, 'bootstrap helper operation id')
    const action = value.action
    if (
        action !== undefined &&
        action !== 'inspect' &&
        action !== 'apply' &&
        action !== 'verify' &&
        action !== 'rank-reconcile'
    )
        throw new Error('Invalid bootstrap helper action')
    if (!value.accepted) {
        const reason = optionalText(value.reason)
        if (
            !reason ||
            reason.length > 256 ||
            value.status !== undefined ||
            value.receipt !== undefined
        )
            throw new Error('Invalid rejected bootstrap helper response')
        return {
            schemaVersion: 1,
            ...(operation ? { operationId: operation } : {}),
            ...(action ? { action } : {}),
            accepted: false,
            reason
        }
    }
    if (!operation || !action || value.reason !== undefined)
        throw new Error('Invalid accepted bootstrap helper response')
    if (action === 'inspect') {
        if (value.status === undefined || value.receipt !== undefined)
            throw new Error('Invalid bootstrap inspect result')
        const status = parseBootstrapStatus(value.status)
        if (status.operationId !== operation || status.phase !== 'inspect')
            throw new Error('Forged bootstrap inspect result')
        return { schemaVersion: 1, operationId: operation, action, accepted: true, status }
    }
    if (action === 'apply') {
        if (value.status === undefined || value.receipt !== undefined)
            throw new Error('Invalid bootstrap apply result')
        const status = parseBootstrapStatus(value.status)
        if (
            status.operationId !== operation ||
            (status.phase !== 'apply' && status.phase !== 'verify')
        )
            throw new Error('Forged bootstrap apply result')
        return { schemaVersion: 1, operationId: operation, action, accepted: true, status }
    }
    if (action === 'verify') {
        if (value.receipt === undefined || value.status !== undefined)
            throw new Error('Invalid bootstrap verify result')
        const receipt = parseBootstrapReceipt(value.receipt)
        if (receipt.operationId !== operation) throw new Error('Forged bootstrap verify result')
        return { schemaVersion: 1, operationId: operation, action, accepted: true, receipt }
    }
    if (value.status !== undefined || value.receipt !== undefined)
        throw new Error('Invalid bootstrap rank-reconcile result')
    return { schemaVersion: 1, operationId: operation, action, accepted: true }
}

function parseBootstrapReference(
    value:
        | JsonValue
        | BootstrapTargetReference
        | BootstrapRequestInvoke
        | BootstrapPlanInvoke
        | BootstrapOperationInvoke
): BootstrapTargetReference {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap target reference')
    return {
        candidateId: operationId(value.candidateId, 'bootstrap candidate id'),
        accessId: operationId(value.accessId, 'bootstrap access id'),
        hostKeySha256: text(value.hostKeySha256, 'bootstrap SSH host-key fingerprint')
    }
}

export function parseBootstrapRequestInvoke(
    value: JsonValue | BootstrapRequestInvoke
): BootstrapRequestInvoke {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap inspect request')
    exactKeys(value, 'bootstrap inspect request', [
        'candidateId',
        'accessId',
        'hostKeySha256',
        'request'
    ])
    return { ...parseBootstrapReference(value), request: parseBootstrapRequest(value.request) }
}

export function parseBootstrapPlanInvoke(
    value: JsonValue | BootstrapPlanInvoke
): BootstrapPlanInvoke {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap plan request')
    exactKeys(value, 'bootstrap plan request', ['candidateId', 'accessId', 'hostKeySha256', 'plan'])
    return { ...parseBootstrapReference(value), plan: parseBootstrapPlan(value.plan) }
}

export function parseBootstrapOperationInvoke(
    value: JsonValue | BootstrapOperationInvoke
): BootstrapOperationInvoke {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Invalid bootstrap operation request')
    exactKeys(value, 'bootstrap operation request', [
        'candidateId',
        'accessId',
        'hostKeySha256',
        'operationId'
    ])
    return {
        ...parseBootstrapReference(value),
        operationId: operationId(value.operationId, 'bootstrap operation id')
    }
}
