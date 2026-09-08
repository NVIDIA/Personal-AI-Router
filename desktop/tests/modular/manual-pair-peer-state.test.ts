// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

// A peer added by address on a network that discovers nothing arrives here as an
// ordinary directory node: the broker synthesizes the record the scanner would
// have produced. These tests pin that the renderer's view of it is the view of a
// discovered peer, with no special case anywhere — same card, same engines, same
// models, same telemetry — and that its address surviving as a host name rather
// than a literal changes none of it.
describe('a manually added PAIR peer in the bridge state', () => {
    it('renders with the card, engines, models and telemetry a discovered peer gets', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-tailnet-peer',
                        name: 'gpu-box.tail1234.ts.net',
                        // A MagicDNS name, not a literal. Nothing downstream may
                        // require an IP: on a tailnet the name is what survives a
                        // node changing address.
                        ipAddress: 'gpu-box.tail1234.ts.net',
                        port: 14318,
                        trusted: true,
                        clustered: true,
                        models: ['llama3.2:latest', 'qwen2.5-7b-instruct'],
                        modelsByEngine: {
                            ollama: ['llama3.2:latest'],
                            lmstudio: ['qwen2.5-7b-instruct']
                        },
                        loadedByEngine: { ollama: ['llama3.2:latest'] }
                    }
                ]
            }
        })

        // The card: keyed by the peer's stable identity, named and addressed by
        // what the operator typed.
        const { nodes } = state.getNodesInitial()
        const card = nodes['uuid-tailnet-peer']
        expect(card).toBeDefined()
        expect(card.name).toBe('gpu-box.tail1234.ts.net')
        expect(card.ipAddress).toBe('gpu-box.tail1234.ts.net')

        // Trust and membership, which is what distinguishes a paired peer from a
        // stranger anywhere it is shown.
        const available = state.getAvailableNodes().find(node => node.id === 'uuid-tailnet-peer')
        expect(available).toMatchObject({ trusted: true, clustered: true })
        expect(available?.ipAddress).toBe('gpu-box.tail1234.ts.net')

        // Models, attributed to the engine that serves each one.
        const engineState = state.getEngineInitialState()
        const ollama = engineState.models.find(
            entry => entry.nodeId === 'uuid-tailnet-peer' && entry.engineType === 'ollama'
        )
        const lmStudio = engineState.models.find(
            entry => entry.nodeId === 'uuid-tailnet-peer' && entry.engineType === 'lm-studio'
        )
        expect(ollama?.models.map(model => model.name)).toEqual(['llama3.2:latest'])
        expect(lmStudio?.models.map(model => model.name)).toEqual(['qwen2.5-7b-instruct'])
        // Loaded state travels too, so a remote card can show what is resident.
        expect(ollama?.models[0]?.status).toBe('loaded')

        // Telemetry: the peer is polled for its hardware at the address it was
        // added by, on its node-info port, exactly as a discovered peer is.
        const target = state
            .getNodeInfoPollTargets()
            .find(entry => entry.id === 'uuid-tailnet-peer')
        expect(target).toBeDefined()
        expect(target?.hosts).toContain('gpu-box.tail1234.ts.net')
        expect(target?.port).toBe(14318)
        expect(card.status).toBe('active')

        // And what comes back lands on the card.
        state.mergeNodeInfoResponse('uuid-tailnet-peer', {
            hostUuid: 'uuid-tailnet-peer',
            GPUs: [
                {
                    name: 'NVIDIA GeForce RTX 4090',
                    vram_bytes: 25_769_803_776,
                    vram_used_bytes: 8_589_934_592,
                    utilization_percent: 37
                }
            ],
            cpu: { name: 'AMD Ryzen 9 5900X', cores: 12, utilization_percent: 9 },
            memory: { total_bytes: 68_719_476_736, used_bytes: 17_179_869_184 }
        })
        const withHardware = state.getNodesInitial().nodes['uuid-tailnet-peer']
        expect(withHardware.topology.cpu.model).toBe('AMD Ryzen 9 5900X')
        expect(withHardware.topology.cpu.cores).toBe(12)
        expect(withHardware.topology.gpus.map(gpu => gpu.name)).toEqual(['NVIDIA GeForce RTX 4090'])
        expect(withHardware.topology.gpus[0].vramTotal).toBe(25_769_803_776)
        expect(withHardware.topology.ram).toBe(68_719_476_736)
    })

    it('keeps a host-name address rather than blanking it', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-name-only',
                        name: 'name-only.tail1234.ts.net',
                        ipAddress: 'name-only.tail1234.ts.net',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        const card = state.getNodesInitial().nodes['uuid-name-only']
        expect(card.ipAddress).toBe('name-only.tail1234.ts.net')
        // Nothing collapses the address to an empty string on the way through: an
        // empty address is what a node with nowhere to be dialed looks like.
        expect(card.ipAddress).not.toBe('')
    })
})
