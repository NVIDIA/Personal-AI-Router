// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
    Badge,
    Button,
    Dropdown,
    Flex,
    Stack,
    Switch,
    Text,
    type DropdownEntry
} from '@nvidia/foundations-react-core'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { Download, OpenInNew } from '@/ui/components/icons'
import type { ServiceStatus, StartupSettings, StartupState } from '@/shared/types/ipc-channels'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import {
    MODULAR_DEFAULT_LOG_LEVEL,
    MODULAR_LOG_LEVELS,
    type ModularLogLevel
} from '@/shared/constants/modular-runtime'
import getErrorString from '@/shared/utils/get-error-string'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import { isElectron } from '@/ui/api/bootstrap'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import ApplicationUpdatesCard from './UpdatesSettings'
import VersionsCard from './VersionsCard'
import WipeAppDataCard from './WipeAppDataCard'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import { useInferenceDemoStore } from '@/ui/stores/inference-demo.store'

const BROWSER_TOOLTIP = 'Only available in the desktop app'
const UNSUPPORTED_STARTUP_TOOLTIP = 'This operating system has no login item to register'

const LOG_LEVEL_LABELS: Record<ModularLogLevel, string> = {
    debug: 'Debug',
    info: 'Info',
    warn: 'Warning',
    error: 'Error'
}

const STATUS_LABELS: Record<ServiceStatus['connectorStatus'], string> = {
    connected: 'Running',
    connecting: 'Starting',
    reconnecting: 'Reconnecting',
    disconnected: 'Stopped'
}

const STATUS_COLORS: Record<ServiceStatus['connectorStatus'], 'green' | 'yellow' | 'red' | 'gray'> =
    {
        connected: 'green',
        connecting: 'yellow',
        reconnecting: 'yellow',
        disconnected: 'red'
    }

function ElectronOnlyButton({
    onClick,
    disabled,
    children
}: {
    onClick: () => void
    disabled: boolean
    children: ReactNode
}) {
    if (isElectron) {
        return (
            <Button kind="secondary" size="small" onClick={onClick} disabled={disabled}>
                {children}
            </Button>
        )
    }

    return (
        <DismissibleTooltip slotContent={BROWSER_TOOLTIP}>
            <span className="inline-flex">
                <Button kind="secondary" size="small" disabled>
                    {children}
                </Button>
            </span>
        </DismissibleTooltip>
    )
}

/**
 * One labelled switch in the Startup section.
 *
 * A switch that is off because the platform cannot support it looks exactly like
 * one the user turned off, so a known reason travels with it as a tooltip on a
 * wrapper — a disabled control fires no pointer events of its own. `disabled` is
 * separate from that reason because the switch is also inert while main has yet
 * to answer, and "still loading" is not a reason worth naming.
 */
function StartupToggle({
    label,
    description,
    checked,
    onCheckedChange,
    disabled,
    unavailableReason
}: {
    label: string
    description: string
    checked: boolean
    onCheckedChange: (next: boolean) => void
    disabled: boolean
    unavailableReason: string | null
}) {
    const control = (
        <Switch
            checked={checked}
            onCheckedChange={onCheckedChange}
            disabled={disabled}
            size="small"
            aria-label={label}
        />
    )

    return (
        <Stack gap="1">
            <Flex align="center" justify="between" gap="4">
                <Text kind="body/regular/md">{label}</Text>
                {unavailableReason !== null ? (
                    <DismissibleTooltip slotContent={unavailableReason}>
                        <span className="inline-flex">{control}</span>
                    </DismissibleTooltip>
                ) : (
                    control
                )}
            </Flex>
            <Text kind="body/regular/sm" className="text-subtle-color">
                {description}
            </Text>
        </Stack>
    )
}

function StatusPill({
    label,
    color
}: {
    label: string
    color: 'green' | 'yellow' | 'red' | 'gray'
}) {
    return (
        <Badge color={color} kind="solid">
            {label}
        </Badge>
    )
}

export default function ServiceSettings() {
    const connected = useConnectionStore(state => state.connected)
    const [status, setStatus] = useState<ServiceStatus | null>(null)
    const [error, setError] = useState<string | null>(null)
    const [loading, setLoading] = useState<string | null>(null)
    const [logLevel, setLogLevel] = useState<ModularLogLevel>(MODULAR_DEFAULT_LOG_LEVEL)
    /** `null` until the main process reports it — not "everything is off". */
    const [startup, setStartup] = useState<StartupState | null>(null)
    const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
    const [startingDemo, setStartingDemo] = useState(false)
    const setActiveTab = useOverviewUiStore(state => state.setActiveTab)
    const demoStatus = useInferenceDemoStore(state => state.state.status)
    const demoActive = demoStatus !== 'idle'

    const fetchStatus = useCallback(async () => {
        try {
            const s = await window.windowApi.service.getStatus()
            setStatus(s)
        } catch {
            /* not connected yet */
        }
    }, [])

    useEffect(() => {
        fetchStatus()
        pollRef.current = setInterval(fetchStatus, 3000)
        return () => {
            if (pollRef.current) clearInterval(pollRef.current)
        }
    }, [fetchStatus, connected])

    // Reflect status changes the instant they happen (e.g. an unexpected broker
    // crash flips the connector to disconnected) rather than waiting for the
    // next 3s poll. The poll above remains as a fallback.
    useEffect(() => {
        if (!isElectron) return
        return window.windowApi.service.onStatusChanged(setStatus)
    }, [])

    useEffect(() => {
        if (!isElectron) return
        window.windowApi.service
            .getLogLevel()
            .then(setLogLevel)
            .catch(() => {})
    }, [])

    const handleLogLevelChange = useCallback(
        async (level: ModularLogLevel) => {
            const prev = logLevel
            setLogLevel(level)
            try {
                await window.windowApi.service.setLogLevel(level)
            } catch (err) {
                setLogLevel(prev)
                setError(getErrorString(err))
            }
        },
        [logLevel]
    )

    useEffect(() => {
        if (!isElectron) return
        window.windowApi.service
            .getStartup()
            .then(setStartup)
            .catch(() => {})
    }, [])

    /**
     * Flip the switch immediately, then reconcile against what main actually
     * wrote. Writing a login item touches the registry (or SMAppService), which
     * can fail on a locked-down machine — a switch left where the user put it
     * after that would claim an autostart that does not exist.
     */
    const handleStartupChange = useCallback(
        async (patch: Partial<StartupSettings>) => {
            if (!startup) return
            setStartup({ ...startup, ...patch })
            try {
                setStartup(await window.windowApi.service.setStartup(patch))
            } catch (err) {
                setStartup(startup)
                setError(getErrorString(err))
            }
        },
        [startup]
    )

    const startupDisabled = !isElectron || startup === null || !startup.supported
    const startupUnavailableReason = !isElectron
        ? BROWSER_TOOLTIP
        : startup !== null && !startup.supported
          ? UNSUPPORTED_STARTUP_TOOLTIP
          : null

    const logLevelItems: DropdownEntry[] = useMemo(
        () =>
            MODULAR_LOG_LEVELS.map(level => ({
                children: LOG_LEVEL_LABELS[level],
                onSelect: () => handleLogLevelChange(level)
            })),
        [handleLogLevelChange]
    )

    const handleAction = useCallback(
        async (action: 'start' | 'stop' | 'restart') => {
            setLoading(action)
            setError(null)
            try {
                if (action === 'stop') await window.windowApi.service.stop()
                else if (action === 'start') await window.windowApi.service.start()
                else await window.windowApi.service.restart()
                await new Promise(r => setTimeout(r, 1000))
                await fetchStatus()
            } catch (err) {
                setError(getErrorString(err))
            } finally {
                setLoading(null)
            }
        },
        [fetchStatus]
    )

    const handleOpenLogFile = useCallback(async () => {
        setError(null)
        try {
            await window.windowApi.service.openLogFile()
        } catch (err) {
            setError(getErrorString(err))
        }
    }, [])

    const handleOpenLogDir = useCallback(async () => {
        setError(null)
        try {
            await window.windowApi.service.openLogDir()
        } catch (err) {
            setError(getErrorString(err))
        }
    }, [])

    const handleSaveLogs = useCallback(async () => {
        setError(null)
        try {
            await window.windowApi.window.saveDebugLogs()
        } catch (err) {
            setError(getErrorString(err))
        }
    }, [])

    /**
     * Start the Inference Demo and hand the user straight to Overview, where the
     * traffic shows up as ordinary job activity. Stop lives in the toast that
     * follows them there, so this button simply goes inert for the run.
     *
     * `startingDemo` covers the IPC round trip: without it a second click lands
     * before the `preparing` broadcast does and main rejects it as "already
     * running", which reads as a spurious error for what felt like one click.
     */
    const handleOpenTest = useCallback(async () => {
        if (startingDemo) return
        setError(null)
        setStartingDemo(true)
        try {
            await window.windowApi.inferenceDemo.start()
            setActiveTab('overview')
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setStartingDemo(false)
        }
    }, [setActiveTab, startingDemo])

    const connStatus = status?.connectorStatus ?? 'disconnected'
    const isRunning = connStatus === 'connected'
    const statusColor = STATUS_COLORS[connStatus]

    return (
        <Stack gap="6" className="relative py-8 px-3 w-full">
            {status?.error && <InlineErrorBanner severity="error" message={status.error} />}
            {error && error !== status?.error && (
                <InlineErrorBanner severity="error" message={error} />
            )}

            <Flex wrap="wrap" align="start" justify="start" gap="4">
                <div className="settings-card pair-paper p-4">
                    <Stack gap="6">
                        <Flex justify="between" align="center" gap="4" wrap="wrap">
                            <Flex align="center" gap="3" wrap="wrap">
                                <Text kind="body/semibold/md">Status</Text>
                                <StatusPill label={STATUS_LABELS[connStatus]} color={statusColor} />
                                {status?.weSpawned === false && isRunning && (
                                    <Text kind="body/regular/sm" className="text-subtle-color">
                                        (attached to external process)
                                    </Text>
                                )}
                            </Flex>

                            <Flex gap="2" wrap="wrap">
                                <ElectronOnlyButton
                                    onClick={() => void handleOpenTest()}
                                    disabled={loading !== null || demoActive || startingDemo}
                                >
                                    Test
                                </ElectronOnlyButton>
                                {isRunning && (
                                    <ElectronOnlyButton
                                        onClick={() => handleAction('stop')}
                                        disabled={loading !== null}
                                    >
                                        {loading === 'stop' ? (
                                            <span
                                                className="spinner-element"
                                                role="status"
                                                aria-label=""
                                            />
                                        ) : (
                                            'Stop'
                                        )}
                                    </ElectronOnlyButton>
                                )}
                                {connStatus === 'disconnected' && (
                                    <ElectronOnlyButton
                                        onClick={() => handleAction('start')}
                                        disabled={loading !== null}
                                    >
                                        {loading === 'start' ? (
                                            <span
                                                className="spinner-element"
                                                role="status"
                                                aria-label=""
                                            />
                                        ) : (
                                            'Start'
                                        )}
                                    </ElectronOnlyButton>
                                )}
                                <ElectronOnlyButton
                                    onClick={() => handleAction('restart')}
                                    disabled={loading !== null || connStatus === 'disconnected'}
                                >
                                    {loading === 'restart' ? (
                                        <span
                                            className="spinner-element"
                                            role="status"
                                            aria-label=""
                                        />
                                    ) : (
                                        'Restart'
                                    )}
                                </ElectronOnlyButton>
                            </Flex>
                        </Flex>

                        <Stack gap="1">
                            <Flex align="center" justify="between" gap="4">
                                <Text kind="body/semibold/md">Log level</Text>
                                {isElectron ? (
                                    <Dropdown
                                        items={logLevelItems}
                                        size="small"
                                        aria-label="Select service log level"
                                    >
                                        {LOG_LEVEL_LABELS[logLevel]}
                                    </Dropdown>
                                ) : (
                                    <DismissibleTooltip slotContent={BROWSER_TOOLTIP}>
                                        <span className="inline-flex">
                                            <Button kind="secondary" size="small" disabled>
                                                {LOG_LEVEL_LABELS[logLevel]}
                                            </Button>
                                        </span>
                                    </DismissibleTooltip>
                                )}
                            </Flex>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Verbosity of the backend service logs. Applies immediately and
                                persists across restarts.
                            </Text>
                            {isElectron && (
                                <Flex align="center" gap="4" className="mt-1">
                                    <button
                                        type="button"
                                        onClick={handleOpenLogFile}
                                        className="no-bg-link cursor-pointer pair-link self-start"
                                    >
                                        <Flex align="center" gap="1">
                                            <Text kind="body/regular/sm">Open log file</Text>
                                            <OpenInNew style={{ fontSize: 14, marginTop: -2 }} />
                                        </Flex>
                                    </button>
                                    <button
                                        type="button"
                                        onClick={handleOpenLogDir}
                                        className="no-bg-link cursor-pointer pair-link self-start"
                                    >
                                        <Flex align="center" gap="1">
                                            <Text kind="body/regular/sm">Open logs directory</Text>
                                            <OpenInNew style={{ fontSize: 14, marginTop: -2 }} />
                                        </Flex>
                                    </button>
                                    <button
                                        type="button"
                                        onClick={handleSaveLogs}
                                        className="no-bg-link cursor-pointer pair-link self-start"
                                    >
                                        <Flex align="center" gap="1">
                                            <Text kind="body/regular/sm">Save logs</Text>
                                            <Download style={{ fontSize: 14, marginTop: -2 }} />
                                        </Flex>
                                    </button>
                                </Flex>
                            )}
                        </Stack>

                        <Stack gap="4">
                            <Text kind="body/semibold/md">Startup</Text>
                            <StartupToggle
                                label="Launch at login"
                                description={`Start ${APP_DISPLAY_NAME} automatically when you sign in to this computer.`}
                                checked={startup?.launchAtLogin ?? false}
                                onCheckedChange={next =>
                                    void handleStartupChange({ launchAtLogin: next })
                                }
                                disabled={startupDisabled}
                                unavailableReason={startupUnavailableReason}
                            />
                            <StartupToggle
                                label="Start minimized to tray"
                                description="When started at login, open to the tray without showing the window. Starting the app yourself always opens it."
                                checked={startup?.startHidden ?? true}
                                onCheckedChange={next =>
                                    void handleStartupChange({ startHidden: next })
                                }
                                disabled={startupDisabled}
                                unavailableReason={startupUnavailableReason}
                            />
                        </Stack>

                        <WipeAppDataCard />
                    </Stack>
                </div>

                <ApplicationUpdatesCard />
            </Flex>

            <VersionsCard />
        </Stack>
    )
}
