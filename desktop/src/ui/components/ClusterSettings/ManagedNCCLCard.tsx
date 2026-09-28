// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from 'react'
import { Badge, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type {
    DiagnosticMPIManagedInventory,
    DiagnosticMPINetwork,
    DiagnosticMPIReview,
    DiagnosticMPIReviewRequest,
    DiagnosticMPISelection
} from '@/shared/types/diagnostic-mpi'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useDiagnosticMPIStore, watchDiagnosticMPI } from '@/ui/stores/diagnostic-mpi.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import NCCLReplacementCard from './NCCLReplacementCard'

const terminalStates = new Set(['passed', 'failed', 'cancelled'])

interface ManagedNCCLCardProps {
    /** Isolated render/test seed. Product callers always use the live typed store. */
    initialInventory?: DiagnosticMPIManagedInventory
    initialReview?: DiagnosticMPIReview
}

export default function ManagedNCCLCard({
    initialInventory,
    initialReview
}: ManagedNCCLCardProps = {}) {
    const selfId = useConnectionStore(state => state.selfId)
    const nodes = useNodesStore(state => state.nodes)
    const diagnostic = useDiagnosticMPIStore()
    const [buildOperationId, setBuildOperationId] = useState('')
    const [selected, setSelected] = useState<string[]>([])
    const [network, setNetwork] = useState<DiagnosticMPINetwork>('management')
    const [approved, setApproved] = useState(false)
    const [now, setNow] = useState(Date.now())

    useEffect(() => watchDiagnosticMPI(), [])
    const inventory = initialInventory ?? diagnostic.inventory
    const review = initialReview ?? diagnostic.review

    useEffect(() => {
        if (!review) return
        const timer = setInterval(() => setNow(Date.now()), 1000)
        return () => clearInterval(timer)
    }, [review])
    useEffect(() => setApproved(false), [review?.reviewId])

    const records = useMemo(() => inventory?.records ?? [], [inventory])
    useEffect(() => {
        if (records.length === 0) {
            setBuildOperationId('')
            setSelected([])
            return
        }
        if (records.some(record => record.buildOperationId === buildOperationId)) return
        const newest = [...records].sort((a, b) => b.adoptedAt - a.adoptedAt)[0]
        setBuildOperationId(newest.buildOperationId)
        setSelected(newest.targets.map(target => target.nodeId))
    }, [records, buildOperationId])

    const record = records.find(item => item.buildOperationId === buildOperationId) ?? null
    const currentNodeIds = useMemo(
        () =>
            new Set(
                record?.targets
                    .filter(target => {
                        const node = nodes.get(target.nodeId)
                        return target.nodeId === selfId || node?.status === 'active'
                    })
                    .map(target => target.nodeId) ?? []
            ),
        [nodes, record, selfId]
    )
    const effectiveSelected = selected.filter(nodeId => currentNodeIds.has(nodeId)).sort()
    const selection: DiagnosticMPISelection | null =
        record && (effectiveSelected.length === 2 || effectiveSelected.length === 3)
            ? { buildOperationId: record.buildOperationId, nodeIds: effectiveSelected }
            : null
    const reviewRequest: DiagnosticMPIReviewRequest | null = selection
        ? { ...selection, network }
        : null
    const operation = diagnostic.operation
    const operationMoving = !!operation && !terminalStates.has(operation.state)
    const operationHeld = !!operation && (operationMoving || !operation.cleanupConfirmed)
    const reviewExpired = !!review && review.expiresAt <= now
    const recoveredUnstarted =
        !!diagnostic.recovery?.reference && diagnostic.recovery.operation === null
    const lockSelection =
        !!review || operationHeld || recoveredUnstarted || !!diagnostic.approvalRecoverySelection
    const recoverySelection = diagnostic.approvalRecoverySelection ?? selection
    const wrong = operation?.samples.reduce((total, sample) => total + sample.wrong, 0) ?? 0

    const nodeName = (nodeId: string) => nodes.get(nodeId)?.name || nodeId
    const chooseBuild = (next: string) => {
        const chosen = records.find(item => item.buildOperationId === next)
        setBuildOperationId(next)
        setSelected(chosen?.targets.map(target => target.nodeId) ?? [])
        setApproved(false)
    }
    const selectRegistered = async (operationId: string) => {
        await diagnostic.refreshInventory()
        const next = useDiagnosticMPIStore
            .getState()
            .inventory?.records.find(item => item.buildOperationId === operationId)
        if (!next)
            throw new Error(
                'Registration was confirmed, but the new build is not in the refreshed registry.'
            )
        setBuildOperationId(operationId)
        setSelected(next.targets.map(target => target.nodeId))
        setApproved(false)
    }
    const toggleNode = (nodeId: string, checked: boolean) => {
        setApproved(false)
        setSelected(current =>
            checked
                ? [...current.filter(value => value !== nodeId), nodeId].slice(0, 3)
                : current.filter(value => value !== nodeId)
        )
    }
    const chooseNetwork = (next: DiagnosticMPINetwork) => {
        setApproved(false)
        setNetwork(next)
    }

    return (
        <div className="pair-paper p-4 w-full">
            <Stack gap="4">
                <Flex justify="between" align="center" gap="3" wrap="wrap">
                    <Stack gap="1">
                        <Text kind="body/semibold/md">Managed NCCL correctness smoke</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Select exactly two or three current nodes from one PAIR-registered NCCL
                            build, review the fixed plan, then start it once.
                        </Text>
                    </Stack>
                    <Flex gap="2" wrap="wrap">
                        <Badge color={records.length ? 'green' : 'gray'} kind="solid">
                            {records.length} registered build{records.length === 1 ? '' : 's'}
                        </Badge>
                        <Badge
                            color={
                                operation?.state === 'passed'
                                    ? 'green'
                                    : operation?.state === 'failed' || inventory?.recoveryRequired
                                      ? 'red'
                                      : operation
                                        ? 'yellow'
                                        : 'gray'
                            }
                            kind="solid"
                        >
                            NCCL {operation?.state ?? 'not run'}
                        </Badge>
                    </Flex>
                </Flex>

                <InlineErrorBanner
                    severity="warning"
                    message="This fixed small correctness smoke runs NCCL Socket transport on the management IPv4 interface or, when selected, the active fabric; MPI launch and SSH stay on management. It does not use RDMA and does not qualify cable directness, RoCE payload, or bandwidth."
                />
                {inventory?.recoveryRequired && (
                    <InlineErrorBanner
                        severity="error"
                        message="PAIR cannot confirm the complete managed NCCL registry. Review and Start stay held until retained diagnostic cleanup is reconciled."
                    />
                )}
                {diagnostic.error && (
                    <InlineErrorBanner severity="error" message={diagnostic.error} />
                )}

                <Stack gap="2">
                    <label>
                        <Text kind="body/semibold/sm">Registered NCCL build</Text>
                        <select
                            aria-label="Registered NCCL build"
                            value={buildOperationId}
                            disabled={lockSelection || !!diagnostic.pending}
                            onChange={event => chooseBuild(event.target.value)}
                        >
                            <option value="">Select a registered build</option>
                            {records.map(item => (
                                <option key={item.buildOperationId} value={item.buildOperationId}>
                                    {item.buildOperationId.slice(0, 8)} · {item.targets.length}{' '}
                                    nodes · {new Date(item.adoptedAt).toLocaleString()}
                                </option>
                            ))}
                        </select>
                    </label>
                    {record && (
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            This registry entry is adoption authority only. PAIR rechecks the
                            current nodes, account access, artifacts, interfaces and owner binding
                            during Review; it is not proof that NCCL has run.
                        </Text>
                    )}
                </Stack>

                {record && (
                    <fieldset disabled={lockSelection || !!diagnostic.pending}>
                        <legend>
                            <Text kind="body/semibold/sm">
                                Current nodes ({effectiveSelected.length}/2 or 3)
                            </Text>
                        </legend>
                        {record.targets.map(target => {
                            const current = currentNodeIds.has(target.nodeId)
                            return (
                                <label key={target.nodeId} className="block">
                                    <input
                                        type="checkbox"
                                        checked={effectiveSelected.includes(target.nodeId)}
                                        disabled={
                                            !current ||
                                            (!effectiveSelected.includes(target.nodeId) &&
                                                effectiveSelected.length >= 3)
                                        }
                                        onChange={event =>
                                            toggleNode(target.nodeId, event.target.checked)
                                        }
                                    />{' '}
                                    {nodeName(target.nodeId)}
                                    {target.nodeId === selfId ? ' (this controller)' : ''}
                                    {!current ? ' — not current' : ''}
                                </label>
                            )
                        })}
                    </fieldset>
                )}

                {records.length > 0 && (
                    <fieldset disabled={lockSelection || !!diagnostic.pending}>
                        <legend>
                            <Text kind="body/semibold/sm">NCCL Socket network</Text>
                        </legend>
                        <label className="block">
                            <input
                                type="radio"
                                name="managed-nccl-network"
                                checked={network === 'management'}
                                onChange={() => chooseNetwork('management')}
                            />{' '}
                            Management network
                        </label>
                        <label className="block">
                            <input
                                type="radio"
                                name="managed-nccl-network"
                                checked={network === 'fabric'}
                                onChange={() => chooseNetwork('fabric')}
                            />{' '}
                            Fabric
                        </label>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {network === 'fabric'
                                ? 'Uses the active fabric this controller applied: one direct lane for two nodes, or the routed ring for three. Review refuses when no fabric joins exactly these nodes.'
                                : 'Uses the management IPv4 interface of each node.'}
                        </Text>
                    </fieldset>
                )}

                <Flex gap="2" wrap="wrap">
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={
                            !reviewRequest ||
                            !!diagnostic.pending ||
                            !!review ||
                            operationHeld ||
                            !!diagnostic.approvalRecoverySelection ||
                            !!inventory?.recoveryRequired
                        }
                        onClick={() =>
                            reviewRequest && void diagnostic.requestReview(reviewRequest)
                        }
                    >
                        {diagnostic.pending === 'review' ? 'Reviewing…' : 'Review NCCL smoke'}
                    </Button>
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={!recoverySelection || !!diagnostic.pending || !!review}
                        onClick={() =>
                            recoverySelection && void diagnostic.recover(recoverySelection)
                        }
                    >
                        {diagnostic.pending === 'recover' ? 'Recovering…' : 'Recover retained run'}
                    </Button>
                    <Button
                        kind="tertiary"
                        size="small"
                        disabled={!!diagnostic.pending}
                        onClick={() => void diagnostic.refreshInventory()}
                    >
                        Refresh registry
                    </Button>
                </Flex>

                {review && (
                    <Stack gap="2" className="bg-surface-sunken rounded p-3">
                        <Flex justify="between" align="center" gap="2" wrap="wrap">
                            <Text kind="body/semibold/sm">Reviewed fixed plan</Text>
                            <Badge color={reviewExpired ? 'red' : 'yellow'} kind="solid">
                                {reviewExpired ? 'expired' : 'ready to approve'}
                            </Badge>
                        </Flex>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {review.targets.length} nodes · Socket on {review.network}
                            {review.fabric
                                ? ` (operation ${review.fabric.operationId.slice(0, 8)})`
                                : ''}{' '}
                            · owner {nodeName(review.ownerNodeId)} · review expires{' '}
                            {new Date(review.expiresAt).toLocaleTimeString()}
                        </Text>
                        {review.targets.map(target => (
                            <Text
                                key={target.nodeId}
                                kind="body/regular/sm"
                                className="text-subtle-color"
                            >
                                {nodeName(target.nodeId)} · {target.interface} · {target.address}
                            </Text>
                        ))}
                        <label>
                            <input
                                type="checkbox"
                                checked={approved}
                                disabled={reviewExpired || !!diagnostic.pending}
                                onChange={event => setApproved(event.target.checked)}
                            />{' '}
                            I approve one bounded dedicated Socket correctness test on these exact
                            nodes.
                        </label>
                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="primary"
                                size="small"
                                disabled={
                                    !approved ||
                                    reviewExpired ||
                                    !!diagnostic.pending ||
                                    diagnostic.approvalAttemptedReviewId === review.reviewId
                                }
                                onClick={() => void diagnostic.approveReview()}
                            >
                                {diagnostic.pending === 'approve'
                                    ? 'Start sent…'
                                    : 'Start NCCL smoke'}
                            </Button>
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={!!diagnostic.pending}
                                onClick={() => void diagnostic.closeReview()}
                            >
                                {diagnostic.pending === 'close' ? 'Closing…' : 'Close review'}
                            </Button>
                        </Flex>
                    </Stack>
                )}

                {diagnostic.recovery?.recoveryRequired && (
                    <InlineErrorBanner
                        severity="error"
                        message="PAIR could not prove an empty retained NCCL inventory for this selection. Diagnostic admission remains held; use the retained cleanup reconciliation before another Start."
                    />
                )}
                {diagnostic.approvalRecoverySelection && !diagnostic.recovery && (
                    <InlineErrorBanner
                        severity="warning"
                        message="Start was sent once and its outcome is unknown. PAIR will not resend it. Recover this exact build and node selection before changing the selection or requesting another review."
                    />
                )}
                {diagnostic.recovery &&
                    !diagnostic.recovery.reference &&
                    !diagnostic.recovery.recoveryRequired && (
                        <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                            No retained review or operation exists for this exact build and node
                            set.
                        </Text>
                    )}
                {recoveredUnstarted && diagnostic.recovery?.reference && (
                    <Stack gap="2" className="bg-surface-sunken rounded p-3">
                        <InlineErrorBanner
                            severity="warning"
                            message="PAIR recovered an unstarted review. No NCCL operation is claimed. Close this exact review before requesting another."
                        />
                        <Button
                            kind="secondary"
                            size="small"
                            disabled={!!diagnostic.pending}
                            onClick={() => void diagnostic.closeReview()}
                        >
                            {diagnostic.pending === 'close' ? 'Closing…' : 'Close recovered review'}
                        </Button>
                    </Stack>
                )}

                {operation && (
                    <Stack gap="2" className="bg-surface-sunken rounded p-3" role="status">
                        <Flex justify="between" align="center" gap="2" wrap="wrap">
                            <Text kind="body/semibold/sm">
                                Operation {operation.operationId.slice(0, 8)}
                            </Text>
                            <Badge
                                color={
                                    operation.state === 'passed'
                                        ? 'green'
                                        : operation.state === 'failed'
                                          ? 'red'
                                          : 'yellow'
                                }
                                kind="solid"
                            >
                                {operation.state}
                            </Badge>
                        </Flex>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {operation.memberNodeIds.map(nodeName).join(' · ')} · NCCL Socket ·
                            cleanup {operation.cleanupConfirmed ? 'confirmed' : 'not confirmed'}
                        </Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {operation.message}
                        </Text>
                        {operation.samples.length > 0 && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {operation.samples.length} correctness samples · {wrong} wrong
                                result{wrong === 1 ? '' : 's'}. Sample rates are smoke output, not a
                                bandwidth qualification.
                            </Text>
                        )}
                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={!!diagnostic.pending}
                                onClick={() => void diagnostic.refreshOperation()}
                            >
                                {diagnostic.pending === 'status' ? 'Refreshing…' : 'Refresh status'}
                            </Button>
                            <Button
                                kind="secondary"
                                color="danger"
                                size="small"
                                disabled={
                                    !!diagnostic.pending ||
                                    !diagnostic.operationKnown ||
                                    (terminalStates.has(operation.state) &&
                                        operation.cleanupConfirmed)
                                }
                                onClick={() => void diagnostic.cancelOperation()}
                            >
                                {diagnostic.pending === 'cancel'
                                    ? 'Cancelling…'
                                    : operationMoving
                                      ? 'Cancel NCCL smoke'
                                      : 'Retry owned cleanup'}
                            </Button>
                        </Flex>
                    </Stack>
                )}
                <NCCLReplacementCard
                    inventory={inventory ?? null}
                    buildOperationId={buildOperationId}
                    blocked={lockSelection || !!diagnostic.pending}
                    onRegistered={selectRegistered}
                />
            </Stack>
        </div>
    )
}
