// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { EngineProcessStatus } from '@/shared/types/engines'
import { InstallButton } from '@/ui/components/BackendRow/InstallButton'

// Render the real control without a browser so the regression catches an omitted button.
function buttonMarkup(processStatus: EngineProcessStatus, isLocalNode: boolean): string {
    return renderToStaticMarkup(
        createElement(InstallButton, {
            backend: {
                type: 'vllm',
                displayName: 'vLLM',
                source: 'detected',
                processStatus,
                port: null,
                proxyPort: null,
                models: [],
                modelsKnown: false,
                installUrl: 'https://docs.vllm.ai/en/latest/getting_started/installation.html'
            },
            // Discovery supplies Windows as the placeholder OS for every peer.
            targetOs: 'Windows',
            isLocalNode,
            disabled: false,
            onInstall: () => {}
        })
    )
}

describe('engine installation availability', () => {
    it('offers Install when a peer reports vLLM as supported but not installed', () => {
        const markup = buttonMarkup('not-installed', false)

        expect(markup).toContain('Install')
        expect(markup).not.toContain('Download')
    })

    it('does not offer installation before a peer reports vLLM support', () => {
        expect(buttonMarkup('initializing', false)).toBe('')
        expect(buttonMarkup('unsupported', false)).toBe('')
    })

    it('retains the platform-specific Download action on the local Windows node', () => {
        const markup = buttonMarkup('not-installed', true)

        expect(markup).toContain('Download')
        expect(markup).not.toContain('>Install<')
    })
})
