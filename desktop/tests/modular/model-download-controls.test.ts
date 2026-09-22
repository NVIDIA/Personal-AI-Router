// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { IncomingSyncPullRow } from '@/ui/components/ModelManager/IncomingSyncPullRow'
import { TransientModelStatusRow } from '@/ui/components/ModelManager/TransientModelStatusRow'
import type { ModelItem } from '@/ui/types/engine-info'

const model: ModelItem = {
    name: 'demo',
    size: 0,
    downloaded: false,
    status: 'pulling',
    parameterSize: '',
    quantization: '',
    family: '',
    digest: '',
    sizeVram: null,
    expiresAt: null,
    expiry: '10m',
    capabilities: []
}

const rows = [
    {
        name: 'incoming download',
        render: (status: string) =>
            createElement(IncomingSyncPullRow, {
                row: { rawModel: 'demo', label: 'Demo', status },
                onCancel: () => {}
            })
    },
    {
        name: 'transient model',
        render: (status: string) =>
            createElement(TransientModelStatusRow, {
                transientModel: model,
                displayName: name => name,
                pullProgress: {
                    engineType: 'ollama',
                    nodeId: 'local',
                    nodeName: 'Local',
                    operation: 'pull',
                    status
                },
                onCancel: () => {}
            })
    }
]

for (const row of rows) {
    describe(row.name, () => {
        it.each([
            { status: 'pulling', disabled: false },
            { status: 'queued', disabled: false },
            { status: 'downloading', disabled: false },
            { status: 'canceling', disabled: true },
            { status: 'idle', disabled: true },
            { status: 'error', disabled: true }
        ])('cancellation availability during $status', ({ status, disabled }) => {
            const html = renderToStaticMarkup(row.render(status))
            const button = html.match(/<button\b[^>]*>[\s\S]*?<\/button>/)?.[0]
            expect(button).toBeDefined()
            expect(button).toContain(status === 'canceling' ? 'Canceling…' : 'Cancel download')
            expect(/<button\b[^>]*\bdisabled(?:\s|=|>)/.test(button!)).toBe(disabled)
        })
    })
}
