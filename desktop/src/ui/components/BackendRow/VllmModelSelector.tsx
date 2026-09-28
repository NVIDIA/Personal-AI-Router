// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { BackendInfo } from '@/ui/types/engine-info'
import { vllmModelSelectionHold } from '@/shared/utils/vllm-model-selection'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useVllmGroupStore, vllmGroupHoldsNode } from '@/ui/stores/vllm-group.store'
import { useVllmModelSelectionStore } from '@/ui/stores/vllm-model-selection.store'

/**
 * Retained model selection for one node's vLLM. Offers only the downloaded
 * canonical ids PAIR reports for this node, holds with a reason unless PAIR
 * state admits the request, and shows the selection PAIR reports back in the
 * refreshed engine status rather than what was asked for.
 */
export function VllmModelSelector({
    nodeId,
    backend,
    disabled
}: {
    nodeId: string
    backend: BackendInfo
    disabled: boolean
}) {
    const selfId = useConnectionStore(state => state.selfId)
    const groupHeld = useVllmGroupStore(state =>
        vllmGroupHoldsNode(state, nodeId, selfId, window.windowApi.platform === 'Linux')
    )
    const pending = useVllmModelSelectionStore(state => state.pending)
    const error = useVllmModelSelectionStore(state => state.error)
    const lastResult = useVllmModelSelectionStore(state => state.lastResult)
    const select = useVllmModelSelectionStore(state => state.select)
    const [choice, setChoice] = useState('')
    const downloaded = backend.models.filter(item => item.downloaded).map(item => item.name)
    const busy = pending === nodeId
    const hold = vllmModelSelectionHold({
        isSelf: !!selfId && nodeId === selfId,
        processStatus: backend.processStatus,
        managed: backend.managed,
        adopted: backend.adopted,
        groupHeld,
        downloadedModels: downloaded,
        model: choice
    })

    return (
        <Stack gap="2" className="border border-subtle-color rounded p-3">
            <Text kind="body/semibold/sm">Model selection</Text>
            <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                Selected model: {backend.selectedModel ?? 'none selected'}
            </Text>
            <Text kind="body/regular/sm" className="text-subtle-color">
                A one-node Start needs this PAIR-managed vLLM stopped, one selected downloaded
                model, and no held serving group. PAIR verifies the retained model before binding it
                and stays authoritative.
            </Text>
            <Flex gap="2" align="center" wrap="wrap">
                <select
                    aria-label="Downloaded vLLM model to select"
                    value={choice}
                    disabled={disabled || busy}
                    onChange={event => setChoice(event.target.value)}
                >
                    <option value="">Choose a downloaded model</option>
                    {downloaded.map(name => (
                        <option key={name} value={name}>
                            {name}
                        </option>
                    ))}
                </select>
                <Button
                    kind="secondary"
                    size="small"
                    disabled={disabled || busy || hold !== null}
                    title={
                        hold ??
                        'PAIR verifies the retained model and binds it for the next owned start.'
                    }
                    onClick={() => void select(nodeId, choice)}
                >
                    {busy ? 'Selecting…' : 'Select model'}
                </Button>
            </Flex>
            {hold && choice && (
                <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                    Held: {hold}
                </Text>
            )}
            {error && (
                <Text kind="body/regular/sm" role="alert">
                    {error}
                </Text>
            )}
            {lastResult && lastResult.nodeId === nodeId && !error && (
                <Text kind="body/regular/sm" className="text-subtle-color" role="status">
                    PAIR bound {lastResult.selectedModel}; the engine status above is refreshed from
                    its reply.
                </Text>
            )}
        </Stack>
    )
}
