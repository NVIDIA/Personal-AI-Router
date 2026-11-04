// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node')
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => []
}))
vi.mock('@/electron/service-bridge/model-catalog', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

const ALPHA_ENTRY = {
    id: 'custom:https://alpha.example:8000/v1',
    baseUrl: 'https://alpha.example:8000/v1',
    keySet: false,
    healthy: true
}

describe('custom endpoint service bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.supervisor.hasProcess.mockReturnValue(true)
        mocks.state.getSelfId.mockReturnValue('local-node')
    })

    describe('a request for this node', () => {
        it('lists the configured endpoints straight out of the broker reply', async () => {
            mocks.supervisor.callProcess.mockResolvedValue([
                ALPHA_ENTRY,
                {
                    id: 'custom:https://beta.example:8000/v1',
                    name: 'beta',
                    baseUrl: 'https://beta.example:8000/v1',
                    keySet: true,
                    healthy: false,
                    lastChecked: '2026-10-28T00:00:00Z'
                }
            ])

            await expect(handleServiceBridgeInvoke('custom:list', {})).resolves.toEqual([
                ALPHA_ENTRY,
                {
                    id: 'custom:https://beta.example:8000/v1',
                    name: 'beta',
                    baseUrl: 'https://beta.example:8000/v1',
                    keySet: true,
                    healthy: false,
                    lastChecked: '2026-10-28T00:00:00Z'
                }
            ])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/list')
        })

        it('drops malformed list entries and never carries a key out of the reply', async () => {
            mocks.supervisor.callProcess.mockResolvedValue([
                {
                    id: 'custom:https://ok.example:8000/v1',
                    baseUrl: 'https://ok.example:8000/v1',
                    keySet: true,
                    apiKey: 'sk-must-not-cross-the-bridge'
                },
                { id: '', baseUrl: 'https://no-id.example/v1', keySet: false },
                { id: 'custom:no-url', keySet: false },
                'not-an-object'
            ])

            await expect(handleServiceBridgeInvoke('custom:list', {})).resolves.toEqual([
                {
                    id: 'custom:https://ok.example:8000/v1',
                    baseUrl: 'https://ok.example:8000/v1',
                    keySet: true
                }
            ])
        })

        it('parses probe state fields and keeps unknown reply data from crossing', async () => {
            // The enriched reply carries three things the flat shape never had:
            // a nullable verdict, a models array, and (in a malformed reply)
            // fields nobody sanctioned. null health is "no verdict" — the
            // renderer's third state — so it parses to absence; only string
            // model names may cross, but an array of them crosses even when
            // empty, because "checked, nothing known" is a state the cards
            // render differently from "never checked".
            mocks.supervisor.callProcess.mockResolvedValue([
                {
                    id: 'custom:https://unprobed.example:8000/v1',
                    baseUrl: 'https://unprobed.example:8000/v1',
                    keySet: false,
                    healthy: null,
                    models: ['unprobed-model'],
                    apiKey: 'sk-must-not-cross-the-bridge'
                },
                {
                    id: 'custom:https://mixed.example:8000/v1',
                    baseUrl: 'https://mixed.example:8000/v1',
                    keySet: false,
                    healthy: true,
                    models: ['ok-model', 42, null, { name: 'object-model' }]
                },
                {
                    id: 'custom:https://broken-models.example:8000/v1',
                    baseUrl: 'https://broken-models.example:8000/v1',
                    keySet: false,
                    healthy: false,
                    models: 'not-an-array'
                },
                {
                    id: 'custom:https://empty-models.example:8000/v1',
                    baseUrl: 'https://empty-models.example:8000/v1',
                    keySet: false,
                    healthy: true,
                    models: []
                }
            ])

            await expect(handleServiceBridgeInvoke('custom:list', {})).resolves.toEqual([
                {
                    id: 'custom:https://unprobed.example:8000/v1',
                    baseUrl: 'https://unprobed.example:8000/v1',
                    keySet: false,
                    models: ['unprobed-model']
                },
                {
                    id: 'custom:https://mixed.example:8000/v1',
                    baseUrl: 'https://mixed.example:8000/v1',
                    keySet: false,
                    healthy: true,
                    models: ['ok-model']
                },
                {
                    id: 'custom:https://broken-models.example:8000/v1',
                    baseUrl: 'https://broken-models.example:8000/v1',
                    keySet: false,
                    healthy: false
                },
                {
                    id: 'custom:https://empty-models.example:8000/v1',
                    baseUrl: 'https://empty-models.example:8000/v1',
                    keySet: false,
                    healthy: true,
                    models: []
                }
            ])
        })

        it('answers an unreachable broker with an empty list, not an error', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(new Error('broker is not running'))

            await expect(handleServiceBridgeInvoke('custom:list', {})).resolves.toEqual([])
        })

        it('runs a self-addressed list as the local path without forwarding the nodeId', async () => {
            // The Custom section keys itself by nodeId, so the local node's own
            // id arrives as addressing. Stripping it keeps the unchanged local
            // wire and prevents a self-targeted request from dialing anyone.
            mocks.supervisor.callProcess.mockResolvedValue([ALPHA_ENTRY])

            await expect(
                handleServiceBridgeInvoke('custom:list', { nodeId: 'local-node' })
            ).resolves.toEqual([ALPHA_ENTRY])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/list')
        })

        it('forwards the add form fields verbatim and reports the stored entry', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'custom:https://alpha.example:8000/v1',
                baseUrl: 'https://alpha.example:8000/v1',
                keySet: true,
                healthy: false
            })

            await expect(
                handleServiceBridgeInvoke('custom:add', {
                    name: 'alpha',
                    url: 'https://alpha.example/v1',
                    port: 8000,
                    apiKey: 'sk-secret'
                })
            ).resolves.toEqual({
                id: 'custom:https://alpha.example:8000/v1',
                baseUrl: 'https://alpha.example:8000/v1',
                keySet: true,
                healthy: false
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/add', {
                name: 'alpha',
                url: 'https://alpha.example/v1',
                port: 8000,
                apiKey: 'sk-secret'
            })
        })

        it('forwards edit intent so saving replaces the entry being edited', async () => {
            // An edit is an add with provenance: replaceId names the edited
            // entry and keyAction:"clear" removes its stored key. Both must
            // reach the broker verbatim — dropping either would fork a new
            // endpoint or silently keep a credential the user deleted.
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'custom:https://alpha.example:8000/v1',
                baseUrl: 'https://alpha.example:8000/v1',
                keySet: false
            })

            await handleServiceBridgeInvoke('custom:add', {
                url: 'https://alpha.example/v1',
                port: 8000,
                replaceId: 'custom:https://alpha.example:8000/v1',
                keyAction: 'clear'
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/add', {
                url: 'https://alpha.example/v1',
                port: 8000,
                replaceId: 'custom:https://alpha.example:8000/v1',
                keyAction: 'clear'
            })
        })

        it('sends no key fields when an edit keeps the stored key', async () => {
            // Blank key field means "keep what is stored": neither apiKey nor
            // keyAction belongs on the wire, or the owning broker would read
            // an unchanged form as a key change.
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'custom:https://alpha.example:8000/v1',
                baseUrl: 'https://alpha.example:8000/v1',
                keySet: true
            })

            await handleServiceBridgeInvoke('custom:add', {
                url: 'https://alpha.example/v1',
                port: 8000,
                apiKey: '',
                replaceId: 'custom:https://alpha.example:8000/v1'
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/add', {
                url: 'https://alpha.example/v1',
                port: 8000,
                replaceId: 'custom:https://alpha.example:8000/v1'
            })
        })

        it('surfaces a broker rejection for an invalid add so the form can show it', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error('custom/add requires an explicit port (never inferred): got 0')
            )

            await expect(
                handleServiceBridgeInvoke('custom:add', {
                    url: 'https://x.example/v1',
                    port: 0
                })
            ).rejects.toThrow('explicit port')
        })

        it('forwards the remove id and reports whether anything was removed', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ removed: true })

            await expect(
                handleServiceBridgeInvoke('custom:remove', {
                    id: 'custom:https://alpha.example:8000/v1'
                })
            ).resolves.toEqual({ removed: true })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/remove', {
                id: 'custom:https://alpha.example:8000/v1'
            })

            mocks.supervisor.callProcess.mockResolvedValue({ removed: false })
            await expect(
                handleServiceBridgeInvoke('custom:remove', { id: 'custom:never-configured' })
            ).resolves.toEqual({ removed: false })
        })
    })

    describe('a request addressed to a paired peer', () => {
        it('relays the list with the nodeId and the relay timeout, parsing the peer views', async () => {
            mocks.supervisor.callProcess.mockResolvedValue([ALPHA_ENTRY])

            await expect(
                handleServiceBridgeInvoke('custom:list', { nodeId: 'peer-9' })
            ).resolves.toEqual([ALPHA_ENTRY])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/list',
                { nodeId: 'peer-9' },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('rejects a peer that answers offline or unsupported instead of faking an empty config', async () => {
            // Empty means "this node has nothing configured". A peer read that
            // failed says nothing about the peer, so it must surface.
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error(
                    'custom endpoints unavailable: device is offline or does not support custom endpoint management'
                )
            )

            await expect(
                handleServiceBridgeInvoke('custom:list', { nodeId: 'peer-9' })
            ).rejects.toThrow('does not support custom endpoint management')
        })

        it('rejects a peer reply that is not an endpoint list', async () => {
            mocks.supervisor.callProcess.mockResolvedValue('somehow-not-an-array')

            await expect(
                handleServiceBridgeInvoke('custom:list', { nodeId: 'peer-9' })
            ).rejects.toThrow('the device returned no endpoint list')
        })

        it('forwards the add fields with the peer nodeId verbatim, key included', async () => {
            // The key crosses the pinned control channel by design — request
            // payload, not an event — and the remote shape check happens on
            // the owning node, so nothing here validates or masks it away.
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'custom:https://beta.example:8000/v1',
                baseUrl: 'https://beta.example:8000/v1',
                keySet: true
            })

            await expect(
                handleServiceBridgeInvoke('custom:add', {
                    url: 'https://beta.example/v1',
                    port: 8000,
                    apiKey: 'sk-remote',
                    nodeId: 'peer-9'
                })
            ).resolves.toEqual({
                id: 'custom:https://beta.example:8000/v1',
                baseUrl: 'https://beta.example:8000/v1',
                keySet: true
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/add',
                {
                    url: 'https://beta.example/v1',
                    port: 8000,
                    apiKey: 'sk-remote',
                    nodeId: 'peer-9'
                },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('forwards edit fields and the peer nodeId verbatim', async () => {
            // The engine-manager origin rebuilds the HTTP body from the exact
            // field list it reads here, so a field dropped at this layer would
            // vanish from the peer edit rather than default safely.
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'custom:https://beta.example:8000/v1',
                baseUrl: 'https://beta.example:8000/v1',
                keySet: false
            })

            await handleServiceBridgeInvoke('custom:add', {
                url: 'https://beta.example/v1',
                port: 8000,
                replaceId: 'custom:https://beta.example:8000/v1',
                keyAction: 'clear',
                nodeId: 'peer-9'
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/add',
                {
                    url: 'https://beta.example/v1',
                    port: 8000,
                    replaceId: 'custom:https://beta.example:8000/v1',
                    keyAction: 'clear',
                    nodeId: 'peer-9'
                },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('surfaces the peer refusal for an add instead of storing anything locally', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error('custom /v1/custom-endpoints/add: HTTP 409: endpoint port is taken here')
            )

            await expect(
                handleServiceBridgeInvoke('custom:add', {
                    url: 'https://beta.example/v1',
                    port: 8000,
                    nodeId: 'peer-9'
                })
            ).rejects.toThrow('HTTP 409')
        })

        it('relays the remove id with the peer nodeId', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ removed: true })

            await expect(
                handleServiceBridgeInvoke('custom:remove', {
                    id: 'custom:https://alpha.example:8000/v1',
                    nodeId: 'peer-9'
                })
            ).resolves.toEqual({ removed: true })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/remove',
                { id: 'custom:https://alpha.example:8000/v1', nodeId: 'peer-9' },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('treats removing an id the peer never configured as converged, not failed', async () => {
            // Idempotency holds across the relay: the owning broker's store is
            // authoritative for whether that id was ever there.
            mocks.supervisor.callProcess.mockResolvedValue({ removed: false })

            await expect(
                handleServiceBridgeInvoke('custom:remove', {
                    id: 'custom:https://never-there.example:8000/v1',
                    nodeId: 'peer-9'
                })
            ).resolves.toEqual({ removed: false })
        })
    })

    describe('a refresh request', () => {
        const ALPHA_PROBED = {
            id: 'custom:https://alpha.example:8000/v1',
            baseUrl: 'https://alpha.example:8000/v1',
            keySet: false,
            healthy: true,
            models: ['alpha-model']
        }

        it('runs the local probe cycle and maps the snapshot reply', async () => {
            // Refresh's reply is the enriched snapshot, shaped like a list
            // reply, so it goes through the same entry parser: malformed
            // entries drop, sanctioned fields survive.
            mocks.supervisor.callProcess.mockResolvedValue([
                ALPHA_PROBED,
                { id: '', baseUrl: 'https://no-id.example/v1', keySet: false }
            ])

            await expect(handleServiceBridgeInvoke('custom:refresh', {})).resolves.toEqual([
                ALPHA_PROBED
            ])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/refresh',
                {}
            )
        })

        it('scopes a single-endpoint refresh by id', async () => {
            mocks.supervisor.callProcess.mockResolvedValue([ALPHA_PROBED])

            await expect(
                handleServiceBridgeInvoke('custom:refresh', {
                    id: 'custom:https://alpha.example:8000/v1'
                })
            ).resolves.toEqual([ALPHA_PROBED])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/refresh', {
                id: 'custom:https://alpha.example:8000/v1'
            })
        })

        it('answers an unreachable broker with an empty list, not an error', async () => {
            // Same contract as the local list: a broker that cannot be reached
            // has no probe to run, and the cards degrade to "Not checked yet"
            // rather than an error dialog.
            mocks.supervisor.callProcess.mockRejectedValue(new Error('broker is not running'))

            await expect(handleServiceBridgeInvoke('custom:refresh', {})).resolves.toEqual([])
        })

        it('runs a self-addressed refresh as the local path without forwarding the nodeId', async () => {
            mocks.supervisor.callProcess.mockResolvedValue([ALPHA_PROBED])

            await expect(
                handleServiceBridgeInvoke('custom:refresh', { nodeId: 'local-node' })
            ).resolves.toEqual([ALPHA_PROBED])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/refresh',
                {}
            )
        })

        it('relays a peer refresh with the nodeId and the relay timeout', async () => {
            // The 45 s budget is why refresh exists as its own channel: a
            // peer probe cycle may take longer than the plain list budget.
            mocks.supervisor.callProcess.mockResolvedValue([ALPHA_PROBED])

            await expect(
                handleServiceBridgeInvoke('custom:refresh', { nodeId: 'peer-9' })
            ).resolves.toEqual([ALPHA_PROBED])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/refresh',
                { nodeId: 'peer-9' },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('relays a scoped peer refresh with the id beside the nodeId', async () => {
            mocks.supervisor.callProcess.mockResolvedValue([ALPHA_PROBED])

            await expect(
                handleServiceBridgeInvoke('custom:refresh', {
                    id: 'custom:https://alpha.example:8000/v1',
                    nodeId: 'peer-9'
                })
            ).resolves.toEqual([ALPHA_PROBED])
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/refresh',
                { id: 'custom:https://alpha.example:8000/v1', nodeId: 'peer-9' },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('rejects a peer refresh failure instead of faking a snapshot', async () => {
            // A failed peer read says nothing about the peer's endpoints;
            // answering [] would render an empty card list as if it were true.
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error(
                    'custom endpoints unavailable: device is offline or does not support custom endpoint management'
                )
            )

            await expect(
                handleServiceBridgeInvoke('custom:refresh', { nodeId: 'peer-9' })
            ).rejects.toThrow('does not support custom endpoint management')
        })

        it('rejects a peer reply that is not an endpoint list', async () => {
            mocks.supervisor.callProcess.mockResolvedValue('somehow-not-an-array')

            await expect(
                handleServiceBridgeInvoke('custom:refresh', { nodeId: 'peer-9' })
            ).rejects.toThrow('the device returned no endpoint list')
        })
    })

    describe('a Custom facade-port request', () => {
        // The facade's listen port travels the same two paths as the endpoint
        // reads, with one difference worth pinning: there is no honest empty
        // answer for a port. A missing read becomes no field at all, never this
        // node's own 8000, because a port read from the wrong machine would
        // render a stranger's proxy as editable here.

        it('reads this node’s live proxy port and lets nothing else cross', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({
                port: 8137,
                running: true,
                apiKey: 'sk-must-not-cross-the-bridge'
            })

            await expect(handleServiceBridgeInvoke('custom:get-port', {})).resolves.toEqual({
                port: 8137,
                running: true
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/get-port')
        })

        it('reads a planned port as one nothing is listening on', async () => {
            // `running:false` is a different fact, not a missing one: the
            // facade is not up, and the panel says so instead of offering to
            // rebind a proxy that is not there.
            mocks.supervisor.callProcess.mockResolvedValue({ port: 8123, running: false })

            await expect(handleServiceBridgeInvoke('custom:get-port', {})).resolves.toEqual({
                port: 8123,
                running: false
            })
        })

        it('answers an unreachable broker with no port instead of a default', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(new Error('broker is not running'))

            await expect(handleServiceBridgeInvoke('custom:get-port', {})).resolves.toBeNull()
        })

        it('answers a reply that names no port with no port, not a guessed one', async () => {
            // A port that arrives as a string, as zero, or past 65535 is not a
            // port; reading one as "8000" would put an editable field on the
            // panel for a proxy nobody reported.
            for (const reply of [
                undefined,
                'somehow-not-an-object',
                { port: 0, running: true },
                { port: 70000, running: true },
                { port: '8137', running: true }
            ]) {
                mocks.supervisor.callProcess.mockResolvedValue(reply)
                await expect(handleServiceBridgeInvoke('custom:get-port', {})).resolves.toBeNull()
            }
        })

        it('runs a self-addressed port read as the local path without forwarding the nodeId', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ port: 8137, running: true })

            await expect(
                handleServiceBridgeInvoke('custom:get-port', { nodeId: 'local-node' })
            ).resolves.toEqual({ port: 8137, running: true })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'custom/get-port')
        })

        it('relays a peer port read with the nodeId and the relay timeout', async () => {
            // The port belongs to the peer. Without the relay budget a slow
            // peer would time out at the plain-call limit first.
            mocks.supervisor.callProcess.mockResolvedValue({ port: 8123, running: true })

            await expect(
                handleServiceBridgeInvoke('custom:get-port', { nodeId: 'peer-9' })
            ).resolves.toEqual({ port: 8123, running: true })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/get-port',
                { nodeId: 'peer-9' },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('rejects a peer that cannot answer the port plane instead of showing this node’s port', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error(
                    'custom endpoints unavailable: device is offline or does not support custom endpoint management'
                )
            )

            await expect(
                handleServiceBridgeInvoke('custom:get-port', { nodeId: 'peer-9' })
            ).rejects.toThrow('does not support custom endpoint management')
        })

        it('rejects a peer reply that is not a port', async () => {
            // "No port" from a peer is a protocol surprise, and reading it as
            // no-proxy would describe a configuration the read never established.
            mocks.supervisor.callProcess.mockResolvedValue('somehow-not-a-port')

            await expect(
                handleServiceBridgeInvoke('custom:get-port', { nodeId: 'peer-9' })
            ).rejects.toThrow('the device returned no proxy port')
        })

        it('forwards the typed port to this node and reports where the proxy landed', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ port: 8137, running: true })

            await expect(
                handleServiceBridgeInvoke('custom:set-port', { port: 8137 })
            ).resolves.toEqual({ port: 8137, running: true })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/set-port',
                { port: 8137 }
            )
        })

        it('relays a peer port move with the nodeId and the relay timeout', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ port: 8137, running: true })

            await expect(
                handleServiceBridgeInvoke('custom:set-port', { port: 8137, nodeId: 'peer-9' })
            ).resolves.toEqual({ port: 8137, running: true })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/set-port',
                { port: 8137, nodeId: 'peer-9' },
                MODULAR_CUSTOM_RELAY_CALL_TIMEOUT_MS
            )
        })

        it('runs a self-addressed port move as the local path without forwarding the nodeId', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ port: 8137, running: true })

            await handleServiceBridgeInvoke('custom:set-port', {
                port: 8137,
                nodeId: 'local-node'
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'custom/set-port',
                { port: 8137 }
            )
        })

        it('surfaces the peer refusal for a port move instead of pretending it applied', async () => {
            // The whole point of the two-hop relay: the reason names the port
            // and who holds it, and the operator across the network has to read
            // the same sentence someone on that node would.
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error(
                    'custom /v1/custom-endpoints/set-port: HTTP 409: port 9292 is already in use on that node: an inference engine is running there'
                )
            )

            await expect(
                handleServiceBridgeInvoke('custom:set-port', { port: 9292, nodeId: 'peer-9' })
            ).rejects.toThrow('an inference engine is running there')
        })

        it('rejects a port move that answers no port', async () => {
            // An empty answer cannot mean "applied": the panel would clear the
            // field to whatever was typed with no proxy behind it.
            mocks.supervisor.callProcess.mockResolvedValue(undefined)

            await expect(
                handleServiceBridgeInvoke('custom:set-port', { port: 8137 })
            ).rejects.toThrow('did not report the proxy port')
        })
    })
})
