// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { beforeEach, describe, expect, it } from 'vitest'
import { MODULAR_CLUSTER_ROUTER_DEFAULT_PORT } from '@/shared/constants/modular-runtime'
import {
    getClusterRouterConfig,
    loadUiConfig,
    setClusterRouterConfig
} from '@/electron/config/ui-config'
import { initPlatform } from '@/electron/globals'
import { assertIsolated } from '../fixtures/isolation'
import { createTmpUserData } from '../fixtures/tmpdir'

/**
 * What `ui-config.json` can hold on disk. Both endpoint keys are wider than
 * their runtime type on purpose: a file edited by hand can carry any scalar
 * where a boolean or a port number belongs.
 */
interface UiConfigFile {
    firstRun?: boolean
    clusterRouterEnabled?: boolean | string | null
    clusterRouterPort?: number | string | null
}

const DEFAULT_ENDPOINT = { enabled: true, port: MODULAR_CLUSTER_ROUTER_DEFAULT_PORT }

describe('cluster routing endpoint preferences in ui-config', () => {
    const tmp = createTmpUserData()
    const configFile = (): string => path.join(tmp.dir, 'configs', 'ui-config.json')

    /**
     * Stand in for a launch whose config file holds `contents`.
     *
     * `loadUiConfig()` replaces the in-process config when a file exists, so
     * writing a file and loading it is also how each case starts from a known
     * state without re-importing the module.
     */
    function launchWith(contents: UiConfigFile): void {
        fs.mkdirSync(path.dirname(configFile()), { recursive: true })
        fs.writeFileSync(configFile(), JSON.stringify(contents), 'utf8')
        loadUiConfig()
    }

    function readConfigFile(): UiConfigFile {
        const parsed: UiConfigFile = JSON.parse(fs.readFileSync(configFile(), 'utf8'))
        return parsed
    }

    beforeEach(() => {
        // This suite writes a real ui-config.json, so it has to resolve to a
        // tmpdir even when a developer has set PAIR_USER_DATA.
        assertIsolated()
        fs.rmSync(tmp.dir, { recursive: true, force: true })
        fs.mkdirSync(tmp.dir, { recursive: true })
        initPlatform({
            getUserData: () => tmp.dir,
            getTemp: () => tmp.dir,
            getResourcesPath: () => process.cwd()
        })
    })

    it('serves the endpoint on the default port when neither key is stored', () => {
        // Routing across the cluster is what the product does, so a machine
        // nobody has configured should route rather than serve only the engines
        // installed on it.
        launchWith({ firstRun: false })
        expect(getClusterRouterConfig()).toEqual(DEFAULT_ENDPOINT)
    })

    it('reads a stored switch and port back on the next launch', () => {
        launchWith({ firstRun: false, clusterRouterEnabled: false, clusterRouterPort: 9100 })
        expect(getClusterRouterConfig()).toEqual({ enabled: false, port: 9100 })
    })

    it('rejects a privileged port instead of storing one that cannot be bound', () => {
        // The proxy runs as the signed-in user, so port 80 can never be bound
        // here. Storing it would look like a configured endpoint that answers
        // nothing, which is worse than an honest default.
        launchWith({ firstRun: false, clusterRouterPort: 80 })
        expect(getClusterRouterConfig().port).toBe(MODULAR_CLUSTER_ROUTER_DEFAULT_PORT)
    })

    it('rejects a port above the 16-bit range', () => {
        launchWith({ firstRun: false, clusterRouterPort: 70000 })
        expect(getClusterRouterConfig().port).toBe(MODULAR_CLUSTER_ROUTER_DEFAULT_PORT)
    })

    it('rejects a string and a fraction in the port slot', () => {
        launchWith({ firstRun: false, clusterRouterPort: '9100' })
        expect(getClusterRouterConfig().port).toBe(MODULAR_CLUSTER_ROUTER_DEFAULT_PORT)

        launchWith({ firstRun: false, clusterRouterPort: 9100.5 })
        expect(getClusterRouterConfig().port).toBe(MODULAR_CLUSTER_ROUTER_DEFAULT_PORT)
    })

    it('rejects a truthy string in the switch slot', () => {
        // `'false'` and `'yes'` are both truthy strings: coercing either would
        // turn a hand-edit into an endpoint the owner did not ask for.
        launchWith({ firstRun: false, clusterRouterEnabled: 'yes' })
        expect(getClusterRouterConfig().enabled).toBe(true)
    })

    it('keeps the stored port when only the switch is sent', () => {
        launchWith({ firstRun: false, clusterRouterPort: 9100 })
        expect(setClusterRouterConfig({ enabled: false })).toEqual({ enabled: false, port: 9100 })
    })

    it('keeps the stored switch when only the port is sent', () => {
        launchWith({ firstRun: false, clusterRouterEnabled: false })
        expect(setClusterRouterConfig({ port: 9100 })).toEqual({ enabled: false, port: 9100 })
    })

    it('answers a refused port with the one still in force', () => {
        // The card decides whether to restart by comparing what it sent with what
        // comes back, so echoing the request would restart for a port that was
        // never accepted.
        launchWith({ firstRun: false, clusterRouterPort: 9100 })
        expect(setClusterRouterConfig({ port: 80 })).toEqual({ enabled: true, port: 9100 })
    })

    it('writes both keys so the next launch reads the same endpoint', () => {
        launchWith({ firstRun: false })
        setClusterRouterConfig({ enabled: false, port: 9100 })

        const saved = readConfigFile()
        expect(saved.clusterRouterEnabled).toBe(false)
        expect(saved.clusterRouterPort).toBe(9100)

        // Re-read from disk: a preference that only lives in memory is lost at
        // the next start, which is the only time it is ever used.
        loadUiConfig()
        expect(getClusterRouterConfig()).toEqual({ enabled: false, port: 9100 })
    })
})
