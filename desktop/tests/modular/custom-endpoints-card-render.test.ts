// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { CustomEndpointAddForm } from '@/ui/components/CustomEndpoints/CustomEndpointAddForm'
import { CustomEndpointCard } from '@/ui/components/CustomEndpoints/CustomEndpointCard'
import { CustomProxyPortField } from '@/ui/components/CustomEndpoints/CustomProxyPortField'
import type { CustomEndpointInfo, CustomProxyPortState } from '@/shared/types/custom-endpoints'

/**
 * Static markup for the Custom surfaces: the endpoint cards, their editor, and
 * the facade-port field above them. The suite runs without a DOM, so these
 * use React's server renderer: nothing here asserts interaction, only that
 * each state's text reaches the markup — what the window would actually
 * render for a given wire entry. Card headers carry wall-clock age suffixes
 * for entries with a `lastChecked`, so pinned fixtures either omit that
 * field or assert around it.
 */

function renderCard(entry: CustomEndpointInfo): string {
    return renderToStaticMarkup(
        createElement(CustomEndpointCard, {
            entry,
            removing: false,
            onSnapshot: () => undefined,
            onRemove: () => undefined
        })
    )
}

describe('custom endpoint card header states', () => {
    const alpha = {
        id: 'custom:https://alpha.example:8000/v1',
        baseUrl: 'https://alpha.example:8000/v1'
    }

    // The status line states the verdict (and model count and age), never
    // credential presence: "No API key" beside a working endpoint read as a
    // second, failing status. The key's state is visible in the editor.
    it('says Not checked yet when no probe has answered', () => {
        const html = renderCard({ ...alpha, keySet: false })
        expect(html).toContain('Not checked yet')
        expect(html).not.toContain('API key')
        expect(html).toContain('No health check has run yet')
    })

    it('says Offline on a failed probe and withholds the stale model list', () => {
        // A failed probe's models are last-known noise; the card must show
        // the verdict, not a list that may name models the endpoint no
        // longer serves.
        const html = renderCard({ ...alpha, keySet: false, healthy: false, models: ['gone-model'] })
        expect(html).toContain('Offline')
        expect(html).not.toContain('API key')
        expect(html).not.toContain('gone-model')
        expect(html).toContain('not answering health checks')
    })

    it('says Online with no model count when the endpoint served nothing', () => {
        const html = renderCard({ ...alpha, keySet: true, healthy: true, models: [] })
        expect(html).toContain('Online')
        expect(html).not.toContain('API key set')
        expect(html).not.toContain('0 models')
        expect(html).toContain('answered but served no models')
    })

    it('counts and lists the served models on an online endpoint', () => {
        const html = renderCard({
            ...alpha,
            keySet: true,
            healthy: true,
            models: ['alpha-7b', 'phi-3-mini']
        })
        expect(html).toContain('Online · 2 models')
        expect(html).not.toContain('API key')
        expect(html).toContain('alpha-7b')
        expect(html).toContain('phi-3-mini')
    })
})

describe('custom endpoint editor key-state rendering', () => {
    function renderForm(editing?: CustomEndpointInfo): string {
        return renderToStaticMarkup(
            createElement(CustomEndpointAddForm, {
                editing,
                onDone: () => undefined
            })
        )
    }

    /**
     * The key field's Show/Hide control, isolated as its own `<button>` tag.
     * The markup carries other buttons — Save changes and Add endpoint are
     * enabled whenever their fields are filled — so a bare search for a
     * disabled attribute would pass with the reveal control wide open.
     */
    function revealControl(html: string): string {
        const tag = (html.match(/<button[^>]*>/g) ?? []).find(button => button.includes('API key'))
        if (!tag) throw new Error('the editor rendered no API-key reveal control')
        return tag
    }

    it('keeps Show disabled while the key field holds nothing', () => {
        // Show/Hide unmasks the text in the field, and a stored credential
        // never enters this input, so a blank field has nothing to reveal: an
        // enabled control read as "click to see the saved key" and could only
        // answer with an empty field. It lights up once there is text to
        // unmask, which the static renderer cannot show because the field
        // starts blank; what it can pin is that neither form opens offering a
        // reveal that cannot happen.
        const add = renderForm()
        const edit = renderForm({
            id: 'custom:https://alpha.example:8000/v1',
            baseUrl: 'https://alpha.example:8000/v1',
            keySet: true
        })

        expect(revealControl(add)).toContain('disabled=""')
        expect(revealControl(add)).toContain('aria-label="Show API key"')
        expect(revealControl(edit)).toContain('disabled=""')
        expect(revealControl(edit)).toContain('aria-label="Show API key"')
        // Masked as well as dark: a hidden value beside a live control is what
        // made the button look like the way to read the stored key.
        expect(edit).toContain('type="password"')
    })

    it('hides the Remove saved key checkbox when no key is stored', () => {
        // The fresh-add form has nothing to remove; showing the checkbox (or
        // the keep-it helper) would describe a stored credential that is not
        // there. The key-state helper is omitted entirely, not rendered
        // empty: the field row aligns against the Name field beside it.
        const html = renderForm()
        expect(html).not.toContain('Remove saved key')
        expect(html).not.toContain('No key')
        expect(html).not.toContain('Key set')
        expect(html).toContain('Add endpoint')
    })

    it('offers to remove the stored key and to keep it when one is set', () => {
        const html = renderForm({
            id: 'custom:https://alpha.example:8000/v1',
            baseUrl: 'https://alpha.example:8000/v1',
            keySet: true
        })
        expect(html).toContain('Remove saved key')
        // The stored credential reads as the key field's placeholder, so the
        // meaning of an empty field travels with the field that empties it.
        expect(html).toContain('placeholder="Key set — leave blank to keep it"')
        expect(html).toContain('Save changes')
    })

    it('prefills url, port and name from the endpoint being edited', () => {
        // An edit starts from the address the endpoint is dialled at — the
        // port from the baseUrl, because the broker never stores a guessed
        // one — with the key field deliberately blank.
        const html = renderForm({
            id: 'custom:https://alpha.example:8000/v1',
            name: 'Office vLLM',
            baseUrl: 'https://alpha.example:8000/v1',
            keySet: false
        })
        expect(html).toContain('value="https://alpha.example:8000/v1"')
        expect(html).toContain('value="8000"')
        expect(html).toContain('Office vLLM')
    })
})

describe('custom proxy-port field rendering', () => {
    function renderPortField(state: CustomProxyPortState): string {
        return renderToStaticMarkup(
            createElement(CustomProxyPortField, {
                state,
                nodeId: 'node-a',
                onPort: () => undefined
            })
        )
    }

    it('shows a listening facade port as an editable field with its meaning', () => {
        const html = renderPortField({ port: 8137, running: true })
        expect(html).toContain('Proxy port')
        expect(html).toContain('value="8137"')
        // The one-number-for-every-endpoint fact is the field's whole point, so
        // it reads beside the field rather than in a tooltip nobody opens.
        expect(html).toContain('Every custom endpoint on this node is reached')
    })

    it('leaves an untouched field unable to submit', () => {
        // The field opens showing the port already in use, so there is nothing
        // for Apply to do until someone types over it.
        const html = renderPortField({ port: 8137, running: true })
        expect(html).toContain('disabled=""')
    })

    it('names a planned port without offering to move a proxy that is not running', () => {
        // `running: false` is still worth reading: that is where the facade
        // lands on the next bring-up and where an external client would be
        // pointed. What it must not offer is an Apply the owning node can only
        // answer by refusing, and it must not look like a locked editor.
        const html = renderPortField({ port: 8123, running: false })
        expect(html).toContain('port 8123 on this node, which nothing is listening on')
        expect(html).toContain('Start the Custom proxy to change its port')
        expect(html).not.toContain('Proxy port')
        expect(html).not.toContain('Apply')
    })
})
