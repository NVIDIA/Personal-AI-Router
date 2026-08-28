// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, BrowserWindow, Menu, nativeImage, screen, Tray } from 'electron'
import { join } from 'path'
import { createTrayWindow, getTrayWindow, createOverviewWindow } from '@/electron/window'
import { createStructuredLogger } from '@/shared/utils/log'
import { currentPlatform } from '@/shared/utils/platform'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

const log = createStructuredLogger('tray')

const TRAY_WINDOW_WIDTH = 380
const MIN_HEIGHT = 100
const MAX_HEIGHT_RATIO = 0.8
const MARGIN = 10
const BLUR_GRACE_MS = 500
const RETRY_DELAY = 1000
const MAX_RETRIES = 3
const VISIBILITY_CHECK_INTERVAL = 5000

class TrayManager {
    private tray: Tray | null = null
    private contextMenu: Menu | null = null
    private isVisible = false
    private retryCount = 0
    private currentHeight = MIN_HEIGHT
    private lastBlurHideAtMs = 0
    private lastShowAtMs = 0
    private lastTrayBounds: Electron.Rectangle | undefined
    private lastCursorPoint: Electron.Point | undefined
    private blurHidingDisabled = false
    private displayChangeListener: (() => void) | undefined
    private visibilityInterval: ReturnType<typeof setInterval> | undefined

    private getPlatformIcon(): Electron.NativeImage {
        const iconsDir = join(__dirname, '../../resources/icons')

        if (currentPlatform() === 'darwin') {
            let image = nativeImage.createFromPath(join(iconsDir, 'logo.png'))
            image = image.resize({ width: 16, height: 16, quality: 'best' })
            try {
                image.setTemplateImage(true)
            } catch {
                /* not supported in test env */
            }
            return image
        }

        if (currentPlatform() === 'win32') {
            return nativeImage.createFromPath(join(iconsDir, 'logo.ico'))
        }

        const image = nativeImage.createFromPath(join(iconsDir, 'logo.png'))
        return image.resize({ width: 22, height: 22, quality: 'best' })
    }

    async init(): Promise<void> {
        await app.whenReady()
        log.info({ sublevel: 'lifecycle', message: 'Initializing tray' })

        if (currentPlatform() === 'linux' && process.env.XDG_CURRENT_DESKTOP?.includes('Unity')) {
            if (!process.env.XDG_CURRENT_DESKTOP.includes('Unity7')) {
                process.env.XDG_CURRENT_DESKTOP = 'Unity7'
            }
        }

        try {
            const icon = this.getPlatformIcon()
            this.tray = new Tray(icon)
            this.tray.setToolTip(APP_DISPLAY_NAME)

            this.setupContextMenu()
            this.setupClickHandlers()
            this.setupVisibilityCheck()
            this.setupDisplayChangeListeners()

            const win = createTrayWindow()
            this.setupTrayWindowEvents(win)

            this.isVisible = true
            this.retryCount = 0
            log.info({ sublevel: 'lifecycle', message: 'Tray initialized successfully' })
        } catch (error) {
            log.error({ sublevel: 'lifecycle', message: `Failed to initialize tray: ${error}` })
            this.handleTrayError()
        }
    }

    private setupContextMenu(): void {
        this.contextMenu = Menu.buildFromTemplate([
            {
                label: 'Overview',
                click: () => this.showOrCreateMainWindow()
            },
            { type: 'separator' },
            {
                label: `Exit ${APP_DISPLAY_NAME}`,
                click: () => app.quit()
            }
        ])

        if (currentPlatform() !== 'darwin') {
            this.tray!.setContextMenu(this.contextMenu)
        }

        if (currentPlatform() === 'linux') {
            this.tray!.setIgnoreDoubleClickEvents(true)
        }
    }

    private setupClickHandlers(): void {
        if (!this.tray) return

        if (currentPlatform() === 'linux') {
            this.tray.on('click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('middle-click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('double-click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('right-click', () => this.tray?.popUpContextMenu())
        } else if (currentPlatform() === 'darwin') {
            this.tray.on('click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('right-click', () => {
                if (this.contextMenu) this.tray?.popUpContextMenu(this.contextMenu)
            })
        } else {
            this.tray.on('click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('right-click', () => this.tray?.popUpContextMenu())
        }
    }

    private handlePrimaryClick = (bounds?: Electron.Rectangle): void => {
        if (bounds) this.lastTrayBounds = bounds
        try {
            this.lastCursorPoint = screen.getCursorScreenPoint()
        } catch {
            this.lastCursorPoint = undefined
        }

        let win = getTrayWindow()
        if (!win) {
            win = createTrayWindow()
            this.setupTrayWindowEvents(win)
        }

        if (win.isVisible()) {
            if (currentPlatform() === 'darwin') {
                win.hide()
            } else if (currentPlatform() === 'win32') {
                if (!win.isMinimized()) win.hide()
            } else {
                win.hide()
            }
            return
        }

        const blurAge = Date.now() - this.lastBlurHideAtMs
        if (blurAge < BLUR_GRACE_MS) return

        this.positionTrayWindow(win)

        this.blurHidingDisabled = true
        this.lastShowAtMs = Date.now()
        win.show()

        if (currentPlatform() === 'darwin') {
            app.focus()
            win.focus()
        } else if (currentPlatform() === 'win32') {
            win.focus()
        }

        setTimeout(() => {
            this.blurHidingDisabled = false
        }, 400)
    }

    private setupTrayWindowEvents(win: BrowserWindow): void {
        win.on('blur', () => {
            if (this.blurHidingDisabled) return
            const showAge = Date.now() - this.lastShowAtMs
            if (showAge < 300) return
            this.lastBlurHideAtMs = Date.now()
            win.hide()
        })
    }

    private positionTrayWindow(win: BrowserWindow): void {
        const trayBounds = this.lastTrayBounds ?? this.getFreshTrayBounds()
        const cursorPoint = this.lastCursorPoint

        if (currentPlatform() === 'darwin') {
            this.positionForMacOS(win, trayBounds, cursorPoint)
        } else if (currentPlatform() === 'win32') {
            this.positionForWindows(win)
        } else {
            this.positionForLinux(win, trayBounds)
        }
    }

    private positionForMacOS(
        win: BrowserWindow,
        trayBounds: Electron.Rectangle,
        cursorPoint?: Electron.Point
    ): void {
        const h = this.currentHeight
        const targetPoint = cursorPoint ?? { x: trayBounds.x, y: trayBounds.y }
        const display = screen.getDisplayNearestPoint(targetPoint)
        const workArea = display.workArea

        const anchorX = trayBounds.x + trayBounds.width / 2
        let x = Math.round(anchorX - TRAY_WINDOW_WIDTH / 2)
        let y = Math.round(trayBounds.y + trayBounds.height + MARGIN)

        x = Math.max(workArea.x, Math.min(x, workArea.x + workArea.width - TRAY_WINDOW_WIDTH))
        y = Math.max(workArea.y, Math.min(y, workArea.y + workArea.height - h))

        win.setBounds({ x, y, width: TRAY_WINDOW_WIDTH, height: h })
        win.setWindowButtonVisibility(false)
        win.setAlwaysOnTop(true, 'floating')
        win.setFullScreenable(false)
    }

    private positionForWindows(win: BrowserWindow): void {
        const h = this.currentHeight
        const workArea = screen.getPrimaryDisplay().workArea
        const x = Math.round(workArea.x + workArea.width - TRAY_WINDOW_WIDTH - MARGIN)
        const y = Math.round(workArea.y + workArea.height - h - MARGIN)

        win.setBounds({ x, y, width: TRAY_WINDOW_WIDTH, height: h })
    }

    private positionForLinux(win: BrowserWindow, trayBounds: Electron.Rectangle): void {
        const h = this.currentHeight
        const workArea = screen.getPrimaryDisplay().workArea
        const x = Math.round(workArea.x + workArea.width - TRAY_WINDOW_WIDTH - MARGIN)

        let y: number
        if (trayBounds.y > workArea.height * 0.75) {
            y = Math.round(trayBounds.y - h - MARGIN)
        } else if (trayBounds.y < workArea.height * 0.25) {
            y = Math.round(trayBounds.y + trayBounds.height + MARGIN)
        } else {
            y = MARGIN
        }

        y = Math.max(workArea.y, Math.min(y, workArea.y + workArea.height - h))
        win.setBounds({ x, y, width: TRAY_WINDOW_WIDTH, height: h })
    }

    private getFreshTrayBounds(): Electron.Rectangle {
        try {
            if (this.tray) return this.tray.getBounds()
        } catch {
            /* fall through */
        }
        const cursor = screen.getCursorScreenPoint()
        return { x: cursor.x - 10, y: cursor.y - 10, width: 20, height: 20 }
    }

    private setupDisplayChangeListeners(): void {
        if (this.displayChangeListener) {
            try {
                screen.removeListener('display-metrics-changed', this.displayChangeListener)
            } catch {
                /* ignore */
            }
        }

        this.displayChangeListener = () => {
            this.lastTrayBounds = undefined
            this.lastCursorPoint = undefined
        }

        try {
            screen.on('display-metrics-changed', this.displayChangeListener)
        } catch {
            /* ignore */
        }
    }

    private setupVisibilityCheck(): void {
        this.visibilityInterval = setInterval(() => {
            if (!this.isVisible && this.retryCount < MAX_RETRIES) {
                log.warn({ sublevel: 'lifecycle', message: 'Tray not visible, recreating' })
                this.recreateTray()
            }
        }, VISIBILITY_CHECK_INTERVAL)
    }

    private handleTrayError(): void {
        if (this.retryCount < MAX_RETRIES) {
            this.retryCount++
            log.info({
                sublevel: 'lifecycle',
                message: `Retrying tray init (attempt ${this.retryCount}/${MAX_RETRIES})`
            })
            setTimeout(() => this.init(), RETRY_DELAY)
        } else {
            log.error({ sublevel: 'lifecycle', message: 'Failed to init tray after max retries' })
        }
    }

    private recreateTray(): void {
        try {
            if (this.tray) this.tray.destroy()
        } catch {
            /* ignore */
        }
        this.init()
    }

    resizeTrayWindow(contentHeight: number): number {
        const maxHeight = Math.ceil(screen.getPrimaryDisplay().workArea.height * MAX_HEIGHT_RATIO)
        const height = Math.max(MIN_HEIGHT, Math.min(Math.ceil(contentHeight), maxHeight))
        this.currentHeight = height

        const win = getTrayWindow()
        if (win && !win.isDestroyed()) {
            win.setResizable(true)
            this.positionTrayWindow(win)
            win.setResizable(false)
        }

        return maxHeight
    }

    showOrCreateMainWindow(): void {
        createOverviewWindow()
    }

    destroy(): void {
        log.info({ sublevel: 'lifecycle', message: 'Destroying tray' })
        if (this.visibilityInterval) clearInterval(this.visibilityInterval)
        if (this.displayChangeListener) {
            try {
                screen.removeListener('display-metrics-changed', this.displayChangeListener)
            } catch {
                /* ignore */
            }
        }
        const win = getTrayWindow()
        if (win && !win.isDestroyed()) win.destroy()
        if (this.tray) {
            this.tray.destroy()
            this.tray = null
        }
    }
}

const trayManager = new TrayManager()

export function initTray(): Promise<void> {
    return trayManager.init()
}

export function destroyTray(): void {
    trayManager.destroy()
}

export function resizeTrayWindow(contentHeight: number): number {
    return trayManager.resizeTrayWindow(contentHeight)
}
