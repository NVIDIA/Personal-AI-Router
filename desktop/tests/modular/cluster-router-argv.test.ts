// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { MODULAR_CLUSTER_ROUTER_DEFAULT_PORT } from '@/shared/constants/modular-runtime'
import { loadUiConfig } from '@/electron/config/ui-config'
import { initPlatform } from '@/electron/globals'
import { assertIsolated } from '../fixtures/isolation'
import { createTmpUserData } from '../fixtures/tmpdir'

vi.mock('electron', () => ({
    app: {
        isPackaged: false,
        getAppPath: () => process.cwd()
    }
}))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: vi.fn(),
        warn: vi.fn(),
        error: vi.fn(),
        verbose: vi.fn()
    })
}))

/**
 * The supervisor as this suite needs it. `brokerStartupArgs` is private because
 * only a spawn reads it, and that is exactly where the cluster endpoint either
 * does or does not come into existence: a mistyped flag, or a preference that
 * never reaches the command line, fails as a missing endpoint with nothing
 * failing in code. So the suite reaches in and reads the argv.
 */
interface StartupArgsHarness {
    brokerStartupArgs: () => string[]
}

describe('cluster routing endpoint argv', () => {
    const tmp = createTmpUserData()
    const supervisor = getModularSupervisor() as unknown as StartupArgsHarness
    const configFile = (): string => path.join(tmp.dir, 'configs', 'ui-config.json')

    /**
     * Stand in for one launch: put `contents` in `ui-config.json`, load it the
     * way a start does, then read the argv that start would spawn the broker
     * with. The preference comes from the file rather than from a call, so the
     * chain a user actually drives — card, file, argv — is what runs here.
     */
    function launchWith(contents: Record<string, unknown>): string[] {
        fs.rmSync(tmp.dir, { recursive: true, force: true })
        fs.mkdirSync(path.dirname(configFile()), { recursive: true })
        fs.writeFileSync(configFile(), JSON.stringify(contents), 'utf8')
        loadUiConfig()
        return supervisor.brokerStartupArgs()
    }

    beforeEach(() => {
        assertIsolated()
        initPlatform({
            getUserData: () => tmp.dir,
            getTemp: () => tmp.dir,
            getResourcesPath: () => process.cwd()
        })
    })

    it('asks for the endpoint on the default port when nothing is configured', () => {
        // The desktop has to say so: the broker's own `--cluster-router` flag
        // defaults to off, so an omitted flag means no endpoint on every machine
        // whose owner never opened the setting.
        const args = launchWith({ firstRun: false })
        expect(args).toContain('--cluster-router')
        expect(args).not.toContain('--cluster-router-port')
    })

    it('names no endpoint flag at all when the switch is off', () => {
        const args = launchWith({
            firstRun: false,
            clusterRouterEnabled: false,
            clusterRouterPort: 9100
        })
        expect(args).not.toContain('--cluster-router')
        expect(args).not.toContain('--cluster-router-port')
    })

    it('passes a port that is not the one the proxy already defaults to', () => {
        const args = launchWith({ firstRun: false, clusterRouterPort: 9100 })
        expect(args).toContain('--cluster-router')
        expect(args).toEqual(expect.arrayContaining(['--cluster-router-port', '9100']))
    })

    it('leaves the default port out of the argv that does not need it', () => {
        // The broker forwards 0 or an absent port as "use the child's default",
        // and the proxy's default is this same number: passing it would tie the
        // two binaries together through a duplicated literal.
        const args = launchWith({
            firstRun: false,
            clusterRouterPort: MODULAR_CLUSTER_ROUTER_DEFAULT_PORT
        })
        expect(args).toContain('--cluster-router')
        expect(args).not.toContain('--cluster-router-port')
    })

    it('keeps a port the proxy could not bind off the argv', () => {
        // Port 80 cannot be bound as the signed-in user. The stored value is
        // refused on the way in, so argv falls back to the default rather than
        // starting a proxy whose endpoint silently never comes up.
        const args = launchWith({ firstRun: false, clusterRouterPort: 80 })
        expect(args).toContain('--cluster-router')
        expect(args).not.toContain('--cluster-router-port')
    })
})
