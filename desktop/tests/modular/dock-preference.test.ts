// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { mkdirSync, writeFileSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { beforeEach, describe, expect, it } from 'vitest'
import { initPlatform } from '@/electron/globals'
import {
    getHideDockIconInBackground,
    setHideDockIconInBackground,
    loadUiConfig
} from '@/electron/config/ui-config'
import { assertIsolated } from '../fixtures/isolation'

describe('background Dock preference', () => {
    let file = ''

    beforeEach(() => {
        assertIsolated()
        const userData = process.env.PAIR_USER_DATA ?? ''
        initPlatform({
            getUserData: () => userData,
            getTemp: () => userData,
            getResourcesPath: () => process.cwd()
        })
        mkdirSync(join(userData, 'configs'), { recursive: true })
        file = join(userData, 'configs/ui-config.json')
        writeFileSync(file, JSON.stringify({ firstRun: false }))
        loadUiConfig()
    })

    it('defaults to showing the Dock for existing configurations', () => {
        expect(getHideDockIconInBackground()).toBe(false)
    })

    it('persists an enabled preference across configuration reloads', () => {
        setHideDockIconInBackground(true)
        const persisted: { hideDockIconInBackground: boolean } = JSON.parse(
            readFileSync(file, 'utf8')
        )
        expect(persisted.hideDockIconInBackground).toBe(true)
        loadUiConfig()
        expect(getHideDockIconInBackground()).toBe(true)
    })

    it('does not enable hiding for a hand-edited string value', () => {
        writeFileSync(file, JSON.stringify({ firstRun: false, hideDockIconInBackground: 'true' }))
        loadUiConfig()
        expect(getHideDockIconInBackground()).toBe(false)
    })
})
