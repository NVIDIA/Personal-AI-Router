// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { GpuInfo, SystemTopology } from '@/shared/types/hardware'

/**
 * Inference-ready GPU selection for the node charts.
 *
 * PAIR UI does NO client-side GPU classification. The backend tells us which
 * hardware is inference-ready via the node-level
 * `SystemTopology.inferenceHardwareIds` list (see the routing limitations in
 * docs/services-parity.md):
 *
 *  - list present -> a GPU is shown iff its id is in the list.
 *  - list absent  -> show every GPU (the backend does not report readiness yet).
 *
 * When no GPU qualifies, callers fall back to CPU + memory rings.
 */
export function isGpuInferenceReady(
    gpu: GpuInfo,
    inferenceHardwareIds: string[] | undefined
): boolean {
    return inferenceHardwareIds ? inferenceHardwareIds.includes(gpu.id) : true
}

/** Whether the node has at least one inference-ready GPU to chart. */
export function hasInferenceReadyGpu(topology: SystemTopology): boolean {
    return topology.gpus.some(gpu => isGpuInferenceReady(gpu, topology.inferenceHardwareIds))
}
