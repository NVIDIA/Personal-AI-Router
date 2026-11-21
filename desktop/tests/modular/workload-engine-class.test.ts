// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import {
    getModularBridgeState,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'
import { workloadEngineLabel } from '@/shared/utils/workloads'

const LOCAL_NODE = 'uuid-routing-host'
const PEER_NODE = 'uuid-serving-host'

// The cluster router reports its own class name (`cluster`) rather than the
// engine that answered, because the engine that answered may live on another
// machine and the row must name the machine instead. These cases hold the two
// paths a job can take into the desktop — the `workloads:get-initial` baseline
// and a live `workloads:upsert` — open to that class and to an engine name this
// build has never seen.
describe('cluster-routed workload rows', () => {
    it('keeps a job the cluster router served from another machine', () => {
        const [workload] = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'job-7',
                    engine: 'cluster',
                    state: 'running',
                    model: 'qwen/qwen3.6-35b-a3b',
                    originatedFrom: LOCAL_NODE,
                    scheduledOn: PEER_NODE,
                    createdAt: 1_700_000_000_000,
                    startedAt: 1_700_000_005_000
                }
            ]
        })

        expect(workload).toMatchObject({
            id: 'job-7',
            engine: 'cluster',
            state: 'running',
            model: 'qwen/qwen3.6-35b-a3b',
            originatedFrom: LOCAL_NODE,
            scheduledOn: PEER_NODE
        })
    })

    it('keeps a cluster-routed job that the router has not placed on a node yet', () => {
        const [workload] = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'job-8',
                    engine: 'cluster',
                    state: 'queued',
                    model: 'qwen/qwen3.6-35b-a3b',
                    originatedFrom: LOCAL_NODE,
                    createdAt: 1_700_000_000_000
                }
            ]
        })

        // A queued job has no run-node, so the row carries no `scheduledOn`
        // rather than guessing the machine it entered the cluster on.
        expect(workload).toMatchObject({ engine: 'cluster', state: 'queued' })
        expect(workload?.scheduledOn).toBeUndefined()
    })

    it('keeps a cluster job and attributes it to the node from a live push', () => {
        const state = getModularBridgeState()

        state.upsertWorkloadFromInfo({
            workloadInfo: {
                id: 'job-9',
                engine: 'cluster',
                state: 'running',
                model: 'deepseek-ai/DeepSeek-R1',
                originatedFrom: LOCAL_NODE,
                scheduledOn: PEER_NODE,
                createdAt: 1_700_000_000_000,
                startedAt: 1_700_000_002_000
            }
        })

        expect(state.getWorkloads()[`${LOCAL_NODE}\u0000job-9`]).toMatchObject({
            engine: 'cluster',
            state: 'running',
            scheduledOn: PEER_NODE
        })
    })

    // A worker that invents an engine name is a contract bug. The job still ran,
    // so it stays in the list with a class that says so rather than vanishing.
    it('keeps a job from an engine this build does not recognize', () => {
        const [workload] = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'job-10',
                    engine: 'sglang',
                    state: 'running',
                    model: 'unseen-engine/model',
                    originatedFrom: LOCAL_NODE,
                    scheduledOn: LOCAL_NODE,
                    createdAt: 1_700_000_000_000
                }
            ]
        })

        expect(workload).toMatchObject({
            id: 'job-10',
            engine: 'unknown',
            model: 'unseen-engine/model'
        })
    })

    it('still maps each installed engine manager name to its engine', () => {
        const rows = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'job-ollama',
                    engine: 'ollama',
                    state: 'running',
                    originatedFrom: LOCAL_NODE
                },
                {
                    id: 'job-lmstudio',
                    engine: 'lmstudio',
                    state: 'running',
                    originatedFrom: LOCAL_NODE
                },
                {
                    id: 'job-llamacpp',
                    engine: 'llamacpp',
                    state: 'running',
                    originatedFrom: LOCAL_NODE
                },
                { id: 'job-vllm', engine: 'vllm', state: 'running', originatedFrom: LOCAL_NODE }
            ]
        })

        expect(rows.map(row => [row.id, row.engine])).toEqual([
            ['job-ollama', 'ollama'],
            ['job-lmstudio', 'lm-studio'],
            ['job-llamacpp', 'llama-cpp'],
            ['job-vllm', 'vllm']
        ])
    })

    it('drops a workload that names no engine at all', () => {
        expect(
            parseWorkloadsInitial({
                workloads: [{ id: 'job-11', state: 'running', originatedFrom: LOCAL_NODE }]
            })
        ).toEqual([])
    })
})

describe('workloadEngineLabel', () => {
    it('calls a job the cluster router served Cluster', () => {
        expect(workloadEngineLabel('cluster')).toBe('Cluster')
    })

    it('says an engine this build does not recognize is unknown', () => {
        expect(workloadEngineLabel('unknown')).toBe('Unknown engine')
    })

    it('names a job served by an installed engine after that engine', () => {
        expect(workloadEngineLabel('lm-studio')).toBe('LM Studio')
        expect(workloadEngineLabel('llama-cpp')).toBe('llama.cpp')
    })
})
