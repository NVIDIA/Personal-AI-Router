// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createHash } from 'node:crypto'
import type { JsonValue } from '@/shared/types/json'
import bootstrapGolden from '../fixtures/bootstrap-golden.json'
import {
    parseBootstrapControllerKeys,
    parseBootstrapHelperResponse,
    parseBootstrapPlan,
    parseBootstrapReceipt,
    parseBootstrapRequest,
    parseBootstrapStatus,
    parseOnboardingAccessRequest,
    parseOnboardingCandidate,
    parseOnboardingCandidates,
    parseOnboardingOperation,
    parseOnboardingReview,
    partitionOnboardingCandidates
} from '@/shared/utils/onboarding-live'

const ID_A = 'a'.repeat(32)
const ID_B = 'b'.repeat(32)
const REVIEW = 'c'.repeat(32)
const OPERATION = 'd'.repeat(32)
const DIGEST = '1'.repeat(64)

const candidate = (id = ID_A) => ({
    candidateId: id,
    label: `Spark ${id === ID_A ? 'B' : 'C'}`,
    address: id === ID_A ? '192.0.2.11' : '192.0.2.12',
    port: 22,
    accessId: '',
    accessLabel: '',
    accessAvailable: false,
    hostKeyTrusted: false,
    bootstrapSource: 'mdns-observed',
    bootstrapState: 'bootstrap-required',
    reason: 'Select device account access before inspection'
})

const artifact = {
    sourceFingerprint: DIGEST,
    artifactId: 'pair-linux-arm64',
    version: '0.95.0-private.20260921',
    platform: 'linux',
    arch: 'arm64',
    sha256: DIGEST,
    provenance: 'self'
}

const reviewWire = {
    reviewId: REVIEW,
    expiresAt: 2_000_000_000_000,
    controllerNodeId: 'node-a',
    targetClusterId: 'cluster-a',
    canApprove: true,
    targets: [
        {
            action: 'upgrade',
            existingInstallation: {
                nodeId: 'node-b',
                clusterId: 'cluster-a',
                version: '1.2.0',
                sourceFingerprint: DIGEST,
                unit: 'nvidia-pair-headless.service',
                bundle: '/opt/pair/current',
                legacyRollbackSha256: DIGEST
            },
            startupLifetime: 'persistent',
            needsLinger: false,
            candidateId: ID_A,
            label: 'Spark B',
            address: '192.0.2.11',
            port: 22,
            accessId: ID_B,
            accessLabel: 'operator (existing-key)',
            hostname: 'spark-b',
            platform: 'linux',
            arch: 'arm64',
            hostKeySha256: 'SHA256:fixture-b',
            artifact,
            status: 'ready'
        }
    ]
}

const operationWire = {
    operationId: OPERATION,
    reviewId: REVIEW,
    targetClusterId: 'cluster-a',
    revision: 1,
    state: 'running',
    targets: [
        {
            candidateId: ID_A,
            stage: 'installing',
            canRetry: false,
            canCancel: true,
            cleanupConfirmed: false
        }
    ],
    startedAt: 1_000
}

describe('live onboarding parsers', () => {
    it('parses exact candidates, review authority, rollback, and operation state', () => {
        const candidates = parseOnboardingCandidates({
            candidates: [candidate(), candidate(ID_B)],
            artifacts: [artifact]
        })
        expect(candidates.candidates).toHaveLength(2)
        expect(candidates.artifacts[0]?.sourceFingerprint).toBe(DIGEST)

        const review = parseOnboardingReview(reviewWire)
        expect(review.canApprove).toBe(true)
        expect(review.targets[0]?.existingInstallation?.legacyRollbackSha256).toBe(DIGEST)

        const operation = parseOnboardingOperation(operationWire)
        expect(operation.targets[0]?.stage).toBe('installing')
        expect(operation.targets[0]?.cleanupConfirmed).toBe(false)
    })

    it('fails closed on duplicate candidates and approval/readiness drift', () => {
        expect(() =>
            parseOnboardingCandidates({ candidates: [candidate(), candidate()], artifacts: [] })
        ).toThrow(/Duplicate/)
        expect(() => parseOnboardingReview({ ...reviewWire, canApprove: false })).toThrow(
            /approval/
        )
    })
})

const mocks = vi.hoisted(() => ({
    supervisor: { ready: true, callProcess: vi.fn() }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))

import { onboardingHandlers } from '@/electron/service-bridge/onboarding-handlers'

describe('live onboarding bridge', () => {
    beforeEach(() => {
        mocks.supervisor.ready = true
        mocks.supervisor.callProcess.mockReset()
    })

    it('sends transient access once and clears the bridge copy after the call', async () => {
        let sent: JsonValue = null
        mocks.supervisor.callProcess.mockImplementation(
            (_process, method, params): Promise<JsonValue> => {
                sent = structuredClone(params)
                expect(method).toBe('engine:onboarding-access')
                return Promise.resolve({
                    candidates: [
                        {
                            ...candidate(),
                            accessId: ID_B,
                            accessLabel: 'operator (password)',
                            accessAvailable: true,
                            hostKeySha256: 'SHA256:fixture-b'
                        }
                    ]
                })
            }
        )
        const result = await onboardingHandlers['engine:onboarding-access']({
            candidateIds: [ID_A],
            username: 'operator',
            auth: 'password',
            startupLifetime: 'persistent',
            password: 'transient-password',
            elevationPassword: 'transient-admin'
        })
        expect(sent).toMatchObject({
            password: 'transient-password',
            elevationPassword: 'transient-admin'
        })
        expect(result.candidates[0]?.accessAvailable).toBe(true)
        expect(mocks.supervisor.callProcess.mock.calls[0]?.[2]).toMatchObject({
            password: '',
            elevationPassword: ''
        })
    })

    it('carries cable-only access purpose with a session lifetime', async () => {
        let sent: JsonValue = null
        mocks.supervisor.callProcess.mockImplementation(
            (_process, _method, params): Promise<JsonValue> => {
                sent = structuredClone(params)
                return Promise.resolve({
                    candidates: [
                        {
                            ...candidate(),
                            accessId: ID_B,
                            accessLabel: 'operator (password)',
                            accessAvailable: true,
                            hostKeySha256: 'SHA256:fixture-b'
                        }
                    ]
                })
            }
        )

        await onboardingHandlers['engine:onboarding-access']({
            purpose: 'cable',
            candidateIds: [ID_A],
            username: 'operator',
            auth: 'password',
            startupLifetime: 'session',
            password: 'transient-password',
            elevationPassword: 'transient-admin'
        })

        expect(sent).toMatchObject({ purpose: 'cable', startupLifetime: 'session' })
        expect(() =>
            parseOnboardingAccessRequest({
                purpose: 'cable',
                candidateIds: [ID_A],
                username: 'operator',
                auth: 'password',
                startupLifetime: 'persistent',
                password: 'transient-password'
            })
        ).toThrow(/session scoped/)
    })

    it('binds inspect, approve, and status to exact identifiers', async () => {
        mocks.supervisor.callProcess
            .mockResolvedValueOnce(reviewWire)
            .mockResolvedValueOnce(operationWire)
            .mockResolvedValueOnce(operationWire)
        const review = await onboardingHandlers['engine:onboarding-inspect']({
            candidateIds: [ID_A],
            acceptedHostKeys: [{ candidateId: ID_A, sha256: 'SHA256:fixture-b' }]
        })
        const operation = await onboardingHandlers['engine:onboarding-approve']({
            reviewId: review.reviewId
        })
        await onboardingHandlers['engine:onboarding-status']({
            operationId: operation.operationId
        })
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:onboarding-inspect',
            {
                candidateIds: [ID_A],
                acceptedHostKeys: [{ candidateId: ID_A, sha256: 'SHA256:fixture-b' }]
            },
            185_000
        )
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:onboarding-approve',
            { reviewId: REVIEW },
            185_000
        )
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            3,
            'broker',
            'engine:onboarding-status',
            { operationId: OPERATION },
            185_000
        )
    })
})

describe('bootstrap contract', () => {
    it('parses the Go golden request, status, plan, complete receipt, and helper result', () => {
        const request = parseBootstrapRequest(bootstrapGolden.request)
        const status = parseBootstrapStatus(bootstrapGolden.status)
        const plan = parseBootstrapPlan(bootstrapGolden.plan)
        const receipt = parseBootstrapReceipt(bootstrapGolden.receipt)
        const helper = parseBootstrapHelperResponse(bootstrapGolden.helperResponse)

        expect(request.binding.target.architecture).toBe('arm64')
        expect(status.phase).toBe('inspect')
        expect(plan.actions).toEqual([
            'ssh-service',
            'firewall',
            'authorized-key',
            'helper',
            'pair-artifact',
            'active-role'
        ])
        expect(receipt.phase).toBe('complete')
        expect(helper.status?.operationId).toBe(request.operationId)
    })

    it('rejects unknown, malformed, and forged nested bootstrap fields', () => {
        const malformedMaterial = btoa('\u0000\u0000\u0000\u000bssh-ed25519')
        const malformedFingerprint = createHash('sha256')
            .update(Buffer.from(malformedMaterial, 'base64'))
            .digest('hex')
        expect(() =>
            parseBootstrapRequest({
                ...bootstrapGolden.request,
                observations: bootstrapGolden.status.observed
            })
        ).toThrow(/unknown/i)
        expect(() =>
            parseBootstrapPlan({
                ...bootstrapGolden.plan,
                actions: ['helper']
            })
        ).toThrow(/deterministic/i)
        expect(() =>
            parseBootstrapReceipt({
                ...bootstrapGolden.receipt,
                phase: 'receipt'
            })
        ).toThrow(/phase/i)
        expect(() =>
            parseBootstrapRequest({
                ...bootstrapGolden.request,
                binding: {
                    ...bootstrapGolden.request.binding,
                    controllerKey: {
                        ...bootstrapGolden.request.binding.controllerKey,
                        fingerprintSha256: 'f'.repeat(64)
                    }
                }
            })
        ).toThrow(/does not match/i)
        expect(() =>
            parseBootstrapRequest({
                ...bootstrapGolden.request,
                binding: {
                    ...bootstrapGolden.request.binding,
                    controllerKey: {
                        algorithm: 'ssh-ed25519',
                        material: malformedMaterial,
                        fingerprintSha256: malformedFingerprint
                    }
                }
            })
        ).toThrow(/material/i)
        expect(() =>
            parseBootstrapControllerKeys({
                schemaVersion: 1,
                keys: [
                    bootstrapGolden.request.binding.controllerKey,
                    bootstrapGolden.request.binding.controllerKey
                ]
            })
        ).toThrow(/duplicate/i)
    })

    it('partitions only backend-declared bootstrap state and excludes paired serving nodes', () => {
        const ready = parseOnboardingCandidate({
            ...candidate(),
            bootstrapSource: 'mdns+ssh-banner',
            bootstrapState: 'ssh-ready'
        })
        const required = parseOnboardingCandidate({
            ...candidate(ID_B),
            bootstrapSource: 'manual',
            bootstrapState: 'bootstrap-required'
        })
        const partition = partitionOnboardingCandidates([ready, required])
        expect(partition.sshReady.map(item => item.candidateId)).toEqual([ID_A])
        expect(partition.bootstrapRequired.map(item => item.candidateId)).toEqual([ID_B])
    })
})

describe('bootstrap bridge authority', () => {
    beforeEach(() => {
        mocks.supervisor.ready = true
        mocks.supervisor.callProcess.mockReset()
    })

    it('forwards only candidate, access, host-key, request, plan, and operation identifiers', async () => {
        mocks.supervisor.callProcess
            .mockResolvedValueOnce(bootstrapGolden.status)
            .mockResolvedValueOnce(bootstrapGolden.plan)
            .mockResolvedValueOnce({ ...bootstrapGolden.status, phase: 'apply', decision: 'apply' })
            .mockResolvedValueOnce(bootstrapGolden.status)
            .mockResolvedValueOnce({ ...bootstrapGolden.status, phase: 'apply', decision: 'apply' })
            .mockResolvedValueOnce(bootstrapGolden.receipt)

        const reference = {
            candidateId: ID_A,
            accessId: ID_B,
            hostKeySha256: 'SHA256:fixture-host-key'
        }
        await onboardingHandlers['engine:onboarding-bootstrap-inspect']({
            ...reference,
            request: parseBootstrapRequest(bootstrapGolden.request)
        })
        const plan = await onboardingHandlers['engine:onboarding-bootstrap-review']({
            ...reference,
            request: parseBootstrapRequest(bootstrapGolden.request)
        })
        await onboardingHandlers['engine:onboarding-bootstrap-apply']({ ...reference, plan })
        await onboardingHandlers['engine:onboarding-bootstrap-status']({
            ...reference,
            operationId: plan.operationId
        })
        await onboardingHandlers['engine:onboarding-bootstrap-recover']({
            ...reference,
            operationId: plan.operationId
        })
        await onboardingHandlers['engine:onboarding-bootstrap-verify']({ ...reference, plan })

        expect(mocks.supervisor.callProcess.mock.calls.map(call => call[1])).toEqual([
            'engine:onboarding-bootstrap-inspect',
            'engine:onboarding-bootstrap-review',
            'engine:onboarding-bootstrap-apply',
            'engine:onboarding-bootstrap-status',
            'engine:onboarding-bootstrap-recover',
            'engine:onboarding-bootstrap-verify'
        ])
        expect(mocks.supervisor.callProcess.mock.calls[0]?.[2]).toEqual({
            ...reference,
            request: bootstrapGolden.request
        })
    })

    it('rejects renderer-authored observations and receipts before broker dispatch', async () => {
        const forgedInspect = {
            candidateId: ID_A,
            accessId: ID_B,
            hostKeySha256: 'SHA256:fixture-host-key',
            request: parseBootstrapRequest(bootstrapGolden.request),
            observations: bootstrapGolden.status.observed
        }
        const forgedApply = {
            candidateId: ID_A,
            accessId: ID_B,
            hostKeySha256: 'SHA256:fixture-host-key',
            plan: parseBootstrapPlan(bootstrapGolden.plan),
            receipt: bootstrapGolden.receipt
        }
        const forgedPlan = parseBootstrapPlan({
            ...bootstrapGolden.plan,
            operationId: 'e'.repeat(32)
        })

        await expect(
            onboardingHandlers['engine:onboarding-bootstrap-inspect'](forgedInspect)
        ).rejects.toThrow(/unknown/i)
        await expect(
            onboardingHandlers['engine:onboarding-bootstrap-apply'](forgedApply)
        ).rejects.toThrow(/unknown/i)
        await expect(
            onboardingHandlers['engine:onboarding-bootstrap-apply']({
                candidateId: ID_A,
                accessId: ID_B,
                hostKeySha256: 'SHA256:fixture-host-key',
                plan: forgedPlan
            })
        ).rejects.toThrow(/not produced/i)
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })
})
