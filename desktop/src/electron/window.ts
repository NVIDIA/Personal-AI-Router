// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { BrowserWindow } from 'electron'
import { join } from 'path'
import { nativeImage } from 'electron'
import { is } from '@electron-toolkit/utils'
import type { OverviewCommand, OverviewMessage } from '@/shared/types/overview'
import { createStructuredLogger } from '@/shared/utils/log'
import { openExternalSafe } from '@/electron/open-external'

const MIN_DIMENSION = 420

/**
 * How long to wait for `ready-to-show` before revealing a window whose renderer
 * has already finished loading. On Linux `ready-to-show` can lag for seconds
 * behind first paint, and with a stalled GPU process it can effectively never
 * fire — which presents as a window that "takes forever to open" or never opens
 * at all. Revealing on a fallback timer bounds that wait; the dark
 * `backgroundColor` means the brief pre-paint frame is black, not a white flash.
 */
const WINDOW_REVEAL_FALLBACK_MS = 4000

/**
 * How long the renderer gets to finish loading before its navigation is treated
 * as wedged rather than slow. A main process blocked on long synchronous or
 * unbounded work can leave a `file://` navigation that never completes and never
 * reports a failure, so nothing paints and none of the error events fire.
 * Reloading is the only recovery available from this side.
 */
const RENDERER_LOAD_DEADLINE_MS = 10_000

/** Reload attempts before the window is revealed unpainted as a last resort. */
const MAX_RENDERER_LOAD_ATTEMPTS = 3

const log = createStructuredLogger('window')

/**
 * Reveals a `show: false` window once there is painted content to show: on
 * `ready-to-show`, or on {@link WINDOW_REVEAL_FALLBACK_MS} once the renderer
 * reports its load finished. A window revealed with nothing painted is just its
 * black `backgroundColor`, which the user cannot read, act on, or recover from,
 * so a renderer that misses {@link RENDERER_LOAD_DEADLINE_MS} has `retryLoad`
 * re-issue its navigation instead of being put on screen empty.
 *
 * Also times the load/show lifecycle so a slow open is attributable (renderer
 * load vs. ready-to-show vs. GPU) from the service log instead of being an
 * invisible "it just hangs".
 */
function revealWhenReady(window: BrowserWindow, label: string, retryLoad: () => void): void {
    const startedAt = Date.now()
    let shown = false
    let loaded = false
    let attempt = 1
    let revealWhenLoaded = false
    let loadTimer: ReturnType<typeof setTimeout> | undefined

    const clearTimers = (): void => {
        clearTimeout(fallbackTimer)
        clearTimeout(loadTimer)
    }

    const isUsable = (): boolean => !window.isDestroyed() && !window.webContents.isDestroyed()

    const reveal = (reason: string): void => {
        if (shown || !isUsable()) return
        shown = true
        clearTimers()
        window.show()
        log.info({
            sublevel: 'window-show',
            message: `${label} window shown via ${reason} after ${Date.now() - startedAt}ms`
        })
    }

    /**
     * The fallback only applies to a window with content ready. If the load
     * lands after the fallback has already elapsed, it reveals right then.
     */
    const revealOnceLoaded = (): void => {
        if (loaded) reveal('fallback-timer')
        else revealWhenLoaded = true
    }

    const handleLoadStalled = (): void => {
        if (shown || loaded || !isUsable()) return
        if (attempt >= MAX_RENDERER_LOAD_ATTEMPTS) {
            log.error({
                sublevel: 'renderer-stall',
                message: `${label} renderer never finished loading after ${attempt} attempts; revealing it unpainted`
            })
            reveal('renderer-stalled')
            return
        }
        attempt += 1
        log.error({
            sublevel: 'renderer-stall',
            message: `${label} renderer did not finish loading within ${RENDERER_LOAD_DEADLINE_MS}ms; retrying its load (attempt ${attempt})`
        })
        loadTimer = setTimeout(handleLoadStalled, RENDERER_LOAD_DEADLINE_MS)
        retryLoad()
    }

    const fallbackTimer = setTimeout(revealOnceLoaded, WINDOW_REVEAL_FALLBACK_MS)
    loadTimer = setTimeout(handleLoadStalled, RENDERER_LOAD_DEADLINE_MS)

    window.once('ready-to-show', () => reveal('ready-to-show'))

    // Not `once`: a reload issued for a stalled navigation has to be able to
    // report its own load, both to reveal the window and to record that the
    // retry took. A late load still paints into an already-revealed window.
    window.webContents.on('did-finish-load', () => {
        loaded = true
        clearTimeout(loadTimer)
        log.info({
            sublevel: 'window-show',
            message: `${label} renderer finished loading after ${Date.now() - startedAt}ms`
        })
        if (revealWhenLoaded) reveal('renderer-loaded')
    })

    window.once('closed', clearTimers)
}

/** Pushes `window:maximized-state` when OS/native maximize changes (e.g. title-bar double-click). */
function attachMaximizedStateForwarding(win: BrowserWindow): void {
    const notify = () => {
        if (win.isDestroyed()) return
        win.webContents.send('window:maximized-state', win.isMaximized())
    }
    win.on('maximize', notify)
    win.on('unmaximize', notify)
    win.on('enter-full-screen', notify)
    win.on('leave-full-screen', notify)
}

function guardExternalNavigation(window: BrowserWindow): void {
    window.webContents.setWindowOpenHandler(({ url }) => {
        openExternalSafe(url)
        return { action: 'deny' }
    })

    window.webContents.on('will-navigate', (event, url) => {
        const currentUrl = window.webContents.getURL()
        if (url !== currentUrl) {
            event.preventDefault()
            openExternalSafe(url)
        }
    })
}

/**
 * Surfaces the two failure modes that present as a blank/gray window so they
 * land in the service log instead of being silent: the renderer failing to load
 * its HTML/asset, and the renderer process crashing. `ERR_ABORTED` (-3) is a
 * benign navigation cancel, and subframe failures are not the gray-window cause,
 * so both are filtered out.
 */
function attachWebContentsDiagnostics(window: BrowserWindow): void {
    const { webContents } = window

    webContents.on(
        'did-fail-load',
        (_event, errorCode, errorDescription, validatedURL, isMainFrame) => {
            if (!isMainFrame || errorCode === -3) return
            log.error({
                sublevel: 'renderer-load',
                message: `Renderer failed to load: ${errorDescription || 'unknown'} (${errorCode})`,
                data: { errorCode, errorDescription, validatedURL }
            })
        }
    )

    webContents.on('render-process-gone', (_event, details) => {
        log.error({
            sublevel: 'renderer-crash',
            message: `Renderer process gone: ${details.reason}`,
            data: { reason: details.reason, exitCode: details.exitCode }
        })
    })
}

const iconPath = join(__dirname, '../resources/icons/logo.png')
const icon = nativeImage.createFromPath(iconPath)
const devTools = true

let overviewWindow: BrowserWindow | null = null
let overviewReady = false
let pendingOverviewCommands: OverviewCommand[] = []

function getOverviewWindow(): BrowserWindow | null {
    return overviewWindow && !overviewWindow.isDestroyed() ? overviewWindow : null
}

/**
 * Flush queued Overview commands once the renderer has mounted and subscribed
 * (signalled via `overview:ready`). Until then they stay queued so a freshly
 * opened Overview never misses a focus/message.
 */
function deliverOverviewCommands(): void {
    const win = getOverviewWindow()
    if (!win || !overviewReady || pendingOverviewCommands.length === 0) return
    const commands = pendingOverviewCommands
    pendingOverviewCommands = []
    for (const command of commands) {
        win.webContents.send('overview:command', command)
    }
}

/** Called from the renderer (`overview:ready`) after it subscribes to commands. */
export function markOverviewReady(): void {
    if (!getOverviewWindow()) return
    overviewReady = true
    deliverOverviewCommands()
}

function isMessageQueued(id: string): boolean {
    return pendingOverviewCommands.some(
        command => command.type === 'message' && command.message.id === id
    )
}

/**
 * Queue a command for the Overview renderer.
 *
 * `focus` is reserved for user-initiated navigation, which may open and raise
 * the window. App-initiated commands pass `false` and only wait in the queue:
 * a background event must never pull the user out of whatever they are doing,
 * including another app in fullscreen. Because such a command can sit queued
 * indefinitely, repeat messages are collapsed by id.
 */
function enqueueOverviewCommand(command: OverviewCommand, focus: boolean): void {
    if (command.type === 'message' && isMessageQueued(command.message.id)) return
    pendingOverviewCommands.push(command)
    if (focus) createOverviewWindow()
    deliverOverviewCommands()
}

/** Open/focus Overview and expand the given node's inline engine settings. */
export function focusNodeInOverview(nodeId: string): void {
    enqueueOverviewCommand({ type: 'focus-node', nodeId }, true)
}

/**
 * Surface a one-off message modal on Overview without stealing focus. If the
 * window is closed the message waits and is delivered the next time the user
 * opens Overview themselves.
 */
export function showOverviewMessage(message: OverviewMessage): void {
    enqueueOverviewCommand({ type: 'message', message }, false)
}

const webPreferences = {
    preload: join(__dirname, '../preload/index.js'),
    // sandbox: false,
    // backgroundThrottling: false,
    devTools,

    contextIsolation: true,
    nodeIntegration: false,
    sandbox: true, // Critical for v28!
    webviewTag: false, // Prevents rendering conflicts
    // This next line is the secret sauce I discovered at 3 AM
    backgroundThrottling: false
}

export function createOverviewWindow(): void {
    const currentWindow = getOverviewWindow()
    if (currentWindow) {
        currentWindow.show()
        currentWindow.focus()
        return
    }

    const window = new BrowserWindow({
        width: 1200,
        height: 900,
        minWidth: MIN_DIMENSION,
        minHeight: MIN_DIMENSION,
        show: false,
        frame: false,
        resizable: true,
        autoHideMenuBar: true,
        backgroundColor: '#000000',
        icon,
        webPreferences
    })

    overviewReady = false
    overviewWindow = window

    window.on('closed', () => {
        overviewWindow = null
        overviewReady = false
    })

    /**
     * Re-callable so a stalled navigation can be retried against the same
     * target. Retrying aborts the in-flight navigation, which rejects the
     * previous call's promise with `ERR_ABORTED`; load failures are reported by
     * `did-fail-load`, so that rejection carries no information.
     */
    const load = (): void => {
        const rendererUrl = process.env['ELECTRON_RENDERER_URL']
        const navigation =
            is.dev && rendererUrl
                ? window.loadURL(rendererUrl)
                : window.loadFile(join(__dirname, '../ui/index.html'))
        navigation.catch(() => {})
    }

    revealWhenReady(window, 'overview', load)
    guardExternalNavigation(window)
    attachWebContentsDiagnostics(window)
    attachMaximizedStateForwarding(window)

    load()
}

let trayWindow: BrowserWindow | null = null

export function getTrayWindow(): BrowserWindow | null {
    return trayWindow && !trayWindow.isDestroyed() ? trayWindow : null
}

export function createTrayWindow(): BrowserWindow {
    if (trayWindow && !trayWindow.isDestroyed()) {
        return trayWindow
    }

    trayWindow = new BrowserWindow({
        width: 380,
        height: 100,
        show: false,
        frame: false,
        resizable: false,
        movable: false,
        skipTaskbar: true,
        alwaysOnTop: true,
        fullscreenable: false,
        autoHideMenuBar: true,
        backgroundColor: '#000000',
        icon,
        webPreferences
    })

    trayWindow.on('closed', () => {
        trayWindow = null
    })

    guardExternalNavigation(trayWindow)
    attachWebContentsDiagnostics(trayWindow)

    const query = '?window=tray'
    if (is.dev && process.env['ELECTRON_RENDERER_URL']) {
        trayWindow.loadURL(process.env['ELECTRON_RENDERER_URL'] + query)
    } else {
        trayWindow.loadFile(join(__dirname, '../ui/index.html'), {
            search: query
        })
    }

    return trayWindow
}
