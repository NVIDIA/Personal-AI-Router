// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState } from 'react'
import { Button, Flex, FormField, Stack, Text, TextInput } from '@nvidia/foundations-react-core'
import type { BackendInfo } from '@/ui/types/engine-info'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import { ModelExpiry } from '@/shared/types/engines'

import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { ModelHubModal } from '@/ui/components/ModelHub/ModelHubModal'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { isEnginePullInProgress } from '@/shared/utils/engine-progress'
import { useConnectionStore } from '@/ui/stores/connection.store'

import ModelRow from './ModelRow'
import { IncomingSyncPullRow } from './IncomingSyncPullRow'
import { TransientModelStatusRow } from './TransientModelStatusRow'
import type { IncomingSyncRow } from '@/ui/types/model-manager'
import type { ModelEntry } from '@/ui/types/model-hub'

export function ModelManager({ backend, nodeId }: { backend: BackendInfo; nodeId: string }) {
    const selfId = useConnectionStore(state => state.selfId)
    const [importPath, setImportPath] = useState('')
    const [llamaRepo, setLlamaRepo] = useState('')
    const [openModelHubModal, setOpenModelHubModal] = useState(false)
    const [modelPendingDelete, setModelPendingDelete] = useState<string | null>(null)
    const models = (backend.models ?? []).sort((a, b) =>
        formatModelDisplayName(a.name, backend.type).localeCompare(
            formatModelDisplayName(b.name, backend.type)
        )
    )
    const externalLlama = backend.type === 'llamacpp' && backend.managed !== true
    const caps = externalLlama
        ? { ...EngineCapabilities[backend.type], hasEject: false, hasDeleteModel: false }
        : EngineCapabilities[backend.type]

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
                <Stack key={p.rawModel} gap="1">
                    <IncomingSyncPullRow row={p} />
                    {backendType === 'llamacpp' && !externalLlama && (
                        <Button
                            size="small"
                            kind="tertiary"
                            onClick={() =>
                                window.pairApi.engines.cancelPull(backendType, nodeId, p.rawModel)
                            }
                        >
                            Cancel download
                        </Button>
                    )}
                </Stack>
            ))}

            {backendType === 'llamacpp' && transientModel && transientPullProgress && (
                <Button
                    size="small"
                    kind="tertiary"
                    onClick={() =>
                        window.pairApi.engines.cancelPull(backendType, nodeId, transientModel.name)
                    }
                >
                    Cancel download
                </Button>
            )}

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
                                    isRunning={isRunning && !externalLlama}
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

            {/*
                llama.cpp has no browsable catalog, so where the other engines
                offer "Add model" and a searchable list, it takes the model's
                identifier directly. These are the same brand-coloured download
                action the hub uses once a row is picked, so the two routes to a
                model read as the same operation.
            */}
            {backendType === 'llamacpp' && !externalLlama && (
                <Flex gap="2" wrap="wrap" align="end">
                    <FormField
                        className="min-w-0 grow"
                        slotLabel="Hugging Face repository"
                        slotHelp="For example ggml-org/gemma-3-1b-it-GGUF, optionally with a quantisation such as :Q4_K_M."
                    >
                        <TextInput
                            placeholder="owner/repository:Q4_K_M"
                            value={llamaRepo}
                            onValueChange={setLlamaRepo}
                            onKeyDown={e => {
                                if (e.key === 'Enter' && llamaRepo.trim()) {
                                    window.pairApi.engines.pullModel(
                                        backendType,
                                        nodeId,
                                        llamaRepo.trim()
                                    )
                                }
                            }}
                            className="min-w-0"
                        />
                    </FormField>
                    <Button
                        kind="primary"
                        color="brand"
                        size="small"
                        className="shrink-0"
                        disabled={!llamaRepo.trim()}
                        onClick={() =>
                            window.pairApi.engines.pullModel(backendType, nodeId, llamaRepo.trim())
                        }
                    >
                        Download model
                    </Button>
                </Flex>
            )}
            {/*
                Importing is deliberately the quieter of the two: it only works
                on this machine, because the path is resolved where the engine
                runs rather than where the window is.
            */}
            {backendType === 'llamacpp' && !externalLlama && nodeId === selfId && (
                <Flex gap="2" wrap="wrap" align="end">
                    <FormField
                        className="min-w-0 grow"
                        slotLabel="Local GGUF file"
                        slotHelp="Full path to a .gguf file on this machine."
                    >
                        <TextInput
                            placeholder="/path/to/model.gguf"
                            value={importPath}
                            onValueChange={setImportPath}
                            onKeyDown={e => {
                                if (e.key === 'Enter' && importPath.trim()) {
                                    window.pairApi.engines.importModel(
                                        backendType,
                                        nodeId,
                                        importPath.trim()
                                    )
                                }
                            }}
                            className="min-w-0"
                        />
                    </FormField>
                    <Button
                        kind="secondary"
                        size="small"
                        className="shrink-0"
                        disabled={!importPath.trim()}
                        onClick={() =>
                            window.pairApi.engines.importModel(
                                backendType,
                                nodeId,
                                importPath.trim()
                            )
                        }
                    >
                        Import GGUF
                    </Button>
                </Flex>
            )}
            {supportsSearch && backendType !== 'llamacpp' && (
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
