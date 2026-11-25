// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineProcessStatus } from '@/shared/types/engines'

/** Preserve vendor build labels such as llama.cpp's b12345. */
export function displayEngineVersion(version: string): string {
    return /^\d+\./.test(version) ? `v${version}` : version
}

export const statusLabel: Record<EngineProcessStatus, string> = {
    running: 'Running',
    'awaiting-model': 'Select a model',
    stopped: 'Stopped',
    'not-installed': 'Not Installed',
    installing: 'Installing...',
    uninstalling: 'Uninstalling...',
    starting: 'Starting...',
    stopping: 'Stopping...',
    initializing: 'Initializing...',
    unsupported: 'Not Supported'
}
