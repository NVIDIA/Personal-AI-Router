// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useState } from 'react'
import { Divider, Stack } from '@nvidia/foundations-react-core'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { EngineProcessStatus } from '@/shared/types/engines'

import { engineProgressKey } from '@/shared/utils/engine-progress'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useNodesStore } from '@/ui/stores/nodes.store'

import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import type { EngineCommandType } from '@/shared/types/engine-api'

import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { ModelSection } from '@/ui/components/ModelManager/ModelSection'
import { BackendHeader } from './BackendHeader'
import { BackendFooter } from './BackendFooter'
import { BackendUpdateBanner } from './BackendUpdateBanner'

import { EngineSettingsSection } from './EngineSettingsSection'

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
    const [expanded, setExpanded] = useState(false)
    const [confirmUninstall, setConfirmUninstall] = useState(false)
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
            displayBackend.processStatus !== 'not-installed' &&
            displayBackend.processStatus !== 'initializing'
        )
    }, [displayBackend.processStatus, isUnavailable])

    const nodeOs = useNodesStore(state => state.nodes.get(nodeId)?.os)
    const targetOs = nodeOs ?? window.windowApi.platform

    const handleToggle = useCallback(() => {
        window.pairApi.engines.toggle(backend.type, nodeId)
    }, [backend.type, nodeId])

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

    // Install/start/stop, model pull, and the settings editor work on clustered
    // peers; uninstall, update, and model load/delete remain local-only.
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

            {canShowAccordions && (
                <EngineSettingsSection
                    key={`${nodeId}:${backend.type}`}
                    nodeId={nodeId}
                    engineType={backend.type}
                    disabled={isTransitioning}
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
        </>
    )
}
