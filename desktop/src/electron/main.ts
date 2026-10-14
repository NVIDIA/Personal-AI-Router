// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, dialog, session } from 'electron'
import { electronApp } from '@electron-toolkit/utils'

import { buildCspHeader } from '@/electron/csp'
import { registerApplicationIconMenu } from '@/electron/application-icon-menu'
import { createOverviewWindow } from '@/electron/window'
import { registerWindowShortcuts } from '@/electron/window-shortcuts'
import { initTray, destroyTray } from '@/electron/tray'
import { createStructuredLogger } from '@/shared/utils/log'
import {
    loadUiConfig,
    getStartupSettings,
    isMacHelperSetupComplete,
    setMacHelperSetupComplete
} from '@/electron/config/ui-config'
import {
    applyLoginItem,
    readLoginItem,
    shouldStartHidden,
    wasOpenedAtLogin
} from '@/electron/login-item'
import { currentPlatform } from '@/shared/utils/platform'
import { APP_DISPLAY_NAME, APP_EXIT_ARGUMENT, APP_ID } from '@/shared/constants/app'
import { migrateAppData } from '@/electron/path'
import { macPrivilege } from '@/electron/services/mac-privilege-service'

import { registerAllIpc } from '@/electron/ipc'
import { destroyConnector, destroyConnectorSync, initializeConnector } from '@/electron/connector'
import { initializeUpdater } from '@/electron/updater'
import { ensureNvpairOnPath } from '@/electron/nvpair-command'
import { isAppDataWipeScheduled } from '@/electron/app-data-wipe-orchestrator'
import { destroyInferenceDemoSync, stopInferenceDemo } from '@/electron/inference-demo'
import { startEventLoopMonitor } from '@/electron/event-loop-monitor'

const gotTheLock = app.requestSingleInstanceLock()
const exitRequested = process.argv.includes(APP_EXIT_ARGUMENT)

app.commandLine.appendSwitch('disable-features', 'CalculateNativeWinOcclusion')
// app.commandLine.appendSwitch('disable-features', 'NvidiaVpSuperResolution')
app.commandLine.appendSwitch('enable-logging')
app.commandLine.appendSwitch('disable-http-cache')

if (currentPlatform() === 'win32') {
    // This prevents the GPU process crash on Windows
    // app.commandLine.appendSwitch('disable-gpu-sandbox')
    // But we still want hardware acceleration, so we add:
    // app.commandLine.appendSwitch('enable-unsafe-webgpu')
    // And this fixes the DirectX initialization race condition
    // app.commandLine.appendSwitch('use-angle', 'gl')
}

if (!gotTheLock || exitRequested) {
    // A launched Exit action should stop here instead of initializing a new app.
    app.quit()
} else {
    // Handle the event when a second instance is launched
    app.on('second-instance', (_event, commandLine, _workingDirectory, _additionalData) => {
        if (commandLine.includes(APP_EXIT_ARGUMENT)) {
            app.quit()
            return
        }
        // A second launch is normally a person double-clicking the app while it
        // sits in the tray, and it opens the window as it always has. The login
        // item firing against an already-running instance is the exception: it
        // carries the hidden argument and must not steal the foreground. Only the
        // argument counts here: `wasOpenedAtLogin` describes how this process
        // started, not the launch that reached it.
        if (!startsHidden(commandLine, false)) {
            createOverviewWindow()
        }
    })

    // macOS fires `activate` when the Dock/Launchpad icon is clicked on an
    // already-running app — no new process, so `second-instance` never fires.
    // Without this the overview window never reopens after being closed, because
    // the tray keeps the process alive (`window-all-closed` is a no-op).
    // `createOverviewWindow` focuses the existing window if one is open, so this
    // is safe to call unconditionally.
    //
    // A macOS login item that is meant to start hidden can receive one `activate`
    // as part of that launch, which would open the window the user asked not to
    // see. `launchActivatePending` swallows that one event, and the ready
    // handler clears it once startup has settled, so a Dock click afterwards
    // always opens the window even if no launch-time `activate` ever arrived.
    app.on('activate', () => {
        if (launchActivatePending) {
            launchActivatePending = false
            return
        }
        createOverviewWindow()
    })

    // Started before the app does any work so startup, runtime, and teardown are
    // all covered — a stall during any of them makes every other timing in the log
    // untrustworthy.
    startEventLoopMonitor()

    // Only the single-instance lock owner migrates the pre-rename app data, so
    // two concurrent launches can never move the same directory. Runs before any
    // user-data read (loadUiConfig below).
    migrateAppData()

    loadUiConfig()

    let launchActivatePending = false

    /**
     * Whether a launch carrying `argv` stays in the tray with no window.
     * `openedAtLogin` is how the OS started this process, which only the first
     * launch has: a second-instance launch passes false.
     */
    function startsHidden(argv: readonly string[], openedAtLogin: boolean): boolean {
        return shouldStartHidden({
            argv,
            platform: currentPlatform(),
            wasOpenedAtLogin: openedAtLogin,
            startHidden: getStartupSettings().startHidden
        })
    }

    app.whenReady().then(async () => {
        // Decided first, before anything awaits: the macOS `activate` that comes
        // with a login launch can arrive while the connector is still starting.
        const hiddenLaunch = startsHidden(process.argv, wasOpenedAtLogin())
        launchActivatePending = hiddenLaunch && currentPlatform() === 'darwin'

        if (app.isPackaged) {
            electronApp.setAppUserModelId(APP_ID)
        } else if (currentPlatform() === 'win32') {
            // Windows toast notifications in dev require the running exe as AUMID.
            // https://www.electronjs.org/docs/latest/tutorial/notifications#windows
            app.setAppUserModelId(process.execPath)
        } else {
            electronApp.setAppUserModelId(APP_ID)
        }

        registerApplicationIconMenu()

        app.on('browser-window-created', (_, window) => {
            registerWindowShortcuts(window)
        })

        session.defaultSession.setPermissionRequestHandler((_webContents, permission, callback) => {
            callback(permission === 'media')
        })

        session.defaultSession.webRequest.onHeadersReceived((details, callback) => {
            const isHtml =
                details.responseHeaders?.['content-type']?.some(v => v.includes('text/html')) ||
                details.responseHeaders?.['Content-Type']?.some(v => v.includes('text/html'))
            if (!isHtml) {
                callback({})
                return
            }
            const csp = buildCspHeader()
            callback({
                responseHeaders: {
                    ...details.responseHeaders,
                    'Content-Security-Policy': [csp]
                }
            })
        })

        registerAllIpc()
        initializeUpdater()

        // Bound startup on authoritative broker + proxy readiness before opening
        // the normal Overview. A failure queues Overview navigation to
        // Settings > Service, then rejects so startup can continue with retry
        // and log controls available instead of an indefinite loading screen.
        try {
            await initializeConnector()
        } catch (err) {
            log.error({ sublevel: 'lifecycle', message: `Connector failed: ${err}` })
        }

        // Make `nvpair` reachable from any terminal by generating a launcher for
        // the bundled terminal UI.
        ensureNvpairOnPath()

        // A login-item launch starts to the tray with no window. The tray is
        // still what keeps the process alive and its Overview entry (and a tray
        // click) creates the window on demand, exactly as it does after the
        // window is closed by hand.
        if (!hiddenLaunch) {
            createOverviewWindow()
        }
        initTray()

        reconcileLoginItem()

        void runMacPrivilegedSetup(hiddenLaunch)

        // Startup has settled: any `activate` from here on is a person.
        launchActivatePending = false
    })

    // Keep app alive in tray -- don't quit when all windows are closed
    app.on('window-all-closed', () => {
        // no-op: tray keeps the process alive
    })

    const log = createStructuredLogger('app')

    /**
     * Rewrite the OS login item from the persisted preferences, once per launch.
     *
     * The registered entry names an executable, and an app update moves that
     * executable, so an entry written by the previous version can point at a
     * path that no longer exists. Re-applying on every ready keeps it current
     * without the user touching the toggle again, and costs nothing when the
     * preference is off — `openAtLogin: false` simply leaves no entry behind.
     */
    function reconcileLoginItem(): void {
        const startup = getStartupSettings()
        try {
            applyLoginItem(startup)
            const applied = readLoginItem(startup)
            if (!applied) return
            log.info({
                sublevel: 'login-item',
                message: `Login item reconciled: requested openAtLogin=${startup.launchAtLogin} startHidden=${startup.startHidden}, reported openAtLogin=${applied.openAtLogin}`
            })
        } catch (err) {
            // Autostart is a convenience; a registry or SMAppService failure must
            // not take the launch down with it.
            log.error({ sublevel: 'login-item', message: `Login item write failed: ${err}` })
        }
    }

    // macOS only: register the SMAppService privileged helper that owns one-time
    // root setup (Application Firewall config) and keep it current across app
    // updates. The Electron app never runs as root — it only spawns the signed
    // nvpair-helper-ctl. The first run explains the macOS approval before it appears;
    // subsequent launches reconcile silently (version-aware reinstall).
    //
    // A hidden launch has nobody to explain the approval to, and the setup opens
    // dialogs and the System Settings approval page on its own. The first run is
    // therefore left for a launch that opens the window; `macHelperSetupComplete`
    // stays false until then. Version-aware reconciliation has no UI and still runs.
    async function runMacPrivilegedSetup(startedHidden: boolean): Promise<void> {
        if (!macPrivilege.isSupported()) return
        const firstTime = !isMacHelperSetupComplete()

        if (firstTime && startedHidden) {
            log.info({
                sublevel: 'mac-helper',
                message: 'First-run helper setup deferred to a launch that opens the window'
            })
            return
        }

        try {
            if (firstTime) {
                await dialog.showMessageBox({
                    type: 'info',
                    buttons: ['Continue'],
                    defaultId: 0,
                    message: `${APP_DISPLAY_NAME} needs administrator permission to complete setup.`,
                    detail: 'macOS will ask you to approve a background helper that configures the firewall for local AI traffic. You only need to do this once.'
                })
            }

            const result = await macPrivilege.ensureConfigured(firstTime)
            if (!result.supported) return

            if (result.registration === 'requiresApproval') {
                if (firstTime) {
                    await dialog.showMessageBox({
                        type: 'warning',
                        buttons: ['OK'],
                        message: `Approve the ${APP_DISPLAY_NAME} helper`,
                        detail: `Open System Settings > General > Login Items & Extensions, enable the ${APP_DISPLAY_NAME} background item, then relaunch the app to finish setup.`
                    })
                }
                log.info({
                    sublevel: 'mac-helper',
                    message: 'Privileged helper requires approval in System Settings'
                })
                return
            }

            if (result.registration === 'enabled' && result.firewallConfigured) {
                setMacHelperSetupComplete(true)
                log.info({ sublevel: 'mac-helper', message: 'Privileged helper configured' })
            } else {
                log.error({
                    sublevel: 'mac-helper',
                    message: `Privileged helper setup incomplete: ${result.registration} ${result.firewallError ?? ''}`
                })
            }
        } catch (err) {
            log.error({ sublevel: 'mac-helper', message: `Privileged setup error: ${err}` })
        }
    }

    let isCleaningUp = false
    app.on('before-quit', event => {
        if (isAppDataWipeScheduled()) {
            return
        }
        if (isCleaningUp) {
            event.preventDefault()
            return
        }
        log.info({ sublevel: 'lifecycle', message: 'App quitting' })
        isCleaningUp = true
        event.preventDefault()
        destroyTray()
        // Cancel any pending demo submissions before we start tearing down.
        // Requests already in flight are reaped by destroyInferenceDemoSync on exit.
        stopInferenceDemo()
        destroyConnector()
            .catch(() => {})
            .finally(() => {
                log.info({ sublevel: 'lifecycle', message: 'App shut down' })
                app.exit(0)
                process.exit(0)
            })
    })

    for (const signal of ['SIGINT', 'SIGTERM'] as const) {
        process.on(signal, () => {
            log.info({ sublevel: 'lifecycle', message: `Received ${signal}` })
            destroyTray()
            stopInferenceDemo()
            destroyConnector()
                .catch(() => {})
                .finally(() => {
                    log.info({ sublevel: 'lifecycle', message: 'App shut down' })
                    process.exit(0)
                })
        })
    }

    // Last-resort sync cleanup — fires even on force quit / crash.
    // Only synchronous code runs here; kill child processes immediately.
    process.on('exit', () => {
        destroyInferenceDemoSync()
        destroyConnectorSync()
    })
}
