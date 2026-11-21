// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getClusterRouterConfig, setClusterRouterConfig } from '@/electron/config/ui-config'
import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>(() => 'local-node')
    },
    supervisor: {
        callProcess: vi.fn<Supervisor['callProcess']>(),
        // The mock models a running service set: the default says a broker is
        // up, and a case that means "stopped services" overrides it.
        hasProcess: vi.fn<Supervisor['hasProcess']>(() => true)
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

/**
 * The Endpoints list and the Cluster Settings card both ask the bridge what is
 * running, and the answer they get has to be the proxy's, not the configured
 * preference. These cases therefore pin where the reply comes
 * from and what a reply that is not a live listener turns into. The endpoint
 * write went to the same proxy the same way — `router/set-enabled` — so the
 * write cases pin when the bridge asks the services and what their answer
 * costs the stored preference.
 *
 * `ui-config` is deliberately the real module: the config pair is answered from
 * it, the point of the read-side cases is that a read of it is not a read of
 * the endpoint, and each write case says what ended up stored, so an untouched
 * store is a checked outcome and not an unobserved one.
 */
describe('cluster routing endpoint bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
    })

    it('asks the proxy process for the endpoint, by its own method name', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({ running: true, port: 4321 })

        await handleServiceBridgeInvoke('cluster:get-router-status', undefined)

        // `router/status` names no engine, so it cannot ride the prefix relay
        // that every engine-scoped read uses.
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'router/status')
    })

    it('reports the port the proxy is listening on', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({ running: true, port: 4321 })

        expect(await handleServiceBridgeInvoke('cluster:get-router-status', undefined)).toEqual({
            running: true,
            port: 4321
        })
    })

    it('reports no endpoint when the proxy answers that it bound none', async () => {
        // The proxy's own answer for "no router in this process", and for "there
        // is no proxy to ask". Both are ordinary configurations.
        mocks.supervisor.callProcess.mockResolvedValue({ running: false, port: 0 })

        expect(await handleServiceBridgeInvoke('cluster:get-router-status', undefined)).toEqual({
            running: false,
            port: 0
        })
    })

    /**
     * Replies that name no bindable port. `undefined` is what a failed call
     * resolves to, `null` is an empty JSON body, and the rest are shapes that
     * cannot describe a listener — each is named for the way it fails, so a
     * regression says which one stopped being refused.
     */
    const notALiveListener: ReadonlyArray<{ name: string; reply: JsonValue | undefined }> = [
        { name: 'nothing at all', reply: undefined },
        { name: 'a null body', reply: null },
        { name: 'an empty body', reply: {} },
        { name: 'a switch with no port', reply: { running: true } },
        { name: 'a port held as text', reply: { running: true, port: '9999' } },
        { name: 'port 0', reply: { running: true, port: 0 } },
        { name: 'a port above 65535', reply: { running: true, port: 70000 } },
        { name: 'a switch held as text', reply: { running: 'yes', port: 9999 } }
    ]

    for (const { name, reply } of notALiveListener) {
        it(`reports no endpoint when the proxy answers ${name}`, async () => {
            // Listing one of these would put a URL in the Endpoints list that no
            // service answers on the other side of.
            mocks.supervisor.callProcess.mockResolvedValue(reply)

            expect(await handleServiceBridgeInvoke('cluster:get-router-status', undefined)).toEqual(
                { running: false, port: 0 }
            )
        })
    }

    it('leaves the endpoint off the list when the read itself fails', async () => {
        // Unknown is not absent: a refused or timed-out read stays a rejection so
        // the UI cannot list a port it only infers from configuration.
        mocks.supervisor.callProcess.mockRejectedValue(new Error('proxy not reachable'))

        await expect(
            handleServiceBridgeInvoke('cluster:get-router-status', undefined)
        ).rejects.toThrow('proxy not reachable')
    })

    it('reports no endpoint while the stored preference says the endpoint is on', async () => {
        // The pairing this whole surface turns on. `ui-config` is seeded on
        // purpose rather than relied on as the module default, so the case says
        // what it proves: the preference is never the source of the answer.
        setClusterRouterConfig({ enabled: true, port: 9999 })
        mocks.supervisor.callProcess.mockResolvedValue({ running: false, port: 0 })

        expect(await handleServiceBridgeInvoke('cluster:get-router-config', undefined)).toEqual({
            enabled: true,
            port: 9999
        })
        expect(await handleServiceBridgeInvoke('cluster:get-router-status', undefined)).toEqual({
            running: false,
            port: 0
        })
    })

    it('answers a refused port from the stored preference, without a service call', async () => {
        setClusterRouterConfig({ enabled: true, port: 9100 })

        // Port 80 cannot be bound here, so the merge keeps the stored one and
        // hands it back — which is what lets the editor snap back.
        expect(await handleServiceBridgeInvoke('cluster:set-router-config', { port: 80 })).toEqual({
            enabled: true,
            port: 9100
        })
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })

    it('applies the endpoint to the broker as router/set-enabled and stores what was accepted', async () => {
        setClusterRouterConfig({ enabled: false, port: 9100 })
        mocks.supervisor.hasProcess.mockReturnValue(true)
        mocks.supervisor.callProcess.mockResolvedValue({ running: true, port: 9100 })

        const written = await handleServiceBridgeInvoke('cluster:set-router-config', {
            enabled: true,
            port: 9100
        })

        // A write means "make this the endpoint", so the payload is the whole
        // desired endpoint, not just the changed field: the child brings an
        // endpoint up or down from `{enabled, port}` in one step.
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'router/set-enabled', {
            enabled: true,
            port: 9100
        })
        expect(written).toEqual({ enabled: true, port: 9100 })
        // The stored preference follows the apply instead of leading it, so the
        // next read of the card and the next start's argv describe the endpoint
        // the services actually serve.
        expect(getClusterRouterConfig()).toEqual({ enabled: true, port: 9100 })
    })

    it('sends the stored port with a switch-only change and keeps the port when the endpoint goes down', async () => {
        setClusterRouterConfig({ enabled: true, port: 9100 })
        mocks.supervisor.hasProcess.mockReturnValue(true)
        // Turning the endpoint off, the child answers with port 0. That names
        // the absence of a listener, not a new port, and it must never enter
        // the store as if the operator had chosen it.
        mocks.supervisor.callProcess.mockResolvedValue({ running: false, port: 0 })

        const written = await handleServiceBridgeInvoke('cluster:set-router-config', {
            enabled: false
        })

        // The patch named no port, so the stored one is what the proxy is told.
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'router/set-enabled', {
            enabled: false,
            port: 9100
        })
        expect(written).toEqual({ enabled: false, port: 9100 })
        expect(getClusterRouterConfig()).toEqual({ enabled: false, port: 9100 })
    })

    it('stores the preference for the next start when no broker is running to apply it', async () => {
        setClusterRouterConfig({ enabled: true, port: 9100 })
        mocks.supervisor.hasProcess.mockReturnValue(false)

        // Stopped services are a configuration state, not a failure: nothing
        // can be applied to what is not running, so the write stores the
        // preference the next start reads into its argv, and answers as plain
        // success.
        expect(
            await handleServiceBridgeInvoke('cluster:set-router-config', { enabled: false })
        ).toEqual({ enabled: false, port: 9100 })
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(getClusterRouterConfig()).toEqual({ enabled: false, port: 9100 })
    })

    it('fails with the refusal message naming the port and stores nothing when the apply is refused', async () => {
        setClusterRouterConfig({ enabled: true, port: 9100 })
        mocks.supervisor.hasProcess.mockReturnValue(true)
        // The proxy's bind refusal arrives as a JSON-RPC rejection whose
        // message names the port, and the bridge passes that sentence through
        // rather than re-wording a machine's failure in TypeScript.
        mocks.supervisor.callProcess.mockRejectedValue(
            new Error('cannot bind the cluster endpoint on 9101: address already in use')
        )

        await expect(
            handleServiceBridgeInvoke('cluster:set-router-config', { enabled: true, port: 9101 })
        ).rejects.toThrow('cannot bind the cluster endpoint on 9101: address already in use')
        // Nothing on a refusal: an endpoint nobody answers must not sit in the
        // store claiming to be configured, and the card snaps its draft back
        // from the rejection.
        expect(getClusterRouterConfig()).toEqual({ enabled: true, port: 9100 })
    })

    it('answers an unbindable port from the stored preference even while a broker runs', async () => {
        setClusterRouterConfig({ enabled: true, port: 9100 })
        mocks.supervisor.hasProcess.mockReturnValue(true)

        // The range check is the bridge's own, made before any service is
        // asked: port 80 could never be bound here, so the editor snaps back
        // to the stored port and no draft that cannot work travels to a child.
        expect(await handleServiceBridgeInvoke('cluster:set-router-config', { port: 80 })).toEqual({
            enabled: true,
            port: 9100
        })
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })
})
