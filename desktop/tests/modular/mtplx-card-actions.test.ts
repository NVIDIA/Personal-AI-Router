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
 * the header row (its on/off control plus an Install affordance when the engine is
 * not installed) and the footer (the Uninstall affordance). Both are rendered as
 * the card renders them, with the links a real card is given, and with the two
 * install facts a real node reports — whether it could install the engine, and who
 * owns the copy that is there. Those facts, not a per-engine list of operating
 * systems, are what the two buttons read, so a card built from a machine that has
 * no way to obtain an engine is a different card from one that does.
 */
function cardFor(
    engineType: EngineType,
    processStatus: EngineProcessStatus,
    facts: { canInstall?: boolean; owner?: string } = {}
): BackendInfo {
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
        installUrl: EngineDefaultLinks[engineType].installUrl,
        ...facts
    }
}

function headerMarkup(backend: BackendInfo, isLocalNode = true): string {
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
            isLocalNode,
            onInstall: () => {}
        })
    )
}

function footerMarkup(backend: BackendInfo): string {
    return renderToStaticMarkup(
        createElement(BackendFooter, {
            backend,
            showUninstall: true,
            disabled: false,
            onUninstall: () => {}
        })
    )
}

describe('MTPLX engine card on Apple Silicon macOS', () => {
    it('offers to install MTPLX through Homebrew where Homebrew is', () => {
        // The engine card stops guessing. An Apple Silicon Mac with brew can have
        // MTPLX put in the prefix brew owns, so that card carries the same Install
        // affordance a vLLM card does — which is how an engine that installs
        // through a package manager gets offered at all, an OS list could not tell
        // the two Macs apart.
        const withBrew = headerMarkup(cardFor('mtplx', 'not-installed', { canInstall: true }))
        const withoutBrew = headerMarkup(cardFor('mtplx', 'not-installed'))

        expect(withBrew).toContain('>Install<')
        // Where there is no brew, reading about MTPLX and getting it from its own
        // distribution is what is offered instead.
        expect(withoutBrew).not.toContain('>Install<')
        expect(withoutBrew).toContain('Download')
    })

    it('offers Install on a remote card only when that node reports it can install', () => {
        // Installing on a clustered peer is a real action, so a remote card is not
        // blanketed off; what it refuses to do is assume. An unclustered peer never
        // reports these facts and an older peer's status carries neither field, so
        // both read as nothing installable rather than as something to try, and a
        // download started from a remote card would land on this machine anyway.
        const reporting = headerMarkup(
            cardFor('mtplx', 'not-installed', { canInstall: true }),
            false
        )
        const silent = headerMarkup(cardFor('mtplx', 'not-installed'), false)

        expect(reporting).toContain('>Install<')
        expect(silent).not.toContain('>Install<')
        expect(silent).not.toContain('Download')
    })

    it('offers no uninstall for a Homebrew MTPLX, where a PAIR install gets one', () => {
        // Installing through brew puts MTPLX in the prefix brew owns, beside the
        // model library the user's own copy of MTPLX shares. PAIR refuses to remove
        // that, and a button for a refusal the service has already made is not
        // worth clicking.
        const brew = footerMarkup(cardFor('mtplx', 'running', { owner: 'Homebrew' }))
        const pairManaged = footerMarkup(cardFor('vllm', 'running', { owner: 'pair' }))

        expect(brew).not.toContain('Uninstall')
        expect(pairManaged).toContain('Uninstall')
        // Instead the card names the tool that does own it.
        expect(brew).toContain('Installed with Homebrew')
        expect(pairManaged).not.toContain('Installed with')
    })

    it('offers no uninstall for a copy that was merely found, either', () => {
        // MTPLX.app brings its own runtime and PAIR only drives the command it
        // finds: nothing on this machine was installed by PAIR, so nothing here is
        // PAIR's to take back out.
        const found = footerMarkup(cardFor('mtplx', 'running', { owner: 'other' }))

        expect(found).not.toContain('Uninstall')
        expect(found).not.toContain('Installed with')
    })

    it('keeps the on/off control for a server the user started themselves', () => {
        // A running MTPLX the user started is still theirs to stop: the card keeps
        // its switch, which for a running engine reads as Stop.
        expect(headerMarkup(cardFor('mtplx', 'running', { owner: 'Homebrew' }))).toContain(
            'Stop MTPLX'
        )
    })

    it('keeps MTPLX model controls available while the engine is stopped', () => {
        // The pack inventory and the resident set arrive on the `engine:models`
        // snapshot, which is read while the engine is stopped, so a stopped
        // engine is not shown as having nothing to load.
        expect(EnginesListingModelsWhenStopped).toContain('mtplx')
        expect(EngineCapabilities.mtplx.hasModelSearchOnlyWhenRunning).toBe(false)
        expect(EngineCapabilities.mtplx.hasEject).toBe(true)
    })

    it('keeps MTPLX out of the onboarding picker, which is a choice about first run', () => {
        // An empty `hasInstall` no longer decides the engine card — brew or no brew
        // does — so the picker's own exclusion needs pinning on its own terms: it
        // would otherwise default-select an engine whose install most fresh Macs
        // cannot run.
        expect(EngineCapabilities.mtplx.hasInstall).toEqual([])
    })
})
