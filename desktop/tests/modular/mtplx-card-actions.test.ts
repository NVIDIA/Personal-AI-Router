// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { EngineProcessStatus, EngineType } from '@/shared/types/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import {
    EnginesListingModelsWhenStopped,
    EngineDefaultLinks,
    EngineDisplayNames
} from '@/shared/constants/engines'
import { BackendHeader } from '@/ui/components/BackendRow/BackendHeader'
import { BackendFooter } from '@/ui/components/BackendRow/BackendFooter'

/**
 * Renders the two parts of an engine card that decide what the user is offered:
 * the header row (its on/off control plus an Install affordance when the engine
 * is not installed) and the footer (the Uninstall affordance). Both are rendered
 * as the card renders them, with the links a real card is given, so a change to
 * the capability table or to a button's condition shows up here rather than only
 * in a person's window.
 */
function cardFor(engineType: EngineType, processStatus: EngineProcessStatus): BackendInfo {
    return {
        type: engineType,
        displayName: EngineDisplayNames[engineType],
        source: 'detected',
        processStatus,
        port: 8101,
        proxyPort: 8100,
        models: [],
        modelsKnown: EnginesListingModelsWhenStopped.includes(engineType),
        docsUrl: EngineDefaultLinks[engineType].docsUrl,
        installUrl: EngineDefaultLinks[engineType].installUrl
    }
}

function headerMarkup(backend: BackendInfo): string {
    return renderToStaticMarkup(
        createElement(BackendHeader, {
            backend,
            isTransitioning: false,
            isUnavailable: false,
            disabled: false,
            onToggle: () => {},
            onStop: () => {},
            onExpand: () => {},
            expanded: false,
            isLocalNode: true,
            targetOs: 'MacOS',
            onInstall: () => {}
        })
    )
}

function footerMarkup(backend: BackendInfo): string {
    return renderToStaticMarkup(
        createElement(BackendFooter, {
            backend,
            targetOs: 'MacOS',
            showUninstall: true,
            disabled: false,
            onUninstall: () => {}
        })
    )
}

describe('MTPLX engine card on Apple Silicon macOS', () => {
    it('offers no install command for MTPLX, where vLLM on the same Mac gets one', () => {
        const mtplx = headerMarkup(cardFor('mtplx', 'not-installed'))
        const vllm = headerMarkup(cardFor('vllm', 'not-installed'))

        expect(vllm).toContain('Install')
        expect(mtplx).not.toContain('>Install<')
        expect(mtplx).not.toContain('Install all')
        // Reading about MTPLX and getting it from its own distribution is still
        // offered; it is only PAIR installing it that is not.
        expect(mtplx).toContain('Download')
    })

    it('offers no uninstall for MTPLX, where vLLM on the same Mac gets one', () => {
        const mtplx = footerMarkup(cardFor('mtplx', 'running'))
        const vllm = footerMarkup(cardFor('vllm', 'running'))

        expect(vllm).toContain('Uninstall')
        expect(mtplx).not.toContain('Uninstall')
        // A server the user started is still theirs to stop: the card keeps its
        // on/off control, which for a running engine reads as Stop.
        expect(headerMarkup(cardFor('mtplx', 'running'))).toContain('Stop MTPLX')
    })

    it('keeps MTPLX model controls available while the engine is stopped', () => {
        // The pack inventory and the resident set arrive on the `engine:models`
        // snapshot, which is read while the engine is stopped, so a stopped
        // engine is not shown as having nothing to load.
        expect(EnginesListingModelsWhenStopped).toContain('mtplx')
        expect(EngineCapabilities.mtplx.hasModelSearchOnlyWhenRunning).toBe(false)
        expect(EngineCapabilities.mtplx.hasInstall).toEqual([])
        expect(EngineCapabilities.mtplx.hasEject).toBe(true)
    })
})
