// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import { completeFirstRun, isFirstRun } from '@/electron/config/ui-config'

export function registerSettingsIpc(): void {
    safeHandle('settings:is-first-run', () => isFirstRun())
    safeHandle('settings:complete-first-run', () => completeFirstRun())
}
