// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { VllmModelSelectionRequest } from '@/shared/types/vllm-model-selection'
import { exactKeys } from '@/shared/utils/vllm-group'

const hubModelPattern = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+@[0-9a-f]{40}$/
const localModelPattern = /^local:[0-9a-f]{64}$/

/** Mirrors the Engine Manager's canonical retained model id rule. */
export function isCanonicalVllmModelId(value: string): boolean {
    return hubModelPattern.test(value) || localModelPattern.test(value)
}

/** Exact request gate: a current node identity and one canonical model id, nothing else. */
export function parseVllmModelSelectionRequest(value: unknown): VllmModelSelectionRequest {
    const row = exactKeys(value, ['nodeId', 'model'])
    if (
        typeof row.nodeId !== 'string' ||
        !row.nodeId ||
        row.nodeId.length > 128 ||
        Array.from(row.nodeId).some(char => char.charCodeAt(0) <= 32 || char.charCodeAt(0) === 127)
    )
        throw new Error('Model selection requires a current node identity.')
    if (typeof row.model !== 'string' || !isCanonicalVllmModelId(row.model))
        throw new Error('Model selection requires an exact retained owner/model@revision id.')
    return { nodeId: row.nodeId, model: row.model }
}

/**
 * Every reason a model may not be selected right now, in the order a user can
 * fix them; null when PAIR state admits the request. The backend re-checks all
 * of it and stays authoritative.
 */
export function vllmModelSelectionHold(input: {
    isSelf: boolean
    processStatus: string | undefined
    managed: boolean | undefined
    adopted: boolean | undefined
    groupHeld: boolean
    downloadedModels: readonly string[]
    model: string
}): string | null {
    if (!input.isSelf)
        return "Model selection is available only for this controller's PAIR-managed vLLM."
    if (input.groupHeld) return 'A retained or unknown serving group holds vLLM.'
    if (input.adopted) return 'The external vLLM process remains under its own manager.'
    if (input.managed !== true) return 'Only a PAIR-managed vLLM can select a retained model.'
    if (input.processStatus !== 'stopped') return 'Stop vLLM before selecting a model.'
    if (!input.model) return 'Choose one downloaded model.'
    if (!isCanonicalVllmModelId(input.model))
        return 'Only an exact retained owner/model@revision id can be selected.'
    if (!input.downloadedModels.includes(input.model))
        return 'The model is not in the retained vLLM library PAIR reports for this node.'
    return null
}
