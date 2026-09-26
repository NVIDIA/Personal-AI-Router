// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { Badge, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { DiagnosticMPIManagedInventory } from '@/shared/types/diagnostic-mpi'
import type {
    NCCLReplacementOperation,
    NCCLReplacementReview,
    NCCLReplacementSelector,
    NCCLReplacementTarget
} from '@/shared/types/diagnostic-runtime-replacement'
import {
    replacementHeld,
    replacementRetryable
} from '@/shared/types/diagnostic-runtime-replacement'
import { parseReplacementSelector } from '@/shared/utils/diagnostic-runtime-replacement'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'

const storageKey = 'pair.managedNcclReplacement.v1'
const adoptionKey = `${storageKey}.adoptionAttempt`

function savedSelector(): NCCLReplacementSelector | null {
    try {
        return parseReplacementSelector(JSON.parse(localStorage.getItem(storageKey) || 'null'))
    } catch {
        return null
    }
}

function saveSelector(selector: NCCLReplacementSelector | null) {
    try {
        if (selector) localStorage.setItem(storageKey, JSON.stringify(selector))
        else localStorage.removeItem(storageKey)
    } catch {
        // The current session still retains the exact selector.
    }
}

function savedAdoptionAttempt(selector: NCCLReplacementSelector | null): boolean {
    try {
        return !!selector && localStorage.getItem(adoptionKey) === selector.operationId
    } catch {
        return false
    }
}

function saveAdoptionAttempt(operationId: string | null) {
    try {
        if (operationId) localStorage.setItem(adoptionKey, operationId)
        else localStorage.removeItem(adoptionKey)
    } catch {
        // The current session still blocks a second uncertain registration.
    }
}

function targetDetails(target: NCCLReplacementTarget) {
    return (
        <div key={target.nodeId}>
            <Text kind="body/regular/sm">
                {target.nodeId} · {target.state}
                {target.attempt > 0 ? ` · attempt ${target.attempt}/3` : ''} · cleanup{' '}
                {target.cleanupConfirmed ? 'confirmed' : 'not confirmed'}
            </Text>
            {target.reason && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {target.reason}
                </Text>
            )}
            {target.plan && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    NCCL {target.plan.nccl} · nccl-tests {target.plan.ncclTests} · CPU only,{' '}
                    {target.plan.parallelJobs} jobs, {target.plan.maxBuildSeconds / 60} minute
                    maximum per attempt
                </Text>
            )}
        </div>
    )
}

interface Props {
    inventory: DiagnosticMPIManagedInventory | null
    buildOperationId: string
    blocked: boolean
    onRegistered(operationId: string): Promise<void>
    /** Isolated render/test seed. Product callers always use the live typed bridge. */
    initialSelector?: NCCLReplacementSelector
    initialReview?: NCCLReplacementReview
    initialOperation?: NCCLReplacementOperation
    initialAdoptionAttempted?: boolean
}

export default function NCCLReplacementCard({
    inventory,
    buildOperationId,
    blocked,
    onRegistered,
    initialSelector,
    initialReview,
    initialOperation,
    initialAdoptionAttempted
}: Props) {
    const [selector, setSelector] = useState<NCCLReplacementSelector | null>(
        initialSelector ?? savedSelector
    )
    const [review, setReview] = useState<NCCLReplacementReview | null>(initialReview ?? null)
    const [operation, setOperation] = useState<NCCLReplacementOperation | null>(
        initialOperation ?? null
    )
    const [pending, setPending] = useState('')
    const [error, setError] = useState('')
    const [approved, setApproved] = useState(false)
    const [approvalAttempted, setApprovalAttempted] = useState(false)
    const [adoptionAttempted, setAdoptionAttempted] = useState(
        () => initialAdoptionAttempted ?? savedAdoptionAttempt(initialSelector ?? savedSelector())
    )
    const [noOperation, setNoOperation] = useState(false)
    const [now, setNow] = useState(Date.now())
    const activeBuild = selector?.buildOperationId ?? buildOperationId
    const registered = inventory?.records.some(record => record.buildOperationId === activeBuild)
    const expires = !!review && review.expiresAt <= now
    const active = !!operation && (replacementHeld(operation) || !operation.adopted)
    const adoptionCancellationAvailable =
        !!operation && ['adopting-runtime', 'cancelling-adoption'].includes(operation.stage)

    const finish = async (operationId: string) => {
        await onRegistered(operationId)
        saveSelector(null)
        saveAdoptionAttempt(null)
        setSelector(null)
        setReview(null)
        setOperation(null)
        setApprovalAttempted(false)
        setAdoptionAttempted(false)
    }

    useEffect(() => {
        if (!review) return
        const timer = setInterval(() => setNow(Date.now()), 1000)
        return () => clearInterval(timer)
    }, [review])

    const refresh = async (exact: NCCLReplacementSelector) => {
        const result = await window.pairApi.setup.getNCCLReplacementStatus(exact)
        setOperation(result.operation)
        setNoOperation(result.operation === null && !result.recoveryRequired)
        if (result.operation) {
            setReview(null)
            setApprovalAttempted(true)
            if (result.operation.adopted || result.registrationConfirmed)
                await finish(result.operation.operationId)
            else if (
                result.registrationAbsent &&
                ['built', 'adoption-not-published'].includes(result.operation.stage)
            ) {
                saveAdoptionAttempt(null)
                setAdoptionAttempted(false)
            }
        }
        if (result.recoveryRequired)
            setError('Retained NCCL build recovery is required before another action.')
        return result
    }

    useEffect(() => {
        if (!selector || operation || pending || !registered) return
        let live = true
        void window.pairApi.setup
            .getNCCLReplacementStatus(selector)
            .then(async result => {
                if (!live) return
                setOperation(result.operation)
                setNoOperation(result.operation === null && !result.recoveryRequired)
                if (result.operation) setApprovalAttempted(true)
                if (result.operation?.adopted || result.registrationConfirmed) {
                    if (result.operation) await finish(result.operation.operationId)
                } else if (
                    result.registrationAbsent &&
                    result.operation &&
                    ['built', 'adoption-not-published'].includes(result.operation.stage)
                ) {
                    saveAdoptionAttempt(null)
                    setAdoptionAttempted(false)
                }
                if (result.recoveryRequired)
                    setError('Retained NCCL build recovery is required before another action.')
            })
            .catch(reason => {
                if (live) setError(getErrorString(reason))
            })
        return () => {
            live = false
        }
        // An exact saved selector is checked once when the registry arrives.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [registered, selector?.operationId])

    useEffect(() => {
        if (!selector || !operation || !['running', 'cancelling'].includes(operation.state)) return
        let inFlight = false
        const timer = setInterval(() => {
            if (pending || inFlight) return
            inFlight = true
            void refresh(selector)
                .catch(reason => setError(getErrorString(reason)))
                .finally(() => {
                    inFlight = false
                })
        }, 2000)
        return () => clearInterval(timer)
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [selector?.operationId, operation?.revision, operation?.state, pending])

    const act = async (name: string, run: () => Promise<void>) => {
        if (pending) return
        setPending(name)
        setError('')
        try {
            await run()
        } catch (reason) {
            setError(getErrorString(reason))
        } finally {
            setPending('')
        }
    }

    const reviewBuild = () =>
        act('review', async () => {
            const next = await window.pairApi.setup.reviewNCCLReplacement(buildOperationId)
            const exact = {
                buildOperationId: next.buildOperationId,
                reviewId: next.reviewId,
                operationId: next.operationId
            }
            saveSelector(exact)
            setSelector(exact)
            setReview(next)
            setOperation(null)
            setNoOperation(false)
            setApproved(false)
            setApprovalAttempted(false)
            setAdoptionAttempted(false)
            saveAdoptionAttempt(null)
        })

    const approveBuild = () =>
        act('approve', async () => {
            if (!selector) return
            setApprovalAttempted(true)
            const next = await window.pairApi.setup.approveNCCLReplacement(selector)
            setOperation(next)
            setReview(null)
        })

    const registerBuild = () =>
        act('register', async () => {
            if (!selector || !operation) return
            setAdoptionAttempted(true)
            saveAdoptionAttempt(selector.operationId)
            const result = await window.pairApi.setup.adoptNCCLReplacement({
                ...selector,
                expectedRevision: operation.revision
            })
            setOperation(result.operation)
            await finish(result.operation.operationId)
        })

    const recover = () =>
        act('status', async () => {
            if (!selector) return
            await refresh(selector)
        })

    const close = () =>
        act('close', async () => {
            if (!selector) return
            const result = await window.pairApi.setup.closeNCCLReplacementReview(selector)
            if (result.operation) {
                setOperation(result.operation)
                setReview(null)
                return
            }
            if (result.reviewClosed) {
                saveSelector(null)
                saveAdoptionAttempt(null)
                setSelector(null)
                setReview(null)
                setNoOperation(false)
                setApproved(false)
                setApprovalAttempted(false)
            }
        })

    return (
        <div className="bg-surface-sunken rounded p-3">
            <Stack gap="3">
                <Flex justify="between" align="center" gap="2" wrap="wrap">
                    <Text kind="body/semibold/md">Replace a managed NCCL build</Text>
                    <Badge
                        color={operation?.adopted ? 'green' : active ? 'yellow' : 'gray'}
                        kind="solid"
                    >
                        {operation?.adopted ? 'registered' : (operation?.state ?? 'not started')}
                    </Badge>
                </Flex>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    Use a new reviewed build when an OS update changes the adopted host artifacts.
                    The existing registered build remains selectable while this one is built. Review
                    rechecks the current hosts and account access; registration does not run NCCL.
                </Text>
                {error && <InlineErrorBanner severity="error" message={error} />}
                {selector && !registered && (
                    <InlineErrorBanner
                        severity="error"
                        message="The adopted source record is absent from the managed inventory. Replacement is held."
                    />
                )}
                <Flex gap="2" wrap="wrap">
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={
                            !buildOperationId ||
                            !registered ||
                            !!selector ||
                            blocked ||
                            !!pending ||
                            !!inventory?.recoveryRequired
                        }
                        onClick={() => void reviewBuild()}
                    >
                        {pending === 'review' ? 'Reviewing…' : 'Review new build'}
                    </Button>
                    {selector && (
                        <Button
                            kind="tertiary"
                            size="small"
                            disabled={!!pending}
                            onClick={() => void recover()}
                        >
                            {pending === 'status' ? 'Refreshing…' : 'Refresh new build status'}
                        </Button>
                    )}
                </Flex>
                {review && (
                    <Stack gap="2">
                        <Text kind="body/semibold/sm">
                            New review {review.reviewId.slice(0, 8)} ·{' '}
                            {review.canBuild ? 'ready' : 'blocked'}
                            {expires ? ' · expired' : ''}
                        </Text>
                        {review.targets.map(targetDetails)}
                        <label>
                            <input
                                type="checkbox"
                                checked={approved}
                                disabled={
                                    !review.canBuild || expires || !!pending || approvalAttempted
                                }
                                onChange={event => setApproved(event.target.checked)}
                            />{' '}
                            I approve this new CPU-only build on these exact nodes.
                        </label>
                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="primary"
                                size="small"
                                disabled={
                                    !approved ||
                                    !review.canBuild ||
                                    expires ||
                                    !!pending ||
                                    approvalAttempted
                                }
                                onClick={() => void approveBuild()}
                            >
                                {pending === 'approve' ? 'Approval sent…' : 'Approve new build'}
                            </Button>
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={!!pending}
                                onClick={() => void close()}
                            >
                                Close new review
                            </Button>
                        </Flex>
                    </Stack>
                )}
                {noOperation && selector && (
                    <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                        No retained build exists for this exact new review and operation. Close the
                        unstarted review before requesting another.
                    </Text>
                )}
                {noOperation && !review && selector && (
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={!!pending}
                        onClick={() => void close()}
                    >
                        Close unstarted review
                    </Button>
                )}
                {operation && (
                    <Stack gap="2" role="status">
                        <Text kind="body/semibold/sm">
                            New build {operation.operationId.slice(0, 8)} · {operation.state} ·{' '}
                            {operation.stage} · revision {operation.revision}
                        </Text>
                        {operation.targets.map(targetDetails)}
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Cleanup {operation.cleanupConfirmed ? 'confirmed' : 'not confirmed'} ·
                            source {operation.retrySourceStatus} · runtime validation not observed
                        </Text>
                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={
                                    !!pending ||
                                    (adoptionAttempted && !adoptionCancellationAvailable) ||
                                    !replacementHeld(operation)
                                }
                                onClick={() =>
                                    void act('cancel', async () => {
                                        if (!selector) return
                                        setOperation(
                                            await window.pairApi.setup.cancelNCCLReplacement(
                                                selector
                                            )
                                        )
                                    })
                                }
                            >
                                Cancel or reconcile new build
                            </Button>
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={!!pending || !replacementRetryable(operation)}
                                onClick={() =>
                                    void act('retry', async () => {
                                        if (!selector) return
                                        setOperation(
                                            await window.pairApi.setup.retryNCCLReplacement({
                                                ...selector,
                                                expectedRevision: operation.revision
                                            })
                                        )
                                    })
                                }
                            >
                                Retry eligible new build
                            </Button>
                            <Button
                                kind="primary"
                                size="small"
                                disabled={
                                    !!pending ||
                                    operation.state !== 'completed' ||
                                    operation.adopted ||
                                    !operation.cleanupConfirmed ||
                                    adoptionAttempted
                                }
                                onClick={() => void registerBuild()}
                            >
                                {pending === 'register' ? 'Registering…' : 'Register new build'}
                            </Button>
                        </Flex>
                        {adoptionAttempted && !operation.adopted && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Registration outcome is uncertain. Refresh this exact new operation;
                                registration will not be resent.
                            </Text>
                        )}
                    </Stack>
                )}
            </Stack>
        </div>
    )
}
