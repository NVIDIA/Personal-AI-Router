// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { SupportedPlatform } from '@/shared/types/platform'

const mocks = vi.hoisted(() => {
    const state: { platform: SupportedPlatform; enabled: boolean; visible: boolean } = {
        platform: 'darwin',
        enabled: false,
        visible: true
    }
    return { state, show: vi.fn<() => Promise<void>>(), hide: vi.fn<() => void>() }
})

vi.mock('electron', () => ({
    app: { dock: { show: mocks.show, hide: mocks.hide, isVisible: () => mocks.state.visible } }
}))
vi.mock('@/shared/utils/platform', () => ({ currentPlatform: () => mocks.state.platform }))
vi.mock('@/electron/config/ui-config', () => ({
    getHideDockIconInBackground: () => mocks.state.enabled
}))
vi.mock('@/shared/utils/log', () => ({ createStructuredLogger: () => ({ error: vi.fn() }) }))

import { createDockVisibilityController } from '@/electron/dock-visibility'

describe('background Dock visibility', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        vi.clearAllMocks()
        mocks.state.platform = 'darwin'
        mocks.state.enabled = false
        mocks.state.visible = true
        mocks.hide.mockImplementation(() => {
            mocks.state.visible = false
        })
        mocks.show.mockImplementation(async () => {
            mocks.state.visible = true
        })
    })

    afterEach(() => vi.useRealTimers())

    it('keeps the Dock visible when the preference is off', () => {
        createDockVisibilityController(() => false)()
        expect(mocks.state.visible).toBe(true)
    })

    it('keeps the Dock visible while Overview is visible', () => {
        mocks.state.enabled = true
        createDockVisibilityController(() => true)()
        expect(mocks.state.visible).toBe(true)
    })

    it('hides the Dock when only the menu bar remains', () => {
        mocks.state.enabled = true
        createDockVisibilityController(() => false)()
        expect(mocks.state.visible).toBe(false)
    })

    it('restores the Dock when Overview reopens', async () => {
        mocks.state.enabled = true
        let overviewVisible = false
        const refresh = createDockVisibilityController(() => overviewVisible)
        refresh()
        overviewVisible = true
        refresh()
        await Promise.resolve()
        expect(mocks.state.visible).toBe(true)
    })

    it('restores the Dock when the preference is disabled', async () => {
        mocks.state.enabled = true
        const refresh = createDockVisibilityController(() => false)
        refresh()
        mocks.state.enabled = false
        refresh()
        await Promise.resolve()
        expect(mocks.state.visible).toBe(true)
    })

    it('waits after showing the Dock before hiding it again', async () => {
        mocks.state.enabled = true
        let overviewVisible = false
        const refresh = createDockVisibilityController(() => overviewVisible)
        refresh()
        await vi.advanceTimersByTimeAsync(2000)
        overviewVisible = true
        refresh()
        await Promise.resolve()
        overviewVisible = false
        refresh()
        expect(mocks.hide).toHaveBeenCalledTimes(1)
        await vi.advanceTimersByTimeAsync(1100)
        expect(mocks.state.visible).toBe(false)
        expect(mocks.hide).toHaveBeenCalledTimes(2)
    })

    it('cancels a deferred hide when Overview reopens', async () => {
        mocks.state.enabled = true
        let overviewVisible = false
        const refresh = createDockVisibilityController(() => overviewVisible)
        refresh()
        overviewVisible = true
        refresh()
        await Promise.resolve()
        overviewVisible = false
        refresh()
        overviewVisible = true
        refresh()
        await vi.advanceTimersByTimeAsync(1100)
        expect(mocks.state.visible).toBe(true)
        expect(mocks.hide).toHaveBeenCalledTimes(1)
    })

    it('rechecks Overview after an in-flight Dock show completes', async () => {
        mocks.state.enabled = true
        mocks.state.visible = false
        let overviewVisible = true
        let finishShow = () => {}
        mocks.show.mockImplementation(
            () =>
                new Promise<void>(resolve => {
                    finishShow = () => {
                        mocks.state.visible = true
                        resolve()
                    }
                })
        )
        const refresh = createDockVisibilityController(() => overviewVisible)
        refresh()
        overviewVisible = false
        refresh()
        finishShow()
        await Promise.resolve()
        expect(mocks.state.visible).toBe(true)
        await vi.advanceTimersByTimeAsync(1100)
        expect(mocks.state.visible).toBe(false)
    })

    it.each<SupportedPlatform>(['win32', 'linux'])('leaves %s unaffected', platform => {
        mocks.state.platform = platform
        mocks.state.enabled = true
        createDockVisibilityController(() => false)()
        expect(mocks.hide).not.toHaveBeenCalled()
        expect(mocks.show).not.toHaveBeenCalled()
    })
})
