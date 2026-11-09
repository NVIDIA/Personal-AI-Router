// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { destroyConnector } from '@/electron/connector'
import type { spawnWipeScript } from '@/electron/run-wipe-script'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { destroyTray } from '@/electron/tray'

type ModularSupervisor = ReturnType<typeof getModularSupervisor>

const mocks = vi.hoisted(() => ({
    exit: vi.fn<(code?: number) => void>(),
    destroyConnector: vi.fn<typeof destroyConnector>(),
    destroyTray: vi.fn<typeof destroyTray>(),
    spawnWipeScript: vi.fn<typeof spawnWipeScript>(),
    supervisor: {
        hasProcess: vi.fn<ModularSupervisor['hasProcess']>(),
        uninstallManagedEngines: vi.fn<ModularSupervisor['uninstallManagedEngines']>()
    }
}))

vi.mock('electron', () => ({ app: { isPackaged: false, exit: mocks.exit } }))
vi.mock('@/electron/connector', () => ({ destroyConnector: mocks.destroyConnector }))
vi.mock('@/electron/tray', () => ({ destroyTray: mocks.destroyTray }))
vi.mock('@/electron/run-wipe-script', () => ({ spawnWipeScript: mocks.spawnWipeScript }))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))

import {
    isAppDataWipeScheduled,
    wipeAppDataAndRelaunch
} from '@/electron/app-data-wipe-orchestrator'

function expectNothingDeleted() {
    expect(mocks.destroyConnector).not.toHaveBeenCalled()
    expect(mocks.spawnWipeScript).not.toHaveBeenCalled()
    expect(mocks.exit).not.toHaveBeenCalled()
    expect(isAppDataWipeScheduled()).toBe(false)
}

describe('resetting app data', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.supervisor.hasProcess.mockReturnValue(true)
    })

    it('wipes once every engine PAIR installed is gone', async () => {
        mocks.supervisor.uninstallManagedEngines.mockResolvedValue([
            { engine: 'ollama', removed: true, error: '' },
            { engine: 'lmstudio', removed: false, error: '' }
        ])

        await wipeAppDataAndRelaunch()

        expect(mocks.destroyConnector).toHaveBeenCalledTimes(1)
        expect(mocks.spawnWipeScript).toHaveBeenCalledTimes(1)
        expect(mocks.exit).toHaveBeenCalledWith(0)
    })

    // The wipe deletes the record that an engine is PAIR's. Wiping after a failed
    // removal left the engine's files on disk with nothing allowed to remove them.
    it('stops before deleting anything when an engine PAIR installed is not removed', async () => {
        mocks.supervisor.uninstallManagedEngines.mockResolvedValue([
            { engine: 'ollama', removed: true, error: '' },
            { engine: 'lmstudio', removed: false, error: 'locked' }
        ])

        await expect(wipeAppDataAndRelaunch()).rejects.toThrow(
            'Could not remove LM Studio, which NVIDIA PAIR installed. Your data was not deleted. ' +
                'Try again, or uninstall it yourself first.'
        )
        expectNothingDeleted()
    })

    it('stops before deleting anything when the engine manager cannot be asked', async () => {
        mocks.supervisor.uninstallManagedEngines.mockRejectedValue(new Error('broker closed'))

        await expect(wipeAppDataAndRelaunch()).rejects.toThrow(
            'Could not remove the engines NVIDIA PAIR installed: broker closed. Your data was not deleted.'
        )
        expectNothingDeleted()
    })

    it('stops before deleting anything when the service is not running', async () => {
        mocks.supervisor.hasProcess.mockReturnValue(false)

        await expect(wipeAppDataAndRelaunch()).rejects.toThrow(
            'The NVIDIA PAIR service is not running, so the engines it installed cannot be removed. ' +
                'Your data was not deleted. Restart the service and try again.'
        )
        expect(mocks.supervisor.uninstallManagedEngines).not.toHaveBeenCalled()
        expectNothingDeleted()
    })
})
