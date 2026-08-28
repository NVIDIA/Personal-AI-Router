// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { BrowserWindow } from 'electron'
import { createStructuredLogger } from '@/shared/utils/log'
import { getModularLogLevel } from '@/electron/config/ui-config'
import getErrorString from '@/shared/utils/get-error-string'
import {
    getModularSupervisor,
    ModularStartupTimeoutError
} from '@/electron/service-bridge/modular-supervisor'
import {
    notifyBrokerCrash,
    notifyBrokerStartupFailure
} from '@/electron/connector/broker-crash-notification'
import type { ConnectorStatus, ServiceStatus } from '@/shared/types/ipc-channels'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import { MODULAR_STARTUP_READY_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

const log = createStructuredLogger('app')

let weSpawned = false
let status: ConnectorStatus = 'disconnected'
let serviceError: string | undefined

/** Serializes destroy+init so overlapping service-triggered or manual restarts cannot run in parallel. */
let connectorRestartTail: Promise<void> = Promise.resolve()

/** Registered once on the supervisor singleton; survives connector restarts. */
let crashHandlerRegistered = false

function setStatus(next: ConnectorStatus, error?: string): void {
    status = next
    serviceError = error
    broadcastServiceStatus()
}

/**
 * Push the current connector status to every open window so the Settings
 * Service tab updates instantly instead of waiting for its 3s poll. Mirrors the
 * updater's `update:status` broadcast.
 */
function broadcastServiceStatus(): void {
    const baseStatus: ServiceStatus = { connectorStatus: status, weSpawned }
    const payload: ServiceStatus = serviceError
        ? { ...baseStatus, error: serviceError }
        : baseStatus
    for (const win of BrowserWindow.getAllWindows()) {
        if (!win.isDestroyed() && !win.webContents.isDestroyed()) {
            win.webContents.send('service:status', payload)
        }
    }
}

/**
 * Register supervisor lifecycle handlers exactly once. The supervisor is a
 * singleton that outlives connector restarts.
 */
function ensureBrokerCrashHandler(): void {
    if (crashHandlerRegistered) return
    crashHandlerRegistered = true
    const supervisor = getModularSupervisor()
    supervisor.setOnBrokerCrash(handleBrokerCrash)
    supervisor.setOnReady(handleSupervisorReady)
}

function handleSupervisorReady(): void {
    if (!weSpawned || (status !== 'connecting' && status !== 'reconnecting')) return
    setStatus('connected')
    log.info({
        sublevel: 'lifecycle',
        message: 'Modular service bridge initialized'
    })
}

/**
 * The broker died unexpectedly (taking its worker tree with it). Reflect the
 * dead state in status (so the Service tab shows Stopped + a Start button) and
 * surface an OS notification — reachable even when the app is running only in
 * the tray — that opens the Service tab so the user can restart.
 */
function handleBrokerCrash(info: { code: number | null }): void {
    const failedDuringStartup = status === 'connecting' || status === 'reconnecting'
    const error = failedDuringStartup
        ? `The ${APP_DISPLAY_NAME} service exited before it became ready.`
        : `The ${APP_DISPLAY_NAME} service exited unexpectedly.`
    log.warn({
        sublevel: 'lifecycle',
        message: 'Broker exited unexpectedly',
        data: { code: info.code }
    })
    weSpawned = false
    setStatus('disconnected', error)
    if (failedDuringStartup) {
        notifyBrokerStartupFailure(error)
    } else {
        notifyBrokerCrash()
    }
}

function enqueueConnectorRestart(options: { logServiceRestart?: boolean } = {}): Promise<void> {
    const run = async (): Promise<void> => {
        if (options.logServiceRestart) {
            log.info({ sublevel: 'lifecycle', message: 'Service restart requested' })
        }
        await destroyConnector()
        await initializeConnector()
    }
    const next = connectorRestartTail.then(run, run).catch((err: unknown) => {
        log.error({
            sublevel: 'lifecycle',
            message: `Connector restart failed: ${getErrorString(err)}`,
            data: { error: String(err) }
        })
        throw err
    })
    connectorRestartTail = next.catch(() => undefined)
    return next
}

export const getConnectorStatus = (): ConnectorStatus => status
export const didWeSpawnCli = (): boolean => weSpawned
export const getConnectorError = (): string | undefined => serviceError

/**
 * The old frontend gateway bootstrap is gone for the modular backend. Keeping
 * this export as `null` lets existing window/preload code compile while the
 * renderer uses `__PAIR_TRANSPORT__` instead.
 */
export function getGatewayAddress(): { port: number; token: string } | null {
    return null
}

export const initializeConnector = async (): Promise<void> => {
    if (status === 'connected' || status === 'connecting') return

    setStatus('connecting')
    log.info({ sublevel: 'lifecycle', message: 'Initializing modular service bridge...' })

    try {
        // Seed the persisted log level so spawn passes the right `--log-level`.
        const supervisor = getModularSupervisor()
        supervisor.setLogLevel(getModularLogLevel())
        ensureBrokerCrashHandler()
        supervisor.start()
        weSpawned = true
        await supervisor.waitUntilReady(MODULAR_STARTUP_READY_TIMEOUT_MS)
        handleSupervisorReady()
    } catch (error) {
        const errorString = getErrorString(error) ?? 'Failed to start modular service bridge'
        if (getConnectorStatus() === 'connected') return
        if (error instanceof ModularStartupTimeoutError) {
            setStatus('reconnecting', errorString)
            notifyBrokerStartupFailure(errorString)
        } else if (status !== 'disconnected' || !serviceError) {
            weSpawned = false
            setStatus('disconnected', errorString)
            notifyBrokerStartupFailure(errorString)
        }
        log.error({
            sublevel: 'lifecycle',
            message: 'Failed to initialize modular service bridge',
            data: { error: errorString }
        })
        if (error instanceof Error) throw error
        throw new Error(errorString)
    }
}

export const restartConnector = (): Promise<void> => {
    return enqueueConnectorRestart()
}

/**
 * Tear down the modular service processes. We always stop the subprocess tree
 * we spawned (and unconditionally when `force` is set) — the service does not
 * outlive the app.
 */
export const destroyConnector = async (options?: { force?: boolean }): Promise<void> => {
    log.info({ sublevel: 'lifecycle', message: 'Destroying connector' })
    setStatus('disconnected')

    const shouldStop = options?.force || weSpawned
    if (shouldStop) {
        await getModularSupervisor().stop()
    }

    weSpawned = false
    log.info({ sublevel: 'lifecycle', message: 'Connector destroyed' })
}

export const destroyConnectorSync = (): void => {
    setStatus('disconnected')

    const shouldStop = weSpawned
    if (shouldStop) {
        void getModularSupervisor().stop()
    }

    weSpawned = false
}
