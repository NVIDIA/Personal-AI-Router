// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { agentsPerCohort, buildDemoSchedule } from '@/electron/inference-demo-schedule'
import {
    DEMO_COHORT_OFFSETS_SECONDS,
    DEMO_MAX_SUBMIT_SECONDS,
    DEMO_STAGES,
    type DemoTarget
} from '@/shared/types/inference-demo'

// Ports are the proxy facades PAIR routes through, never an engine's own
// backend port. At runtime these come from the broker's proxy registry.
function targets(count: number): DemoTarget[] {
    return Array.from({ length: count }, (_, index) => ({
        backend: index % 2 === 0 ? ('ollama' as const) : ('lmstudio' as const),
        port: index % 2 === 0 ? 11434 : 1234,
        model: `model-${index}`
    }))
}

function key(target: DemoTarget): string {
    return `${target.backend}:${target.port}:${target.model}`
}

describe('inference demo schedule', () => {
    it('submits nothing at or after the 60 second ceiling', () => {
        for (const count of [1, 2, 7, 60, 61, 250]) {
            const schedule = buildDemoSchedule(targets(count))
            const latest = Math.max(...schedule.map(request => request.atMs))
            expect(latest).toBeLessThan(DEMO_MAX_SUBMIT_SECONDS * 1000)
        }
    })

    it('places the final submission at 58 seconds', () => {
        const schedule = buildDemoSchedule(targets(4))
        const latest = Math.max(...schedule.map(request => request.atMs))
        expect(latest).toBe(58_000)
    })

    it('produces 60 requests for a standard run', () => {
        // 5 cohorts x 2 agents x 6 stages, matching the packaged steady profile
        // minus its 50 second cohort.
        expect(buildDemoSchedule(targets(4))).toHaveLength(60)
    })

    it('attempts every engine/model target at least once', () => {
        for (const count of [1, 3, 17, 59, 60, 61, 137]) {
            const all = targets(count)
            const schedule = buildDemoSchedule(all)
            const touched = new Set(schedule.map(request => key(request.target)))
            expect(touched.size).toBe(count)
        }
    })

    it('touches every target before revisiting any of them', () => {
        const all = targets(23)
        const schedule = buildDemoSchedule(all)
        const firstPass = schedule.slice(0, all.length).map(request => key(request.target))
        expect(new Set(firstPass).size).toBe(all.length)
    })

    it('only adds agents once targets exceed the 60 request base capacity', () => {
        expect(agentsPerCohort(1)).toBe(2)
        expect(agentsPerCohort(60)).toBe(2)
        expect(agentsPerCohort(61)).toBe(3)
        expect(agentsPerCohort(180)).toBe(6)
    })

    it('overlaps waves rather than waiting on earlier requests', () => {
        const schedule = buildDemoSchedule(targets(4))
        // The 10s cohort's intake stage lands while the 0s cohort still has
        // stages pending at 10s, 14s and 18s — this is the open-loop property.
        const atTenSeconds = schedule.filter(request => request.atMs === 10_000)
        expect(atTenSeconds.length).toBeGreaterThan(1)
        expect(schedule.some(request => request.atMs > 10_000)).toBe(true)
    })

    it('returns an empty schedule when no engine exposed a model', () => {
        expect(buildDemoSchedule([])).toEqual([])
    })

    it('keeps every stage index within the stage table', () => {
        for (const request of buildDemoSchedule(targets(5))) {
            expect(DEMO_STAGES[request.stageIndex]).toBeDefined()
        }
    })

    it('starts a cohort at each defined offset', () => {
        const schedule = buildDemoSchedule(targets(4))
        for (const offset of DEMO_COHORT_OFFSETS_SECONDS) {
            expect(schedule.some(request => request.atMs === offset * 1000)).toBe(true)
        }
    })
})
