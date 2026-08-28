// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { Stack } from '@nvidia/foundations-react-core'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useEngineUpdateAvailableStore } from '@/ui/stores/engine-update-available.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import type { BackendInfo } from '@/ui/types/engine-info'
import {
    EnabledEngineTypes,
    EngineDefaultLinks,
    EngineDisplayNames
} from '@/shared/constants/engines'
import { BackendRow } from '@/ui/components/BackendRow/BackendRow'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'

/**
 * Inline engine editor for a single node, rendered inside the overview node
 * card's expandable "Engine settings" section. This is the reusable body that
 * used to live in the standalone Edit Node window — minus the window chrome
 * (AppBar / WindowDropDown / ErrorModal / background shell).
 */
export default function NodeEngineSettings({ nodeId }: { nodeId: string }) {
    const node = useNodesStore(state => state.nodes.get(nodeId))
    const selfId = useConnectionStore(state => state.selfId)

    // The fingerprints below subscribe to the engine stores so this component
    // re-renders when the underlying status/models/update data changes. The
    // actual data is read from the stores via getState() in the map below.
    const engineFingerprint = useEngineStatusStore(state => {
        const nodeStatuses = state.statusByNode.get(nodeId)
        if (!nodeStatuses) return ''
        const parts: string[] = []
        for (const [type, s] of nodeStatuses) {
            parts.push(
                `${type}:${s.processStatus}:${s.enginePort ?? ''}:${s.proxyPort ?? ''}:${s.installedVersion ?? ''}`
            )
        }
        return parts.join('|')
    })
    const updateAvailableFingerprint = useEngineUpdateAvailableStore(state => {
        const parts: string[] = []
        for (const [k, v] of state.entries) {
            if (!k.startsWith(`${nodeId}:`)) continue
            parts.push(`${k}:${v.currentVersion}->${v.latestVersion}:${v.installType}`)
        }
        return parts.join('|')
    })
    void updateAvailableFingerprint
    const modelsFingerprint = useEngineModelsStore(state => {
        let fp = ''
        for (const type of EnabledEngineTypes) {
            const key = `${nodeId}:${type}`
            const m = state.models.get(key)
            if (!m) {
                fp += `${type}:0;`
                continue
            }
            const modelParts = m.models.map(
                mi => `${mi.name}:${mi.status ?? ''}:${mi.expiry ?? ''}`
            )
            fp += `${type}:${modelParts.join(',')};`
        }
        return fp
    })

    const isRemote = useMemo(() => nodeId !== selfId, [nodeId, selfId])
    const isDisconnected = useMemo(() => {
        return isRemote && node?.status === 'offline'
    }, [node, isRemote])

    void engineFingerprint
    void modelsFingerprint

    const allBackends: { backend: BackendInfo; statusKnown: boolean }[] = EnabledEngineTypes.map(
        type => {
            const statusStore = useEngineStatusStore.getState()
            // Distinguish "the service reported this engine" from "we synthesized
            // a placeholder". The placeholder's processStatus is 'initializing';
            // without this flag a never-reported remote engine spins forever.
            const statusKnown = statusStore.statusByNode.get(nodeId)?.has(type) ?? false
            const status = statusStore.getStatus(nodeId, type)
            const models = useEngineModelsStore.getState().getModels(nodeId, type)
            const updateAvailable = useEngineUpdateAvailableStore.getState().getInfo(nodeId, type)

            return {
                backend: {
                    type,
                    displayName: EngineDisplayNames[type],
                    source: 'detected',
                    processStatus: status.processStatus,
                    port: status.enginePort,
                    proxyPort: status.proxyPort,
                    models: models.models,
                    docsUrl: EngineDefaultLinks[type]?.docsUrl,
                    installUrl: EngineDefaultLinks[type]?.installUrl,
                    installedVersion: status.installedVersion,
                    updateAvailable
                },
                statusKnown
            }
        }
    )

    return (
        <Stack gap="6" className="pt-1 pb-2">
            {isDisconnected ? (
                <InlineErrorBanner
                    severity="warning"
                    message="Node is disconnected. Controls are disabled until it reconnects."
                />
            ) : null}
            {allBackends.map(({ backend, statusKnown }, i) => (
                <BackendRow
                    key={`${backend.type}-${i}`}
                    backend={backend}
                    nodeId={nodeId}
                    statusKnown={statusKnown}
                />
            ))}
        </Stack>
    )
}
