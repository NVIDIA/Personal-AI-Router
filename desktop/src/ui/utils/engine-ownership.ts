// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineProcessStatus, EngineType } from '@/shared/types/engines'

/**
 * A llama.cpp runtime PAIR detected but does not manage is observe-only: its
 * owner keeps lifecycle, settings and model changes, so no control that would
 * mutate it is offered on any surface. `managed` is the engine manager's own
 * fact about the running instance; an engine that is not installed has nothing
 * to observe and stays installable.
 */
export function isExternalRuntime(
    engineType: EngineType,
    processStatus: EngineProcessStatus,
    managed: boolean | undefined
): boolean {
    return engineType === 'llamacpp' && processStatus !== 'not-installed' && managed !== true
}
