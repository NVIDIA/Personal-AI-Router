// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import {
    getInferenceDemoState,
    startInferenceDemo,
    stopInferenceDemo
} from '@/electron/inference-demo'

export function registerInferenceDemoIpc(): void {
    safeHandle('demo:get-state', () => getInferenceDemoState())
    safeHandle('demo:start', () => startInferenceDemo())
    safeHandle('demo:stop', () => stopInferenceDemo())
}
