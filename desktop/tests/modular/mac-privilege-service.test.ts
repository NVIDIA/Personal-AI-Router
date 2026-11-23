// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ExecFileOptions } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { app } from 'electron'
import {
    getMacHelperRepairAttempts,
    isMacHelperSetupComplete,
    loadUiConfig
} from '@/electron/config/ui-config'
import { initPlatform } from '@/electron/globals'
import { macPrivilege } from '@/electron/services/mac-privilege-service'
import { assertIsolated } from '../fixtures/isolation'

const ctl = vi.hoisted(() => vi.fn<(args: string[]) => string>())
const waitForLaunchRepair = vi.hoisted(() => vi.fn<(ms: number) => Promise<void>>())

vi.mock('node:timers/promises', () => ({ setTimeout: waitForLaunchRepair }))

vi.mock('node:fs', async importOriginal => {
    const original = await importOriginal<typeof import('node:fs')>()
    return {
        ...original,
        existsSync: (file: fs.PathLike) =>
            path.basename(file.toString()) === 'nvpair-helper-ctl' || original.existsSync(file)
    }
})
vi.mock('node:child_process', () => ({
    execFile: (
        _file: string,
        args: string[],
        _options: ExecFileOptions,
        callback: (error: Error | null, stdout: string, stderr: string) => void
    ) => callback(null, ctl(args), '')
}))
vi.mock('electron', () => ({ app: { isPackaged: true, getVersion: vi.fn(() => '1.1.0') } }))
vi.mock('@/shared/utils/platform', () => ({ currentPlatform: () => 'darwin' }))
vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({ info: vi.fn(), error: vi.fn() })
}))

const unreachable = {
    registration: 'enabled',
    reachable: false,
    ctlVersion: '1.1.0',
    daemonVersion: null
}
const healthy = {
    registration: 'enabled',
    reachable: true,
    ctlVersion: '1.1.0',
    daemonVersion: '1.1.0'
}
const uninstalled = { action: 'uninstall', ok: true, status: 'notRegistered' }
const installed = { action: 'install', ok: true, status: 'enabled', approvalOpened: false }
const firewallConfigured = { ok: true }

beforeEach(() => {
    assertIsolated()
    const userData = process.env.PAIR_USER_DATA ?? ''
    initPlatform({
        getUserData: () => userData,
        getTemp: () => userData,
        getResourcesPath: () => process.cwd()
    })
    fs.mkdirSync(path.join(userData, 'configs'), { recursive: true })
    fs.writeFileSync(
        path.join(userData, 'configs', 'ui-config.json'),
        JSON.stringify({
            firstRun: false,
            macHelperSetupComplete: true
        })
    )
    loadUiConfig()
    vi.mocked(app.getVersion).mockReturnValue('1.1.0')
    ctl.mockReset()
    ctl.mockReturnValue(JSON.stringify(unreachable))
    waitForLaunchRepair.mockReset()
    waitForLaunchRepair.mockResolvedValue()
})

describe('macOS helper setup', () => {
    it('retries registration after macOS replaces a stale launch constraint', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))
        let finishWaiting = () => {}
        const waiting = new Promise<void>(resolve => {
            finishWaiting = resolve
        })
        waitForLaunchRepair.mockReturnValueOnce(waiting)

        const setup = macPrivilege.ensureConfigured(false)
        await vi.waitFor(() => expect(waitForLaunchRepair).toHaveBeenCalledWith(12000))
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(1)
        finishWaiting()
        const result = await setup

        expect(result).toMatchObject({ firewallConfigured: true, firewallError: null })
        expect(waitForLaunchRepair).toHaveBeenCalledOnce()
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(2)
    })

    it('keeps the remaining repair attempt after approval and a configuration reload', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify({ ...installed, status: 'requiresApproval' }))
            .mockReturnValueOnce(
                JSON.stringify({ ...unreachable, registration: 'requiresApproval' })
            )
        expect((await macPrivilege.ensureConfigured(false)).registration).toBe('requiresApproval')
        expect(waitForLaunchRepair).not.toHaveBeenCalled()
        loadUiConfig()
        ctl.mockClear()
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(true)
        expect(waitForLaunchRepair).toHaveBeenCalledWith(12000)
    })

    it('uses a helper that becomes reachable during the wait without replacing it again', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        expect((await macPrivilege.ensureConfigured(false)).firewallConfigured).toBe(true)
        expect(waitForLaunchRepair).toHaveBeenCalledWith(12000)
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(1)
    })

    it('stops recovery if approval becomes required during the wait', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(
                JSON.stringify({ ...unreachable, registration: 'requiresApproval' })
            )

        expect(await macPrivilege.ensureConfigured(false)).toMatchObject({
            registration: 'requiresApproval',
            firewallConfigured: false
        })
        expect(waitForLaunchRepair).toHaveBeenCalledWith(12000)
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(1)
        expect(ctl).not.toHaveBeenCalledWith(['configure-firewall'])
    })

    it('limits failed replacements to two attempts across application restarts', async () => {
        ctl.mockImplementation(([command]) => {
            if (command === 'uninstall') return JSON.stringify(uninstalled)
            if (command === 'install') return JSON.stringify(installed)
            return JSON.stringify(unreachable)
        })

        expect((await macPrivilege.ensureConfigured(false)).firewallConfigured).toBe(false)
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(2)
        loadUiConfig()
        ctl.mockClear()
        waitForLaunchRepair.mockClear()

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(false)
        expect(ctl.mock.calls).toEqual([[['status']]])
        expect(waitForLaunchRepair).not.toHaveBeenCalled()
    })

    it('re-registers an enabled helper without a reachable daemon after an upgrade', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result).toMatchObject({ firewallConfigured: true, firewallError: null })
        expect(ctl.mock.calls).toEqual([
            [['status']],
            [['uninstall']],
            [['install']],
            [['status']],
            [['configure-firewall']]
        ])
    })

    it('does not report setup complete when the replacement helper is still unreachable', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result).toMatchObject({ registration: 'enabled', firewallConfigured: false })
        expect(result.firewallError).toBeTruthy()
        expect(ctl).not.toHaveBeenCalledWith(['configure-firewall'])
    })

    it('does not repeat a failed repair after reloading the persisted configuration', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
        await macPrivilege.ensureConfigured(false)
        loadUiConfig()
        ctl.mockClear()

        const result = await macPrivilege.ensureConfigured(true)

        expect(result.firewallConfigured).toBe(false)
        expect(ctl.mock.calls).toEqual([[['status']]])
    })

    it('allows another repair after the application version changes', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
        await macPrivilege.ensureConfigured(false)
        loadUiConfig()
        ctl.mockClear()
        vi.mocked(app.getVersion).mockReturnValue('1.2.0')
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify({ ...healthy, daemonVersion: '1.2.0' }))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(true)
        expect(ctl).toHaveBeenCalledWith(['uninstall'])
    })

    it('persists the repair attempt and incomplete setup before unregistering', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable)).mockImplementationOnce(() => {
            loadUiConfig()
            expect(isMacHelperSetupComplete()).toBe(false)
            expect(getMacHelperRepairAttempts('1.1.0')).toBe(1)
            throw new Error('interrupted repair')
        })
        await expect(macPrivilege.ensureConfigured(false)).rejects.toThrow('interrupted repair')
        loadUiConfig()
        ctl.mockClear()

        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockImplementationOnce(() => {
                loadUiConfig()
                expect(getMacHelperRepairAttempts('1.1.0')).toBe(2)
                expect(isMacHelperSetupComplete()).toBe(false)
                throw new Error('interrupted retry')
            })
        await expect(macPrivilege.ensureConfigured(true)).rejects.toThrow('interrupted retry')
        loadUiConfig()
        ctl.mockClear()

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(false)
        expect(ctl.mock.calls).toEqual([[['status']]])
    })

    it('counts a saved repair version without an attempt count as one attempt', async () => {
        const userData = process.env.PAIR_USER_DATA ?? ''
        fs.writeFileSync(
            path.join(userData, 'configs', 'ui-config.json'),
            JSON.stringify({
                macHelperRepairAttemptVersion: '1.1.0',
                macHelperSetupComplete: false
            })
        )
        loadUiConfig()
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(true)
        loadUiConfig()
        expect(getMacHelperRepairAttempts('1.1.0')).toBe(2)
        expect(waitForLaunchRepair).toHaveBeenCalledWith(12000)
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(1)
    })

    it('stops the repair if unregistering the old helper fails', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable)).mockReturnValueOnce(
            JSON.stringify({ ok: false, error: 'unregister failed' })
        )

        const result = await macPrivilege.ensureConfigured(false)

        expect(result.firewallConfigured).toBe(false)
        expect(result.firewallError).toBeTruthy()
        expect(ctl).not.toHaveBeenCalledWith(['install'])
    })

    it('recovers when registration temporarily fails after unregistering', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(
                JSON.stringify({
                    ok: false,
                    status: 'notRegistered',
                    error: 'Operation not permitted'
                })
            )
            .mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))
            .mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result).toMatchObject({ firewallConfigured: true, firewallError: null })
        expect(waitForLaunchRepair).toHaveBeenCalledWith(2000)
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(1)
        expect(getMacHelperRepairAttempts('1.1.0')).toBe(1)
    })

    it('bounds registration retries when the helper remains unregistered', async () => {
        ctl.mockImplementation(([command]) => {
            if (command === 'uninstall') return JSON.stringify(uninstalled)
            if (command === 'install') {
                return JSON.stringify({
                    ok: false,
                    status: 'notRegistered',
                    error: 'Operation not permitted'
                })
            }
            return JSON.stringify({ ...unreachable, registration: 'notRegistered' })
        }).mockReturnValueOnce(JSON.stringify(unreachable))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result).toMatchObject({
            registration: 'notRegistered',
            firewallConfigured: false,
            firewallError: 'Operation not permitted'
        })
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'install')).toHaveLength(3)
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'uninstall')).toHaveLength(1)
        expect(ctl).not.toHaveBeenCalledWith(['configure-firewall'])
    })

    it('stops registration retries if approval becomes required during the delay', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(
                JSON.stringify({
                    ok: false,
                    status: 'notRegistered',
                    error: 'Operation not permitted'
                })
            )
            .mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))
            .mockReturnValueOnce(
                JSON.stringify({ ...unreachable, registration: 'requiresApproval' })
            )

        const result = await macPrivilege.ensureConfigured(false)

        expect(result).toMatchObject({
            registration: 'requiresApproval',
            firewallConfigured: false
        })
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'install')).toHaveLength(1)
    })

    it('does not retry registration errors outside a helper replacement', async () => {
        ctl.mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))
            .mockReturnValueOnce(
                JSON.stringify({
                    ok: false,
                    status: 'notRegistered',
                    error: 'Operation not permitted'
                })
            )
            .mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result.firewallConfigured).toBe(false)
        expect(waitForLaunchRepair).not.toHaveBeenCalled()
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'install')).toHaveLength(1)
    })

    it('does not retry a registration error when the command reports an enabled helper', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(
                JSON.stringify({ ...installed, ok: false, error: 'register failed' })
            )
            .mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result).toMatchObject({
            firewallConfigured: false,
            firewallError: 'register failed'
        })
        expect(ctl.mock.calls.filter(([args]) => args[0] === 'install')).toHaveLength(1)
    })

    it('waits for approval without re-registering the pending helper', async () => {
        ctl.mockReturnValue(JSON.stringify({ ...unreachable, registration: 'requiresApproval' }))

        const result = await macPrivilege.ensureConfigured(true)

        expect(result).toMatchObject({
            registration: 'requiresApproval',
            firewallConfigured: false
        })
        expect(ctl.mock.calls).toEqual([[['status']]])
    })

    it('finishes a repaired helper setup after approval on a subsequent launch', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(
                JSON.stringify({ ...installed, status: 'requiresApproval', approvalOpened: true })
            )
            .mockReturnValueOnce(
                JSON.stringify({ ...unreachable, registration: 'requiresApproval' })
            )
        expect(await macPrivilege.ensureConfigured(false)).toMatchObject({
            registration: 'requiresApproval',
            firewallConfigured: false,
            firewallError: null
        })
        loadUiConfig()
        ctl.mockClear()
        ctl.mockReturnValueOnce(JSON.stringify(healthy)).mockReturnValueOnce(
            JSON.stringify(firewallConfigured)
        )

        expect(
            (await macPrivilege.ensureConfigured(!isMacHelperSetupComplete())).firewallConfigured
        ).toBe(true)
        expect(ctl.mock.calls).toEqual([[['status']], [['configure-firewall']]])
    })

    it('registers a missing helper without trying to unregister it', async () => {
        ctl.mockReturnValueOnce(JSON.stringify({ ...unreachable, registration: 'notRegistered' }))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(true)
        expect(ctl).not.toHaveBeenCalledWith(['uninstall'])
    })

    it('preserves the fast path for a reachable current helper with completed setup', async () => {
        ctl.mockReturnValue(JSON.stringify(healthy))

        expect((await macPrivilege.ensureConfigured(false)).firewallConfigured).toBe(true)
        expect(ctl.mock.calls).toEqual([[['status']]])
    })

    it('refreshes a reachable helper from an older version', async () => {
        ctl.mockReturnValueOnce(JSON.stringify({ ...healthy, daemonVersion: '1.0.0' }))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify(firewallConfigured))

        expect((await macPrivilege.ensureConfigured(false)).firewallConfigured).toBe(true)
        expect(ctl).toHaveBeenCalledWith(['uninstall'])
        expect(ctl).toHaveBeenCalledWith(['configure-firewall'])
    })

    it('reports firewall configuration errors after a successful repair', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify({ ok: false, error: 'firewall rejected request' }))

        expect(await macPrivilege.ensureConfigured(false)).toMatchObject({
            firewallConfigured: false,
            firewallError: 'firewall rejected request'
        })
    })

    it('retries firewall configuration without another repair after a firewall failure', async () => {
        ctl.mockReturnValueOnce(JSON.stringify(unreachable))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify(healthy))
            .mockReturnValueOnce(JSON.stringify({ ok: false, error: 'firewall rejected request' }))
        await macPrivilege.ensureConfigured(false)
        loadUiConfig()
        ctl.mockClear()
        ctl.mockReturnValueOnce(JSON.stringify(healthy)).mockReturnValueOnce(
            JSON.stringify(firewallConfigured)
        )

        expect(
            (await macPrivilege.ensureConfigured(!isMacHelperSetupComplete())).firewallConfigured
        ).toBe(true)
        expect(ctl.mock.calls).toEqual([[['status']], [['configure-firewall']]])
    })

    it('does not report success if the helper still runs the old version after replacement', async () => {
        ctl.mockReturnValueOnce(JSON.stringify({ ...healthy, daemonVersion: '1.0.0' }))
            .mockReturnValueOnce(JSON.stringify(uninstalled))
            .mockReturnValueOnce(JSON.stringify(installed))
            .mockReturnValueOnce(JSON.stringify({ ...healthy, daemonVersion: '1.0.0' }))

        const result = await macPrivilege.ensureConfigured(false)

        expect(result.firewallConfigured).toBe(false)
        expect(result.firewallError).toBeTruthy()
        expect(ctl).not.toHaveBeenCalledWith(['configure-firewall'])
    })
})
