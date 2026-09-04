// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

interface SenderFixture {
    url: string
    window: object | null
    getURL: () => string
}

const mocks = vi.hoisted(() => ({
    sender: null as SenderFixture | null,
    handlers: new Map<string, (event: unknown, ...args: unknown[]) => unknown>()
}))

vi.mock('electron', () => ({
    BrowserWindow: {
        fromWebContents: (contents: SenderFixture | null) => (contents ? contents.window : null)
    },
    ipcMain: {
        handle: (channel: string, fn: (event: unknown, ...args: unknown[]) => unknown) => {
            mocks.handlers.set(channel, fn)
        }
    }
}))

import { safeHandle } from '@/electron/ipc/safe-handle'

function senderWith(url: string, withWindow = true): { sender: SenderFixture } {
    mocks.sender = { url, window: withWindow ? {} : null, getURL: () => url }
    return { sender: mocks.sender }
}

function invoke(channel: string, url: string, withWindow = true) {
    const handler = mocks.handlers.get(channel)
    if (!handler) throw new Error(`no handler registered for ${channel}`)
    const ev = senderWith(url, withWindow).sender
    return handler({ sender: ev } as never)
}

const previousDevUrl = process.env.ELECTRON_RENDERER_URL

afterEach(() => {
    mocks.handlers.clear()
    mocks.sender = null
    if (previousDevUrl === undefined) {
        delete process.env.ELECTRON_RENDERER_URL
    } else {
        process.env.ELECTRON_RENDERER_URL = previousDevUrl
    }
})

describe('safeHandle sender authorization', () => {
    it('registers the handler and authorizes a sender on the dev URL', async () => {
        process.env.ELECTRON_RENDERER_URL = 'http://localhost:5173'
        safeHandle('test:sender-ok' as never, (() => 'ok') as never)
        await expect(invoke('test:sender-ok', 'http://localhost:5173/app')).resolves.toEqual({
            success: true,
            data: 'ok'
        })
    })

    it('authorizes a file:// sender regardless of the dev URL', async () => {
        delete process.env.ELECTRON_RENDERER_URL
        safeHandle('test:sender-file' as never, (() => 'ok') as never)
        await expect(invoke('test:sender-file', 'file://app/index.html')).resolves.toEqual({
            success: true,
            data: 'ok'
        })
    })

    it('rejects a sender when ELECTRON_RENDERER_URL is unset', async () => {
        delete process.env.ELECTRON_RENDERER_URL
        safeHandle('test:sender-nor-dev' as never, (() => 'ok') as never)
        await expect(invoke('test:sender-nor-dev', 'http://localhost:5173/app')).resolves.toEqual({
            success: false,
            error: 'Unauthorized sender'
        })
    })

    it('rejects a sender when ELECTRON_RENDERER_URL is set to an empty string', async () => {
        process.env.ELECTRON_RENDERER_URL = ''
        safeHandle('test:sender-empty-dev' as never, (() => 'ok') as never)
        await expect(invoke('test:sender-empty-dev', 'http://anything.test/app')).resolves.toEqual({
            success: false,
            error: 'Unauthorized sender'
        })
    })

    it('authorizes an exact dev-URL match with no path', async () => {
        process.env.ELECTRON_RENDERER_URL = 'http://localhost:5173'
        safeHandle('test:sender-exact' as never, (() => 'ok') as never)
        await expect(invoke('test:sender-exact', 'http://localhost:5173')).resolves.toEqual({
            success: true,
            data: 'ok'
        })
    })

    it('rejects a sender whose URL only shares a prefix with the dev URL (lookalike host)', async () => {
        process.env.ELECTRON_RENDERER_URL = 'http://localhost:5173'
        safeHandle('test:sender-lookalike' as never, (() => 'ok') as never)
        await expect(
            invoke('test:sender-lookalike', 'http://localhost:5173.evil.test/app')
        ).resolves.toEqual({
            success: false,
            error: 'Unauthorized sender'
        })
    })

    it('rejects a sender with dev-URL userinfo spoofing', async () => {
        process.env.ELECTRON_RENDERER_URL = 'http://localhost:5173'
        safeHandle('test:sender-userinfo' as never, (() => 'ok') as never)
        await expect(
            invoke('test:sender-userinfo', 'http://localhost:5173@evil.test/')
        ).resolves.toEqual({
            success: false,
            error: 'Unauthorized sender'
        })
    })

    it('rejects a sender with no attached BrowserWindow', async () => {
        process.env.ELECTRON_RENDERER_URL = 'http://localhost:5173'
        safeHandle('test:sender-nowin' as never, (() => 'ok') as never)
        const handler = mocks.handlers.get('test:sender-nowin')
        if (!handler) throw new Error('handler missing')
        mocks.sender = {
            url: 'http://localhost:5173/app',
            window: null,
            getURL: () => 'http://localhost:5173/app'
        }
        await expect(handler({ sender: mocks.sender } as never)).resolves.toEqual({
            success: false,
            error: 'Unauthorized sender'
        })
    })
})
