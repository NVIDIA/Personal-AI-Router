// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ModelAlias } from '@/shared/types/cluster'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>(() => 'local-node')
    },
    supervisor: {
        callProcess: vi.fn<Supervisor['callProcess']>()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/service-bridge/model-catalog', () => ({
    getEngineHubModels: vi.fn()
}))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

/**
 * The cluster model alias bridge: the one place between the alias editor and
 * `nvpair-node-settings`.
 *
 * An alias is two names — the one a caller types and the model it stands for —
 * and both are spelled the same on every hop, so there is nothing to translate
 * here. What the bridge does instead is refuse, and the cases below are mostly
 * about that. A document that arrives without a model still decodes, into
 * aliases that stand for nothing, and a cluster holding them keeps listing the
 * names while routing none of them: silent, and invisible from the editor that
 * shows a shorter list than the one stored. The alias editor saves its whole
 * list, so anything a lenient read dropped is also deleted by the next save.
 */
describe('cluster model aliases bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
    })

    describe('reading the stored list', () => {
        it('keeps both names of every stored alias', async () => {
            // The document as `nvpair-node-settings` keeps it under
            // `model_aliases` and answers it on `settings/get-cluster-model-aliases`.
            mocks.supervisor.callProcess.mockResolvedValue({
                aliases: [
                    { name: 'fast-code', model: 'qwen2.5-coder:1.5b' },
                    { name: 'tiny-chat', model: 'smollm2-135m' }
                ]
            })

            const aliases = await handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/get-cluster-model-aliases'
            )
            expect(aliases).toEqual([
                { name: 'fast-code', model: 'qwen2.5-coder:1.5b' },
                { name: 'tiny-chat', model: 'smollm2-135m' }
            ])
        })

        it('hands over an alias for a model nothing here serves', async () => {
            // Whether the target exists anywhere is the cluster's answer, given
            // per computer at routing time. A stored synonym that resolves
            // nowhere is inert, not malformed: the editor has to be able to show
            // it, because that is where a person goes to fix it.
            mocks.supervisor.callProcess.mockResolvedValue({
                aliases: [{ name: 'gone', model: 'qwen3:latest' }]
            })

            await expect(
                handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)
            ).resolves.toEqual([{ name: 'gone', model: 'qwen3:latest' }])
        })

        it('reads an unconfigured node (null aliases) as an empty list', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ aliases: null })

            await expect(
                handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)
            ).resolves.toEqual([])
        })

        it('reads a reply without an aliases key as an empty list', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({})

            await expect(
                handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)
            ).resolves.toEqual([])
        })

        // Every one of these decodes in Go without an error; read leniently,
        // each of them becomes a list that stops short of what is stored.
        const malformedReplies: [string, JsonValue][] = [
            [
                'a document that cites providers instead of a model',
                [
                    {
                        name: 'quick-chat',
                        providers: [{ node_id: 'node-a', engine: 'ollama', model: 'llama3.2:1b' }]
                    }
                ]
            ],
            ['a list holding a bare name', ['llama3:latest']],
            ['an alias with no name', [{ model: 'llama3:latest' }]],
            ['an alias with a blank name', [{ name: '   ', model: 'llama3:latest' }]],
            ['an alias with no model', [{ name: 'quick-chat' }]],
            ['an alias with a blank model', [{ model: '   ', name: 'quick-chat' }]]
        ]

        it.each(malformedReplies)('refuses a malformed document: %s', async (_label, reply) => {
            mocks.supervisor.callProcess.mockResolvedValue({ aliases: reply })

            await expect(
                handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)
            ).rejects.toThrow(/malformed model alias/)
        })

        it('refuses a document whose aliases key is not a list', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ aliases: 'fast-code=qwen3' })

            await expect(
                handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)
            ).rejects.toThrow('malformed model alias list: expected a list')
        })

        it('propagates a failed settings relay instead of showing an empty editor', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error('settings call failed: connection refused')
            )

            await expect(
                handleServiceBridgeInvoke('cluster:get-model-aliases', undefined)
            ).rejects.toThrow('settings call failed: connection refused')
        })
    })

    // Writing a node's document is the same document whether the node is this
    // computer or one at the other end of a cluster link: the broker holds this
    // node's cluster id, so it decides whether the list is written here or
    // carried to the machine that owns it, and this bridge never gets to choose.
    // What the bridge adds is refusing earlier — the draft rules below mirror
    // `validateModelAliases` in `nvpair-node-settings`, which stays the authority
    // because a refusal here costs the operator a banner while a half-typed row
    // that travels is announced from that machine to every peer, including this
    // one.
    describe('writing another node’s document', () => {
        it('sends the node with the document so the broker picks the writer', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ ok: true })

            const result = await handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                nodeId: 'peer-node-uuid',
                aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
            })

            expect(result).toBeNull()
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/set-node-model-aliases',
                {
                    nodeId: 'peer-node-uuid',
                    aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
                }
            )
        })

        it('sends an explicit empty list to clear a peer’s aliases', async () => {
            // The one way to say "this node configures none" on a document that
            // is replaced whole; an absent list is refused rather than read as
            // empty, because a peer editor opens over an announced empty list.
            mocks.supervisor.callProcess.mockResolvedValue({ ok: true })

            await handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                nodeId: 'peer-node-uuid',
                aliases: []
            })

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/set-node-model-aliases',
                { nodeId: 'peer-node-uuid', aliases: [] }
            )
        })

        it('refuses a write that does not name a node', async () => {
            // A document with no destination is a write that could land on any
            // machine, and only one of them owns it.
            await expect(
                handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                    nodeId: '   ',
                    aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
                })
            ).rejects.toThrow('A destination node is required.')

            expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        })

        // The draft rules the settings service enforces on its own document,
        // checked here too so an incomplete list never reaches another machine.
        const incompleteDrafts: [string, ModelAlias[]][] = [
            ['an alias with no name', [{ name: '', model: 'llama3:latest' }]],
            ['an alias with no model', [{ name: 'quick-chat', model: '' }]],
            ['an alias whose model is only spaces', [{ name: 'quick-chat', model: '   ' }]],
            [
                'a duplicate alias name',
                [
                    { name: 'quick-chat', model: 'llama3:latest' },
                    { name: 'quick-chat', model: 'qwen3:4b' }
                ]
            ],
            ['an alias that stands for itself', [{ name: 'quick-chat', model: 'quick-chat' }]],
            // A pasted space at either end would persist as part of the name, and
            // the endpoint matches the name a caller types exactly, so the row
            // would show everywhere and answer nothing.
            [
                'an alias whose name ends in a space',
                [{ name: 'quick-chat ', model: 'llama3:latest' }]
            ],
            [
                'an alias whose model starts with a space',
                [{ name: 'quick-chat', model: ' llama3:latest' }]
            ],
            [
                'an alias naming another alias',
                [
                    { name: 'chat', model: 'llama3:latest' },
                    { name: 'fast-chat', model: 'chat' }
                ]
            ]
        ]

        it.each(incompleteDrafts)('refuses an incomplete draft: %s', async (_label, aliases) => {
            const result = handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                nodeId: 'peer-node-uuid',
                aliases
            })

            await expect(result).rejects.toThrow()
            expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        })

        it('names the row it refused so the editor knows which one to fix', async () => {
            await expect(
                handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                    nodeId: 'peer-node-uuid',
                    aliases: [
                        { name: 'chat', model: 'llama3:latest' },
                        { name: 'fast-chat', model: 'chat' }
                    ]
                })
            ).rejects.toThrow('aliases[1] (fast-chat): model "chat" is another alias\'s name')
        })

        it('refuses a padded name in the words that node would have used', async () => {
            // Refusing here rather than there is a saved round trip, and nothing
            // more, so the sentence has to be the one the owning node would have
            // written: an operator reading it should not be able to tell which
            // machine turned them away.
            await expect(
                handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                    nodeId: 'peer-node-uuid',
                    aliases: [{ name: 'quick-chat ', model: 'llama3:latest' }]
                })
            ).rejects.toThrow(
                'aliases[0] (quick-chat ): name must not have leading or trailing spaces; ' +
                    'the cluster endpoint matches the name a caller types exactly, so a padded ' +
                    'name is a row that answers nothing'
            )
        })

        it('sends this machine’s own id the same way as a peer’s', async () => {
            // The node card edits every list, this computer's included, through
            // this one channel. Deciding where a document lives is the broker's
            // job — it holds this node's cluster id — so a bridge that special-
            // cased its own machine would be a second, quieter write path.
            mocks.supervisor.callProcess.mockResolvedValue({ ok: true })

            await handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                nodeId: 'local-node',
                aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
            })

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/set-node-model-aliases',
                { nodeId: 'local-node', aliases: [{ name: 'office-chat', model: 'qwen3:4b' }] }
            )
        })

        it('propagates the owning node’s refusal in its own words', async () => {
            // The draft here is sound by this bridge's rules, so a rejection can
            // only have come from the peer's settings service — the authority on
            // its own document. Its sentence is the one worth showing, and this
            // bridge has no way to know what that node's engines serve.
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error(
                    'settings/set-node-model-aliases failed: aliases[0] (office-chat): model "qwen3:4b" is not served by any engine on this node'
                )
            )

            await expect(
                handleServiceBridgeInvoke('cluster:set-node-model-aliases', {
                    nodeId: 'peer-node-uuid',
                    aliases: [{ name: 'office-chat', model: 'qwen3:4b' }]
                })
            ).rejects.toThrow('model "qwen3:4b" is not served by any engine on this node')
        })
    })

    describe('reading the cluster alias view', () => {
        it('keeps peer entries with the node that wrote them', async () => {
            // The merged view as the proxy child answers it on
            // `router/get-aliases`: local entries carry no origin, a peer's
            // entry names the peer by id and display name — the display name
            // is what the card prints, and without it a reader has a uuid
            // they cannot open anywhere.
            mocks.supervisor.callProcess.mockResolvedValue({
                aliases: [
                    { name: 'mine', model: 'llama3.2:1b' },
                    {
                        name: 'theirs',
                        model: 'qwen3:4b',
                        originNode: 'peer-uuid',
                        originHost: 'peer-one'
                    }
                ]
            })

            const view = await handleServiceBridgeInvoke('cluster:get-router-aliases', undefined)

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'router/get-aliases'
            )
            expect(view).toEqual([
                { name: 'mine', model: 'llama3.2:1b', originNode: '', originHost: '' },
                {
                    name: 'theirs',
                    model: 'qwen3:4b',
                    originNode: 'peer-uuid',
                    originHost: 'peer-one'
                }
            ])
        })

        it('reads a router-less machine (no aliases key) as an empty view', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({})

            await expect(
                handleServiceBridgeInvoke('cluster:get-router-aliases', undefined)
            ).resolves.toEqual([])
        })

        it('drops a malformed row without losing the sound ones beside it', async () => {
            // Permissive where the settings read is strict, because nothing
            // writes this back: one bad row costs one row on screen, not the
            // whole annotation — and never a delete, since there is no save
            // behind this read.
            mocks.supervisor.callProcess.mockResolvedValue({
                aliases: [{ name: 'theirs' }, { name: 'sound', model: 'llama3.2:1b' }]
            })

            await expect(
                handleServiceBridgeInvoke('cluster:get-router-aliases', undefined)
            ).resolves.toEqual([
                { name: 'sound', model: 'llama3.2:1b', originNode: '', originHost: '' }
            ])
        })

        it('refuses a reply that cannot be a view at all', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ aliases: 'mine, theirs' })

            await expect(
                handleServiceBridgeInvoke('cluster:get-router-aliases', undefined)
            ).rejects.toThrow('malformed cluster alias view: expected a list')
        })

        it('propagates a failed relay instead of showing an empty cluster', async () => {
            // "Cannot read" and "no aliases anywhere" are different answers,
            // and the card draws them differently — only a rejection keeps
            // them apart across the bridge.
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error('router/get-aliases failed: cluster alias view unavailable')
            )

            await expect(
                handleServiceBridgeInvoke('cluster:get-router-aliases', undefined)
            ).rejects.toThrow('cluster alias view unavailable')
        })
    })
})
