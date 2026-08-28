// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ModelItem } from '@/ui/types/engine-info'
import type { ModelExpiry } from '@/shared/types/engines'
import type { EngineProgress } from '@/shared/types/engines'
import type { EngineCaps } from '@/ui/types/engine-manifest'
import type { EngineCommandType } from '@/shared/types/engine-api'

/** Matches {@link useDropdownClickable} `get()` return shape for menu wiring. */
export type ModelRowDropdownControl = {
    onOpenChange: (open: boolean) => void
    dropDownContentClassName: string
}

export interface ModelRowProps {
    model: ModelItem
    isRunning: boolean
    capabilities: EngineCaps
    progress?: EngineProgress
    /**
     * Optimistic in-flight action for this model (load/eject/delete/pull) fired
     * from the UI before the backend acknowledged it. When set, the action menu
     * is disabled and a "working" spinner shows. Cleared by the pending-actions
     * store on the superseding push / timeout.
     */
    pendingAction?: EngineCommandType
    displayName: (name: string) => string
    onAction: (modelName: string, action: string) => void
    onExpiryChange: (modelName: string, value: ModelExpiry) => void
}
