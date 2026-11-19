// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload } from '@/shared/types/workloads'

/**
 * The verb before a job card's execution node. A job that ends before it
 * starts keeps the node it was sent to, so whether it ran is decided by
 * `startedAt`, not by its state.
 */
export function workloadNodeLabel(workload: Pick<Workload, 'state' | 'startedAt'>): string {
    if (workload.state === 'running') return 'Running on'
    return workload.startedAt === null ? 'Sent to' : 'Ran on'
}
