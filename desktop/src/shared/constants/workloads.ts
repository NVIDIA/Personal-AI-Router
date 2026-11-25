// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export const WorkloadStates = ['queued', 'running', 'completed', 'failed', 'cancelled'] as const

/**
 * What a job's row says when no engine on this machine reports having run it.
 *
 * `cluster` is the class the cluster router stamps: the engine that answered may
 * live on another machine, so the row names the routing rather than pretending
 * to name an engine. `unknown` is a name no engine manager in this build uses —
 * a contract drift worth seeing, and the job still ran.
 */
export const WorkloadEngineClasses = ['cluster', 'unknown'] as const

export const WorkloadEngineClassLabels: Record<(typeof WorkloadEngineClasses)[number], string> = {
    cluster: 'Cluster',
    unknown: 'Unknown engine'
}
