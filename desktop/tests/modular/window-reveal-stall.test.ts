// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * A renderer whose `file://` navigation never completes emits no `did-finish-load`,
 * no `did-fail-load` and no `render-process-gone` — nothing paints and nothing
 * reports an error. Revealing that window puts its bare black `backgroundColor`
 * on screen with no way for the user to read, act on, or recover from it, so the
 * reveal must wait for content and the stalled load must be retried.
 */

const mocks = vi.hoisted(() => {
    type Listener = (...args: unknown[]) => void

    class FakeEmitter {
        private listeners = new Map<string, Listener[]>()

        on(event: string, listener: Listener): this {
            const existing = this.listeners.get(event) ?? []
            existing.push(listener)
            this.listeners.set(event, existing)
            return this
        }

        once(event: string, listener: Listener): this {
            return this.on(event, listener)
        }

        emit(event: string, ...args: unknown[]): void {
            for (const listener of [...(this.listeners.get(event) ?? [])]) listener(...args)
        }
    }

    class FakeWebContents extends FakeEmitter {
        send = vi.fn()
        setWindowOpenHandler = vi.fn()
        getURL = vi.fn(() => 'file:///index.html')
        isDestroyed = vi.fn(() => false)
    }

    class FakeBrowserWindow extends FakeEmitter {
        static instances: FakeBrowserWindow[] = []

        webContents = new FakeWebContents()
        show = vi.fn()
        focus = vi.fn()
        loadFile = vi.fn(() => Promise.resolve())
        loadURL = vi.fn(() => Promise.resolve())
        isMaximized = vi.fn(() => false)
        isDestroyed = vi.fn(() => false)

        constructor() {
            super()
            FakeBrowserWindow.instances.push(this)
        }
    }

    return {
        BrowserWindow: FakeBrowserWindow,
        logged: [] as { level: string; sublevel?: string; message: string }[]
    }
})

vi.mock('electron', () => ({
    BrowserWindow: mocks.BrowserWindow,
    nativeImage: { createFromPath: () => ({}) }
}))

vi.mock('@electron-toolkit/utils', () => ({ is: { dev: false } }))

vi.mock('@/electron/open-external', () => ({ openExternalSafe: vi.fn() }))

vi.mock('@/shared/utils/log', () => {
    const record =
        (level: string) =>
        (payload: { sublevel?: string; message: string }): void => {
            mocks.logged.push({ level, ...payload })
        }
    return {
        createStructuredLogger: () => ({
            info: record('info'),
            warn: record('warn'),
            error: record('error'),
            verbose: record('verbose')
        })
    }
})

import { createOverviewWindow } from '@/electron/window'

/** The reveal reasons recorded so far, in order. */
function shownReasons(): string[] {
    return mocks.logged
        .filter(entry => entry.message.includes('window shown via'))
        .map(entry => entry.message.replace(/^.*window shown via (\S+).*$/, '$1'))
}

function stallErrors(): string[] {
    return mocks.logged
        .filter(entry => entry.level === 'error' && entry.sublevel === 'renderer-stall')
        .map(entry => entry.message)
}

function openOverview(): InstanceType<typeof mocks.BrowserWindow> {
    createOverviewWindow()
    const window = mocks.BrowserWindow.instances.at(-1)
    if (!window) throw new Error('createOverviewWindow did not create a window')
    return window
}

describe('overview window reveal', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        mocks.BrowserWindow.instances = []
        mocks.logged.length = 0
    })

    afterEach(() => {
        // Releases the module-level overview reference so the next test opens a
        // fresh window instead of focusing this one.
        for (const window of mocks.BrowserWindow.instances) window.emit('closed')
        vi.useRealTimers()
    })

    it('keeps an unpainted window hidden when the fallback timer elapses', () => {
        const window = openOverview()

        vi.advanceTimersByTime(4000)

        expect(window.show).not.toHaveBeenCalled()
        expect(shownReasons()).toEqual([])
    })

    it('reveals a window whose renderer loaded before the fallback timer', () => {
        const window = openOverview()

        vi.advanceTimersByTime(1000)
        window.webContents.emit('did-finish-load')
        vi.advanceTimersByTime(3000)

        expect(window.show).toHaveBeenCalledOnce()
        expect(shownReasons()).toEqual(['fallback-timer'])
    })

    it('reveals as soon as a slow load lands after the fallback timer', () => {
        const window = openOverview()

        vi.advanceTimersByTime(4000)
        expect(window.show).not.toHaveBeenCalled()

        window.webContents.emit('did-finish-load')

        expect(window.show).toHaveBeenCalledOnce()
        expect(shownReasons()).toEqual(['renderer-loaded'])
    })

    it('reveals immediately on ready-to-show and stops watching for a stall', () => {
        const window = openOverview()

        window.emit('ready-to-show')

        expect(shownReasons()).toEqual(['ready-to-show'])

        vi.advanceTimersByTime(60_000)

        expect(window.loadFile).toHaveBeenCalledTimes(1)
        expect(stallErrors()).toEqual([])
    })

    it('does not retry the load of a renderer that finished loading', () => {
        const window = openOverview()

        window.webContents.emit('did-finish-load')
        vi.advanceTimersByTime(60_000)

        expect(window.loadFile).toHaveBeenCalledTimes(1)
        expect(stallErrors()).toEqual([])
    })

    it('retries a stalled load, and reveals it unpainted only as a last resort', () => {
        const window = openOverview()

        expect(window.loadFile).toHaveBeenCalledTimes(1)

        vi.advanceTimersByTime(10_000)
        expect(window.loadFile).toHaveBeenCalledTimes(2)
        expect(window.show).not.toHaveBeenCalled()

        vi.advanceTimersByTime(10_000)
        expect(window.loadFile).toHaveBeenCalledTimes(3)
        expect(window.show).not.toHaveBeenCalled()

        vi.advanceTimersByTime(10_000)
        expect(window.loadFile).toHaveBeenCalledTimes(3)
        expect(shownReasons()).toEqual(['renderer-stalled'])
        expect(stallErrors()).toHaveLength(3)
    })

    it('reveals a retried renderer that eventually loads', () => {
        const window = openOverview()

        vi.advanceTimersByTime(10_000)
        expect(window.loadFile).toHaveBeenCalledTimes(2)

        window.webContents.emit('did-finish-load')

        expect(shownReasons()).toEqual(['renderer-loaded'])

        vi.advanceTimersByTime(60_000)
        expect(window.loadFile).toHaveBeenCalledTimes(2)
    })
})
