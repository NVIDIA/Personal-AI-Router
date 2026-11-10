// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ExecFileOptions } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { app } from 'electron'
import { isMacHelperSetupComplete, loadUiConfig } from '@/electron/config/ui-config'
import { initPlatform } from '@/electron/globals'
import { macPrivilege } from '@/electron/services/mac-privilege-service'
import { assertIsolated } from '../fixtures/isolation'

const ctl = vi.hoisted(() => vi.fn<(args: string[]) => string>())

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
})

describe('macOS helper setup', () => {
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
            throw new Error('interrupted repair')
        })
        await expect(macPrivilege.ensureConfigured(false)).rejects.toThrow('interrupted repair')
        loadUiConfig()
        ctl.mockClear()

        expect((await macPrivilege.ensureConfigured(true)).firewallConfigured).toBe(false)
        expect(ctl.mock.calls).toEqual([[['status']]])
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

    it('reports a registration failure without installing twice in the same launch', async () => {
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
