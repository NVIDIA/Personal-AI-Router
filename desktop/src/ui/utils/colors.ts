// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadState } from '@/shared/types/workloads'
import { BadgeColor } from '@/ui/types/types'
import { GPU_COLOR_PALETTE, VRAM_COLOR_PALETTE, WORKLOAD_COLOR_MAP } from '@/ui/constants/colors'

export function getWorkloadStateColor(state: WorkloadState): BadgeColor {
    switch (state) {
        case 'initializing':
            return 'gray'
        case 'queued':
            return 'yellow'
        case 'running':
            return 'blue'
        case 'completed':
            return 'gray'
        case 'failed':
            return 'red'
        default:
            return 'gray'
    }
}

export function getWorkloadColorBar(state: WorkloadState): string {
    switch (state) {
        case 'initializing':
            return WORKLOAD_COLOR_MAP.gray
        case 'queued':
            return WORKLOAD_COLOR_MAP.yellow
        case 'running':
            return WORKLOAD_COLOR_MAP.blue
        case 'completed':
            return WORKLOAD_COLOR_MAP.green
        case 'failed':
            return WORKLOAD_COLOR_MAP.red
        default:
            return WORKLOAD_COLOR_MAP.gray
    }
}

/**
 * Get GPU color by cycling through the color palette
 * Uses modulo to cycle when there are more GPUs than colors
 */
export function getGpuColor(index: number): string {
    return GPU_COLOR_PALETTE[index % GPU_COLOR_PALETTE.length]
}

/**
 * Get VRAM color by cycling through the color palette
 * Uses modulo to cycle when there are more GPUs than colors
 */
export function getVramColor(index: number): string {
    return VRAM_COLOR_PALETTE[index % VRAM_COLOR_PALETTE.length]
}
