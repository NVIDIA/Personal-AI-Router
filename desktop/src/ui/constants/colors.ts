// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { BadgeColor } from '@/ui/types/types'

// Color mapping for CSS
export const WORKLOAD_COLOR_MAP: Record<BadgeColor, string> = {
    yellow: '#F5A623',
    blue: '#4A90E2',
    green: '#7ED321',
    gray: '#9B9B9B',
    red: '#D0021B',
    teal: '#50E3C2',
    purple: '#BD10E0'
} as const

/**
 * Chart color palette for metrics visualization
 */
export const CHART_COLORS = {
    // GPU Utilization - NVIDIA brand green
    // GPU1: '#10b1fb',
    // VRAM1: '#1dbba4',
    // GPU2: '#f9c500',
    // VRAM2: '#df6500',
    // GPU3: '#0074df',
    // VRAM3: '#0d8473',
    // GPU4: '#ff8181',
    // VRAM4: '#c21e1e',
    // CPU Utilization - Google blue
    // CPU: '#1A73E8',
    CPU: '#c359ef',
    // VRAM Usage - Google yellow
    // VRAM: '#F9AB00',
    // Memory Usage - Purple
    // MEMORY: '#9C27B0',
    MEMORY: '#b62475'
    // Temperature - Google red
    // TEMPERATURE: '#EA4335'
} as const

/**
 * Color palettes for cycling through multiple GPUs
 */
export const GPU_COLOR_PALETTE = [
    '#0074df', // GPU3 - Dark Blue
    '#f9b400', // GPU2 - Yellow
    '#8689ff', // GPU1 - Blue
    '#ff8181' // GPU4 - Red
] as const

export const VRAM_COLOR_PALETTE = [
    '#0d8473', // VRAM3 - Dark Teal
    '#df6500', // VRAM2 - Orange
    '#6327ff', // VRAM1 - Teal
    '#c21e1e' // VRAM4 - Dark Red
] as const
