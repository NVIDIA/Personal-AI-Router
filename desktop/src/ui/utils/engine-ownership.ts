// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineProcessStatus, EngineType } from '@/shared/types/engines'

/**
 * A llama.cpp runtime PAIR detected but does not manage is observe-only: its
 * owner keeps lifecycle, settings and model changes, so no control that would
 * mutate it is offered on any surface.
 *
 * `managed` is the engine manager's own fact about the installed instance and
 * it always reports one as a boolean (installed, not adopted, on the managed
 * install path). An absent flag therefore means no fact has arrived yet — a
 * placeholder, a discovery-only peer, or a pending operation on a node with no
 * facts — and is not evidence of an external runtime. Nor is a transitional
 * status: `installing`, `uninstalling`, `starting` and `stopping` are
 * operations PAIR itself is performing, and `not-installed` has nothing to
 * observe. Only a settled `running` or `stopped` runtime that the manager
 * reports as unmanaged is external.
 */
export function isExternalRuntime(
    engineType: EngineType,
    processStatus: EngineProcessStatus,
    managed: boolean | undefined
): boolean {
    return (
        engineType === 'llamacpp' &&
        managed === false &&
        (processStatus === 'running' || processStatus === 'stopped')
    )
}
