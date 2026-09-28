// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Retained model selection for a stopped, PAIR-managed vLLM on this
 * controller. The store only offers canonical ids that PAIR already reports as
 * downloaded for the node and refuses everything else before any request
 * leaves the renderer. PAIR remains authoritative: its reply carries the
 * refreshed engine status, and the Desktop displays `selectedModel` from that
 * status rather than from the request.
 */
import { create } from 'zustand'
import type { VllmModelSelectionResult } from '@/shared/types/vllm-model-selection'
import { vllmModelSelectionHold } from '@/shared/utils/vllm-model-selection'
import getErrorString from '@/shared/utils/get-error-string'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useVllmGroupStore, vllmGroupHoldsNode } from '@/ui/stores/vllm-group.store'

interface VllmModelSelectionApi {
    selectVllmModel(nodeId: string, model: string): Promise<VllmModelSelectionResult>
}

let api: VllmModelSelectionApi | null = null
export function bindVllmModelSelectionApi(next: VllmModelSelectionApi | null): void {
    api = next
}

interface VllmModelSelectionState {
    /** Node whose selection request is in flight, or null. */
    pending: string | null
    error: string | null
    lastResult: VllmModelSelectionResult | null
    select(nodeId: string, model: string): Promise<void>
    clear(): void
}

/** Downloaded canonical ids PAIR reports for a node's vLLM; the only offer set. */
export function downloadedVllmModelIds(nodeId: string): string[] {
    return useEngineModelsStore
        .getState()
        .getModels(nodeId, 'vllm')
        .models.filter(item => item.downloaded)
        .map(item => item.name)
}

/** Why a model may not be selected for this node right now; null when PAIR state admits it. */
export function vllmModelSelectionHoldFor(
    nodeId: string,
    model: string,
    localVllmSupported: boolean
): string | null {
    const selfId = useConnectionStore.getState().selfId
    const status = useEngineStatusStore.getState().getStatus(nodeId, 'vllm')
    return vllmModelSelectionHold({
        isSelf: !!selfId && nodeId === selfId,
        processStatus: status.processStatus,
        managed: status.managed,
        adopted: status.adopted,
        groupHeld: vllmGroupHoldsNode(
            useVllmGroupStore.getState(),
            nodeId,
            selfId,
            localVllmSupported
        ),
        downloadedModels: downloadedVllmModelIds(nodeId),
        model
    })
}

export const useVllmModelSelectionStore = create<VllmModelSelectionState>((set, get) => ({
    pending: null,
    error: null,
    lastResult: null,
    clear: () => set({ error: null, lastResult: null }),
    select: async (nodeId, model) => {
        if (get().pending) {
            set({ error: 'A model selection is already in progress.' })
            return
        }
        const localVllmSupported =
            typeof window !== 'undefined' && window.windowApi?.platform === 'Linux'
        const hold = vllmModelSelectionHoldFor(nodeId, model, localVllmSupported)
        if (hold) {
            set({ error: hold })
            return
        }
        if (!api) {
            set({ error: 'Model selection is unavailable until PAIR connects.' })
            return
        }
        set({ pending: nodeId, error: null, lastResult: null })
        try {
            const result = await api.selectVllmModel(nodeId, model)
            if (result.nodeId !== nodeId || result.selectedModel !== model)
                throw new Error('PAIR returned a selection for a different model.')
            set({ lastResult: result })
        } catch (error) {
            set({ error: getErrorString(error) })
        } finally {
            set({ pending: null })
        }
    }
}))
