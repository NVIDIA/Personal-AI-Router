// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useState } from 'react'
import { Divider, Stack } from '@nvidia/foundations-react-core'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { EngineProcessStatus } from '@/shared/types/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { engineProgressKey } from '@/shared/utils/engine-progress'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import type { EngineCommandType } from '@/shared/types/engine-api'

import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { ModelSection } from '@/ui/components/ModelManager/ModelSection'
import { BackendHeader } from './BackendHeader'
import { BackendFooter } from './BackendFooter'
import { BackendUpdateBanner } from './BackendUpdateBanner'
import { EditState } from '@/ui/types/engine-edit-state'
import { PortsSection } from './PortsSection'

/** Strip non-digits and parse; returns NaN for empty/invalid input. */
function parsePort(raw: string): number {
    return parseInt((raw || '').replace(/[^0-9]/g, ''), 10)
}

function isValidPort(port: number): boolean {
    return Number.isFinite(port) && port >= 1 && port <= 65535
}

/**
 * The transitional status to display while an optimistic lifecycle command is
 * in flight but the backend has not yet acknowledged it. Feeding this synthetic
 * status through `displayBackend` makes every existing gate (`isTransitioning`,
 * `controlsDisabled`, the header spinner + label, `canShowAccordions`) reflect
 * the pending action with no other changes. It is only ever shown in the gap
 * between the click and the first real push (the entry clears the moment the
 * backend reports a status that differs from the captured baseline).
 */
function syntheticPendingStatus(
    action: EngineCommandType,
    current: EngineProcessStatus
): EngineProcessStatus | null {
    switch (action) {
        case 'install':
        case 'update':
            return 'installing'
        case 'uninstall':
            return 'uninstalling'
        case 'toggle':
            return current === 'running' ? 'stopping' : 'starting'
        default:
            return null
    }
}

export function BackendRow({
    backend,
    nodeId,
    statusKnown
}: {
    backend: BackendInfo
    nodeId: string
    /** False when the service never reported this node/engine pair (placeholder status). */
    statusKnown: boolean
}) {
    const [edit, setEdit] = useState<EditState>({
        serverPort: String(backend.port ?? ''),
        proxyPort: String(backend.proxyPort ?? '')
    })

    const addLocalError = useErrorsStore(state => state.addLocalError)

    useEffect(() => {
        setEdit(prev => ({ ...prev, serverPort: String(backend.port ?? '') }))
    }, [backend.port])

    useEffect(() => {
        setEdit(prev => ({ ...prev, proxyPort: String(backend.proxyPort ?? '') }))
    }, [backend.proxyPort])

    const [expanded, setExpanded] = useState(false)
    const [confirmUninstall, setConfirmUninstall] = useState(false)
    const [confirmPorts, setConfirmPorts] = useState(false)

    const caps = EngineCapabilities[backend.type]
    const hasManagedInstall = caps.hasInstall.length > 0

    const serverPortChanged = useMemo(
        () => caps.hasEnginePort && edit.serverPort !== String(backend.port ?? ''),
        [caps.hasEnginePort, edit.serverPort, backend.port]
    )

    const proxyPortChanged = useMemo(
        () => Boolean(edit.proxyPort) && edit.proxyPort !== String(backend.proxyPort ?? ''),
        [edit.proxyPort, backend.proxyPort]
    )

    const portsChanged = serverPortChanged || proxyPortChanged

    const installProgress = useEngineProgressStore(s => {
        const installKey = engineProgressKey({
            nodeId,
            engineType: backend.type,
            operation: 'install'
        })
        const uninstallKey = engineProgressKey({
            nodeId,
            engineType: backend.type,
            operation: 'uninstall'
        })
        const ip = s.progress.get(installKey)
        const up = s.progress.get(uninstallKey)
        if (ip !== undefined && ip.status !== 'idle') return ip
        if (up !== undefined && up.status !== 'idle') return up
        return undefined
    })
    const lifecyclePending = usePendingActionsStore(s =>
        s.getLifecyclePending(nodeId, backend.type)
    )

    const displayBackend = useMemo((): BackendInfo => {
        // While an optimistic lifecycle command is in flight, show its
        // transitional status so the header spins immediately. Real transition
        // pushes clear the pending entry, at which point backend truth wins.
        const synthetic = lifecyclePending
            ? syntheticPendingStatus(lifecyclePending, backend.processStatus)
            : null
        const base: BackendInfo =
            synthetic && synthetic !== backend.processStatus
                ? { ...backend, processStatus: synthetic }
                : backend
        if (installProgress) {
            return {
                ...base,
                installProgress: {
                    status: installProgress.status,
                    percent: installProgress.percent
                }
            }
        }
        return base
    }, [installProgress, backend, lifecyclePending])

    const selfId = useConnectionStore(state => state.selfId)
    const isLocalNode = nodeId === selfId

    // Remote peers may not have polled engine facts yet; refresh status when the
    // accordion opens so LM Studio and other engines are not stuck as Unavailable.
    useEffect(() => {
        if (!isLocalNode && nodeId) {
            void useEngineStatusStore.getState().initialize()
            void useEngineModelsStore.getState().initialize()
        }
    }, [isLocalNode, nodeId])

    // The modular backend reports engine status via remote-get-installed and
    // proxy discovery; refresh engine stores when viewing a remote node so
    // LM Studio and other engines are not stuck as Unavailable before facts arrive.
    const isUnavailable = !statusKnown && !isLocalNode

    const isTransitioning = useMemo(() => {
        if (isUnavailable) return false
        return (
            displayBackend.processStatus === 'installing' ||
            displayBackend.processStatus === 'uninstalling' ||
            displayBackend.processStatus === 'starting' ||
            displayBackend.processStatus === 'stopping' ||
            displayBackend.processStatus === 'initializing'
        )
    }, [displayBackend.processStatus, isUnavailable])

    const canShowAccordions = useMemo(() => {
        if (isUnavailable) return false
        return (
            displayBackend.processStatus !== 'installing' &&
            displayBackend.processStatus !== 'uninstalling' &&
            (displayBackend.processStatus !== 'not-installed' || !hasManagedInstall) &&
            displayBackend.processStatus !== 'initializing'
        )
    }, [displayBackend.processStatus, isUnavailable, hasManagedInstall])

    const nodeOs = useNodesStore(state => state.nodes.get(nodeId)?.os)
    const targetOs = nodeOs ?? window.windowApi.platform

    const handleToggle = useCallback(() => {
        window.pairApi.engines.toggle(backend.type, nodeId)
    }, [backend.type, nodeId])

    // Snap the inputs back to backend truth. After firing an apply we reset here
    // so the fields always reflect what the service reports: on success the
    // engine:state-changed / proxy:ready push updates the value; on failure no
    // push arrives and the input stays at the last-known-good value (the revert).
    const resetPortsToBackend = useCallback(() => {
        setEdit({
            serverPort: String(backend.port ?? ''),
            proxyPort: String(backend.proxyPort ?? '')
        })
    }, [backend.port, backend.proxyPort])

    const validateAndConfirmPorts = useCallback(() => {
        if (!serverPortChanged && !proxyPortChanged) return
        if (serverPortChanged && !isValidPort(parsePort(edit.serverPort))) {
            addLocalError('Enter a valid server port (1–65535).')
            return
        }
        if (proxyPortChanged && !isValidPort(parsePort(edit.proxyPort))) {
            addLocalError('Enter a valid proxy port (1–65535).')
            return
        }
        // Guard the one collision the backend cannot resolve atomically yet: a
        // single transaction setting the server and proxy to the same port.
        if (
            serverPortChanged &&
            proxyPortChanged &&
            parsePort(edit.serverPort) === parsePort(edit.proxyPort)
        ) {
            addLocalError('Server and proxy ports must be different.')
            return
        }
        setConfirmPorts(true)
    }, [serverPortChanged, proxyPortChanged, edit.serverPort, edit.proxyPort, addLocalError])

    const handleApplyPorts = useCallback(() => {
        const enginePort = serverPortChanged ? parsePort(edit.serverPort) : undefined
        const proxyPort = proxyPortChanged ? parsePort(edit.proxyPort) : undefined
        if (enginePort === undefined && proxyPort === undefined) return
        window.pairApi.engines.setPorts(backend.type, nodeId, { enginePort, proxyPort })
        resetPortsToBackend()
    }, [
        serverPortChanged,
        proxyPortChanged,
        edit.serverPort,
        edit.proxyPort,
        backend.type,
        nodeId,
        resetPortsToBackend
    ])

    const portsConfirmMessage = useMemo(() => {
        if (serverPortChanged && proxyPortChanged) {
            return 'Changing the server and proxy ports will restart the engine and proxy.'
        }
        if (serverPortChanged) {
            return 'Changing the server port will restart the engine.'
        }
        return 'Changing the proxy port will restart the proxy.'
    }, [serverPortChanged, proxyPortChanged])

    const handleInstall = useCallback(() => {
        window.pairApi.engines.install(backend.type, nodeId)
    }, [backend.type, nodeId])

    const handleUninstall = useCallback(() => {
        window.pairApi.engines.uninstall(backend.type, nodeId)
    }, [backend.type, nodeId])

    const handleUpdate = useCallback(() => {
        window.pairApi.engines.update(backend.type, nodeId)
    }, [backend.type, nodeId])

    const requestUninstall = useCallback(() => {
        setConfirmUninstall(true)
    }, [])

    const handleExpand = useCallback(() => {
        if (isUnavailable) return
        setExpanded(prev => !prev)
    }, [isUnavailable])

    // Install/start/stop and model pull work on clustered peers; uninstall,
    // update, port edits, and model load/delete remain local-only.
    const controlsDisabled = isTransitioning

    const content = expanded ? (
        <Stack gap="4" className="max-w-full overflow-hidden pt-4">
            <BackendUpdateBanner
                backend={displayBackend}
                disabled={controlsDisabled || !isLocalNode}
                onUpdate={handleUpdate}
            />

            {canShowAccordions && (
                <ModelSection backend={displayBackend} nodeId={nodeId} disabled={isTransitioning} />
            )}

            {canShowAccordions && (caps.hasEnginePort || edit.proxyPort) && (
                <PortsSection
                    edit={edit}
                    portsChanged={portsChanged}
                    anyLoading={isTransitioning}
                    isLocalNode={isLocalNode}
                    caps={caps}
                    onApplyPorts={validateAndConfirmPorts}
                    onServerChange={v => setEdit(prev => ({ ...prev, serverPort: v }))}
                    onProxyChange={v => setEdit(prev => ({ ...prev, proxyPort: v }))}
                />
            )}

            <BackendFooter
                backend={displayBackend}
                targetOs={targetOs}
                showUninstall={isLocalNode}
                disabled={controlsDisabled}
                onUninstall={requestUninstall}
            />
        </Stack>
    ) : null

    return (
        <>
            <div className="pair-paper w-full p-4">
                <Stack gap="0" className="max-w-full min-w-0">
                    <BackendHeader
                        backend={displayBackend}
                        isTransitioning={isTransitioning}
                        isUnavailable={isUnavailable}
                        disabled={controlsDisabled}
                        onToggle={handleToggle}
                        onExpand={handleExpand}
                        expanded={expanded}
                        isLocalNode={isLocalNode}
                        targetOs={targetOs}
                        onInstall={handleInstall}
                    />
                    {expanded && <Divider />}
                    {content}
                </Stack>
            </div>

            <ConfirmModal
                open={confirmUninstall}
                onOpenChange={setConfirmUninstall}
                title="Uninstall"
                message={`Are you sure you want to uninstall ${backend.displayName}?`}
                confirmLabel="Uninstall"
                confirmColor="danger"
                onConfirm={handleUninstall}
            />

            <ConfirmModal
                open={confirmPorts}
                onOpenChange={setConfirmPorts}
                title="Apply ports"
                message={portsConfirmMessage}
                confirmLabel="Confirm"
                onConfirm={handleApplyPorts}
            />
        </>
    )
}
