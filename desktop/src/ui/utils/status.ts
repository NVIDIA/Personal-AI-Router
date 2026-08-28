// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineProcessStatus } from '@/shared/types/engines'

export const statusLabel: Record<EngineProcessStatus, string> = {
    running: 'Running',
    stopped: 'Stopped',
    'not-installed': 'Not Installed',
    installing: 'Installing...',
    uninstalling: 'Uninstalling...',
    starting: 'Starting...',
    stopping: 'Stopping...',
    initializing: 'Initializing...'
}
