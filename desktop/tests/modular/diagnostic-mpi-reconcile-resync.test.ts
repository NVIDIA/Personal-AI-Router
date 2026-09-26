// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

interface ServiceStoreSnapshot {
    status: { connectorStatus: string }
}

const harness = vi.hoisted(() => {
    const store = (methods: string[]) =>
        Object.fromEntries(methods.map(method => [method, vi.fn(async () => undefined)]))
    const stores = {
        connection: store(['cleanup', 'initialize']),
        nodes: store(['cleanup', 'initialize', 'refresh']),
        errors: store(['cleanup', 'initialize', 'refresh']),
        metrics: store(['cleanup', 'initialize', 'clearAll']),
        workloads: store(['cleanup', 'initialize', 'refresh']),
        engineStatus: store(['cleanup', 'initialize']),
        engineModels: store(['cleanup', 'initialize']),
        engineProgress: store(['cleanup', 'initialize']),
        engineUpdate: store(['cleanup', 'initialize']),
        pending: store(['cleanup', 'initialize']),
        discovered: store(['cleanup', 'initialize', 'refresh']),
        invitations: store(['cleanup', 'initialize', 'hydrate', 'refresh']),
        inference: store(['initialize']),
        service: store(['initialize']),
        group: store(['reset']),
        fabric: store(['reset']),
        diagnostic: store(['reset']),
        diagnosticMPI: store(['reset', 'refreshInventory'])
    }
    return {
        stores,
        statusCallback: null as
            | null
            | ((state: ServiceStoreSnapshot, previous: ServiceStoreSnapshot) => void)
    }
})

vi.mock('@/ui/stores/connection.store', () => ({
    useConnectionStore: { getState: () => harness.stores.connection }
}))
vi.mock('@/ui/stores/nodes.store', () => ({
    useNodesStore: { getState: () => harness.stores.nodes }
}))
vi.mock('@/ui/stores/errors.store', () => ({
    useErrorsStore: { getState: () => harness.stores.errors }
}))
vi.mock('@/ui/stores/metrics.store', () => ({
    useMetricsStore: { getState: () => harness.stores.metrics }
}))
vi.mock('@/ui/stores/workloads.store', () => ({
    useWorkloadsStore: { getState: () => harness.stores.workloads }
}))
vi.mock('@/ui/stores/engine-status.store', () => ({
    useEngineStatusStore: { getState: () => harness.stores.engineStatus }
}))
vi.mock('@/ui/stores/engine-models.store', () => ({
    useEngineModelsStore: { getState: () => harness.stores.engineModels }
}))
vi.mock('@/ui/stores/engine-progress.store', () => ({
    useEngineProgressStore: { getState: () => harness.stores.engineProgress }
}))
vi.mock('@/ui/stores/engine-update-available.store', () => ({
    useEngineUpdateAvailableStore: { getState: () => harness.stores.engineUpdate }
}))
vi.mock('@/ui/stores/pending-actions.store', () => ({
    usePendingActionsStore: { getState: () => harness.stores.pending }
}))
vi.mock('@/ui/stores/discovered-nodes.store', () => ({
    useDiscoveredNodesStore: { getState: () => harness.stores.discovered }
}))
vi.mock('@/ui/stores/cluster-invitations.store', () => ({
    useClusterInvitationsStore: { getState: () => harness.stores.invitations }
}))
vi.mock('@/ui/stores/inference-demo.store', () => ({
    useInferenceDemoStore: { getState: () => harness.stores.inference }
}))
vi.mock('@/ui/stores/service-status.store', () => ({
    useServiceStatusStore: {
        getState: () => harness.stores.service,
        subscribe: (
            callback: (state: ServiceStoreSnapshot, previous: ServiceStoreSnapshot) => void
        ) => {
            harness.statusCallback = callback
            return () => undefined
        }
    }
}))
vi.mock('@/ui/stores/vllm-group.store', () => ({
    useVllmGroupStore: { getState: () => harness.stores.group }
}))
vi.mock('@/ui/stores/fabric.store', () => ({
    useFabricStore: { getState: () => harness.stores.fabric }
}))
vi.mock('@/ui/stores/diagnostic-mpi-reconcile.store', () => ({
    useDiagnosticMPIReconcileStore: { getState: () => harness.stores.diagnostic }
}))
vi.mock('@/ui/stores/diagnostic-mpi.store', () => ({
    useDiagnosticMPIStore: { getState: () => harness.stores.diagnosticMPI }
}))

import { connectAndInitialize } from '@/ui/stores/init'

describe('diagnostic cleanup ownership across service generations', () => {
    beforeEach(() => {
        harness.statusCallback = null
        for (const store of Object.values(harness.stores)) {
            for (const method of Object.values(store)) method.mockClear()
        }
        vi.stubGlobal('window', {
            pairApi: {
                app: { getInitial: vi.fn().mockResolvedValue({}) },
                cluster: { getInitial: vi.fn().mockResolvedValue({}) },
                engines: {
                    getInitialState: vi.fn().mockResolvedValue({
                        statuses: [],
                        models: [],
                        activeProgress: [],
                        updateAvailable: []
                    })
                },
                connection: { onStateRequestRefresh: vi.fn(() => () => undefined) }
            }
        })
    })

    afterEach(() => vi.unstubAllGlobals())

    it('invalidates a pending reconcile as soon as the service reconnects', async () => {
        await connectAndInitialize()
        expect(harness.stores.diagnostic.reset).toHaveBeenCalledTimes(1)
        expect(harness.stores.diagnosticMPI.reset).toHaveBeenCalledTimes(1)

        harness.statusCallback?.(
            { status: { connectorStatus: 'connected' } },
            { status: { connectorStatus: 'disconnected' } }
        )

        expect(harness.stores.diagnostic.reset).toHaveBeenCalledTimes(2)
        expect(harness.stores.diagnosticMPI.reset).toHaveBeenCalledTimes(2)
        await vi.waitFor(() => expect(harness.stores.workloads.refresh).toHaveBeenCalledOnce())
        expect(harness.stores.diagnosticMPI.refreshInventory).toHaveBeenCalledOnce()
    })
})
