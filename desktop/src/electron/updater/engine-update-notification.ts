// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { subscribePush } from '@/electron/service-bridge/push-bus'
import { dismissOverviewMessage, showOverviewMessage } from '@/electron/window'
import { EngineDisplayNames } from '@/shared/constants/engines'
import type { EngineType } from '@/shared/types/engines'

export function subscribeEngineUpdateNotifications(
    getLocalNodeId: () => string | null
): () => void {
    const notifiedVersions = new Set<string>()
    const currentNotices = new Map<EngineType, string | null>()
    return subscribePush(event => {
        if (event.channel !== 'engines:state-changed') return
        const { nodeId, engineType, updateAvailable } = event.payload
        if (nodeId !== getLocalNodeId() || updateAvailable === undefined) return

        const id = updateAvailable
            ? `engine-update:${nodeId}:${engineType}:${updateAvailable.latestVersion}`
            : null
        const currentNotice = currentNotices.get(engineType)
        if (currentNotice && currentNotice !== id) dismissOverviewMessage(currentNotice)
        currentNotices.set(engineType, id)
        if (!id || !updateAvailable || notifiedVersions.has(id)) return
        notifiedVersions.add(id)
        const name = EngineDisplayNames[engineType]
        showOverviewMessage({
            id,
            kind: 'update',
            title: `${name} update available`,
            body: `${name} ${updateAvailable.latestVersion} is available. This node has ${updateAvailable.currentVersion} installed.`,
            actionLabel: 'Open engine settings',
            action: { type: 'open-engine-settings', nodeId }
        })
    })
}
