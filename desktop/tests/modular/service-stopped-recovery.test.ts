// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServiceStatus } from '@/shared/types/ipc-channels'
import { useServiceStatusStore } from '@/ui/stores/service-status.store'
import { resolveShellView } from '@/ui/utils/shell-view'

/**
 * The shell renders nodes, engines, and models only while the service is up, so
 * it needs a truthful answer to "is the service stopped or still starting?".
 * The service bridge cannot give one — a stopped broker sends nothing — so this
 * store carries the Electron connector status instead. If it gets that wrong,
 * Overview either flashes a stopped notice during a normal launch or, worse,
 * spins forever on a service that will never come up on its own.
 *
 * Runs in the `node` unit project with a stubbed `window`.
 */

let statusCb: ((status: ServiceStatus) => void) | null = null
let unsubCount = 0

function makeFakeWindow(getStatus: () => Promise<ServiceStatus>) {
    return {
        windowApi: {
            service: {
                getStatus,
                onStatusChanged: (cb: (status: ServiceStatus) => void) => {
                    statusCb = cb
                    return () => {
                        unsubCount += 1
                    }
                }
            }
        }
    }
}

function currentStatus(): ServiceStatus['connectorStatus'] {
    return useServiceStatusStore.getState().status.connectorStatus
}

describe('renderer mirror of the modular connector status', () => {
    beforeEach(() => {
        statusCb = null
        unsubCount = 0
        useServiceStatusStore.setState({
            status: { connectorStatus: 'connecting', weSpawned: false }
        })
    })

    afterEach(() => {
        useServiceStatusStore.getState().cleanup()
        vi.unstubAllGlobals()
    })

    it('reports the stopped service the main process knows about', async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => ({ connectorStatus: 'disconnected', weSpawned: false }))
        )

        await useServiceStatusStore.getState().initialize()

        expect(currentStatus()).toBe('disconnected')
    })

    it('follows later status pushes so a restart is visible without a reload', async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => ({ connectorStatus: 'disconnected', weSpawned: false }))
        )
        await useServiceStatusStore.getState().initialize()

        statusCb?.({ connectorStatus: 'connecting', weSpawned: true })
        expect(currentStatus()).toBe('connecting')

        statusCb?.({ connectorStatus: 'connected', weSpawned: true })
        expect(currentStatus()).toBe('connected')
    })

    it('lets a push win over an initial read that resolves after it', async () => {
        let finishRead: (status: ServiceStatus) => void = () => {}
        const read = new Promise<ServiceStatus>(resolve => {
            finishRead = resolve
        })
        vi.stubGlobal(
            'window',
            makeFakeWindow(() => read)
        )

        const initializing = useServiceStatusStore.getState().initialize()
        statusCb?.({ connectorStatus: 'connected', weSpawned: true })
        finishRead({ connectorStatus: 'connecting', weSpawned: false })
        await initializing

        expect(currentStatus()).toBe('connected')
    })

    it('keeps following pushes when the initial read fails', async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => {
                throw new Error('bridge unavailable')
            })
        )

        await useServiceStatusStore.getState().initialize()
        statusCb?.({ connectorStatus: 'disconnected', weSpawned: false })

        expect(currentStatus()).toBe('disconnected')
    })

    it('does not claim the service is stopped before it has been read', async () => {
        vi.stubGlobal('window', {})

        await useServiceStatusStore.getState().initialize()

        expect(currentStatus()).toBe('connecting')
    })

    it('drops its subscription on cleanup', async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => ({ connectorStatus: 'connected', weSpawned: true }))
        )
        await useServiceStatusStore.getState().initialize()

        useServiceStatusStore.getState().cleanup()

        expect(unsubCount).toBe(1)
    })
})

describe('what a window shows while the service is unavailable', () => {
    it('offers the stopped notice instead of spinning, in a window opened after the stop', () => {
        // The regression: no bridge to answer, so the snapshot looks exactly
        // like a slow start, and nothing would ever arrive to end the spinner.
        expect(
            resolveShellView({
                connectorStatus: 'disconnected',
                connected: false,
                fetchedNodes: false
            })
        ).toBe('service-stopped')
    })

    it('offers the stopped notice when the service dies under an open window', () => {
        // The snapshot still says connected because it only refreshes while the
        // bridge is alive.
        expect(
            resolveShellView({
                connectorStatus: 'disconnected',
                connected: true,
                fetchedNodes: true
            })
        ).toBe('service-stopped')
    })

    it('keeps spinning while the service is genuinely still starting', () => {
        expect(
            resolveShellView({
                connectorStatus: 'connecting',
                connected: false,
                fetchedNodes: false
            })
        ).toBe('loading')
        expect(
            resolveShellView({
                connectorStatus: 'reconnecting',
                connected: false,
                fetchedNodes: false
            })
        ).toBe('loading')
    })

    it('waits for the node snapshot once the service is up', () => {
        expect(
            resolveShellView({
                connectorStatus: 'connected',
                connected: true,
                fetchedNodes: false
            })
        ).toBe('loading')
    })

    it('shows content once the service is up and nodes have arrived', () => {
        expect(
            resolveShellView({
                connectorStatus: 'connected',
                connected: true,
                fetchedNodes: true
            })
        ).toBe('content')
    })
})
