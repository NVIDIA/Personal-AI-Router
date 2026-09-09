// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { WorkloadStats } from '@/shared/types/workloads'

/**
 * Summarize a job's inference statistics for the job card as short phrases the
 * card joins with separators, e.g. `["42.3 tok/s", "512 tokens", "0.8 s to first
 * token"]`. Counts the proxy only estimated (from stream chunks) are prefixed
 * with `~`. Empty when nothing worth showing was measured.
 */
export function formatWorkloadStats(stats: WorkloadStats | undefined): string[] {
    if (!stats) return []
    const approx = stats.estimated ? '~' : ''
    const parts: string[] = []
    if (stats.tokensPerSecond) {
        parts.push(`${approx}${formatRate(stats.tokensPerSecond)} tok/s`)
    }
    if (stats.completionTokens) {
        parts.push(`${approx}${stats.completionTokens.toLocaleString()} tokens`)
    } else if (stats.promptTokens) {
        // An embeddings request has a prompt but generates nothing.
        parts.push(`${stats.promptTokens.toLocaleString()} prompt tokens`)
    }
    if (stats.ttftMs) {
        parts.push(`${formatDuration(stats.ttftMs)} to first token`)
    }
    return parts
}

function formatRate(tokensPerSecond: number): string {
    return tokensPerSecond >= 100
        ? Math.round(tokensPerSecond).toString()
        : tokensPerSecond.toFixed(1)
}

function formatDuration(ms: number): string {
    return ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(1)} s`
}
