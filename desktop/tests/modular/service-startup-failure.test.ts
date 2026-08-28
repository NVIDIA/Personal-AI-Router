// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => {
    class StartupTimeoutError extends Error {}

    let readyCallback: (() => void) | null = null

    const supervisor = {
        setLogLevel: vi.fn((_level: string): void => {}),
        setOnBrokerCrash: vi.fn((_callback: (info: { code: number | null }) => void): void => {}),
        setOnReady: vi.fn((callback: () => void): void => {
            readyCallback = callback
        }),
        start: vi.fn((): void => {}),
        waitUntilReady: vi.fn((_timeoutMs: number): Promise<void> => Promise.resolve()),
        stop: vi.fn((): Promise<void> => Promise.resolve())
    }

    return {
        StartupTimeoutError,
        supervisor,
        notifyBrokerCrash: vi.fn(),
        notifyBrokerStartupFailure: vi.fn(),
        invokeReady: (): void => readyCallback?.()
    }
})

vi.mock('electron', () => ({
    BrowserWindow: {
        getAllWindows: () => []
    }
}))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: vi.fn(),
        warn: vi.fn(),
        error: vi.fn()
    })
}))

vi.mock('@/electron/config/ui-config', () => ({
    getModularLogLevel: () => 'debug'
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor,
    ModularStartupTimeoutError: mocks.StartupTimeoutError
}))

vi.mock('@/electron/connector/broker-crash-notification', () => ({
    notifyBrokerCrash: mocks.notifyBrokerCrash,
    notifyBrokerStartupFailure: mocks.notifyBrokerStartupFailure
}))

import {
    destroyConnector,
    didWeSpawnCli,
    getConnectorError,
    getConnectorStatus,
    initializeConnector
} from '@/electron/connector'

describe('service startup failure handling', () => {
    beforeEach(async () => {
        mocks.supervisor.start.mockImplementation((): void => {})
        mocks.supervisor.waitUntilReady.mockImplementation(
            (_timeoutMs: number): Promise<void> => Promise.resolve()
        )
        mocks.supervisor.stop.mockResolvedValue()
        await destroyConnector({ force: true })
    })

    it('surfaces an immediate startup failure and leaves the service stopped', async () => {
        mocks.supervisor.start.mockImplementationOnce(() => {
            throw new Error('Required broker binary is missing')
        })

        await expect(initializeConnector()).rejects.toThrow('Required broker binary is missing')

        expect(getConnectorStatus()).toBe('disconnected')
        expect(didWeSpawnCli()).toBe(false)
        expect(getConnectorError()).toBe('Required broker binary is missing')
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledWith(
            'Required broker binary is missing'
        )
    })

    it('surfaces a readiness timeout and recovers if readiness arrives later', async () => {
        const timeout = new mocks.StartupTimeoutError(
            'Personal AI Router service did not become ready within 15 seconds'
        )
        mocks.supervisor.waitUntilReady.mockRejectedValueOnce(timeout)

        await expect(initializeConnector()).rejects.toThrow('service did not become ready')

        expect(getConnectorStatus()).toBe('reconnecting')
        expect(didWeSpawnCli()).toBe(true)
        expect(getConnectorError()).toContain('service did not become ready')
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledWith(
            'Personal AI Router service did not become ready within 15 seconds'
        )

        mocks.invokeReady()

        expect(getConnectorStatus()).toBe('connected')
        expect(getConnectorError()).toBeUndefined()
    })
})
