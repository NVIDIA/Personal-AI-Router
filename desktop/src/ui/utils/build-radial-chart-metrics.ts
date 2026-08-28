// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { NodeItem } from '@/shared/types/nodes'
import type { RadialMetric } from '@/ui/components/GpuRadialChartSvg'
import type { NodeMetricsHistory } from '@/ui/stores/metrics.store'
import { CHART_COLORS } from '@/ui/constants/colors'
import { getGpuColor, getVramColor } from '@/ui/utils/colors'
import { hasInferenceReadyGpu, isGpuInferenceReady } from '@/ui/utils/gpu-inference'

function getLatest(data: { timestamp: number; value: number }[]): number {
    return data.length === 0 ? 0 : (data[data.length - 1].value ?? 0)
}

export function buildRadialChartMetrics(
    topology: NodeItem['topology'],
    nodeMetrics: NodeMetricsHistory | undefined,
    // Show only the GPUs the backend marks inference-ready (or all GPUs when it
    // reports no readiness list); see gpu-inference.ts. A node with no
    // inference-ready GPU falls through to the CPU + RAM rings below.
    hasGpu = hasInferenceReadyGpu(topology)
): RadialMetric[] {
    if (!hasGpu) {
        return [
            {
                value: nodeMetrics ? getLatest(nodeMetrics.cpuUtilization) : 0,
                color: CHART_COLORS.CPU,
                shown: true
            },
            {
                value: nodeMetrics ? getLatest(nodeMetrics.memoryUsage) : 0,
                color: CHART_COLORS.MEMORY,
                shown: true
            }
        ]
    }

    // Keep the original GPU index (metrics arrays and colors are keyed
    // positionally to topology.gpus) but omit the rings for any GPU the backend
    // did not mark inference-ready (when it sends a readiness list).
    return topology.gpus.flatMap((gpu, gpuIdx) => {
        if (!isGpuInferenceReady(gpu, topology.inferenceHardwareIds)) return []
        const gpuUtil = nodeMetrics?.gpuUtilization[gpuIdx]
        const gpuVram = nodeMetrics?.gpuVramUsage[gpuIdx]
        return [
            {
                value: gpuUtil ? getLatest(gpuUtil.data) : 0,
                color: getGpuColor(gpuIdx),
                shown: true
            },
            {
                value: gpuVram ? getLatest(gpuVram.data) : 0,
                color: getVramColor(gpuIdx),
                shown: true
            }
        ]
    })
}
