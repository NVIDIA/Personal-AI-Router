// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { JsonValue as SharedJsonValue } from '@/shared/types/json'
import type {
    BootstrapArtifactIdentity,
    BootstrapBinding,
    BootstrapObservations,
    BootstrapOperationInvoke,
    BootstrapPlan,
    BootstrapPlanInvoke,
    BootstrapRequest,
    BootstrapRequestInvoke,
    BootstrapTargetReference
} from '@/shared/types/onboarding-live'
import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import {
    parseBootstrapCatalog,
    parseBootstrapControllerKeys,
    parseBootstrapOperationInvoke,
    parseOnboardingAccessRequest,
    parseOnboardingAccessResult,
    parseOnboardingCandidate,
    parseOnboardingCandidates,
    parseOnboardingOperation,
    parseOnboardingOperationRequest,
    parseOnboardingReview,
    parseOnboardingReviewRequest,
    parseOnboardingScopes,
    parseBootstrapPlan,
    parseBootstrapPlanInvoke,
    parseBootstrapReceipt,
    parseBootstrapRequestInvoke,
    parseBootstrapStatus
} from '@/shared/utils/onboarding-live'
import { getModularSupervisor } from './modular-supervisor'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

const ONBOARDING_TIMEOUT_MS = 185_000
const MAX_REVIEWED_BOOTSTRAP_PLANS = 32

interface ReviewedBootstrapPlan {
    reference: BootstrapTargetReference
    plan: BootstrapPlan
}

const reviewedBootstrapPlans = new Map<string, ReviewedBootstrapPlan>()

type Method =
    | 'engine:onboarding-candidates'
    | 'engine:onboarding-add-target'
    | 'engine:onboarding-access'
    | 'engine:onboarding-inspect'
    | 'engine:onboarding-approve'
    | 'engine:onboarding-status'
    | 'engine:onboarding-cancel'
    | 'engine:onboarding-retry'
    | 'engine:onboarding-scopes'
    | 'engine:onboarding-discover'
    | 'engine:onboarding-import-artifact'
    | 'engine:onboarding-bootstrap-catalog'
    | 'engine:onboarding-bootstrap-controller-keys'
    | 'engine:onboarding-bootstrap-inspect'
    | 'engine:onboarding-bootstrap-review'
    | 'engine:onboarding-bootstrap-apply'
    | 'engine:onboarding-bootstrap-status'
    | 'engine:onboarding-bootstrap-recover'
    | 'engine:onboarding-bootstrap-verify'

async function call(method: Method, params: JsonObject): Promise<SharedJsonValue> {
    const supervisor = getModularSupervisor()
    if (!supervisor.ready) throw new Error('PAIR service is unavailable for device setup.')
    const value: JsonValue | undefined = await supervisor.callProcess(
        'broker',
        method,
        params,
        ONBOARDING_TIMEOUT_MS
    )
    if (value === undefined) throw new Error('PAIR returned no device setup result.')
    return value
}

function operationParams(
    payload:
        | WsInvokeRequest<'engine:onboarding-status'>
        | WsInvokeRequest<'engine:onboarding-cancel'>
        | WsInvokeRequest<'engine:onboarding-retry'>
        | undefined
): JsonObject {
    if (!payload) throw new Error('A setup operation is required.')
    const request = parseOnboardingOperationRequest(payload)
    return {
        operationId: request.operationId,
        ...(request.candidateId ? { candidateId: request.candidateId } : {})
    }
}

export const onboardingHandlers = {
    'engine:onboarding-candidates': async () =>
        parseOnboardingCandidates(await call('engine:onboarding-candidates', {})),

    'engine:onboarding-add-target': async (
        payload?: WsInvokeRequest<'engine:onboarding-add-target'>
    ) => {
        if (
            !payload ||
            !payload.address.trim() ||
            !Number.isSafeInteger(payload.port) ||
            payload.port < 1 ||
            payload.port > 65535
        )
            throw new Error('Enter a concrete device address and valid SSH port.')
        return parseOnboardingCandidate(
            await call('engine:onboarding-add-target', {
                address: payload.address.trim(),
                port: payload.port,
                ...(payload.label?.trim() ? { label: payload.label.trim() } : {})
            })
        )
    },

    'engine:onboarding-access': async (payload?: WsInvokeRequest<'engine:onboarding-access'>) => {
        if (!payload) throw new Error('Device account access is required.')
        const request = parseOnboardingAccessRequest(payload)
        const params: JsonObject = {
            ...(request.purpose ? { purpose: request.purpose } : {}),
            candidateIds: [...request.candidateIds],
            username: request.username,
            auth: request.auth,
            startupLifetime: request.startupLifetime
        }
        if (request.keyPath) params.keyPath = request.keyPath
        if (request.password) params.password = request.password
        if (request.passphrase) params.passphrase = request.passphrase
        if (request.elevationPassword) params.elevationPassword = request.elevationPassword
        if (request.privateKeyBase64) params.privateKeyBase64 = request.privateKeyBase64
        if (request.credentialProvider) params.credentialProvider = request.credentialProvider
        if (request.credentialPurpose) params.credentialPurpose = request.credentialPurpose
        if (request.publicKeySha256) params.publicKeySha256 = request.publicKeySha256
        if (request.credentialExpiresAt) params.credentialExpiresAt = request.credentialExpiresAt
        try {
            return parseOnboardingAccessResult(await call('engine:onboarding-access', params))
        } finally {
            if (params.password !== undefined) params.password = ''
            if (params.passphrase !== undefined) params.passphrase = ''
            if (params.elevationPassword !== undefined) params.elevationPassword = ''
            if (params.privateKeyBase64 !== undefined) params.privateKeyBase64 = ''
            request.password = undefined
            request.passphrase = undefined
            request.elevationPassword = undefined
            request.privateKeyBase64 = undefined
        }
    },

    'engine:onboarding-inspect': async (payload?: WsInvokeRequest<'engine:onboarding-inspect'>) => {
        if (!payload) throw new Error('A setup selection is required.')
        const request = parseOnboardingReviewRequest(payload)
        return parseOnboardingReview(
            await call('engine:onboarding-inspect', {
                candidateIds: [...request.candidateIds],
                acceptedHostKeys: request.acceptedHostKeys.map(key => ({ ...key })),
                ...(request.artifactId ? { artifactId: request.artifactId } : {})
            })
        )
    },

    'engine:onboarding-approve': async (payload?: WsInvokeRequest<'engine:onboarding-approve'>) => {
        if (!payload || !/^[a-f0-9]{32}$/.test(payload.reviewId))
            throw new Error('An exact setup review is required.')
        return parseOnboardingOperation(
            await call('engine:onboarding-approve', { reviewId: payload.reviewId })
        )
    },

    'engine:onboarding-status': async (payload?: WsInvokeRequest<'engine:onboarding-status'>) =>
        parseOnboardingOperation(await call('engine:onboarding-status', operationParams(payload))),

    'engine:onboarding-cancel': async (payload?: WsInvokeRequest<'engine:onboarding-cancel'>) =>
        parseOnboardingOperation(await call('engine:onboarding-cancel', operationParams(payload))),

    'engine:onboarding-retry': async (payload?: WsInvokeRequest<'engine:onboarding-retry'>) =>
        parseOnboardingOperation(await call('engine:onboarding-retry', operationParams(payload))),

    'engine:onboarding-scopes': async () =>
        parseOnboardingScopes(await call('engine:onboarding-scopes', {})),

    'engine:onboarding-discover': async (
        payload?: WsInvokeRequest<'engine:onboarding-discover'>
    ) => {
        if (!payload || !/^[a-f0-9]{32}$/.test(payload.scopeId))
            throw new Error('Select an observed local network.')
        const discovered = await call('engine:onboarding-discover', {
            scopeId: payload.scopeId,
            ...(payload.cancel ? { cancel: true } : {})
        })
        const parsed = objectValue(discovered, 'setup discovery')
        const candidates = Array.isArray(parsed.candidates)
            ? parsed.candidates.map(parseOnboardingCandidate)
            : []
        return { candidates, artifacts: [] }
    },

    'engine:onboarding-import-artifact': async (
        payload?: WsInvokeRequest<'engine:onboarding-import-artifact'>
    ) => {
        if (!payload?.file.trim()) throw new Error('Select an absolute local package file.')
        return parseArtifactResponse(
            await call('engine:onboarding-import-artifact', { file: payload.file })
        )
    },

    'engine:onboarding-bootstrap-catalog': async () =>
        parseBootstrapCatalog(await call('engine:onboarding-bootstrap-catalog', {})),

    'engine:onboarding-bootstrap-controller-keys': async () =>
        parseBootstrapControllerKeys(await call('engine:onboarding-bootstrap-controller-keys', {})),

    'engine:onboarding-bootstrap-inspect': async (
        payload?: WsInvokeRequest<'engine:onboarding-bootstrap-inspect'>
    ) => {
        if (!payload) throw new Error('A bootstrap inspection request is required.')
        const invocation = parseBootstrapRequestInvoke(payload)
        const status = parseBootstrapStatus(
            await call('engine:onboarding-bootstrap-inspect', requestInvokeParams(invocation))
        )
        requireRequestResult(status.operationId, status.binding, invocation.request)
        return status
    },

    'engine:onboarding-bootstrap-review': async (
        payload?: WsInvokeRequest<'engine:onboarding-bootstrap-review'>
    ) => {
        if (!payload) throw new Error('A bootstrap review request is required.')
        const invocation = parseBootstrapRequestInvoke(payload)
        const plan = parseBootstrapPlan(
            await call('engine:onboarding-bootstrap-review', requestInvokeParams(invocation))
        )
        requireRequestResult(plan.operationId, plan.binding, invocation.request)
        rememberReviewedBootstrapPlan(invocation, plan)
        return plan
    },

    'engine:onboarding-bootstrap-apply': async (
        payload?: WsInvokeRequest<'engine:onboarding-bootstrap-apply'>
    ) => {
        if (!payload) throw new Error('A reviewed bootstrap plan is required.')
        const invocation = parseBootstrapPlanInvoke(payload)
        requireTargetProducedPlan(invocation)
        const status = parseBootstrapStatus(
            await call('engine:onboarding-bootstrap-apply', planInvokeParams(invocation))
        )
        requirePlanResult(status.operationId, status.binding, status.decision, invocation.plan)
        return status
    },

    'engine:onboarding-bootstrap-status': async (
        payload?: WsInvokeRequest<'engine:onboarding-bootstrap-status'>
    ) => {
        if (!payload) throw new Error('A bootstrap operation is required.')
        const invocation = parseBootstrapOperationInvoke(payload)
        const status = parseBootstrapStatus(
            await call('engine:onboarding-bootstrap-status', operationInvokeParams(invocation))
        )
        if (status.operationId !== invocation.operationId)
            throw new Error('PAIR returned a different bootstrap operation.')
        return status
    },

    'engine:onboarding-bootstrap-recover': async (
        payload?: WsInvokeRequest<'engine:onboarding-bootstrap-recover'>
    ) => {
        if (!payload) throw new Error('A bootstrap recovery operation is required.')
        const invocation = parseBootstrapOperationInvoke(payload)
        const status = parseBootstrapStatus(
            await call('engine:onboarding-bootstrap-recover', operationInvokeParams(invocation))
        )
        if (status.operationId !== invocation.operationId)
            throw new Error('PAIR recovered a different bootstrap operation.')
        return status
    },

    'engine:onboarding-bootstrap-verify': async (
        payload?: WsInvokeRequest<'engine:onboarding-bootstrap-verify'>
    ) => {
        if (!payload) throw new Error('A reviewed bootstrap plan is required.')
        const invocation = parseBootstrapPlanInvoke(payload)
        requireTargetProducedPlan(invocation)
        const receipt = parseBootstrapReceipt(
            await call('engine:onboarding-bootstrap-verify', planInvokeParams(invocation))
        )
        requirePlanResult(receipt.operationId, receipt.binding, receipt.decision, invocation.plan)
        reviewedBootstrapPlans.delete(receipt.operationId)
        return receipt
    }
}

function objectValue(value: SharedJsonValue, label: string): { [key: string]: SharedJsonValue } {
    if (value === null || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid ${label}`)
    return value
}

function targetReferenceParams(reference: BootstrapTargetReference): JsonObject {
    return {
        candidateId: reference.candidateId,
        accessId: reference.accessId,
        hostKeySha256: reference.hostKeySha256
    }
}

function requestInvokeParams(invocation: BootstrapRequestInvoke): JsonObject {
    return {
        ...targetReferenceParams(invocation),
        request: bootstrapRequestParams(invocation.request)
    }
}

function planInvokeParams(invocation: BootstrapPlanInvoke): JsonObject {
    return {
        ...targetReferenceParams(invocation),
        plan: bootstrapPlanParams(invocation.plan)
    }
}

function operationInvokeParams(invocation: BootstrapOperationInvoke): JsonObject {
    return {
        ...targetReferenceParams(invocation),
        operationId: invocation.operationId
    }
}

function bootstrapRequestParams(request: BootstrapRequest): JsonObject {
    return {
        schemaVersion: request.schemaVersion,
        operationId: request.operationId,
        binding: bootstrapBindingParams(request.binding)
    }
}

function bootstrapBindingParams(binding: BootstrapBinding): JsonObject {
    return {
        target: {
            platform: binding.target.platform,
            architecture: binding.target.architecture
        },
        lane: binding.lane,
        role: binding.role,
        runtimeOwner: binding.runtimeOwner,
        account: {
            name: binding.account.name,
            homePath: binding.account.homePath,
            authorizedKeysPath: binding.account.authorizedKeysPath
        },
        controllerKey: {
            algorithm: binding.controllerKey.algorithm,
            material: binding.controllerKey.material,
            fingerprintSha256: binding.controllerKey.fingerprintSha256
        },
        endpoint: {
            address: binding.endpoint.address,
            port: binding.endpoint.port
        },
        product: artifactIdentityParams(binding.product),
        helper: artifactIdentityParams(binding.helper)
    }
}

function artifactIdentityParams(identity: BootstrapArtifactIdentity): JsonObject {
    return {
        id: identity.id,
        version: identity.version,
        sha256: identity.sha256,
        path: identity.path
    }
}

function bootstrapPlanParams(plan: BootstrapPlan): JsonObject {
    return {
        schemaVersion: plan.schemaVersion,
        operationId: plan.operationId,
        phase: plan.phase,
        decision: plan.decision,
        binding: bootstrapBindingParams(plan.binding),
        observed: bootstrapObservationsParams(plan.observed),
        actions: [...plan.actions]
    }
}

function bootstrapObservationsParams(observed: BootstrapObservations): JsonObject {
    return {
        sshService: {
            ownership: observed.sshService.ownership,
            endpoint: observed.sshService.endpoint
                ? {
                      address: observed.sshService.endpoint.address,
                      port: observed.sshService.endpoint.port
                  }
                : null
        },
        firewall: {
            ownership: observed.firewall.ownership,
            endpoint: observed.firewall.endpoint
                ? {
                      address: observed.firewall.endpoint.address,
                      port: observed.firewall.endpoint.port
                  }
                : null
        },
        authorizedKey: {
            ownership: observed.authorizedKey.ownership,
            identity: observed.authorizedKey.identity
                ? {
                      accountName: observed.authorizedKey.identity.accountName,
                      path: observed.authorizedKey.identity.path,
                      fingerprintSha256: observed.authorizedKey.identity.fingerprintSha256
                  }
                : null
        },
        helper: {
            ownership: observed.helper.ownership,
            identity: observed.helper.identity
                ? artifactIdentityParams(observed.helper.identity)
                : null
        },
        product: {
            ownership: observed.product.ownership,
            identity: observed.product.identity
                ? artifactIdentityParams(observed.product.identity)
                : null
        },
        runtimeOwners: observed.runtimeOwners.map(owner => ({
            ownership: owner.ownership,
            owner: owner.owner
        }))
    }
}

function requireRequestResult(
    operationId: string,
    binding: BootstrapBinding,
    request: BootstrapRequest
): void {
    if (
        operationId !== request.operationId ||
        JSON.stringify(binding) !== JSON.stringify(request.binding)
    )
        throw new Error('PAIR returned bootstrap state for a different request.')
}

function requirePlanResult(
    operationId: string,
    binding: BootstrapBinding,
    decision: string | undefined,
    plan: BootstrapPlan
): void {
    if (
        operationId !== plan.operationId ||
        decision !== plan.decision ||
        JSON.stringify(binding) !== JSON.stringify(plan.binding)
    )
        throw new Error('PAIR returned bootstrap state for a different reviewed plan.')
}

function rememberReviewedBootstrapPlan(
    reference: BootstrapTargetReference,
    plan: BootstrapPlan
): void {
    if (reviewedBootstrapPlans.size >= MAX_REVIEWED_BOOTSTRAP_PLANS) {
        const oldest = reviewedBootstrapPlans.keys().next().value
        if (oldest !== undefined) reviewedBootstrapPlans.delete(oldest)
    }
    reviewedBootstrapPlans.set(plan.operationId, {
        reference: {
            candidateId: reference.candidateId,
            accessId: reference.accessId,
            hostKeySha256: reference.hostKeySha256
        },
        plan
    })
}

function requireTargetProducedPlan(invocation: BootstrapPlanInvoke): void {
    const reviewed = reviewedBootstrapPlans.get(invocation.plan.operationId)
    if (
        !reviewed ||
        JSON.stringify(reviewed.reference) !==
            JSON.stringify({
                candidateId: invocation.candidateId,
                accessId: invocation.accessId,
                hostKeySha256: invocation.hostKeySha256
            }) ||
        JSON.stringify(reviewed.plan) !== JSON.stringify(invocation.plan)
    )
        throw new Error('Bootstrap plan was not produced by this target review.')
}

function parseArtifactResponse(value: SharedJsonValue) {
    const artifact = objectValue(value, 'setup artifact')
    const sha256 = artifact.sha256
    if (typeof sha256 !== 'string' || !/^[a-fA-F0-9]{64}$/.test(sha256))
        throw new Error('Invalid setup artifact digest')
    if (
        typeof artifact.artifactId !== 'string' ||
        typeof artifact.version !== 'string' ||
        typeof artifact.platform !== 'string' ||
        typeof artifact.arch !== 'string' ||
        typeof artifact.provenance !== 'string'
    )
        throw new Error('Invalid setup artifact')
    return {
        ...(typeof artifact.sourceFingerprint === 'string'
            ? { sourceFingerprint: artifact.sourceFingerprint }
            : {}),
        artifactId: artifact.artifactId,
        version: artifact.version,
        platform: artifact.platform,
        arch: artifact.arch,
        sha256,
        provenance: artifact.provenance
    }
}
