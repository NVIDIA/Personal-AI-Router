// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineInitialState } from '@/shared/types/engine-api'
import type { AppInitialSnapshot, ClusterInitialSnapshot } from '@/shared/types/bootstrap'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useWorkloadsStore } from '@/ui/stores/workloads.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { useEngineUpdateAvailableStore } from '@/ui/stores/engine-update-available.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { useDiscoveredNodesStore } from '@/ui/stores/discovered-nodes.store'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import { useInferenceDemoStore } from '@/ui/stores/inference-demo.store'
import { useServiceStatusStore } from '@/ui/stores/service-status.store'

let unsubStateRefresh: (() => void) | null = null
let unsubServiceStatus: (() => void) | null = null

export async function connectAndInitialize(): Promise<void> {
    // Backed by `windowApi`, not the service bridge, so it initializes before
    // the `pairApi` guard and stays out of `initializeAllStores`/
    // `cleanupAllStores` — those cycle with the cluster connection, and a
    // node-local demo must survive that cycle and remain stoppable.
    useInferenceDemoStore.getState().initialize()

    // Also `windowApi`-backed, and seeded before the watcher below so that
    // reading an already-connected service is not mistaken for a restart.
    await useServiceStatusStore.getState().initialize()
    watchServiceRestart()

    if (!window.pairApi) return
    await initializeAllStores()
}

/**
 * Re-read every snapshot when the service comes back.
 *
 * A restarted broker builds a fresh state projection, so whatever this renderer
 * holds is stale. For a renderer that started while the service was stopped
 * that snapshot is empty, and nothing else would ever replace it — the window
 * would keep loading forever even after the user started the service again.
 */
function watchServiceRestart(): void {
    unsubServiceStatus?.()
    unsubServiceStatus = useServiceStatusStore.subscribe((state, previous) => {
        if (previous.status.connectorStatus === 'connected') return
        if (state.status.connectorStatus !== 'connected') return
        enqueueResync()
    })
}

async function initializeAllStores(): Promise<void> {
    cleanupAllStores()

    const appInitial = await fetchAppInitial()
    const clusterInitial = await fetchClusterInitial()

    await useConnectionStore.getState().initialize(appInitial, clusterInitial)
    await useErrorsStore.getState().initialize()
    await useNodesStore.getState().initialize()
    await useDiscoveredNodesStore.getState().initialize()
    await useClusterInvitationsStore.getState().initialize(clusterInitial)
    useMetricsStore.getState().initialize()

    let engineInitial: EngineInitialState | undefined
    if (window.pairApi) {
        try {
            engineInitial = await window.pairApi.engines.getInitialState()
        } catch {
            console.error('Failed to fetch engine initial state')
            engineInitial = {
                statuses: [],
                models: [],
                activeProgress: [],
                updateAvailable: []
            }
        }
    }
    await useEngineStatusStore.getState().initialize(engineInitial)
    await useEngineModelsStore.getState().initialize(engineInitial)
    await useEngineProgressStore.getState().initialize(engineInitial)
    await useEngineUpdateAvailableStore.getState().initialize(engineInitial)
    usePendingActionsStore.getState().initialize()
    await useWorkloadsStore.getState().initialize()

    if (window.pairApi) {
        unsubStateRefresh = window.pairApi.connection.onStateRequestRefresh(() => {
            enqueueResync()
        })
    }
}

/** Serializes resyncs so a restart and a cluster-leave refresh cannot interleave. */
let resyncTail: Promise<void> = Promise.resolve()

function enqueueResync(): void {
    if (!window.pairApi) return
    resyncTail = resyncTail.then(resyncAllStores, resyncAllStores).catch(() => undefined)
}

function cleanupAllStores(): void {
    unsubStateRefresh?.()
    unsubStateRefresh = null
    useConnectionStore.getState().cleanup()
    useErrorsStore.getState().cleanup()
    useNodesStore.getState().cleanup()
    useDiscoveredNodesStore.getState().cleanup()
    useClusterInvitationsStore.getState().cleanup()
    useMetricsStore.getState().cleanup()
    useEngineStatusStore.getState().cleanup()
    useEngineModelsStore.getState().cleanup()
    useEngineProgressStore.getState().cleanup()
    useEngineUpdateAvailableStore.getState().cleanup()
    usePendingActionsStore.getState().cleanup()
    useWorkloadsStore.getState().cleanup()
}

/** Re-reads every domain snapshot in place, keeping existing subscriptions. */
async function resyncAllStores(): Promise<void> {
    const appInitial = await fetchAppInitial()
    const clusterInitial = await fetchClusterInitial()

    await useConnectionStore.getState().initialize(appInitial, clusterInitial)
    await useNodesStore.getState().refresh()
    await useDiscoveredNodesStore.getState().refresh()
    if (clusterInitial) {
        useClusterInvitationsStore.getState().hydrate(clusterInitial)
    } else {
        await useClusterInvitationsStore.getState().refresh()
    }
    await useWorkloadsStore.getState().refresh()
    useErrorsStore.getState().refresh()
    useMetricsStore.getState().clearAll()

    let engineInitial: EngineInitialState | undefined
    if (window.pairApi) {
        try {
            engineInitial = await window.pairApi.engines.getInitialState()
        } catch {
            engineInitial = {
                statuses: [],
                models: [],
                activeProgress: [],
                updateAvailable: []
            }
        }
    }
    useEngineStatusStore.getState().initialize(engineInitial)
    useEngineModelsStore.getState().initialize(engineInitial)
    useEngineProgressStore.getState().initialize(engineInitial)
    useEngineUpdateAvailableStore.getState().initialize(engineInitial)
    usePendingActionsStore.getState().initialize()
}

async function fetchAppInitial(): Promise<AppInitialSnapshot | undefined> {
    if (!window.pairApi) return undefined
    try {
        return await window.pairApi.app.getInitial()
    } catch {
        console.error('Failed to fetch app initial state')
        return {
            connected: false,
            selfId: null
        }
    }
}

async function fetchClusterInitial(): Promise<ClusterInitialSnapshot | undefined> {
    if (!window.pairApi) return undefined
    try {
        return await window.pairApi.cluster.getInitial()
    } catch {
        console.error('Failed to fetch cluster initial state')
        return {
            info: { clusterId: null, isClustered: false, clusterFriendlyName: '' },
            identity: { nodeUuid: '', nodeId: '', name: '', certFingerprint: '', clusterId: '' },
            members: [],
            pendingInvites: []
        }
    }
}
