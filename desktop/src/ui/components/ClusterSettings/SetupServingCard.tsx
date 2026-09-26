// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useState } from 'react'
import {
    Badge,
    Button,
    Flex,
    FormField,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import type { OnboardingHistorySummary } from '@/shared/types/onboarding-history'
import type { VllmGroupElevation, VllmGroupSelection } from '@/shared/types/vllm-group'
import { VLLM_QWEN38_MODEL } from '@/shared/constants/vllm'
import type { VllmModelReceipt, VllmRuntimePrepareResult } from '@/shared/types/vllm-model-journey'
import {
    exactVllmModel,
    vllmDistributionOperationId,
    vllmPrepareOperationId,
    vllmPullOperationId
} from '@/shared/utils/vllm-model-journey'
import { vllmGroupCleanupBinding } from '@/shared/utils/vllm-group-cleanup'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import {
    currentVllmGroupOwner,
    useVllmGroupStore,
    vllmGroupSelfHold,
    vllmMutationBlockReason,
    watchVllmGroup
} from '@/ui/stores/vllm-group.store'
import { vllmGroupControls } from '@/ui/utils/vllm-group-controls'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import {
    buildVllmServingReadiness,
    type VllmServingReadiness
} from '@/ui/utils/vllm-serving-readiness'
import getErrorString from '@/shared/utils/get-error-string'
import { useDiagnosticMPIReconcileStore } from '@/ui/stores/diagnostic-mpi-reconcile.store'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import SetupEnrollmentLane from './SetupEnrollmentLane'
import FleetCompatibilityCard from './FleetCompatibilityCard'

const DGX_SPARK_UPDATE_GUIDE = 'https://docs.nvidia.com/dgx/dgx-spark/os-and-component-update.html'

type JourneyKind = 'pull' | 'prepare' | 'distribute'
interface JourneyPending {
    kind: JourneyKind
    operationId: string
    model?: string
    sourceNodeId?: string
}

interface SetupServingCardProps {
    /** Isolated render/test seed. Product callers always use the live backend and stores. */
    initialHistory?: OnboardingHistorySummary
    initialReadiness?: VllmServingReadiness
    initialJourney?: {
        selectedNodeIds: string[]
        sourceNodeId: string
        model: string
        modelReceipts?: Record<string, VllmModelReceipt>
        runtimeResults?: Record<string, VllmRuntimePrepareResult>
    }
}

export default function SetupServingCard({
    initialHistory,
    initialReadiness,
    initialJourney
}: SetupServingCardProps = {}) {
    const nodeMap = useNodesStore(state => state.nodes)
    const statusByNode = useEngineStatusStore(state => state.statusByNode)
    const modelMap = useEngineModelsStore(state => state.models)
    const focusNodeEngineSettings = useOverviewUiStore(state => state.focusNodeEngineSettings)
    const connection = useConnectionStore()
    const group = useVllmGroupStore()
    const diagnosticCleanup = useDiagnosticMPIReconcileStore()
    const engineProgress = useEngineProgressStore(state => state.progress)
    const [history, setHistory] = useState<OnboardingHistorySummary | null>(initialHistory ?? null)
    const [historyError, setHistoryError] = useState<string | null>(null)
    const [historyLoading, setHistoryLoading] = useState(!initialHistory)
    const [historyUpdatedAt, setHistoryUpdatedAt] = useState<number | null>(
        initialHistory ? Date.now() : null
    )
    const [refreshNonce, setRefreshNonce] = useState(0)
    const [selected, setSelected] = useState<string[]>(initialJourney?.selectedNodeIds ?? [])
    const [model, setModel] = useState(initialJourney?.model ?? '')
    const [sourceNodeId, setSourceNodeId] = useState(initialJourney?.sourceNodeId ?? '')
    const [exactModelInput, setExactModelInput] = useState(
        initialJourney?.model ?? VLLM_QWEN38_MODEL
    )
    const [journeyPending, setJourneyPending] = useState<Record<string, JourneyPending>>({})
    const [journeyErrors, setJourneyErrors] = useState<Record<string, string>>({})
    const [modelReceipts, setModelReceipts] = useState<Record<string, VllmModelReceipt>>(
        initialJourney?.modelReceipts ?? {}
    )
    const [runtimeResults, setRuntimeResults] = useState<Record<string, VllmRuntimePrepareResult>>(
        initialJourney?.runtimeResults ?? {}
    )
    const [parallelism, setParallelism] = useState<VllmGroupSelection['parallelism']>()
    const [administratorPasswords, setAdministratorPasswords] = useState<Record<string, string>>({})
    const [now, setNow] = useState(Date.now())

    const selfId = connection.selfId
    const connectionOwner = currentVllmGroupOwner()

    useEffect(() => {
        if (initialHistory) return
        return watchVllmGroup()
    }, [initialHistory])

    useEffect(() => {
        if (!group.review) return
        const timer = setInterval(() => setNow(Date.now()), 1000)
        return () => clearInterval(timer)
    }, [group.review])

    useEffect(() => {
        if (selfId && !selected.includes(selfId)) setSelected([selfId])
    }, [selfId, selected])

    useEffect(() => {
        if (initialHistory) return
        let current = true
        setHistoryLoading(true)
        window.pairApi.setup
            .getHistory()
            .then(value => {
                if (!current) return
                setHistory(value)
                setHistoryError(null)
                setHistoryUpdatedAt(Date.now())
            })
            .catch(error => {
                if (!current) return
                setHistory(null)
                setHistoryError(getErrorString(error))
            })
            .finally(() => {
                if (current) setHistoryLoading(false)
            })
        return () => {
            current = false
        }
    }, [initialHistory, refreshNonce])

    const observedReadiness = useMemo(
        () =>
            buildVllmServingReadiness(
                Array.from(nodeMap.values()),
                Array.from(statusByNode.values()).flatMap(statuses =>
                    Array.from(statuses.values())
                ),
                Array.from(modelMap.values())
            ),
        [nodeMap, statusByNode, modelMap]
    )
    const readiness = initialReadiness ?? observedReadiness

    const candidates = useMemo(
        () =>
            Array.from(nodeMap.values()).filter(
                node => node.id === selfId || node.status === 'active'
            ),
        [nodeMap, selfId]
    )
    // A node that left the candidate list must not stay silently selected:
    // the effective selection is always what the user can currently see.
    const effectiveSelected = useMemo(() => {
        if (initialJourney) return selected
        const candidateIds = new Set(candidates.map(node => node.id))
        return selected.filter(id => candidateIds.has(id))
    }, [candidates, initialJourney, selected])
    useEffect(() => {
        if (!sourceNodeId || !effectiveSelected.includes(sourceNodeId))
            setSourceNodeId(
                selfId && effectiveSelected.includes(selfId) ? selfId : (effectiveSelected[0] ?? '')
            )
    }, [effectiveSelected, selfId, sourceNodeId])
    const coordinatorModels = useMemo(() => {
        const reported = Array.from(modelMap.values())
            .filter(set => set.engineType === 'vllm' && set.nodeId === selfId)
            .flatMap(set => set.models.filter(item => item.downloaded))
        const retained = selfId ? modelReceipts[selfId] : undefined
        if (!retained || reported.some(item => item.name === retained.id)) return reported
        return [
            ...reported,
            {
                name: retained.id,
                size: retained.bytes,
                downloaded: true,
                status: 'idle' as const,
                parameterSize: '',
                quantization: '',
                family: '',
                digest: retained.digest,
                sizeVram: null,
                expiresAt: null,
                expiry: '10m' as const,
                capabilities: []
            }
        ]
    }, [modelMap, modelReceipts, selfId])
    const nodeHasModel = useCallback(
        (nodeId: string, exactModel: string) =>
            modelReceipts[nodeId]?.id === exactModel ||
            Array.from(modelMap.values()).some(
                set =>
                    set.engineType === 'vllm' &&
                    set.nodeId === nodeId &&
                    set.models.some(item => item.downloaded && item.name === exactModel)
            ),
        [modelMap, modelReceipts]
    )
    const missingModelOn = useMemo(
        () => (model ? effectiveSelected.filter(id => !nodeHasModel(id, model)) : []),
        [model, effectiveSelected, nodeHasModel]
    )

    const controls = vllmGroupControls({
        state: group,
        selfId,
        connectionOwner,
        now,
        selectedNodeIds: effectiveSelected,
        model,
        knownNodeIds: new Set(nodeMap.keys())
    })
    const status = group.status
    const retained = !!status?.run
    const run = status?.run ?? group.lastRun
    const groupLoading = !group.known && !group.error
    const selfHold = vllmGroupSelfHold(group)
    const qwenSelected = model === VLLM_QWEN38_MODEL
    const selectedRuntimeResults = effectiveSelected.map(id => runtimeResults[id])
    const allSelectedRuntimeReady =
        effectiveSelected.length >= 2 &&
        effectiveSelected.every(nodeId => {
            const result = runtimeResults[nodeId]
            const status = statusByNode.get(nodeId)?.get('vllm')
            return (
                result?.state === 'ready' &&
                result.provider.qualified &&
                status?.managed === true &&
                status.processStatus === 'stopped' &&
                status.installedVersion === result.runtimeVersion
            )
        })
    const providerClosures = new Set(
        selectedRuntimeResults
            .map(result => result?.provider.observedClosureSha256)
            .filter((value): value is string => !!value)
    )
    const mixedProviderClosures = providerClosures.size > 1
    const providerDrift = selectedRuntimeResults.some(
        result => !!result && (!result.provider.qualified || result.state === 'blocked-provider')
    )
    const sourceHasExactModel = !!sourceNodeId && nodeHasModel(sourceNodeId, exactModelInput)
    let exactInputValid = true
    try {
        exactVllmModel(exactModelInput)
    } catch {
        exactInputValid = false
    }
    let selectedModelIsExactPublic = false
    try {
        if (model) exactVllmModel(model)
        selectedModelIsExactPublic = !!model
    } catch {
        selectedModelIsExactPublic = false
    }
    // ponytail: recovered progress is display-only; local pending owns the cancel request.
    const activeVllmProgress = Array.from(engineProgress.values()).filter(
        entry =>
            entry.engineType === 'vllm' &&
            (entry.operation === 'pull' ||
                entry.operation === 'prepare' ||
                entry.operation === 'distribute') &&
            !['idle', 'complete', 'error'].includes(entry.status.toLowerCase())
    )
    const activeJourneyProgress = activeVllmProgress.filter(entry =>
        entry.operation === 'prepare'
            ? /^[0-9a-f]{32}$/.test(entry.operationId ?? '') &&
              (entry.model === undefined || entry.model === VLLM_QWEN38_MODEL)
            : entry.model === VLLM_QWEN38_MODEL
    )
    const qwenJourneyBusy =
        activeJourneyProgress.length > 0 ||
        Object.values(journeyPending).some(
            pending => pending.kind === 'prepare' || pending.model === VLLM_QWEN38_MODEL
        )
    const qwenJourneyReady =
        qwenSelected &&
        !qwenJourneyBusy &&
        missingModelOn.length === 0 &&
        allSelectedRuntimeReady &&
        !mixedProviderClosures
    const qwenJourneyHold = qwenSelected && !qwenJourneyReady
    const recoveredJourneyBusy = activeVllmProgress.some(entry => !journeyPending[entry.nodeId])
    const anyJourneyPending = Object.keys(journeyPending).length > 0 || recoveredJourneyBusy
    const candidateIds = new Set(candidates.map(node => node.id))
    const matrixNodeIds = Array.from(
        new Set([
            ...effectiveSelected,
            ...activeJourneyProgress.map(entry => entry.nodeId).filter(id => candidateIds.has(id))
        ])
    )
    const administratorNodeIds = group.review
        ? group.review.plan.members.map(member => member.nodeId)
        : run && !run.cleanupConfirmed
          ? run.ranks
                .filter(rank => rank.attempted && !rank.cleanupConfirmed)
                .map(rank => rank.nodeId)
          : effectiveSelected

    const toggleNode = (id: string, checked: boolean) => {
        group.discardReview()
        setSelected(previous =>
            checked ? [...previous, id] : previous.filter(value => value !== id)
        )
    }

    const historyLabel = historyLoading
        ? 'Loading'
        : historyError
          ? 'Unavailable'
          : history?.recoveryRequired
            ? 'Recovery required'
            : history
              ? 'Snapshot'
              : 'Unavailable'
    const historyColor =
        historyError || history?.recoveryRequired ? 'red' : history ? 'green' : 'gray'

    const nodeName = (id: string) => nodeMap.get(id)?.name || id
    const selectedModelOn = (id: string) => statusByNode.get(id)?.get('vllm')?.selectedModel
    const takeAdministratorAccess = (nodeIds: string[]): VllmGroupElevation[] => {
        const elevation = nodeIds.map(nodeId => {
            const entry: VllmGroupElevation = { nodeId }
            const password = administratorPasswords[nodeId]
            if (password) entry.elevationPassword = password
            else entry.nonInteractive = true
            return entry
        })
        setAdministratorPasswords({})
        return elevation
    }

    const reconcileDiagnosticCleanup = async () => {
        const result = await diagnosticCleanup.reconcile()
        if (result?.state !== 'completed') return
        setRefreshNonce(value => value + 1)
        await group.refresh()
    }

    const beginJourney = (nodeId: string, pending: JourneyPending) => {
        setJourneyPending(current => ({ ...current, [nodeId]: pending }))
        setJourneyErrors(current => {
            const next = { ...current }
            delete next[nodeId]
            return next
        })
    }

    const finishJourney = (nodeId: string) =>
        setJourneyPending(current => {
            const next = { ...current }
            delete next[nodeId]
            return next
        })

    const failJourney = (nodeId: string, error: unknown) =>
        setJourneyErrors(current => ({ ...current, [nodeId]: getErrorString(error) }))

    const pullExactOnSource = async () => {
        if (!sourceNodeId) return
        let exactModel: string
        try {
            exactModel = exactVllmModel(exactModelInput)
        } catch (error) {
            failJourney(sourceNodeId, error)
            return
        }
        const operationId = await vllmPullOperationId(sourceNodeId, exactModel)
        beginJourney(sourceNodeId, { kind: 'pull', operationId, model: exactModel })
        try {
            const receipt = await window.pairApi.engines.pullVllmExact({
                nodeId: sourceNodeId,
                model: exactModel,
                operationId
            })
            setModelReceipts(current => ({ ...current, [sourceNodeId]: receipt }))
            setExactModelInput(receipt.id)
            setModel(receipt.id)
            group.discardReview()
        } catch (error) {
            failJourney(sourceNodeId, error)
        } finally {
            finishJourney(sourceNodeId)
        }
    }

    const cancelSourcePullByIdentity = async () => {
        if (!sourceNodeId) return
        try {
            const exactModel = exactVllmModel(exactModelInput)
            const operationId = await vllmPullOperationId(sourceNodeId, exactModel)
            await window.pairApi.engines.cancelVllmPull({
                nodeId: sourceNodeId,
                model: exactModel,
                operationId
            })
        } catch (error) {
            failJourney(sourceNodeId, error)
        }
    }

    const prepareSelectedRuntimes = async () => {
        await Promise.all(
            effectiveSelected.map(async nodeId => {
                const operationId = await vllmPrepareOperationId(nodeId)
                beginJourney(nodeId, { kind: 'prepare', operationId })
                try {
                    const result = await window.pairApi.engines.prepareVllmRuntime({
                        nodeId,
                        operationId
                    })
                    setRuntimeResults(current => ({ ...current, [nodeId]: result }))
                    if (result.state !== 'ready')
                        failJourney(nodeId, result.message || `Runtime ${result.state}.`)
                } catch (error) {
                    failJourney(nodeId, error)
                } finally {
                    finishJourney(nodeId)
                }
            })
        )
        group.discardReview()
    }

    const cancelSelectedPreparations = async () => {
        await Promise.all(
            effectiveSelected.map(async nodeId => {
                try {
                    await window.pairApi.engines.cancelVllmPrepare({
                        nodeId,
                        operationId: await vllmPrepareOperationId(nodeId)
                    })
                } catch (error) {
                    failJourney(nodeId, error)
                }
            })
        )
    }

    const distributeToMissing = async () => {
        if (!sourceNodeId || !model || !nodeHasModel(sourceNodeId, model)) return
        const targets = effectiveSelected.filter(
            id => id !== sourceNodeId && !nodeHasModel(id, model)
        )
        await Promise.all(
            targets.map(async nodeId => {
                const operationId = await vllmDistributionOperationId(sourceNodeId, nodeId, model)
                beginJourney(nodeId, {
                    kind: 'distribute',
                    operationId,
                    model,
                    sourceNodeId
                })
                try {
                    const receipt = await window.pairApi.engines.distributeVllmModel({
                        nodeId,
                        sourceNodeId,
                        model,
                        operationId
                    })
                    setModelReceipts(current => ({ ...current, [nodeId]: receipt }))
                } catch (error) {
                    failJourney(nodeId, error)
                } finally {
                    finishJourney(nodeId)
                }
            })
        )
        group.discardReview()
    }

    const clearMissingDistributions = async () => {
        if (!sourceNodeId || !model) return
        const targets = effectiveSelected.filter(
            id => id !== sourceNodeId && !nodeHasModel(id, model)
        )
        await Promise.all(
            targets.map(async nodeId => {
                const operationId = await vllmDistributionOperationId(sourceNodeId, nodeId, model)
                try {
                    await window.pairApi.engines.cancelVllmDistribution({
                        nodeId,
                        sourceNodeId,
                        model,
                        operationId
                    })
                    finishJourney(nodeId)
                } catch (error) {
                    failJourney(nodeId, error)
                }
            })
        )
    }

    const cancelJourney = async (nodeId: string) => {
        const pending = journeyPending[nodeId]
        if (!pending) return
        try {
            if (pending.kind === 'pull' && pending.model) {
                await window.pairApi.engines.cancelVllmPull({
                    nodeId,
                    model: pending.model,
                    operationId: pending.operationId
                })
            } else if (pending.kind === 'prepare') {
                await window.pairApi.engines.cancelVllmPrepare({
                    nodeId,
                    operationId: pending.operationId
                })
            } else if (pending.kind === 'distribute' && pending.model && pending.sourceNodeId) {
                await window.pairApi.engines.cancelVllmDistribution({
                    nodeId,
                    sourceNodeId: pending.sourceNodeId,
                    model: pending.model,
                    operationId: pending.operationId
                })
            }
        } catch (error) {
            failJourney(nodeId, error)
        }
    }

    const progressFor = (nodeId: string, pending: JourneyPending | undefined) =>
        activeJourneyProgress.find(
            entry =>
                entry.nodeId === nodeId &&
                (!pending ||
                    (entry.operation === pending.kind &&
                        (pending.model === undefined || entry.model === pending.model) &&
                        (!entry.operationId || entry.operationId === pending.operationId)))
        )

    return (
        <div className="pair-paper p-4 w-full">
            <Stack gap="4">
                <Flex justify="between" align="center" gap="3" wrap="wrap">
                    <Stack gap="1">
                        <Text kind="body/semibold/md">Setup &amp; inference readiness</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Point-in-time setup history with current vLLM runtime status and
                            PAIR-retained model receipts.
                        </Text>
                    </Stack>
                    <Flex align="center" gap="2">
                        <Badge color={historyColor} kind="solid">
                            {historyLabel}
                        </Badge>
                        {!initialHistory && (
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={historyLoading || !!group.pending}
                                onClick={() => {
                                    setRefreshNonce(value => value + 1)
                                    void group.refresh()
                                }}
                            >
                                Refresh
                            </Button>
                        )}
                    </Flex>
                </Flex>

                {historyUpdatedAt && (
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Setup snapshot updated {new Date(historyUpdatedAt).toLocaleTimeString()}.
                    </Text>
                )}

                {historyError && (
                    <InlineErrorBanner
                        severity="error"
                        message={`Setup history unavailable: ${historyError}`}
                    />
                )}
                {history?.recoveryRequired && (
                    <InlineErrorBanner
                        severity="error"
                        message={`${history.invalid} invalid retained setup record${history.invalid === 1 ? '' : 's'} block discovery until recovered.`}
                    />
                )}
                {history?.diagnosticRecoveryRequired && (
                    <Stack gap="2" className="bg-surface-sunken rounded p-3">
                        <InlineErrorBanner
                            severity="error"
                            message="Retained diagnostic MPI cleanup is unconfirmed. Engine changes stay held until PAIR rechecks every owned lease."
                        />
                        <Flex gap="2" align="center" wrap="wrap">
                            <Button
                                kind="primary"
                                size="small"
                                disabled={
                                    diagnosticCleanup.pending || historyLoading || !!group.pending
                                }
                                onClick={() => void reconcileDiagnosticCleanup()}
                            >
                                {diagnosticCleanup.pending
                                    ? 'Reconciling diagnostic cleanup…'
                                    : 'Reconcile diagnostic cleanup'}
                            </Button>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                PAIR uses retained identities only; this action takes no account
                                credentials.
                            </Text>
                        </Flex>
                    </Stack>
                )}
                {diagnosticCleanup.error && (
                    <InlineErrorBanner
                        severity="error"
                        message={`Diagnostic cleanup recovery failed: ${diagnosticCleanup.error}`}
                    />
                )}
                {diagnosticCleanup.result && (
                    <Stack gap="2" className="bg-surface-sunken rounded p-3" role="status">
                        <Flex justify="between" align="center" gap="2" wrap="wrap">
                            <Text kind="body/semibold/sm">Diagnostic cleanup result</Text>
                            <Badge
                                color={diagnosticCleanup.result.recoveryRequired ? 'red' : 'green'}
                                kind="solid"
                            >
                                {diagnosticCleanup.result.state.replace('-', ' ')}
                            </Badge>
                        </Flex>
                        {diagnosticCleanup.result.operations.map((operation, index) => (
                            <Stack
                                key={`${operation.nodeId}-${operation.operationId ?? index}`}
                                gap="0"
                            >
                                <Flex justify="between" align="center" gap="2" wrap="wrap">
                                    <Text kind="body/semibold/sm">
                                        {nodeName(operation.nodeId)}
                                    </Text>
                                    <Badge
                                        color={operation.cleanupConfirmed ? 'green' : 'red'}
                                        kind="solid"
                                    >
                                        {operation.state.replace('-', ' ')}
                                    </Badge>
                                </Flex>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {operation.operationId
                                        ? `Operation ${operation.operationId.slice(0, 8)}${operation.ownerNodeId ? ` · owner ${nodeName(operation.ownerNodeId)}` : ''}`
                                        : 'Node or inventory-level recovery check'}
                                    {operation.code
                                        ? ` · ${operation.code.replaceAll('-', ' ')}`
                                        : ''}
                                </Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {operation.message}
                                </Text>
                            </Stack>
                        ))}
                        {diagnosticCleanup.result.operations.length === 0 && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                No retained diagnostic operation required cleanup.
                            </Text>
                        )}
                    </Stack>
                )}
                {group.error && (
                    <InlineErrorBanner
                        severity="error"
                        message={`Serving-group ownership unknown: ${group.error} Every vLLM action stays held until a fresh status read succeeds.`}
                    />
                )}

                <Flex gap="6" wrap="wrap">
                    <Stack gap="0">
                        <Text kind="body/semibold/lg">{history?.total ?? '—'}</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Setup records
                        </Text>
                    </Stack>
                    <Stack gap="0">
                        <Text kind="body/semibold/lg">{readiness.servingReplicas}</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Serving replicas
                        </Text>
                    </Stack>
                    <Stack gap="0">
                        <Text kind="body/semibold/lg">{readiness.reusableModels}</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Models reported
                        </Text>
                    </Stack>
                    <Stack gap="0">
                        <Text kind="body/semibold/lg">{readiness.replicatedModels}</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Models on 2+ nodes
                        </Text>
                    </Stack>
                </Flex>

                <Stack gap="2">
                    {readiness.nodes.map(node => (
                        <Flex
                            key={node.nodeId}
                            justify="between"
                            align="center"
                            gap="3"
                            wrap="wrap"
                        >
                            <Stack gap="0">
                                <Text kind="body/semibold/sm">{node.nodeName}</Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {node.gpuCount} GPU{node.gpuCount === 1 ? '' : 's'} ·{' '}
                                    {node.downloadedModels} model
                                    {node.downloadedModels === 1 ? '' : 's'} reported ·{' '}
                                    {node.loadedModels} loaded · selected model:{' '}
                                    {selectedModelOn(node.nodeId) ?? 'none'}
                                </Text>
                            </Stack>
                            <Badge
                                color={
                                    node.ready
                                        ? 'green'
                                        : node.processStatus === 'running'
                                          ? 'yellow'
                                          : 'gray'
                                }
                                kind="solid"
                            >
                                {node.ready ? 'Ready' : node.processStatus.replace('-', ' ')}
                            </Badge>
                        </Flex>
                    ))}
                    {readiness.nodes.length === 0 && (
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Waiting for node inventory.
                        </Text>
                    )}
                </Stack>

                <FleetCompatibilityCard
                    modelReceipts={modelReceipts}
                    runtimeResults={runtimeResults}
                />

                <SetupEnrollmentLane />

                <Stack gap="3" className="border border-subtle-color rounded p-3">
                    <Stack gap="1">
                        <Text kind="body/semibold/sm">Managed serving group</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Select this controller and one or two current nodes with one exact
                            downloaded model. PAIR reviews the plan, checks every participant, and
                            admits or refuses each step. Review and check are proposals, never a
                            start.
                        </Text>
                    </Stack>

                    {run && (
                        <Stack gap="2">
                            <Flex justify="between" align="center" gap="3" wrap="wrap">
                                <Stack gap="0">
                                    <Text kind="body/semibold/sm">
                                        {retained && controls.fresh
                                            ? 'Retained serving group'
                                            : 'Last reported serving group (not in the current status)'}
                                    </Text>
                                    <Text kind="body/regular/sm" className="text-subtle-color">
                                        Generation {run.generation} · TP{' '}
                                        {run.plan.topology.tensorParallel} · PP{' '}
                                        {run.plan.topology.pipelineParallel} · DP{' '}
                                        {run.plan.topology.dataParallel} · {run.plan.members.length}{' '}
                                        nodes · {run.plan.model}
                                    </Text>
                                </Stack>
                                <Badge
                                    color={
                                        run.state === 'ready'
                                            ? 'green'
                                            : run.state === 'cleanup-required' ||
                                                run.state === 'failed'
                                              ? 'red'
                                              : 'yellow'
                                    }
                                    kind="solid"
                                >
                                    {run.state.replaceAll('-', ' ')}
                                </Badge>
                            </Flex>
                            {run.failure && (
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {run.failure}
                                </Text>
                            )}
                            {run.ranks.map(rank => (
                                <Text
                                    key={rank.nodeId}
                                    kind="body/regular/sm"
                                    className="text-subtle-color"
                                >
                                    {nodeName(rank.nodeId)}:{' '}
                                    {rank.startFailure
                                        ? `start ${rank.startFailure.code === 'cancelled' ? 'cancelled' : 'failed'} at ${rank.startFailure.stage}/${rank.startFailure.code}${rank.startFailure.stderrCode !== 'none' ? ` (${rank.startFailure.stderrCode})` : ''}${rank.startFailure.exit >= 0 ? `, exit ${rank.startFailure.exit}` : ''}`
                                        : rank.started
                                          ? 'started'
                                          : rank.attempted
                                            ? 'attempted'
                                            : 'not attempted'}{' '}
                                    · cleanup{' '}
                                    {rank.cleanupConfirmed
                                        ? 'confirmed'
                                        : rank.cleanupFailure
                                          ? `unconfirmed (${rank.cleanupFailure.stderrCode !== 'none' ? rank.cleanupFailure.stderrCode : `${rank.cleanupFailure.stage}/${rank.cleanupFailure.code}`})`
                                          : 'unconfirmed'}
                                </Text>
                            ))}
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {run.cleanupConfirmed
                                    ? 'Cleanup confirmed for every attempted rank.'
                                    : status?.reason ||
                                      'Owned rank cleanup is unconfirmed; PAIR holds vLLM until it is.'}
                            </Text>
                            {retained && (
                                <Flex gap="2" wrap="wrap">
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!controls.stop.enabled}
                                        title={
                                            controls.stop.hold ??
                                            'Stops only this exact retained operation.'
                                        }
                                        onClick={() =>
                                            void group.stop({
                                                runId: run.runId,
                                                generation: run.generation
                                            })
                                        }
                                    >
                                        {group.pending === 'stop' ? 'Stopping…' : 'Stop group'}
                                    </Button>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!controls.reconcile.enabled}
                                        title={
                                            controls.reconcile.hold ??
                                            'Reconciles this exact retained operation. On a selected node, PAIR may first close its exact retained predecessor before fencing the shown operation.'
                                        }
                                        onClick={() =>
                                            void group.reconcile(
                                                {
                                                    runId: run.runId,
                                                    generation: run.generation
                                                },
                                                takeAdministratorAccess(administratorNodeIds)
                                            )
                                        }
                                    >
                                        {group.pending === 'reconcile'
                                            ? 'Reconciling…'
                                            : 'Reconcile'}
                                    </Button>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!controls.cleanup.enabled}
                                        title={
                                            controls.cleanup.hold ??
                                            'Asks PAIR to reconcile and close every attempted rank of this exact retained operation. Cleanup is confirmed only when the returned status shows every attempted rank cleaned; unresolved ranks stay held.'
                                        }
                                        onClick={() => {
                                            const binding = status
                                                ? vllmGroupCleanupBinding(status)
                                                : null
                                            if (binding) void group.requestCleanup(binding)
                                        }}
                                    >
                                        {group.pending === 'cleanup'
                                            ? 'Requesting cleanup…'
                                            : 'Request cleanup'}
                                    </Button>
                                </Flex>
                            )}
                        </Stack>
                    )}

                    {group.uncertainStart && (
                        <InlineErrorBanner
                            severity="error"
                            message="Start was sent and no fresh status read has reported what PAIR retained yet. PAIR will not resend Start; every vLLM action is held until the next read settles it."
                        />
                    )}

                    <Stack gap="3" className="bg-surface-sunken rounded p-3">
                        <Flex justify="between" align="center" gap="2" wrap="wrap">
                            <Stack gap="0">
                                <Text kind="body/semibold/sm">Model &amp; runtime preparation</Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Pull one immutable public snapshot on a selected source, prepare
                                    the exact runtime, then let PAIR copy verified chunks directly
                                    between paired nodes. Model bytes never pass through the
                                    renderer.
                                </Text>
                            </Stack>
                            <Badge color={qwenJourneyReady ? 'green' : 'gray'} kind="solid">
                                {qwenJourneyReady
                                    ? 'Ready for review'
                                    : qwenJourneyBusy
                                      ? 'Preparation in progress'
                                      : 'Preparation required'}
                            </Badge>
                        </Flex>

                        <Flex gap="2" align="end" wrap="wrap">
                            <FormField slotLabel="Exact public model">
                                <TextInput
                                    value={exactModelInput}
                                    disabled={anyJourneyPending}
                                    onValueChange={value => setExactModelInput(value)}
                                    placeholder="owner/repository@40-character-commit"
                                />
                            </FormField>
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={anyJourneyPending}
                                onClick={() => {
                                    setExactModelInput(VLLM_QWEN38_MODEL)
                                    if (
                                        effectiveSelected.some(id =>
                                            nodeHasModel(id, VLLM_QWEN38_MODEL)
                                        )
                                    )
                                        setModel(VLLM_QWEN38_MODEL)
                                }}
                            >
                                Use fixed Qwen3.8
                            </Button>
                        </Flex>
                        {!exactInputValid && (
                            <Text kind="body/regular/sm" role="alert">
                                Enter owner/repository plus one full lowercase 40-character commit.
                            </Text>
                        )}

                        <label>
                            <Text kind="body/semibold/sm">Source node</Text>
                            <select
                                aria-label="vLLM model source node"
                                value={sourceNodeId}
                                disabled={anyJourneyPending}
                                onChange={event => setSourceNodeId(event.target.value)}
                            >
                                <option value="">Choose a selected source</option>
                                {effectiveSelected.map(nodeId => (
                                    <option key={nodeId} value={nodeId}>
                                        {nodeName(nodeId)}
                                        {nodeId === selfId ? ' (this controller)' : ''}
                                    </option>
                                ))}
                            </select>
                        </label>

                        <Flex gap="2" wrap="wrap">
                            <Button
                                kind="primary"
                                size="small"
                                disabled={
                                    !sourceNodeId ||
                                    !exactInputValid ||
                                    anyJourneyPending ||
                                    controls.held
                                }
                                onClick={() => void pullExactOnSource()}
                            >
                                {sourceNodeId && journeyPending[sourceNodeId]?.kind === 'pull'
                                    ? 'Pulling exact snapshot…'
                                    : sourceHasExactModel
                                      ? 'Verify / resume on source'
                                      : 'Pull on source'}
                            </Button>
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={
                                    effectiveSelected.length < 2 ||
                                    exactModelInput !== VLLM_QWEN38_MODEL ||
                                    anyJourneyPending ||
                                    controls.held
                                }
                                onClick={() => void prepareSelectedRuntimes()}
                            >
                                Prepare exact runtime on selected nodes
                            </Button>
                            <Button
                                kind="secondary"
                                size="small"
                                disabled={
                                    !model ||
                                    !selectedModelIsExactPublic ||
                                    !sourceNodeId ||
                                    !nodeHasModel(sourceNodeId, model) ||
                                    missingModelOn.length === 0 ||
                                    anyJourneyPending ||
                                    controls.held
                                }
                                onClick={() => void distributeToMissing()}
                            >
                                Distribute to missing selected nodes
                            </Button>
                            {!recoveredJourneyBusy && (
                                <>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={
                                            !model ||
                                            !selectedModelIsExactPublic ||
                                            !sourceNodeId ||
                                            missingModelOn.length === 0 ||
                                            Object.values(journeyPending).some(
                                                pending => pending.kind !== 'distribute'
                                            )
                                        }
                                        title="Cancels the exact active copy, waits for it to settle, and removes only its owned resumable checkpoint."
                                        onClick={() => void clearMissingDistributions()}
                                    >
                                        Cancel / clear retained copies
                                    </Button>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={!sourceNodeId || !exactInputValid}
                                        title="Cancels only the exact source/model pull identity. A verified partial remains resumable."
                                        onClick={() => void cancelSourcePullByIdentity()}
                                    >
                                        Cancel source pull
                                    </Button>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        disabled={effectiveSelected.length === 0}
                                        title="Requests cancellation only for each selected node's fixed Qwen3.8 preparation identity."
                                        onClick={() => void cancelSelectedPreparations()}
                                    >
                                        Cancel runtime preparation
                                    </Button>
                                </>
                            )}
                        </Flex>

                        {exactModelInput !== VLLM_QWEN38_MODEL && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Automatic runtime preparation is currently fixed to the supported
                                Qwen3.8 profile. Other exact public models keep their existing
                                runtime.
                            </Text>
                        )}

                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Public, ungated Hugging Face snapshots only. PAIR records the declared
                            license and retains README/LICENSE notices with the model. Local
                            acquisition does not grant redistribution rights.
                        </Text>

                        <Stack gap="2" aria-label="vLLM preparation compatibility matrix">
                            <Text kind="body/semibold/sm">
                                Selected-node compatibility and active work
                            </Text>
                            {matrixNodeIds.map(nodeId => {
                                const pending = journeyPending[nodeId]
                                const progress = progressFor(nodeId, pending)
                                const activeKind = pending?.kind ?? progress?.operation
                                const runtime = runtimeResults[nodeId]
                                const receipt = modelReceipts[nodeId]
                                const closure = runtime?.provider.observedClosureSha256
                                const nodeStatus = statusByNode.get(nodeId)?.get('vllm')
                                const runtimeCurrent =
                                    runtime?.state === 'ready' &&
                                    runtime.provider.qualified &&
                                    nodeStatus?.managed === true &&
                                    nodeStatus.processStatus === 'stopped' &&
                                    nodeStatus.installedVersion === runtime.runtimeVersion
                                return (
                                    <Stack
                                        key={nodeId}
                                        gap="0"
                                        className="border border-subtle-color rounded p-2"
                                    >
                                        <Flex justify="between" align="center" gap="2" wrap="wrap">
                                            <Text kind="body/semibold/sm">{nodeName(nodeId)}</Text>
                                            <Badge
                                                color={
                                                    activeKind
                                                        ? 'gray'
                                                        : runtimeCurrent &&
                                                            model &&
                                                            nodeHasModel(nodeId, model)
                                                          ? 'green'
                                                          : journeyErrors[nodeId]
                                                            ? 'red'
                                                            : 'gray'
                                                }
                                                kind="solid"
                                            >
                                                {activeKind
                                                    ? activeKind
                                                    : runtimeCurrent &&
                                                        model &&
                                                        nodeHasModel(nodeId, model)
                                                      ? 'prepared'
                                                      : 'not ready'}
                                            </Badge>
                                        </Flex>
                                        {!effectiveSelected.includes(nodeId) && (
                                            <Text
                                                kind="body/regular/sm"
                                                className="text-subtle-color"
                                            >
                                                Active on an unselected node
                                            </Text>
                                        )}
                                        <Text kind="body/regular/sm" className="text-subtle-color">
                                            Model:{' '}
                                            {model && nodeHasModel(nodeId, model)
                                                ? 'exact receipt present'
                                                : 'missing'}
                                            {receipt
                                                ? ` · ${receipt.license} · ${receipt.digest.slice(0, 12)}`
                                                : ''}
                                        </Text>
                                        <Text kind="body/regular/sm" className="text-subtle-color">
                                            Runtime: {runtime?.state ?? 'not checked'}
                                            {runtime?.runtimeVersion
                                                ? ` · ${runtime.runtimeVersion}`
                                                : ''}
                                            {runtime?.provider.profileId
                                                ? ` · ${runtime.provider.profileId}`
                                                : ''}
                                            {closure ? ` · provider ${closure.slice(0, 12)}` : ''}
                                            {runtime?.state === 'ready' && !runtimeCurrent
                                                ? ' · current engine status does not match this receipt'
                                                : ''}
                                        </Text>
                                        {(pending || progress) && (
                                            <Flex gap="2" align="center" wrap="wrap">
                                                <Text
                                                    kind="body/regular/sm"
                                                    className="text-subtle-color"
                                                    role="status"
                                                >
                                                    {progress?.status ??
                                                        `${pending?.kind} in progress`}
                                                    {progress?.percent !== undefined &&
                                                    Number.isFinite(progress.percent) &&
                                                    progress.percent >= 0 &&
                                                    progress.percent <= 100
                                                        ? ` · ${progress.percent}%`
                                                        : ''}
                                                    {progress?.operation === 'distribute' &&
                                                    progress.network
                                                        ? progress.network === 'fabric'
                                                            ? ' · over the fabric'
                                                            : ' · over the management network'
                                                        : ''}
                                                </Text>
                                                {pending && (
                                                    <Button
                                                        kind="secondary"
                                                        size="small"
                                                        onClick={() => void cancelJourney(nodeId)}
                                                    >
                                                        Cancel
                                                    </Button>
                                                )}
                                            </Flex>
                                        )}
                                        {journeyErrors[nodeId] && (
                                            <Text kind="body/regular/sm" role="alert">
                                                {journeyErrors[nodeId]}
                                            </Text>
                                        )}
                                        {runtime?.provider.mismatches.slice(0, 3).map(mismatch => (
                                            <Text
                                                key={`${mismatch.name}-${mismatch.reason}`}
                                                kind="body/regular/sm"
                                                className="text-subtle-color"
                                            >
                                                {mismatch.name}: {mismatch.reason}
                                            </Text>
                                        ))}
                                    </Stack>
                                )
                            })}
                        </Stack>

                        {(mixedProviderClosures || providerDrift) && (
                            <Stack gap="2">
                                <InlineErrorBanner
                                    severity="error"
                                    message={
                                        mixedProviderClosures
                                            ? 'Selected Sparks report different qualified provider closures. Start stays held until they are homogeneous.'
                                            : 'At least one selected Spark has provider drift. PAIR will not prepare or start arbitrary packages from the renderer.'
                                    }
                                />
                                <Button
                                    kind="secondary"
                                    size="small"
                                    onClick={() =>
                                        void window.windowApi.window.openExternal(
                                            DGX_SPARK_UPDATE_GUIDE
                                        )
                                    }
                                >
                                    Open DGX Dashboard update guide
                                </Button>
                            </Stack>
                        )}
                    </Stack>

                    <fieldset disabled={controls.held || !!group.pending || anyJourneyPending}>
                        <legend>
                            <Text kind="body/semibold/sm">Participating nodes</Text>
                        </legend>
                        {candidates.map(node => (
                            <label key={node.id} className="block">
                                <input
                                    type="checkbox"
                                    checked={effectiveSelected.includes(node.id)}
                                    disabled={
                                        node.id === selfId ||
                                        (!effectiveSelected.includes(node.id) &&
                                            effectiveSelected.length >= 3)
                                    }
                                    onChange={event => toggleNode(node.id, event.target.checked)}
                                />{' '}
                                {node.name || node.id}
                                {node.id === selfId ? ' (this controller, coordinator)' : ''}
                            </label>
                        ))}
                        {candidates.length === 0 && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Waiting for current cluster nodes.
                            </Text>
                        )}
                    </fieldset>
                    <label>
                        <Text kind="body/semibold/sm">Downloaded model on this controller</Text>
                        <select
                            aria-label="Serving-group model"
                            value={model}
                            disabled={anyJourneyPending || controls.held || !!group.pending}
                            onChange={event => {
                                group.discardReview()
                                setModel(event.target.value)
                                if (event.target.value === VLLM_QWEN38_MODEL)
                                    setParallelism(undefined)
                            }}
                        >
                            <option value="">Select a model</option>
                            {coordinatorModels.map(item => (
                                <option key={item.name} value={item.name}>
                                    {item.name}
                                </option>
                            ))}
                        </select>
                    </label>
                    <label>
                        <Text kind="body/semibold/sm">Parallelism</Text>
                        <select
                            aria-label="Serving-group parallelism"
                            value={parallelism ?? ''}
                            disabled={
                                qwenSelected ||
                                anyJourneyPending ||
                                controls.held ||
                                !!group.pending
                            }
                            onChange={event => {
                                const value = event.target.value
                                if (value !== '' && value !== 'tensor' && value !== 'pipeline')
                                    return
                                group.discardReview()
                                setParallelism(value === '' ? undefined : value)
                            }}
                        >
                            <option value="">
                                {qwenSelected
                                    ? 'Fixed Qwen profile (exactly 2 nodes: TP2+EP2)'
                                    : 'Default (2 nodes: TP2 on an active direct fabric, otherwise PP2; 3 nodes: PP3)'}
                            </option>
                            <option value="tensor">
                                Tensor parallel — split each layer (2 nodes need an active direct
                                fabric)
                            </option>
                            <option value="pipeline">Pipeline parallel — split layers</option>
                        </select>
                    </label>
                    {model && missingModelOn.length > 0 && (
                        <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                            {missingModelOn.length} selected{' '}
                            {missingModelOn.length === 1 ? 'node has' : 'nodes have'} not reported
                            this model. Pull it once on the selected source, then use the typed
                            distribution action above before review.
                        </Text>
                    )}

                    {administratorNodeIds.length > 0 && (
                        <Stack gap="2" className="bg-surface-sunken rounded p-3">
                            <Text kind="body/semibold/sm">
                                Administrator access for owned ranks
                            </Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Start uses one fresh choice per participant. Reconcile uses one
                                fresh choice per unresolved participant for that single product
                                action; on the same selected node, PAIR may first close its exact
                                retained predecessor before fencing the shown operation. Passwords
                                are sent once and cleared; leave a field blank only when that Spark
                                has product-approved passwordless sudo.
                            </Text>
                            <Flex gap="2" wrap="wrap">
                                {administratorNodeIds.map(nodeId => (
                                    <FormField key={nodeId} slotLabel={nodeName(nodeId)}>
                                        <TextInput
                                            type="password"
                                            value={administratorPasswords[nodeId] ?? ''}
                                            disabled={!!group.pending}
                                            onValueChange={value =>
                                                setAdministratorPasswords(current => ({
                                                    ...current,
                                                    [nodeId]: value
                                                }))
                                            }
                                            placeholder="Administrator password"
                                        />
                                    </FormField>
                                ))}
                            </Flex>
                        </Stack>
                    )}

                    <Flex gap="2" wrap="wrap">
                        <Button
                            kind="secondary"
                            size="small"
                            disabled={
                                !controls.review.enabled || qwenJourneyHold || anyJourneyPending
                            }
                            title={
                                (qwenJourneyHold
                                    ? 'Qwen3.8 review requires the exact model and homogeneous prepared runtime on every selected node.'
                                    : controls.review.hold) ??
                                'PAIR reviews the plan; a review is not a start.'
                            }
                            onClick={() =>
                                void group.requestReview({
                                    nodeIds: effectiveSelected,
                                    model,
                                    ...(!qwenSelected && parallelism ? { parallelism } : {})
                                })
                            }
                        >
                            {group.pending === 'review' ? 'Reviewing…' : 'Review group'}
                        </Button>
                        <Button
                            kind="secondary"
                            size="small"
                            disabled={!controls.check.enabled}
                            title={
                                controls.check.hold ??
                                'PAIR checks every reviewed participant; readiness is not a start.'
                            }
                            onClick={() => void group.checkReview()}
                        >
                            {group.pending === 'check' ? 'Checking…' : 'Check participants'}
                        </Button>
                        <Button
                            kind="primary"
                            size="small"
                            disabled={
                                !controls.start.enabled || qwenJourneyHold || anyJourneyPending
                            }
                            title={
                                (qwenJourneyHold
                                    ? 'Qwen3.8 Start is held until every selected node has the exact model, runtime, and one homogeneous provider closure.'
                                    : controls.start.hold) ??
                                'Consumes this exact review once. PAIR may still refuse; the outcome is settled only by a status read.'
                            }
                            onClick={() => {
                                if (group.review)
                                    void group.start(
                                        group.review.reviewId,
                                        takeAdministratorAccess(
                                            group.review.plan.members.map(member => member.nodeId)
                                        )
                                    )
                            }}
                        >
                            {group.pending === 'start' ? 'Start sent…' : 'Start group'}
                        </Button>
                    </Flex>
                    {(controls.start.hold || qwenJourneyHold) && group.review && (
                        <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                            Start held:{' '}
                            {qwenJourneyHold
                                ? 'prepare the exact model/runtime on every selected node and resolve provider-closure drift.'
                                : controls.start.hold}
                        </Text>
                    )}

                    {group.review && (
                        <Stack gap="1">
                            <Text kind="body/semibold/sm">
                                Reviewed plan: {group.review.plan.model}
                            </Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {group.review.plan.members.length} nodes · TP{' '}
                                {group.review.plan.topology.tensorParallel} · PP{' '}
                                {group.review.plan.topology.pipelineParallel} · DP{' '}
                                {group.review.plan.topology.dataParallel}
                                {group.review.plan.topology.expertParallel
                                    ? ` · EP ${group.review.plan.topology.expertParallel}`
                                    : ''}{' '}
                                · vLLM {group.review.plan.runtime}
                                {group.review.plan.transport
                                    ? group.review.plan.transport.subnetAwareRouting === undefined
                                        ? ' · host-buffer RoCE · historical peer-subnet policy not recorded · socket payload fallback off'
                                        : ' · host-buffer RoCE · merged NICs on · socket payload fallback off'
                                    : ''}
                                {group.review.plan.directSocket
                                    ? ` · NCCL Socket on direct fabric lane ${group.review.plan.directSocket.lanes[0].interfaceName}; control stays on the management network`
                                    : ''}
                            </Text>
                            {group.review.plan.members.map(member => (
                                <Text
                                    key={member.nodeId}
                                    kind="body/regular/sm"
                                    className="text-subtle-color"
                                >
                                    {nodeName(member.nodeId)}: {member.gpuUuid}
                                    {member.resources
                                        ? ` · memory fraction ${member.resources.gpu_memory_utilization ?? 'vendor default'} · context ${member.resources.max_model_len ?? 'vendor default'}`
                                        : ''}
                                </Text>
                            ))}
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {controls.reviewExpired
                                    ? 'Review expired. Request a new review.'
                                    : `Review expires ${new Date(group.review.expiresAt).toLocaleTimeString()}.`}{' '}
                                {group.review.activationEnabled
                                    ? 'PAIR reports an admitted native owner for this review.'
                                    : group.review.reason ||
                                      'PAIR has not admitted activation for this review.'}
                            </Text>
                        </Stack>
                    )}
                    {group.check && (
                        <Stack gap="0">
                            <Text kind="body/semibold/sm">
                                Participant check:{' '}
                                {group.check.activationEnabled
                                    ? 'all participants admitted'
                                    : 'not admitted'}
                            </Text>
                            {group.check.participants.map(participant => (
                                <Text
                                    key={participant.nodeId}
                                    kind="body/regular/sm"
                                    className="text-subtle-color"
                                >
                                    {nodeName(participant.nodeId)}: {participant.state}
                                    {participant.reason ? ` — ${participant.reason}` : ''}
                                </Text>
                            ))}
                        </Stack>
                    )}
                    {group.actionError && (
                        <InlineErrorBanner severity="error" message={group.actionError} />
                    )}
                    {group.pending && (
                        <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                            {group.pending === 'start'
                                ? 'Start request sent; waiting for PAIR. Start will not be resent.'
                                : `${group.pending} in progress…`}
                        </Text>
                    )}
                </Stack>

                <Flex gap="2" wrap="wrap">
                    <Button
                        kind="primary"
                        size="small"
                        disabled={!readiness.controlsNodeId || groupLoading || selfHold}
                        title={
                            selfHold
                                ? vllmMutationBlockReason()
                                : 'Opens the vLLM engine settings; a one-node Start needs a selected model.'
                        }
                        onClick={() => {
                            if (readiness.controlsNodeId)
                                focusNodeEngineSettings(readiness.controlsNodeId)
                        }}
                    >
                        Open vLLM controls
                    </Button>
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={
                            !model ||
                            !selectedModelIsExactPublic ||
                            !sourceNodeId ||
                            !nodeHasModel(sourceNodeId, model) ||
                            missingModelOn.length === 0 ||
                            anyJourneyPending ||
                            controls.held
                        }
                        title="PAIR copies verified chunks directly between paired Engine Managers and retains one exact receipt per target."
                        onClick={() => void distributeToMissing()}
                    >
                        Distribute to missing selected nodes
                    </Button>
                </Flex>

                <Text kind="body/regular/sm" className="text-subtle-color">
                    A one-node Start needs a PAIR-managed vLLM that is stopped, one selected
                    downloaded model (Engine settings → vLLM → Model selection), and no held serving
                    group. For a serving group, PAIR can pull one immutable public snapshot on a
                    selected source, copy verified resumable chunks directly over paired mTLS, and
                    review only after every participant reports the exact retained model and
                    runtime.
                </Text>
            </Stack>
        </div>
    )
}
