// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { isHubEntryDownloaded } from '@/ui/utils/match-downloaded-model'
import type { ModelItem } from '@/ui/types/engine-info'
import type { ModelEntry } from '@/ui/types/model-hub'

function modelEntry(id: string): ModelEntry {
    return {
        id,
        name: id,
        author: id.slice(0, id.indexOf('/')),
        url: `https://huggingface.co/${id.slice(0, id.lastIndexOf(':'))}`,
        updatedAt: new Date(0)
    }
}

function modelItem(name: string, downloaded = true): ModelItem {
    return {
        name,
        size: 0,
        downloaded,
        status: 'idle',
        parameterSize: '',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '10m',
        capabilities: []
    }
}

describe('downloaded model matching', () => {
    it('hides only an exact downloaded llama.cpp router model id', () => {
        const id = 'owner/model:Q4_K_M'
        const entry = modelEntry(id)

        expect(isHubEntryDownloaded('llama-cpp', entry, [modelItem(id)])).toBe(true)
        expect(
            isHubEntryDownloaded('llama-cpp', entry, [modelItem(id.slice(0, id.lastIndexOf(':')))])
        ).toBe(false)
        expect(isHubEntryDownloaded('llama-cpp', entry, [modelItem(id, false)])).toBe(false)
    })
})
