// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { NodeItem } from '@/shared/types/nodes'
import type { NodeMetricsHistory } from '@/ui/stores/metrics.store'
import { hasInferenceReadyGpu } from '@/ui/utils/gpu-inference'

/**
 * Node chart / activity ring rendering gate.
 *
 * Suppresses the initial chart flash where the CPU fallback renders for a
 * frame before the GPU rings take over. A node is considered "ready to chart"
 * once we've received our first metrics update **and** — if it has an
 * inference-ready GPU — the metrics stream has at least one GPU entry. Before
 * then, callers should render a same-sized placeholder so layout doesn't shift.
 *
 * Gated on `hasInferenceReadyGpu`, not raw GPU count, so a node whose only GPU
 * the backend marks not-inference-ready (and therefore renders the CPU fallback
 * as its final state) does not needlessly wait for a GPU metric it will never
 * display. See gpu-inference.ts.
 */
export function hasFirstMetrics(node: NodeItem, metrics?: NodeMetricsHistory): boolean {
    if (!metrics) return false
    if (hasInferenceReadyGpu(node.topology) && metrics.gpuUtilization.length === 0) return false
    return true
}
