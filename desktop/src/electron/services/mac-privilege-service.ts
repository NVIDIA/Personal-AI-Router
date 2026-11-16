// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { execFile } from 'node:child_process'
import { existsSync } from 'node:fs'
import path from 'node:path'
import { setTimeout } from 'node:timers/promises'
import { app } from 'electron'
import {
    getMacHelperRepairAttempts,
    recordMacHelperRepairAttempt
} from '@/electron/config/ui-config'
import { currentPlatform } from '@/shared/utils/platform'
import { createStructuredLogger } from '@/shared/utils/log'

/**
 * Thin, UI-free wrapper around the bundled `nvpair-helper-ctl` control tool. It is
 * the only path Electron uses to drive the macOS privileged helper: it never
 * runs privileged shell commands itself. Every call spawns the signed
 * `Contents/MacOS/nvpair-helper-ctl`, which registers/unregisters the SMAppService
 * LaunchDaemon and relays validated requests to it over XPC, and prints one JSON
 * object we parse here.
 *
 * The dialog/first-run/System-Settings UX lives in the lifecycle layer
 * (src/electron/main.ts); this module stays free of UI per the privileged-helper
 * design.
 */

const log = createStructuredLogger('mac-helper')

const CTL_BINARY_NAME = 'nvpair-helper-ctl'
const MANUAL_RECOVERY_URL =
    'https://github.com/NVIDIA/Personal-AI-Router/blob/develop/desktop/docs/macos-privileged-helper.md#manual-recovery'
const MAX_REPAIR_ATTEMPTS = 2
const MAX_REGISTRATION_RETRIES = 2
const REGISTRATION_RETRY_WAIT_MS = 2000
// launchd's observed stale-constraint repair runs about 10 seconds after rejection.
const LAUNCH_REPAIR_WAIT_MS = 12000

type HelperRegistration = 'enabled' | 'requiresApproval' | 'notRegistered' | 'notFound' | 'unknown'

interface HelperStatus {
    registration: HelperRegistration
    reachable: boolean
    ctlVersion: string
    daemonVersion: string | null
}

interface HelperInstallResult {
    ok: boolean
    registration: HelperRegistration
    approvalOpened: boolean
    error: string | null
}

interface EnsureConfiguredResult {
    supported: boolean
    registration: HelperRegistration
    firewallConfigured: boolean
    firewallError: string | null
}

type JsonPrimitive = string | number | boolean | null
type JsonValue = JsonPrimitive | JsonValue[] | { [key: string]: JsonValue }

function parseJson(text: string): JsonValue | null {
    try {
        const parsed: JsonValue = JSON.parse(text)
        return parsed
    } catch {
        return null
    }
}

function asObject(value: JsonValue | null): { [key: string]: JsonValue } | null {
    return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : null
}

function stringField(obj: { [key: string]: JsonValue }, key: string): string {
    const value = obj[key]
    return typeof value === 'string' ? value : ''
}

function boolField(obj: { [key: string]: JsonValue }, key: string): boolean {
    return obj[key] === true
}

function toRegistration(value: string): HelperRegistration {
    switch (value) {
        case 'enabled':
        case 'requiresApproval':
        case 'notRegistered':
        case 'notFound':
            return value
        default:
            return 'unknown'
    }
}

/** `process.execPath` is `<App>.app/Contents/MacOS/<exe>`. */
function macOsDir(): string {
    return path.dirname(process.execPath)
}

function ctlPath(): string {
    return path.join(macOsDir(), CTL_BINARY_NAME)
}

/**
 * Only meaningful on a packaged macOS build: the helper binaries ship inside the
 * signed `.app`, so an unsigned dev run (or any other platform) has nothing to
 * drive.
 */
function isSupported(): boolean {
    return app.isPackaged && currentPlatform() === 'darwin' && existsSync(ctlPath())
}

function runCtl(args: string[], timeoutMs: number): Promise<JsonValue | null> {
    return new Promise(resolve => {
        execFile(ctlPath(), args, { timeout: timeoutMs, encoding: 'utf8' }, (error, stdout) => {
            const text = stdout.trim()
            if (!text) {
                if (error) {
                    log.error({
                        sublevel: 'ctl',
                        message: `ctl ${args[0]} failed: ${error.message}`
                    })
                }
                resolve(null)
                return
            }
            resolve(parseJson(text))
        })
    })
}

async function status(): Promise<HelperStatus> {
    const result = asObject(await runCtl(['status'], 8000))
    if (!result) {
        return { registration: 'unknown', reachable: false, ctlVersion: '', daemonVersion: null }
    }
    const daemonVersion = stringField(result, 'daemonVersion')
    return {
        registration: toRegistration(stringField(result, 'registration')),
        reachable: boolField(result, 'reachable'),
        ctlVersion: stringField(result, 'ctlVersion'),
        daemonVersion: daemonVersion ? daemonVersion : null
    }
}

async function install(): Promise<HelperInstallResult> {
    const result = asObject(await runCtl(['install'], 30000))
    if (!result) {
        return {
            ok: false,
            registration: 'unknown',
            approvalOpened: false,
            error: 'no response from helper control tool'
        }
    }
    const errorText = stringField(result, 'error')
    return {
        ok: boolField(result, 'ok'),
        registration: toRegistration(stringField(result, 'status')),
        approvalOpened: boolField(result, 'approvalOpened'),
        error: errorText ? errorText : null
    }
}

async function uninstall(): Promise<boolean> {
    const result = asObject(await runCtl(['uninstall'], 20000))
    return result ? boolField(result, 'ok') : false
}

async function configureFirewall(): Promise<{ ok: boolean; error: string | null }> {
    // No `--app-path`: the daemon derives the target bundle from this tool's own
    // verified identity, closing the confused-deputy the argument enabled.
    const result = asObject(await runCtl(['configure-firewall'], 30000))
    if (!result) return { ok: false, error: 'no response from helper control tool' }
    const errorText = stringField(result, 'error')
    return { ok: boolField(result, 'ok'), error: errorText ? errorText : null }
}

async function removeFirewall(): Promise<boolean> {
    const result = asObject(await runCtl(['remove-firewall'], 30000))
    return result ? boolField(result, 'ok') : false
}

function isStale(current: HelperStatus, targetVersion: string): boolean {
    return (
        current.registration === 'enabled' &&
        (!current.reachable ||
            (current.daemonVersion !== null && current.daemonVersion !== targetVersion))
    )
}

/**
 * Drive the helper to the desired end state in one call (no UI). Registers the
 * daemon if missing, refreshes a stale daemon after an in-place app update
 * (version mismatch or unreachable), and configures the firewall when reachable.
 * Replacement is bounded per app version, persisted before each unregister.
 *
 * `forceFirewall` should be true on the first-run flow; the firewall is also
 * (re)configured automatically after a version refresh or fresh registration so
 * a release that adds a networked binary still gets its rule. Otherwise the
 * idempotent socketfilterfw work is skipped to keep launches cheap.
 */
async function ensureConfigured(forceFirewall: boolean): Promise<EnsureConfiguredResult> {
    if (!isSupported()) {
        return {
            supported: false,
            registration: 'unknown',
            firewallConfigured: false,
            firewallError: null
        }
    }

    const targetVersion = app.getVersion()
    let current = await status()
    let reconfigureFirewall = forceFirewall

    for (let round = 0; round < MAX_REPAIR_ATTEMPTS; round++) {
        const attempts = getMacHelperRepairAttempts(targetVersion)
        if (isStale(current, targetVersion) && attempts > 0 && attempts < MAX_REPAIR_ATTEMPTS) {
            await setTimeout(LAUNCH_REPAIR_WAIT_MS)
            current = await status()
        }
        const refresh = isStale(current, targetVersion) && attempts < MAX_REPAIR_ATTEMPTS
        if (refresh) {
            recordMacHelperRepairAttempt(targetVersion)
            log.info({
                sublevel: 'mac-helper',
                message: `Refreshing helper ${current.daemonVersion ?? 'unreachable'} -> ${targetVersion} (attempt ${attempts + 1}/${MAX_REPAIR_ATTEMPTS})`
            })
            if (!(await uninstall())) {
                return {
                    supported: true,
                    registration: current.registration,
                    firewallConfigured: false,
                    firewallError: `Could not unregister the stale privileged helper. For manual recovery, see ${MANUAL_RECOVERY_URL}`
                }
            }
        }

        if (
            !refresh &&
            current.registration !== 'notRegistered' &&
            current.registration !== 'notFound' &&
            current.registration !== 'unknown'
        ) {
            break
        }

        let installation = await install()
        current = await status()
        for (
            let retry = 0;
            refresh &&
            !installation.ok &&
            installation.registration === 'notRegistered' &&
            current.registration === 'notRegistered' &&
            retry < MAX_REGISTRATION_RETRIES;
            retry++
        ) {
            await setTimeout(REGISTRATION_RETRY_WAIT_MS)
            current = await status()
            if (current.registration !== 'notRegistered') break
            installation = await install()
            current = await status()
        }
        reconfigureFirewall = true
        if (!installation.ok) {
            return {
                supported: true,
                registration: current.registration,
                firewallConfigured: false,
                firewallError: installation.error ?? 'Could not register the privileged helper'
            }
        }
        if (!refresh) break
    }

    if (current.registration !== 'enabled') {
        return {
            supported: true,
            registration: current.registration,
            firewallConfigured: false,
            firewallError: null
        }
    }

    if (
        !current.reachable ||
        (current.daemonVersion !== null && current.daemonVersion !== targetVersion)
    ) {
        return {
            supported: true,
            registration: current.registration,
            firewallConfigured: false,
            firewallError: current.reachable
                ? `Privileged helper is still running a different version. For manual recovery, see ${MANUAL_RECOVERY_URL}`
                : `Could not communicate with the privileged helper. For manual recovery, see ${MANUAL_RECOVERY_URL}`
        }
    }

    if (!reconfigureFirewall) {
        return {
            supported: true,
            registration: 'enabled',
            firewallConfigured: true,
            firewallError: null
        }
    }

    const firewall = await configureFirewall()
    return {
        supported: true,
        registration: 'enabled',
        firewallConfigured: firewall.ok,
        firewallError: firewall.error
    }
}

export const macPrivilege = {
    isSupported,
    status,
    install,
    uninstall,
    configureFirewall,
    removeFirewall,
    ensureConfigured
}
