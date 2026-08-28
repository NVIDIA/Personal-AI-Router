// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { BackendInfo } from '@/ui/types/engine-info'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import { ModelExpiry } from '@/shared/types/engines'

import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { ModelHubModal } from '@/ui/components/ModelHub/ModelHubModal'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { isEnginePullInProgress } from '@/shared/utils/engine-progress'

import ModelRow from './ModelRow'
import { IncomingSyncPullRow } from './IncomingSyncPullRow'
import { TransientModelStatusRow } from './TransientModelStatusRow'
import type { IncomingSyncRow } from '@/ui/types/model-manager'
import type { ModelEntry } from '@/ui/types/model-hub'

export function ModelManager({ backend, nodeId }: { backend: BackendInfo; nodeId: string }) {
    const [openModelHubModal, setOpenModelHubModal] = useState(false)
    const [modelPendingDelete, setModelPendingDelete] = useState<string | null>(null)
    const models = (backend.models ?? []).sort((a, b) =>
        formatModelDisplayName(a.name, backend.type).localeCompare(
            formatModelDisplayName(b.name, backend.type)
        )
    )
    const caps = EngineCapabilities[backend.type]

    const backendType = backend.type
    const getProgress = useEngineProgressStore(state => state.getProgress)

    /**
     * Track only this engine's pull-progress keys so we re-render only when a
     * pull for THIS engine changes — not every progress tick cluster-wide.
     */
    const pullProgressFingerprint = useEngineProgressStore(state => {
        const prefix = `${nodeId}:${backend.type}:pull`
        const parts: string[] = []
        for (const [key, p] of state.progress) {
            if (key.startsWith(prefix)) {
                parts.push(`${p.model ?? ''}:${p.status}:${p.percent ?? ''}`)
            }
        }
        return parts.join('|')
    })

    /**
     * Re-render when an optimistic model action for this engine begins/clears;
     * the per-model pending action is read below via getState().
     */
    const modelPendingFingerprint = usePendingActionsStore(state => {
        const prefix = `${nodeId}:${backend.type}:model:`
        const parts: string[] = []
        for (const [key, p] of state.pending) {
            if (key.startsWith(prefix)) parts.push(`${p.model ?? ''}:${p.action}`)
        }
        return parts.join('|')
    })
    void modelPendingFingerprint

    const displayName = useCallback(
        (name: string) => formatModelDisplayName(name, backend.type),
        [backend.type]
    )

    const isBusy = models.some(
        m => m.status === 'loading' || m.status === 'ejecting' || m.status === 'pulling'
    )
    const transientModel = models.find(
        m => m.status === 'loading' || m.status === 'ejecting' || m.status === 'pulling'
    )

    // pullProgressFingerprint triggers re-renders when progress changes;
    // the actual data is read from the store below.
    void pullProgressFingerprint

    const incomingSyncs: IncomingSyncRow[] = (() => {
        const modelNames = new Set(models.map(m => m.name))
        const results: IncomingSyncRow[] = []

        for (const p of useEngineProgressStore.getState().getProgressForNode(nodeId)) {
            if (
                p.engineType !== backend.type ||
                !isEnginePullInProgress(p) ||
                !p.model ||
                modelNames.has(p.model)
            ) {
                continue
            }

            results.push({
                rawModel: p.model,
                label: displayName(p.model),
                status: p.status,
                percent: p.percent,
                completed: p.completed,
                total: p.total
            })
        }

        return results
    })()

    // Only an engine that restarts to pick a deletion up (LM Studio today)
    // confirms first, because the restart interrupts in-flight inference.
    // Ollama and every other engine delete straight away — this stays false for
    // them, which `tests/modular/delete-model-restart.test.ts` pins against the
    // engine-manager manifests. The restart itself belongs to the engine
    // manager, not to this click.
    const confirmBeforeDelete = caps?.restartsOnModelDelete ?? false

    const handleAction = useCallback(
        (modelName: string, action: string) => {
            switch (action) {
                case 'load':
                    window.pairApi.engines.loadModel(backendType, nodeId, modelName)
                    break
                case 'eject':
                    window.pairApi.engines.unloadModel(backendType, nodeId, modelName)
                    break
                case 'delete':
                    if (confirmBeforeDelete) {
                        setModelPendingDelete(modelName)
                        break
                    }
                    window.pairApi.engines.deleteModel(backendType, nodeId, modelName)
                    break
            }
        },
        [backendType, confirmBeforeDelete, nodeId]
    )

    // `ConfirmModal` closes (`onOpenChange(false)`) before it calls `onConfirm`,
    // which clears `modelPendingDelete`. This still reads the right model: the
    // callback closes over the value from the render that showed the modal, and
    // React's state update does not mutate that binding.
    const handleConfirmDelete = useCallback(() => {
        if (modelPendingDelete) {
            window.pairApi.engines.deleteModel(backendType, nodeId, modelPendingDelete)
        }
    }, [backendType, modelPendingDelete, nodeId])

    const handleExpiryChange = useCallback(
        (modelName: string, value: ModelExpiry) => {
            window.pairApi.engines.setModelExpiry(backendType, nodeId, modelName, value)
        },
        [backendType, nodeId]
    )

    const transientPullProgress = transientModel
        ? getProgress(nodeId, backend.type, 'pull', transientModel.name)
        : undefined

    const hasModelSearchOnlyWhenRunning = useMemo(
        () => caps?.hasModelSearchOnlyWhenRunning ?? false,
        [caps?.hasModelSearchOnlyWhenRunning]
    )
    const modelOpsWhenStopped = useMemo(
        () => caps?.modelOpsWhenStopped ?? false,
        [caps?.modelOpsWhenStopped]
    )
    const isRunning = useMemo(() => backend.processStatus === 'running', [backend.processStatus])
    const supportsSearch = useMemo(
        () => !hasModelSearchOnlyWhenRunning || isRunning,
        [hasModelSearchOnlyWhenRunning, isRunning]
    )

    const handleDownload = useCallback(
        (entries: ModelEntry[]) => {
            if (!entries || entries.length === 0) return

            setOpenModelHubModal(false)

            for (const entry of entries) {
                if (entry.name) {
                    window.pairApi.engines.pullModel(backendType, nodeId, entry.name)
                }
            }
        },
        [backendType, nodeId]
    )

    return (
        <Stack gap="4">
            {transientModel && (
                <TransientModelStatusRow
                    transientModel={transientModel}
                    displayName={displayName}
                    pullProgress={transientPullProgress}
                />
            )}

            {incomingSyncs.map(p => (
                <IncomingSyncPullRow key={p.rawModel} row={p} />
            ))}

            {!isBusy && (
                <>
                    {(isRunning || modelOpsWhenStopped) &&
                        models.length === 0 &&
                        incomingSyncs.length === 0 && (
                            <Text kind="body/regular/sm" className="text-subtle-color pl-2">
                                No model/weight files downloaded
                            </Text>
                        )}
                    {!modelOpsWhenStopped &&
                        models.length === 0 &&
                        incomingSyncs.length === 0 &&
                        backend.processStatus === 'stopped' && (
                            <Text kind="body/regular/sm" className="text-subtle-color pl-2">
                                Start engine to see models
                            </Text>
                        )}

                    {models.length > 0 && (
                        <Stack gap="1">
                            {models.map(model => (
                                <ModelRow
                                    key={model.name}
                                    model={model}
                                    isRunning={isRunning}
                                    capabilities={caps}
                                    progress={getProgress(nodeId, backend.type, 'pull', model.name)}
                                    pendingAction={usePendingActionsStore
                                        .getState()
                                        .getModelPending(nodeId, backendType, model.name)}
                                    displayName={displayName}
                                    onAction={handleAction}
                                    onExpiryChange={handleExpiryChange}
                                />
                            ))}
                        </Stack>
                    )}
                </>
            )}

            <ConfirmModal
                open={modelPendingDelete !== null}
                onOpenChange={open => {
                    if (!open) setModelPendingDelete(null)
                }}
                title="Delete model?"
                message={`Deleting ${displayName(modelPendingDelete ?? '')} requires ${backend.displayName} to be restarted.`}
                confirmLabel="Delete and restart"
                confirmColor="danger"
                onConfirm={handleConfirmDelete}
            />

            {supportsSearch && (
                <Flex justify="end">
                    <Button
                        size="small"
                        kind="primary"
                        color="brand"
                        onClick={() => setOpenModelHubModal(true)}
                    >
                        Add model
                    </Button>
                    <ModelHubModal
                        engine={openModelHubModal ? backend.type : null}
                        onOpenChange={setOpenModelHubModal}
                        onSubmit={handleDownload}
                        downloadedModels={models}
                    />
                </Flex>
            )}
        </Stack>
    )
}
