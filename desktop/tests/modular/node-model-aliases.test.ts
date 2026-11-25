// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
vi.mock('@/electron/service-bridge/broadcaster', () => ({
    emitBridgePush: vi.fn<typeof emitBridgePush>()
}))

import { emitBridgePush } from '@/electron/service-bridge/broadcaster'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

afterEach(() => vi.clearAllMocks())

/**
 * A node's own model alias list, from its announcement to that node's card.
 *
 * Each node keeps its own aliases and announces them with its model list, and
 * the node card is where a peer's list is edited. Editing means saving the whole
 * list, so the bridge has to carry three answers rather than two: the rows a
 * node named, the node's statement that it configures none, and the node that
 * has said nothing at all. The last two look identical to a card that parses
 * loosely — an empty list either way — and a card that draws an empty editor for
 * a silence is one Save away from overwriting aliases it never showed. So a
 * missing key, a null, and a list this build cannot read whole all land on
 * silence, and only an announced `[]` is allowed to look like an empty list.
 *
 * A peer's rows also have to survive the proxy merge and reach a card that is
 * already open, because the announcement that carries them is the only news a
 * peer's card ever gets.
 */
describe('announced node model aliases', () => {
    /**
     * One broker node snapshot, announced as the whole list of nodes. `reported`
     * is the wire keys to add beyond the identity of the node, so the three
     * answers stay distinct in the call itself: no second argument leaves
     * `aliases` off the wire (silence), `{ aliases: [] }` is a node that
     * configures none, and `{ aliases: [...] }` is a node that names rows.
     */
    const announceAliases = (hostUuid: string, reported?: Record<string, JsonValue>) => {
        const state = getModularBridgeState()
        const snapshot: Record<string, JsonValue> = {
            hostUuid,
            name: `alias-host-${hostUuid}`,
            ipAddress: '192.0.2.40',
            port: 14318,
            trusted: true,
            clustered: true,
            identityVerification: 'pinned',
            ...reported
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [snapshot]
        })
        return state
    }

    const aliasesOf = (hostUuid: string) =>
        getModularBridgeState().getNodesInitial().nodes[hostUuid].aliases

    it('carries the rows a peer announced onto its node row', () => {
        announceAliases('uuid-alias-rows', {
            aliases: [
                { name: 'quick-chat', model: 'qwen/qwen3.6-35b-a3b' },
                { name: 'embeddings', model: 'nomic-embed' }
            ]
        })

        // Both names of every row, in the order that node listed them: its card
        // shows that order and saves it back, so the list is not reordered here.
        expect(aliasesOf('uuid-alias-rows')).toEqual([
            { name: 'quick-chat', model: 'qwen/qwen3.6-35b-a3b' },
            { name: 'embeddings', model: 'nomic-embed' }
        ])
    })

    it('reads an announced empty list as a node that configures no aliases', () => {
        // The distinction this file is about: this peer answered, and its answer
        // was none, so its card may offer an editor for adding the first row.
        announceAliases('uuid-alias-empty', { aliases: [] })

        expect(aliasesOf('uuid-alias-empty')).toEqual([])
    })

    it('leaves a node that announced no list with no alias list', () => {
        // An older build, or a model read that has not answered: nothing is
        // known, which the card must not draw as a list of none.
        announceAliases('uuid-alias-missing')

        expect(aliasesOf('uuid-alias-missing')).toBeUndefined()
    })

    it('reads an announced null as no list rather than an empty one', () => {
        announceAliases('uuid-alias-null', { aliases: null })

        expect(aliasesOf('uuid-alias-null')).toBeUndefined()
    })

    it('silences a list with a row it cannot read whole', () => {
        // A row without a model is not a shortened list to display anyway: the
        // card saves back whatever it shows, so reading the whole thing as
        // silence is the only interpretation that cannot destroy a peer's row.
        announceAliases('uuid-alias-partial', {
            aliases: [{ name: 'sound', model: 'llama3.2:1b' }, { name: 'no-model' }]
        })

        expect(aliasesOf('uuid-alias-partial')).toBeUndefined()
    })

    it('keeps the announced list across the proxy merge', () => {
        // The proxy feed reports the same host and says nothing about aliases, so
        // a merge that rebuilds the row from its own event would blank a peer's
        // editor while that peer has changed nothing.
        const announced = [{ name: 'quick-chat', model: 'qwen/qwen3.6-35b-a3b' }]
        const state = announceAliases('uuid-alias-merge', { aliases: announced })
        vi.mocked(emitBridgePush).mockClear()

        // The proxy speaks from its own address, which is what makes the merge
        // observable below rather than merely assumed.
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: {
                id: 'uuid-alias-merge',
                host: 'alias-host-uuid-alias-merge',
                port: 11434,
                addresses: ['192.0.2.41'],
                ip: '192.0.2.41'
            }
        })

        // The projected address list is the broker's ranking with the proxy's own
        // address behind it, so this row really is both feeds merged.
        const projected = state
            .getAvailableNodes()
            .find(node => node.id === 'uuid-alias-merge')?.ipAddresses
        expect(projected).toEqual(expect.arrayContaining(['192.0.2.40', '192.0.2.41']))

        // And the merged row the card is handed still carries the list. A push
        // with no aliases, or an empty one, would read as a node that stopped
        // reporting what it announced a moment ago.
        expect(emitBridgePush).toHaveBeenCalledWith(
            'nodes:upsert',
            expect.objectContaining({ id: 'uuid-alias-merge', aliases: announced })
        )
    })

    it('pushes a peer’s edited list to the open card', () => {
        const state = announceAliases('uuid-alias-edit', {
            aliases: [{ name: 'quick-chat', model: 'llama3:latest' }]
        })
        vi.mocked(emitBridgePush).mockClear()

        // The peer wrote a new list and announced it; the announcement is the only
        // path for that change to reach a card that is already open.
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid: 'uuid-alias-edit',
                    name: 'alias-host-uuid-alias-edit',
                    ipAddress: '192.0.2.40',
                    port: 14318,
                    trusted: true,
                    clustered: true,
                    identityVerification: 'pinned',
                    aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
                }
            ]
        })

        expect(emitBridgePush).toHaveBeenCalledWith(
            'nodes:upsert',
            expect.objectContaining({
                id: 'uuid-alias-edit',
                aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
            })
        )
    })

    it('pushes a node that starts reporting an empty list', () => {
        // silence → [] is news about what is known, not a no-op: a card that
        // treats the two as equal keeps offering nothing to a node whose list has
        // just become editable.
        const state = announceAliases('uuid-alias-silent-to-empty')
        vi.mocked(emitBridgePush).mockClear()

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid: 'uuid-alias-silent-to-empty',
                    name: 'alias-host-uuid-alias-silent-to-empty',
                    ipAddress: '192.0.2.40',
                    port: 14318,
                    trusted: true,
                    clustered: true,
                    identityVerification: 'pinned',
                    aliases: []
                }
            ]
        })

        expect(emitBridgePush).toHaveBeenCalledWith(
            'nodes:upsert',
            expect.objectContaining({ id: 'uuid-alias-silent-to-empty', aliases: [] })
        )
    })
})
