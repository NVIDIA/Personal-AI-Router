// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { AppInitialSnapshot, ClusterInitialSnapshot } from '@/shared/types/bootstrap'
import { getPairTransport } from '@/ui/api/bootstrap'

interface ConnectionStore {
    connected: boolean
    selfId: string | null
    clusterId: string | null
    clusterFriendlyName: string
    initialize: (
        appInitial?: AppInitialSnapshot,
        clusterInitial?: ClusterInitialSnapshot
    ) => Promise<void>
    hydrateInitial: (app: AppInitialSnapshot, cluster: ClusterInitialSnapshot) => void
    cleanup: () => void
}

let unsubs: Array<() => void> = []

export const useConnectionStore = create<ConnectionStore>((set, get) => ({
    connected: false,
    selfId: null,
    clusterId: null,
    clusterFriendlyName: '',

    hydrateInitial: (app, cluster) => {
        set({
            connected: app.connected,
            selfId: app.selfId,
            clusterId: cluster.info.clusterId,
            clusterFriendlyName: cluster.info.clusterFriendlyName
        })
    },

    initialize: async (appInitial, clusterInitial) => {
        get().cleanup()

        try {
            const app = appInitial ?? (await window.pairApi.app.getInitial())
            const cluster = clusterInitial ?? (await window.pairApi.cluster.getInitial())
            get().hydrateInitial(app, cluster)
        } catch {
            set({
                connected: false,
                selfId: null,
                clusterId: null,
                clusterFriendlyName: ''
            })
        }

        const serviceTransport = getPairTransport()
        if (serviceTransport) {
            unsubs.push(
                serviceTransport.onConnect(() => {
                    void (async () => {
                        try {
                            const [app, cluster] = await Promise.all([
                                window.pairApi.app.getInitial(),
                                window.pairApi.cluster.getInitial()
                            ])
                            set({
                                connected: app.connected,
                                selfId: app.selfId,
                                clusterId: cluster.info.clusterId,
                                clusterFriendlyName: cluster.info.clusterFriendlyName
                            })
                        } catch {
                            set({
                                connected: false,
                                selfId: null,
                                clusterId: null,
                                clusterFriendlyName: ''
                            })
                        }
                    })()
                }),
                serviceTransport.onDisconnect(() => {
                    set({
                        connected: false,
                        clusterId: null,
                        clusterFriendlyName: ''
                    })
                })
            )
        }

        if (window.pairApi) {
            unsubs.push(
                window.pairApi.connection.onClusterIdentity(payload => {
                    set({
                        clusterId: payload.clusterId,
                        clusterFriendlyName: payload.clusterFriendlyName
                    })
                })
            )
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    }
}))
