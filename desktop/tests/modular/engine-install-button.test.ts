// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { EngineProcessStatus } from '@/shared/types/engines'
import type { BackendInfo } from '@/ui/types/engine-info'
import { InstallButton } from '@/ui/components/BackendRow/InstallButton'

const VLLM_INSTALL_URL = 'https://docs.vllm.ai/en/latest/getting_started/installation.html'

/**
 * Render the real control without a browser so a regression catches an omitted
 * button. The only thing about a machine the control is told is what the node that
 * owns the engine reports with its state: whether it could install this engine if
 * asked. Nothing here works that out on its own any more, so each case names its
 * own fact — including the cases that name none, which is what an unclustered peer,
 * a peer without an engine manager, or an older peer's status reads as.
 */
function buttonMarkup(
    facts: { canInstall?: boolean },
    processStatus: EngineProcessStatus,
    isLocalNode: boolean
): string {
    const backend: BackendInfo = {
        type: 'vllm',
        displayName: 'vLLM',
        source: 'detected',
        processStatus,
        port: null,
        proxyPort: null,
        models: [],
        modelsKnown: false,
        installUrl: VLLM_INSTALL_URL,
        ...facts
    }
    return renderToStaticMarkup(
        createElement(InstallButton, {
            backend,
            isLocalNode,
            disabled: false,
            onInstall: () => {}
        })
    )
}

describe('engine installation availability', () => {
    it('offers Install on a peer that reports the engine as installable', () => {
        // Discovery gives every peer Windows as a placeholder OS, so an operating
        // system looked up on this side hid the button for Linux and Apple Silicon
        // peers. The peer's own engine manager answers for itself now.
        expect(buttonMarkup({ canInstall: true }, 'not-installed', false)).toContain('>Install<')
    })

    it('offers no install for a peer that reports no install fact at all', () => {
        // A guess here would put a button on the card of any machine whose answer
        // has not arrived, and the most that button can do is refuse.
        expect(buttonMarkup({}, 'not-installed', false)).toBe('')
        expect(buttonMarkup({ canInstall: false }, 'not-installed', false)).toBe('')
    })

    it('offers nothing before the engine reports, once it is installed, or while it changes', () => {
        for (const status of ['initializing', 'unsupported', 'stopped', 'running'] as const) {
            expect(buttonMarkup({ canInstall: true }, status, true)).toBe('')
        }
    })

    it('keeps the vendor download page for an engine this machine cannot install', () => {
        const markup = buttonMarkup({}, 'not-installed', true)

        expect(markup).toContain('Download')
        expect(markup).not.toContain('>Install<')
    })

    it('does not offer a download for a remote engine', () => {
        // A download started from this window lands on this machine, not on the
        // one the card describes.
        expect(buttonMarkup({}, 'not-installed', false)).toBe('')
    })

    it('prefers installing over the download page where both are possible', () => {
        const markup = buttonMarkup({ canInstall: true }, 'not-installed', true)

        expect(markup).toContain('>Install<')
        expect(markup).not.toContain('Download')
    })
})
