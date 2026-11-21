// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { subscribePush } from '@/electron/service-bridge/push-bus'
import { dismissOverviewMessage, showOverviewMessage } from '@/electron/window'
import { EngineDisplayNames } from '@/shared/constants/engines'

export function subscribeEngineUpdateNotifications(
    getNodeLabel: (nodeId: string) => string
): () => void {
    const notifiedVersions = new Set<string>()
    const currentNotices = new Map<string, string>()
    return subscribePush(event => {
        if (event.channel !== 'engines:state-changed') return
        const { nodeId, engineType, updateAvailable } = event.payload
        if (updateAvailable === undefined) return

        const key = `engine-update:${nodeId}:${engineType}`
        const id = updateAvailable ? `${key}:${updateAvailable.latestVersion}` : null
        const currentNotice = currentNotices.get(key)
        if (currentNotice && currentNotice !== id) dismissOverviewMessage(currentNotice)
        if (!id || !updateAvailable) {
            currentNotices.delete(key)
            return
        }
        currentNotices.set(key, id)
        if (notifiedVersions.has(id)) return
        notifiedVersions.add(id)
        const name = EngineDisplayNames[engineType]
        showOverviewMessage({
            id,
            kind: 'update',
            title: `${name} update available`,
            body: `${name} ${updateAvailable.latestVersion} is available. ${getNodeLabel(nodeId)} has ${updateAvailable.currentVersion} installed.`,
            actionLabel: 'Open engine settings',
            action: { type: 'open-engine-settings', nodeId }
        })
    })
}
