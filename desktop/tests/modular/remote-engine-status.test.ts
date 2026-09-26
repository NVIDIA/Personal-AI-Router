// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

describe('remote engine status', () => {
    const releaseVllmGroupGate = () =>
        getModularBridgeState().setVllmGroupStatus({
            activationEnabled: false,
            reserved: false,
            reason: ''
        })

    it('allows atomic update only for locally managed vLLM and keeps its version', () => {
        const state = getModularBridgeState()
        const localNodeId = 'local-vllm-update-authority'
        state.setSelfId(localNodeId)
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: false,
            healthy: false,
            enabled: false,
            managed: true,
            adopted: false,
            install_supported: true,
            version: '0.29.0',
            port: 8001
        })
        releaseVllmGroupGate()

        expect(state.isEngineCommandAllowed(localNodeId, 'vllm', 'update')).toBe(true)
        expect(
            state
                .getEngineInitialState()
                .statuses.find(
                    status => status.nodeId === localNodeId && status.engineType === 'vllm'
                )?.installedVersion
        ).toBe('0.29.0')
    })

    it('prefers authoritative stopped facts over proxy presence', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-engine-status-remote'

        state.setSelfId('remote-engine-status-local')
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: remoteNodeId,
                host: remoteNodeId,
                port: 1234,
                addresses: ['192.0.2.190'],
                ip: '192.0.2.190'
            }
        })
        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(true)

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'lmstudio',
                    installed: true,
                    running: false,
                    healthy: false,
                    port: 1235
                }
            ]
        })
        releaseVllmGroupGate()

        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(false)
        expect(
            state
                .getEngineInitialState()
                .statuses.find(
                    status => status.nodeId === remoteNodeId && status.engineType === 'lm-studio'
                )
        ).toMatchObject({
            processStatus: 'stopped',
            enginePort: 1235,
            // The proxy remains discoverable even though the engine behind it is stopped.
            proxyPort: 1234
        })
    })

    it('keeps vLLM disabled until a peer reports explicit enabled and managed state', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-vllm-authority'
        state.setSelfId('remote-vllm-local')
        releaseVllmGroupGate()
        state.handleNotification({
            source: 'vllm-proxy',
            method: 'node/discovered',
            params: {
                id: remoteNodeId,
                host: remoteNodeId,
                port: 8000,
                addresses: ['192.0.2.191'],
                ip: '192.0.2.191'
            }
        })

        expect(state.isRemoteEngineEnabled(remoteNodeId, 'vllm')).toBe(false)

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'vllm',
                    installed: false,
                    running: false,
                    healthy: false,
                    managed: false,
                    adopted: false,
                    install_supported: true,
                    port: 8001
                }
            ]
        })
        expect(state.isRemoteEngineEnabled(remoteNodeId, 'vllm')).toBe(false)
        expect(state.isEngineManaged(remoteNodeId, 'vllm')).toBe(false)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'install')).toBe(true)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'update')).toBe(false)
        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'vllm',
                    installed: true,
                    running: true,
                    healthy: true,
                    enabled: true,
                    managed: true,
                    adopted: false,
                    install_supported: true,
                    version: '0.29.0',
                    port: 8001
                }
            ]
        })
        expect(state.isRemoteEngineEnabled(remoteNodeId, 'vllm')).toBe(true)
        expect(state.isEngineManaged(remoteNodeId, 'vllm')).toBe(true)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'install')).toBe(false)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'toggle')).toBe(true)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'update')).toBe(true)
        expect(
            state
                .getEngineInitialState()
                .statuses.find(
                    status => status.nodeId === remoteNodeId && status.engineType === 'vllm'
                )?.installedVersion
        ).toBe('0.29.0')
    })

    it('propagates exact local and remote serving-group execution identity', () => {
        const state = getModularBridgeState()
        const localNodeId = 'serving-group-route-local'
        const remoteNodeId = 'serving-group-route-remote'
        const runId = 'a'.repeat(32)
        const servingGroup = {
            runId,
            generation: 23,
            model: 'example/model@revision',
            coordinator: localNodeId,
            role: 'coordinator',
            state: 'ready',
            members: [localNodeId, remoteNodeId]
        }
        state.setSelfId(localNodeId)
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: remoteNodeId,
                host: remoteNodeId,
                port: 1234,
                addresses: ['192.0.2.192'],
                ip: '192.0.2.192'
            }
        })
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: true,
            healthy: true,
            enabled: false,
            managed: true,
            adopted: false,
            install_supported: true,
            version: '0.29.0',
            port: 8001,
            serving_group: servingGroup
        })
        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'vllm',
                    installed: true,
                    running: true,
                    healthy: false,
                    enabled: false,
                    managed: true,
                    adopted: false,
                    install_supported: true,
                    version: '0.29.0',
                    port: 8001,
                    serving_group: { ...servingGroup, role: 'participant' }
                }
            ]
        })

        const statuses = state.getEngineInitialState().statuses
        expect(
            statuses.find(status => status.nodeId === localNodeId && status.engineType === 'vllm')
                ?.servingGroup
        ).toEqual(servingGroup)
        expect(
            statuses.find(status => status.nodeId === remoteNodeId && status.engineType === 'vllm')
                ?.servingGroup
        ).toEqual({ ...servingGroup, role: 'participant' })
    })

    it('holds every vLLM mutation while group ownership is unknown or retained', () => {
        const state = getModularBridgeState()
        const nodeId = 'vllm-group-gate-local'
        state.setSelfId(nodeId)
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: false,
            healthy: false,
            enabled: false,
            managed: true,
            adopted: false,
            install_supported: true,
            version: '0.28.0',
            port: 8001
        })
        state.holdVllmGroupStatus()
        expect(state.isEngineCommandAllowed(nodeId, 'vllm', 'update')).toBe(false)
        state.setVllmGroupStatus({
            activationEnabled: false,
            reserved: true,
            reason: 'fresh native admission required'
        })
        expect(state.isEngineCommandAllowed(nodeId, 'vllm', 'toggle')).toBe(false)
        releaseVllmGroupGate()
        expect(state.isEngineCommandAllowed(nodeId, 'vllm', 'update')).toBe(true)
    })

    it('does not let an unsupported local host over-hold an unrelated remote Linux target', () => {
        const state = getModularBridgeState()
        const localNodeId = 'windows-controller'
        const remoteNodeId = 'remote-linux-vllm'
        state.setSelfId(localNodeId)
        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'vllm',
                    installed: true,
                    running: false,
                    healthy: false,
                    enabled: false,
                    managed: true,
                    adopted: false,
                    install_supported: true,
                    port: 8001
                }
            ]
        })
        state.holdVllmGroupStatus()
        expect(state.isEngineCommandAllowed(localNodeId, 'vllm', 'toggle')).toBe(false)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'toggle')).toBe(true)

        const digest = 'a'.repeat(64)
        state.setVllmGroupStatus({
            activationEnabled: false,
            reserved: true,
            reason: 'cleanup required',
            run: {
                runId: 'facfa61ad839f1108ea0972f3f389b3c',
                generation: 13,
                planDigest: digest,
                plan: {
                    coordinator: remoteNodeId,
                    model: 'example/model',
                    runtime: '0.28.0',
                    topology: {
                        tensorParallel: 1,
                        pipelineParallel: 1,
                        dataParallel: 1,
                        configSha256: digest
                    },
                    members: [
                        {
                            nodeId: remoteNodeId,
                            pinSha256: digest,
                            gpuUuid: 'GPU-remote',
                            modelDigest: digest,
                            runtimeDigest: digest,
                            runtimeCompatibilitySha256: digest
                        }
                    ]
                },
                state: 'cleanup-required',
                ranks: [
                    {
                        nodeId: remoteNodeId,
                        attempted: true,
                        started: false,
                        cleanupConfirmed: false
                    }
                ],
                cleanupConfirmed: false
            }
        })
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'toggle')).toBe(false)
        expect(state.isEngineCommandAllowed(remoteNodeId, 'vllm', 'update')).toBe(false)
    })
})
