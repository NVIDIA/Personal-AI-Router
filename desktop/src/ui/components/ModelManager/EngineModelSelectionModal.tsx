// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useId, useRef, useState } from 'react'
import {
    Button,
    Flex,
    ModalContent,
    ModalDialog,
    ModalHeading,
    ModalRoot,
    Stack,
    Text
} from '@nvidia/foundations-react-core'
import { ModelHubModal } from '@/ui/components/ModelHub/ModelHubModal'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import type { ModelEntry } from '@/ui/types/model-hub'

/** Mounted only for an explicit turn-on/install, never for Eject or restored idle state. */
export function EngineModelSelectionModal({ nodeId }: { nodeId: string }) {
    const dialogId = useId()
    const status = useEngineStatusStore(s => s.statusByNode.get(nodeId)?.get('vllm')?.processStatus)
    const inventory = useEngineModelsStore(s => s.models.get(`${nodeId}:vllm`))
    const pending = usePendingActionsStore(s => s.getLifecyclePending(nodeId, 'vllm'))
    const isRemote = useConnectionStore(s => s.selfId !== nodeId)
    const isDisconnected = useNodesStore(s => isRemote && s.nodes.get(nodeId)?.status === 'offline')
    const [addingModel, setAddingModel] = useState(false)
    const hasOpened = useRef(false)
    const errors = useErrorsStore(s => s.errors)
    const initialErrors = useOverviewUiStore(s => s.modelSelectionErrorBaseline)
    const waiting = status === 'awaiting-model' && !pending && !isDisconnected

    useEffect(() => {
        const failed = errors.some(
            error =>
                initialErrors.get(error.id) !== error.timestamp &&
                error.severity !== 'info' &&
                error.severity !== 'warning' &&
                (error.engineType === 'vllm' || error.id === 'engine-cmd:toggle:vllm') &&
                (error.nodeId ? error.nodeId === nodeId : !isRemote)
        )
        if (failed) useOverviewUiStore.getState().dismissModelSelection()
    }, [errors, initialErrors, nodeId, isRemote])

    useEffect(() => {
        const ui = useOverviewUiStore.getState()
        if (waiting) {
            if (!hasOpened.current) ui.focusNodeEngineSettings(nodeId)
            hasOpened.current = true
        } else if (
            hasOpened.current ||
            status === 'running' ||
            status === 'stopping' ||
            status === 'unsupported' ||
            status === undefined ||
            isDisconnected
        ) {
            ui.dismissModelSelection()
        }
        // Keep the dialog intent through installation's intermediate stopped
        // and starting reports. A saved model resuming normally clears it above.
    }, [waiting, status, isDisconnected, nodeId])

    const dismiss = () => {
        useOverviewUiStore.getState().dismissModelSelection()
        // Read current service state so a request that has already started
        // loading a model is never canceled by a stale dialog close event.
        if (
            useEngineStatusStore.getState().statusByNode.get(nodeId)?.get('vllm')?.processStatus ===
            'awaiting-model'
        ) {
            window.pairApi.engines.stop('vllm', nodeId)
        }
    }

    const loadModel = (name: string) => {
        useOverviewUiStore.getState().dismissModelSelection()
        window.pairApi.engines.loadModel('vllm', nodeId, name)
    }

    const downloadModels = (entries: ModelEntry[]) => {
        if (entries.length === 0) return
        useOverviewUiStore.getState().dismissModelSelection()
        for (const entry of entries) {
            window.pairApi.engines.pullModel('vllm', nodeId, entry.name)
        }
    }

    if (!waiting) return null

    if (addingModel) {
        return (
            <ModelHubModal
                engine="vllm"
                nodeId={nodeId}
                onOpenChange={open => {
                    if (!open) setAddingModel(false)
                }}
                onSubmit={downloadModels}
                downloadedModels={inventory?.models}
            />
        )
    }

    const models = (inventory?.models ?? [])
        .filter(model => model.downloaded && model.status === 'idle')
        .toSorted((a, b) => a.name.localeCompare(b.name))

    return (
        <ModalRoot id={dialogId} open onOpenChange={open => !open && dismiss()}>
            <ModalDialog>
                <ModalContent
                    className="no-drag-elements"
                    style={{
                        width: 'min(560px, calc(100vw - 32px))',
                        maxHeight: 'calc(100vh - 32px)'
                    }}
                >
                    <ModalHeading>Choose a model for vLLM</ModalHeading>
                    <Stack gap="4" className="min-h-0">
                        <Text kind="body/regular/sm">
                            vLLM needs a loaded model, please click on an existing model below to
                            load or add one.
                        </Text>
                        {models.length > 0 ? (
                            <Stack gap="2" className="overflow-y-auto max-h-[45vh]">
                                {models.map(model => (
                                    <Button
                                        key={model.name}
                                        kind="secondary"
                                        className="w-full justify-start text-left h-auto whitespace-normal"
                                        title={model.name}
                                        aria-label={`Load ${model.name}`}
                                        onClick={() => loadModel(model.name)}
                                    >
                                        {formatModelDisplayName(model.name, 'vllm')}
                                    </Button>
                                ))}
                            </Stack>
                        ) : (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {inventory?.inventoryKnown
                                    ? 'No models downloaded yet. Add a model to get started.'
                                    : 'Downloaded models are not available yet.'}
                            </Text>
                        )}
                        <Flex justify="end" gap="2">
                            <Button kind="tertiary" size="small" onClick={dismiss}>
                                Cancel
                            </Button>
                            <Button
                                kind="primary"
                                color="brand"
                                size="small"
                                onClick={() => setAddingModel(true)}
                            >
                                Add model
                            </Button>
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
