// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useRef, useState } from 'react'
import { Badge, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { FabricInventorySnapshot, FabricPortRef, FabricSelection } from '@/shared/types/fabric'
import type { NodeItem } from '@/shared/types/nodes'
import {
    acceptedCleanupHostKeys,
    acceptedHostKeys,
    cableSucceeded,
    fabricOperationCarriesCableMatch,
    parseFabricSelection
} from '@/shared/utils/fabric'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useFabricStore, watchFabric } from '@/ui/stores/fabric.store'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import FabricTopologyDiagram from './FabricTopologyDiagram'
import {
    buildFabricTopologyCandidate,
    fabricInventoryExpiryDelay
} from '@/ui/utils/fabric-topology'

type Layout = 'direct' | 'ring'

const portKey = (nodeId: string, portName: string) => `${nodeId}\n${portName}`
const switchKey = (nodeId: string, portName: string) => `${portKey(nodeId, portName)}\nswitch`
const directNameKey = (nodeId: string) => `${nodeId}\ndirect-name`

interface FabricSetupCardProps {
    /** Isolated render/test seed. Product callers use current stores and Engine Manager. */
    initialTopology?: {
        selfId: string
        nodes: NodeItem[]
        inventory: FabricInventorySnapshot
    }
}

export default function FabricSetupCard({ initialTopology }: FabricSetupCardProps = {}) {
    const currentSelfId = useConnectionStore(state => state.selfId)
    const currentNodes = useNodesStore(state => state.nodes)
    const selfId = initialTopology?.selfId ?? currentSelfId
    const nodes = useMemo(
        () =>
            initialTopology
                ? new Map(initialTopology.nodes.map(node => [node.id, node]))
                : currentNodes,
        [currentNodes, initialTopology]
    )
    const fabric = useFabricStore()
    const [layout, setLayout] = useState<Layout>('direct')
    const [selected, setSelected] = useState<string[]>([])
    const [portValues, setPortValues] = useState<Record<string, string>>({})
    const [cableApproved, setCableApproved] = useState(false)
    const [cleanupApproved, setCleanupApproved] = useState(false)
    const [fabricApproved, setFabricApproved] = useState(false)
    const [pauseApproved, setPauseApproved] = useState(false)
    const [manualOverride, setManualOverride] = useState(false)
    const [inventoryNow, setInventoryNow] = useState(() => Date.now())
    const appliedCandidate = useRef('')
    const refreshedExpiredInventory = useRef('')

    useEffect(() => (initialTopology ? undefined : watchFabric()), [initialTopology])
    useEffect(() => {
        if (selfId && !selected.includes(selfId)) setSelected(previous => [selfId, ...previous])
    }, [selfId, selected])

    const candidates = useMemo(
        () =>
            Array.from(nodes.values()).filter(
                node => node.id === selfId || node.status === 'active'
            ),
        [nodes, selfId]
    )
    const detectionNodeIds = useMemo(() => {
        const ids = candidates.map(node => node.id)
        return selfId ? [selfId, ...ids.filter(id => id !== selfId)].slice(0, 3) : ids.slice(0, 3)
    }, [candidates, selfId])
    useEffect(() => {
        if (initialTopology) return
        if (detectionNodeIds.length > 0) void fabric.refreshInventory(detectionNodeIds)
    }, [detectionNodeIds.join('\n'), initialTopology]) // eslint-disable-line react-hooks/exhaustive-deps
    const inventory = initialTopology?.inventory ?? fabric.inventory
    useEffect(() => {
        if (initialTopology) return
        const delay = fabricInventoryExpiryDelay(inventory, detectionNodeIds)
        if (delay === null) return
        const identity = (inventory?.nodes ?? [])
            .map(node => `${node.nodeId}:${node.observedAt ?? 0}`)
            .sort()
            .join('\n')
        if (delay === 0 && refreshedExpiredInventory.current === identity) return
        const timer = setTimeout(() => {
            refreshedExpiredInventory.current = identity
            setInventoryNow(Date.now())
            void fabric.refreshInventory(detectionNodeIds)
        }, delay + 1)
        return () => clearTimeout(timer)
    }, [detectionNodeIds.join('\n'), initialTopology, inventory]) // eslint-disable-line react-hooks/exhaustive-deps

    const operation = fabric.fabricOperation
    const cable = fabric.cableRun
    const cablePass = cableSucceeded(cable) || fabricOperationCarriesCableMatch(operation)
    const cableActive = !!cable && ['preparing', 'running', 'cancelling'].includes(cable.state)
    const cableHold = !!cable && !cable.cleanupConfirmed && !cable.cleanupRecovery?.holdReleased
    const journeyLocked = (!!operation && !operation.cleanupConfirmed) || cableHold
    const candidateNames = useMemo(
        () => new Map(candidates.map(node => [node.id, node.name || node.id])),
        [candidates]
    )
    const detected = useMemo(
        () =>
            buildFabricTopologyCandidate(
                detectionNodeIds,
                inventory,
                cable,
                candidateNames,
                inventoryNow
            ),
        [detectionNodeIds, inventory, cable, candidateNames, inventoryNow]
    )

    const applyDetected = (discard = true) => {
        const detectedSelection = detected.selection
        if (!detectedSelection) return
        const nextLayout: Layout = detectedSelection.nodeIds.length === 3 ? 'ring' : 'direct'
        const nextPorts: Record<string, string> = {}
        for (const port of detectedSelection.ports) {
            if (nextLayout === 'direct') nextPorts[directNameKey(port.nodeId)] = port.portName
            nextPorts[switchKey(port.nodeId, port.portName)] = port.switchId
        }
        setLayout(nextLayout)
        setSelected(detectedSelection.nodeIds)
        setPortValues(nextPorts)
        setManualOverride(false)
        setCableApproved(false)
        if (discard) fabric.discardReview()
    }

    useEffect(() => {
        if (manualOverride || journeyLocked) return
        if (!detected.selection) {
            if (!appliedCandidate.current) return
            appliedCandidate.current = ''
            setSelected(selfId ? [selfId] : [])
            setPortValues({})
            setCableApproved(false)
            fabric.discardReview()
            return
        }
        const key = JSON.stringify(detected.selection)
        if (appliedCandidate.current === key) return
        appliedCandidate.current = key
        applyDetected(false)
    }, [detected.selection, journeyLocked, manualOverride]) // eslint-disable-line react-hooks/exhaustive-deps

    const requiredNodes = layout === 'direct' ? 2 : 3
    const effectiveSelected = selected
        .filter(id => candidates.some(node => node.id === id))
        .slice(0, requiredNodes)

    const selection = useMemo((): FabricSelection | null => {
        if (effectiveSelected.length !== requiredNodes) return null
        const ports: FabricPortRef[] = []
        for (const nodeId of effectiveSelected) {
            const names =
                layout === 'ring'
                    ? ['p0', 'p1']
                    : [(portValues[directNameKey(nodeId)] ?? 'p0').trim()]
            for (const portName of names) {
                const switchId = (portValues[switchKey(nodeId, portName)] ?? '').trim()
                if (!switchId || !portName) return null
                ports.push({ nodeId, switchId, portName })
            }
        }
        try {
            return parseFabricSelection({ nodeIds: effectiveSelected, ports })
        } catch {
            return null
        }
    }, [effectiveSelected, layout, portValues, requiredNodes])

    const needsPause = Boolean(
        fabric.fabricReview?.targets.some(target =>
            target.interfaces.some(iface => iface.generatedDefault)
        )
    )
    const untrustedKeys = fabric.cableReview ? acceptedHostKeys(fabric.cableReview) : []
    const cleanupKeys = fabric.cleanupReview ? acceptedCleanupHostKeys(fabric.cleanupReview) : []

    const nodeName = (id: string) => nodes.get(id)?.name || id
    const resetJourney = (nextLayout: Layout) => {
        setManualOverride(true)
        setLayout(nextLayout)
        setSelected(selfId ? [selfId] : [])
        setPortValues({})
        setCableApproved(false)
        setCleanupApproved(false)
        setFabricApproved(false)
        setPauseApproved(false)
        fabric.discardReview()
    }
    const toggleNode = (nodeId: string, checked: boolean) => {
        setManualOverride(true)
        fabric.discardReview()
        setCableApproved(false)
        setSelected(previous =>
            checked
                ? [...previous.filter(id => id !== nodeId), nodeId].slice(0, requiredNodes)
                : previous.filter(id => id !== nodeId)
        )
    }
    const changePort = (key: string, value: string) => {
        setManualOverride(true)
        fabric.discardReview()
        setCableApproved(false)
        setPortValues(previous => ({ ...previous, [key]: value }))
    }
    const reviewCable = (extra?: FabricSelection['acceptedHostKeys']) => {
        if (selection)
            void fabric.reviewCable({ ...selection, ...(extra ? { acceptedHostKeys: extra } : {}) })
    }

    return (
        <div className="pair-paper p-4 w-full">
            <Stack gap="4">
                <Flex justify="between" align="center" gap="3" wrap="wrap">
                    <Stack gap="1">
                        <Text kind="body/semibold/md">Physical cable &amp; fabric setup</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Verify the selected physical ports, then review temporary fast-network
                            addresses. PAIR remains authoritative for every status and cleanup hold.
                        </Text>
                    </Stack>
                    <Flex gap="2" wrap="wrap">
                        <Badge color={cablePass ? 'green' : cable ? 'yellow' : 'gray'} kind="solid">
                            Cable {cablePass ? 'matched' : (cable?.state ?? 'not checked')}
                        </Badge>
                        <Badge
                            color={
                                operation?.state === 'active'
                                    ? 'green'
                                    : operation?.state === 'recovery-required'
                                      ? 'red'
                                      : operation
                                        ? 'yellow'
                                        : 'gray'
                            }
                            kind="solid"
                        >
                            Fabric {operation?.state?.replaceAll('-', ' ') ?? 'not configured'}
                        </Badge>
                    </Flex>
                </Flex>

                <InlineErrorBanner
                    severity="warning"
                    message="Cable observations can match the selected ports, but directness remains unverified. Fabric qualification proves a current certificate-pinned control route and RoCE-v2 binding only; it does not prove RDMA traffic, bandwidth, NCCL, or inference."
                />
                {fabric.error && <InlineErrorBanner severity="error" message={fabric.error} />}
                {fabric.retained?.held && (
                    <InlineErrorBanner
                        severity="error"
                        message={
                            fabric.retained.reason ||
                            'A retained cable operation or unconfirmed cleanup holds another check.'
                        }
                    />
                )}

                {!journeyLocked && (
                    <Stack gap="3">
                        <Stack gap="2" className="bg-surface-sunken rounded p-3">
                            <Flex justify="between" align="center" gap="2" wrap="wrap">
                                <Stack gap="0">
                                    <Text kind="body/semibold/sm">
                                        Detected{' '}
                                        {detected.layout === 'single'
                                            ? 'one-node setup'
                                            : detected.layout === 'direct'
                                              ? 'two-node direct candidate'
                                              : 'three-node ring candidate'}
                                    </Text>
                                    <Text kind="body/regular/sm" className="text-subtle-color">
                                        {detected.selection
                                            ? 'PAIR prefilled current eligible physical-port identities.'
                                            : 'PAIR needs a current eligible port observation before review.'}
                                    </Text>
                                </Stack>
                                <Badge color={detected.selection ? 'green' : 'yellow'} kind="solid">
                                    {detected.selection ? 'Exact ports found' : 'Needs attention'}
                                </Badge>
                            </Flex>
                            <FabricTopologyDiagram candidate={detected} nodeName={nodeName} />
                            {detected.issues.map(issue => (
                                <Text
                                    key={issue}
                                    kind="body/regular/sm"
                                    className="text-subtle-color"
                                >
                                    • {issue}
                                </Text>
                            ))}
                            <Flex gap="2" wrap="wrap">
                                <Button
                                    kind="secondary"
                                    size="small"
                                    disabled={!!fabric.pending || cableActive}
                                    onClick={() => {
                                        appliedCandidate.current = ''
                                        setManualOverride(false)
                                        void fabric.refreshInventory(detectionNodeIds)
                                    }}
                                >
                                    Refresh detection
                                </Button>
                                {detected.selection && manualOverride && (
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!!fabric.pending || cableActive}
                                        onClick={() => applyDetected()}
                                    >
                                        Use detected wiring
                                    </Button>
                                )}
                            </Flex>
                        </Stack>

                        <details>
                            <summary>Advanced fallback: choose nodes and physical-port IDs</summary>
                            <Stack gap="3" className="pt-3">
                                <fieldset disabled={!!fabric.pending || cableActive}>
                                    <legend>
                                        <Text kind="body/semibold/sm">Topology</Text>
                                    </legend>
                                    <label className="mr-4">
                                        <input
                                            type="radio"
                                            name="fabric-layout"
                                            checked={layout === 'direct'}
                                            onChange={() => resetJourney('direct')}
                                        />{' '}
                                        Two-node direct
                                    </label>
                                    <label>
                                        <input
                                            type="radio"
                                            name="fabric-layout"
                                            checked={layout === 'ring'}
                                            onChange={() => resetJourney('ring')}
                                        />{' '}
                                        Three-node ring
                                    </label>
                                </fieldset>

                                <fieldset disabled={!!fabric.pending || cableActive}>
                                    <legend>
                                        <Text kind="body/semibold/sm">
                                            Nodes ({effectiveSelected.length}/{requiredNodes})
                                        </Text>
                                    </legend>
                                    {candidates.map(node => (
                                        <label key={node.id} className="block">
                                            <input
                                                type="checkbox"
                                                checked={effectiveSelected.includes(node.id)}
                                                disabled={
                                                    node.id === selfId ||
                                                    (!effectiveSelected.includes(node.id) &&
                                                        effectiveSelected.length >= requiredNodes)
                                                }
                                                onChange={event =>
                                                    toggleNode(node.id, event.target.checked)
                                                }
                                            />{' '}
                                            {node.name || node.id}
                                            {node.id === selfId ? ' (this controller)' : ''}
                                        </label>
                                    ))}
                                </fieldset>

                                {effectiveSelected.map(nodeId => (
                                    <Stack gap="1" key={nodeId}>
                                        <Text kind="body/semibold/sm">
                                            {nodeName(nodeId)} physical ports
                                        </Text>
                                        {(layout === 'ring' ? ['p0', 'p1'] : ['direct']).map(
                                            slot => {
                                                const portName =
                                                    slot === 'direct'
                                                        ? (portValues[directNameKey(nodeId)] ??
                                                          'p0')
                                                        : slot
                                                return (
                                                    <Flex
                                                        key={slot}
                                                        gap="2"
                                                        wrap="wrap"
                                                        align="end"
                                                    >
                                                        {slot === 'direct' && (
                                                            <label>
                                                                <Text
                                                                    kind="body/regular/sm"
                                                                    className="text-subtle-color"
                                                                >
                                                                    Port name
                                                                </Text>
                                                                <input
                                                                    aria-label={`${nodeName(nodeId)} port name`}
                                                                    value={portName}
                                                                    disabled={
                                                                        !!fabric.pending ||
                                                                        cableActive
                                                                    }
                                                                    onChange={event =>
                                                                        changePort(
                                                                            directNameKey(nodeId),
                                                                            event.target.value
                                                                        )
                                                                    }
                                                                />
                                                            </label>
                                                        )}
                                                        <label>
                                                            <Text
                                                                kind="body/regular/sm"
                                                                className="text-subtle-color"
                                                            >
                                                                {slot === 'direct'
                                                                    ? 'Switch / physical-port ID'
                                                                    : `${slot} switch ID`}
                                                            </Text>
                                                            <input
                                                                aria-label={`${nodeName(nodeId)} ${portName} switch ID`}
                                                                value={
                                                                    portValues[
                                                                        switchKey(nodeId, portName)
                                                                    ] ?? ''
                                                                }
                                                                disabled={
                                                                    !!fabric.pending || cableActive
                                                                }
                                                                onChange={event =>
                                                                    changePort(
                                                                        switchKey(nodeId, portName),
                                                                        event.target.value
                                                                    )
                                                                }
                                                                placeholder="Exact ID reported for this port"
                                                            />
                                                        </label>
                                                    </Flex>
                                                )
                                            }
                                        )}
                                    </Stack>
                                ))}
                            </Stack>
                        </details>

                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={!selection || !!fabric.pending || cableActive}
                                onClick={() => reviewCable()}
                            >
                                {fabric.pending === 'cable-review'
                                    ? 'Reviewing…'
                                    : 'Review cable check'}
                            </Button>
                            {untrustedKeys.length > 0 && (
                                <Button
                                    kind="secondary"
                                    size="small"
                                    disabled={!!fabric.pending}
                                    onClick={() => reviewCable(untrustedKeys)}
                                >
                                    Re-review with listed host keys
                                </Button>
                            )}
                            {cableActive && (
                                <Button
                                    kind="secondary"
                                    color="danger"
                                    size="small"
                                    disabled={!!fabric.pending || !fabric.cableKnown}
                                    onClick={() => void fabric.cancelCable()}
                                >
                                    Cancel cable check
                                </Button>
                            )}
                        </Flex>

                        {fabric.cableReview && (
                            <Stack gap="2">
                                <Text kind="body/semibold/sm">
                                    Review {fabric.cableReview.available ? 'ready' : 'held'} · PAIR
                                    reported {Math.ceil(fabric.cableReview.remainingMs / 1000)} s
                                    remaining when read
                                </Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {fabric.cableReview.reason ||
                                        'Review the exact finite effects.'}
                                </Text>
                                {fabric.cableReview.targets.map(target => (
                                    <Text
                                        key={target.nodeId}
                                        kind="body/regular/sm"
                                        className="text-subtle-color"
                                    >
                                        {nodeName(target.nodeId)} · raw-network privilege{' '}
                                        {target.rawPrivilege} · {target.ports.length} exact port
                                        {target.ports.length === 1 ? '' : 's'}
                                    </Text>
                                ))}
                                {fabric.cableReview.permission?.effects.map(effect => (
                                    <Text
                                        key={effect}
                                        kind="body/regular/sm"
                                        className="text-subtle-color"
                                    >
                                        • {effect}
                                    </Text>
                                ))}
                                <label>
                                    <input
                                        type="checkbox"
                                        checked={cableApproved}
                                        onChange={event => setCableApproved(event.target.checked)}
                                    />{' '}
                                    I approve only this reviewed finite administrator cable check.
                                </label>
                                <Button
                                    kind="primary"
                                    size="small"
                                    disabled={
                                        !fabric.cableReview.available ||
                                        !cableApproved ||
                                        !!fabric.pending
                                    }
                                    onClick={() => void fabric.approveCable()}
                                >
                                    {fabric.pending === 'cable-approve'
                                        ? 'Approval sent…'
                                        : 'Approve cable check'}
                                </Button>
                            </Stack>
                        )}
                    </Stack>
                )}

                {cable && (
                    <Stack gap="1">
                        <Text kind="body/semibold/sm">
                            Cable check: {cable.state} · {cable.result}
                        </Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {cable.message} Cleanup{' '}
                            {cable.cleanupConfirmed ? 'confirmed' : 'unconfirmed'}; directness{' '}
                            {cable.directness}.
                        </Text>
                        {cable.topology && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {cable.topology.layout} topology: {cable.topology.status};{' '}
                                {cable.edges.length} reciprocal edge
                                {cable.edges.length === 1 ? '' : 's'} reported.
                            </Text>
                        )}
                        {cableHold && (
                            <Stack gap="2">
                                <InlineErrorBanner
                                    severity="error"
                                    message="Owned cable-worker cleanup is unconfirmed. PAIR keeps another cable or fabric operation held until this exact run is verified and released."
                                />
                                <Flex gap="2" wrap="wrap">
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!!fabric.pending}
                                        onClick={() => void fabric.reviewCableCleanup()}
                                    >
                                        {fabric.pending === 'cable-cleanup-review'
                                            ? 'Reviewing cleanup…'
                                            : 'Review exact cleanup recovery'}
                                    </Button>
                                    {cleanupKeys.length > 0 && (
                                        <Button
                                            kind="secondary"
                                            size="small"
                                            disabled={!!fabric.pending}
                                            onClick={() =>
                                                void fabric.reviewCableCleanup(cleanupKeys)
                                            }
                                        >
                                            Re-review cleanup with listed host keys
                                        </Button>
                                    )}
                                    {cable.cleanupRecovery &&
                                        ['verifying', 'release-pending'].includes(
                                            cable.cleanupRecovery.state
                                        ) && (
                                            <Button
                                                kind="secondary"
                                                color="danger"
                                                size="small"
                                                disabled={!!fabric.pending || !fabric.cableKnown}
                                                onClick={() => void fabric.cancelCableCleanup()}
                                            >
                                                Cancel cleanup verification
                                            </Button>
                                        )}
                                </Flex>
                                {fabric.cleanupReview && (
                                    <Stack gap="1">
                                        <Text kind="body/semibold/sm">
                                            Cleanup review{' '}
                                            {fabric.cleanupReview.available ? 'ready' : 'held'} ·
                                            PAIR reported{' '}
                                            {Math.ceil(fabric.cleanupReview.remainingMs / 1000)} s
                                            remaining when read
                                        </Text>
                                        {fabric.cleanupReview.effects.map(effect => (
                                            <Text
                                                key={effect}
                                                kind="body/regular/sm"
                                                className="text-subtle-color"
                                            >
                                                • {effect}
                                            </Text>
                                        ))}
                                        <label>
                                            <input
                                                type="checkbox"
                                                checked={cleanupApproved}
                                                onChange={event =>
                                                    setCleanupApproved(event.target.checked)
                                                }
                                            />{' '}
                                            I approve only the reviewed inspection and cleanup of
                                            this exact held cable run.
                                        </label>
                                        <Button
                                            kind="primary"
                                            size="small"
                                            disabled={
                                                !fabric.cleanupReview.available ||
                                                !cleanupApproved ||
                                                !!fabric.pending
                                            }
                                            onClick={() => void fabric.verifyCableCleanup()}
                                        >
                                            {fabric.pending === 'cable-cleanup-verify'
                                                ? 'Verification sent…'
                                                : 'Approve cleanup verification'}
                                        </Button>
                                    </Stack>
                                )}
                                {cable.cleanupRecovery && (
                                    <Text kind="body/regular/sm" className="text-subtle-color">
                                        Cleanup recovery {cable.cleanupRecovery.state}:{' '}
                                        {cable.cleanupRecovery.message} Hold{' '}
                                        {cable.cleanupRecovery.holdReleased
                                            ? 'released'
                                            : 'still active'}
                                        .
                                    </Text>
                                )}
                            </Stack>
                        )}
                        {cable.cleanupRecovery?.holdReleased && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Exact recovery verification released the scheduling hold. The
                                original missing cleanup acknowledgement remains historical and is
                                not relabeled as confirmed.
                            </Text>
                        )}
                    </Stack>
                )}

                {cablePass && !operation && (
                    <Stack gap="2">
                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={!!fabric.pending}
                                onClick={() => void fabric.reviewFabric(false)}
                            >
                                {fabric.pending === 'fabric-review'
                                    ? 'Reviewing…'
                                    : 'Review fabric setup'}
                            </Button>
                            {fabric.fabricReview?.inspectionRequired &&
                                fabric.fabricReview.inspectionAvailable && (
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!!fabric.pending}
                                        onClick={() => void fabric.reviewFabric(true)}
                                    >
                                        {fabric.pending === 'fabric-inspect'
                                            ? 'Inspecting…'
                                            : 'Inspect selected profiles'}
                                    </Button>
                                )}
                        </Flex>

                        {fabric.fabricReview && (
                            <Stack gap="2">
                                <Text kind="body/semibold/sm">
                                    Fabric review {fabric.fabricReview.state} ·{' '}
                                    {fabric.fabricReview.persistence.replace('-', ' ')}
                                </Text>
                                {fabric.fabricReview.blockers.map(blocker => (
                                    <Text
                                        key={blocker}
                                        kind="body/regular/sm"
                                        className="text-subtle-color"
                                    >
                                        Hold: {blocker}
                                    </Text>
                                ))}
                                {fabric.fabricReview.permission?.effects.map(effect => (
                                    <Text
                                        key={effect}
                                        kind="body/regular/sm"
                                        className="text-subtle-color"
                                    >
                                        • {effect}
                                    </Text>
                                ))}
                                <label>
                                    <input
                                        type="checkbox"
                                        checked={fabricApproved}
                                        onChange={event => setFabricApproved(event.target.checked)}
                                    />{' '}
                                    I approve only these reviewed temporary addresses and exact
                                    rollback ownership.
                                </label>
                                {needsPause && (
                                    <label>
                                        <input
                                            type="checkbox"
                                            checked={pauseApproved}
                                            onChange={event =>
                                                setPauseApproved(event.target.checked)
                                            }
                                        />{' '}
                                        I approve the reviewed selected-port interruption during
                                        this administration window.
                                    </label>
                                )}
                                <Button
                                    kind="primary"
                                    size="small"
                                    disabled={
                                        !fabric.fabricReview.executable ||
                                        !fabricApproved ||
                                        (needsPause && !pauseApproved) ||
                                        !!fabric.pending
                                    }
                                    onClick={() => void fabric.approveFabric(pauseApproved)}
                                >
                                    {fabric.pending === 'fabric-approve'
                                        ? 'Approval sent…'
                                        : 'Approve fabric setup'}
                                </Button>
                            </Stack>
                        )}
                    </Stack>
                )}

                {operation && (
                    <Stack gap="2">
                        <Text kind="body/semibold/sm">
                            Fabric operation: {operation.state.replaceAll('-', ' ')}
                        </Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {operation.message}
                        </Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Effects{' '}
                            {operation.effectsApplied ? 'reported applied' : 'not reported applied'}
                            {operation.effectsUnconfirmed ? ' (readback unconfirmed)' : ''}; cleanup{' '}
                            {operation.cleanupConfirmed ? 'confirmed' : 'unconfirmed'}.
                        </Text>
                        {operation.state === 'active' && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {operation.qualifiedAt && operation.candidateIPs?.length
                                    ? `${operation.candidateIPs.length} peer-specific fast-control endpoints are qualified.`
                                    : 'Fast-control qualification is not reported.'}{' '}
                                RDMA traffic, RoCE payload, NCCL, bandwidth and inference remain
                                unverified.
                            </Text>
                        )}
                        <Flex gap="2" wrap="wrap">
                            {['applying', 'active'].includes(operation.state) && (
                                <Button
                                    kind="secondary"
                                    color="danger"
                                    size="small"
                                    disabled={!!fabric.pending || !fabric.fabricKnown}
                                    onClick={() => void fabric.cancelFabric()}
                                >
                                    {fabric.pending === 'fabric-cancel'
                                        ? 'Rolling back…'
                                        : 'Rollback owned fabric addresses'}
                                </Button>
                            )}
                            {operation.state === 'recovery-required' && (
                                <Button
                                    kind="primary"
                                    size="small"
                                    disabled={!!fabric.pending || !fabric.fabricKnown}
                                    onClick={() => void fabric.recoverFabric()}
                                >
                                    {fabric.pending === 'fabric-recover'
                                        ? 'Recovering…'
                                        : 'Recover exact owned cleanup'}
                                </Button>
                            )}
                        </Flex>
                    </Stack>
                )}
            </Stack>
        </div>
    )
}
