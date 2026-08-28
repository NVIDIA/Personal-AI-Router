// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    DEMO_BASE_AGENTS_PER_COHORT,
    DEMO_COHORT_OFFSETS_SECONDS,
    DEMO_MAX_SUBMIT_SECONDS,
    DEMO_STAGES,
    type DemoTarget
} from '@/shared/types/inference-demo'

/**
 * Pure planning half of the Inference Demo.
 *
 * Kept free of Electron and child-process concerns so the schedule's hard
 * guarantees — nothing at or after 60s, every target touched, open-loop
 * overlap — can be asserted directly in unit tests.
 */

export interface ScheduledRequest {
    /** Milliseconds after the schedule starts. */
    atMs: number
    target: DemoTarget
    /** Index into `DEMO_STAGES`. */
    stageIndex: number
}

/**
 * Enough simulated agents per cohort that the schedule has at least one request
 * per target.
 *
 * A run produces `cohorts × agents × stages` requests — 5 × 2 × 6 = 60 at the
 * base agent count. Targets are assigned per request, so the base count already
 * covers up to 60 targets; beyond that we add agents, which is exactly the
 * ">60 targets" escalation the spec calls for.
 */
export function agentsPerCohort(targetCount: number): number {
    const capacityPerAgent = DEMO_STAGES.length
    const cohorts = DEMO_COHORT_OFFSETS_SECONDS.length
    const needed = Math.ceil(targetCount / (cohorts * capacityPerAgent))
    return Math.max(DEMO_BASE_AGENTS_PER_COHORT, needed)
}

/**
 * Build the full submission plan up front.
 *
 * Slots are generated per (cohort, agent, stage), sorted by submission time, and
 * only then assigned targets round-robin. Assigning in time order is what gives
 * the spec's "prioritize targets that have not yet received a request" property
 * for free: the first pass through the ring touches every target before any
 * target is revisited.
 *
 * This deliberately does not reproduce `resolve_routes()` from the reference
 * script, which would funnel all work through a single smallest/largest model
 * pair per backend.
 *
 * The schedule is open-loop: offsets are absolute wall-clock positions and no
 * slot waits on an earlier request completing.
 */
export function buildDemoSchedule(targets: DemoTarget[]): ScheduledRequest[] {
    if (targets.length === 0) return []

    const perCohort = agentsPerCohort(targets.length)
    const slots: { atMs: number; stageIndex: number }[] = []

    for (const cohortOffset of DEMO_COHORT_OFFSETS_SECONDS) {
        for (let agent = 0; agent < perCohort; agent++) {
            DEMO_STAGES.forEach((stage, stageIndex) => {
                const at = cohortOffset + stage.offsetSeconds
                // Hard ceiling: nothing is planned at or after the limit.
                if (at >= DEMO_MAX_SUBMIT_SECONDS) return
                slots.push({ atMs: at * 1000, stageIndex })
            })
        }
    }

    slots.sort((a, b) => a.atMs - b.atMs)

    // flatMap rather than map: `targets[…]` is `DemoTarget | undefined` under
    // noUncheckedIndexedAccess even though the modulo keeps it in range. Dropping
    // an undefined slot is the honest narrowing; a run with one request fewer
    // beats asserting a shape the type system cannot confirm.
    return slots.flatMap((slot, index) => {
        const target = targets[index % targets.length]
        return target ? [{ atMs: slot.atMs, stageIndex: slot.stageIndex, target }] : []
    })
}
