// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { errorDismissalKey } from '@/ui/utils/error-modal-dismissal'
import type { ServiceError } from '@/shared/types/errors'

function serviceError(partial: Partial<ServiceError> & { id: string }): ServiceError {
    return { message: 'boom', timestamp: 1000, ...partial }
}

/**
 * `ErrorModal` latches a dismissal on this key so a snapshot re-delivered while
 * the dialog is closing cannot re-open it. The key therefore has to be stable
 * for "the same errors" and different for "a new error", where error identity is
 * the `(nodeId, id)` pair `nvpair-errors` stores plus the timestamp it refreshes
 * on every report.
 */
describe('errorDismissalKey', () => {
    it('is stable when the same snapshot is re-delivered', () => {
        const errors = [
            serviceError({ id: 'engine-manager:install-failed:ollama', nodeId: 'node-a' })
        ]
        expect(errorDismissalKey(errors)).toBe(errorDismissalKey([...errors]))
    })

    it('changes when the same id is reported again with a fresh timestamp', () => {
        const first = serviceError({
            id: 'engine-manager:install-failed:ollama',
            nodeId: 'node-a',
            timestamp: 1000
        })
        const retry = { ...first, timestamp: 2000 }
        expect(errorDismissalKey([retry])).not.toBe(errorDismissalKey([first]))
    })

    it('distinguishes the same producer id on two different nodes', () => {
        const id = 'engine-manager:install-failed:ollama'
        const onA = serviceError({ id, nodeId: 'node-a' })
        const onB = serviceError({ id, nodeId: 'node-b' })
        expect(errorDismissalKey([onA])).not.toBe(errorDismissalKey([onB]))
        expect(errorDismissalKey([onA, onB])).not.toBe(errorDismissalKey([onA]))
    })

    it('does not depend on snapshot ordering', () => {
        const a = serviceError({ id: 'manual-nodes:probe-failed:peer-1', nodeId: 'node-a' })
        const b = serviceError({ id: 'ollama-local:not-running', nodeId: 'node-b' })
        expect(errorDismissalKey([a, b])).toBe(errorDismissalKey([b, a]))
    })

    it('cannot be forged by an id containing the field separators', () => {
        const forged = serviceError({ id: 'x", "y', nodeId: 'node-a', timestamp: 1000 })
        const plain = serviceError({ id: 'x', nodeId: 'node-a', timestamp: 1000 })
        expect(errorDismissalKey([forged])).not.toBe(errorDismissalKey([plain, plain]))
    })
})
