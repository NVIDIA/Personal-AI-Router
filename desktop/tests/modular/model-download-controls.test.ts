// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { IncomingSyncPullRow } from '@/ui/components/ModelManager/IncomingSyncPullRow'
import { TransientModelStatusRow } from '@/ui/components/ModelManager/TransientModelStatusRow'
import { isEnginePullInProgress, isEnginePullOutcome } from '@/shared/utils/engine-progress'
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

const renderIncomingDownload = (status: string) =>
    createElement(IncomingSyncPullRow, {
        row: { rawModel: 'demo', label: 'Demo', status },
        onCancel: () => {}
    })

const renderTransientModel = (status: string) =>
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

const rows = [
    { name: 'incoming download', render: renderIncomingDownload },
    { name: 'transient model', render: renderTransientModel }
]

function cancelButton(html: string): string | undefined {
    return html.match(/<button\b[^>]*>[\s\S]*?<\/button>/)?.[0]
}

function isDisabled(button: string): boolean {
    return /<button\b[^>]*\bdisabled(?:\s|=|>)/.test(button)
}

for (const row of rows) {
    describe(row.name, () => {
        // "canceling" stays enabled: a cancel that outlives its budget stops
        // being awaited while the download keeps running, and this button is
        // the only way to ask again. Duplicate clicks are absorbed by the
        // bridge, which knows when a cancel is genuinely outstanding.
        it.each([
            { status: 'pulling', disabled: false },
            { status: 'queued', disabled: false },
            { status: 'downloading', disabled: false },
            { status: 'canceling', disabled: false },
            { status: 'idle', disabled: true }
        ])('cancellation availability during $status', ({ status, disabled }) => {
            const button = cancelButton(renderToStaticMarkup(row.render(status)))
            expect(button).toBeDefined()
            expect(button).toContain(status === 'canceling' ? 'Canceling…' : 'Cancel download')
            expect(isDisabled(button!)).toBe(disabled)
        })
    })
}

// The Model Manager lists an incoming download only while it runs, but the row
// itself still refuses a cancel for one that has ended.
describe('incoming download', () => {
    it.each(['success', 'cancelled', 'error'])('cancellation availability after %s', status => {
        const button = cancelButton(renderToStaticMarkup(renderIncomingDownload(status)))
        expect(button).toBeDefined()
        expect(isDisabled(button!)).toBe(true)
    })
})

// A pull's last frame names how it ended, and the row stays on it until the
// model list refreshes. Nothing is running by then, so the row shows no
// spinner, progress bar or Cancel.
describe('transient model', () => {
    it.each(['success', 'cancelled', 'error'])(
        'a download that ended on %s shows no activity',
        status => {
            const html = renderToStaticMarkup(renderTransientModel(status))
            expect(cancelButton(html)).toBeUndefined()
            expect(html).not.toContain('spinner-element')
            expect(html).not.toContain('Downloading demo')
        }
    )
})

// A pull's last frame names how it ended, and its row stays on that status
// while the model list refreshes. Read as a download still running, it kept a
// spinner and an enabled Cancel on one that had finished.
describe('a pull in progress', () => {
    it.each([
        { status: 'pulling', inProgress: true },
        { status: 'queued', inProgress: true },
        { status: 'downloading', inProgress: true },
        { status: 'canceling', inProgress: true },
        { status: 'idle', inProgress: false },
        { status: 'success', inProgress: false },
        { status: 'cancelled', inProgress: false },
        { status: 'error', inProgress: false }
    ])('during $status', ({ status, inProgress }) => {
        expect(isEnginePullInProgress({ operation: 'pull', status })).toBe(inProgress)
    })
})

describe('a pull outcome', () => {
    it.each([
        { status: 'pulling', outcome: false },
        { status: 'downloading', outcome: false },
        { status: 'canceling', outcome: false },
        { status: 'idle', outcome: false },
        { status: 'complete', outcome: true },
        { status: 'success', outcome: true },
        { status: 'cancelled', outcome: true },
        { status: 'Error', outcome: true }
    ])('on $status', ({ status, outcome }) => {
        expect(isEnginePullOutcome({ operation: 'pull', status })).toBe(outcome)
    })

    it('is only ever a pull', () => {
        expect(isEnginePullOutcome({ operation: 'load', status: 'success' })).toBe(false)
        expect(isEnginePullOutcome(undefined)).toBe(false)
    })
})
