// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { engineUpdateTargetAllowed } from '@/ui/utils/engine-control-policy'
import type { BackendInfo } from '@/ui/types/engine-info'

function backend(type: BackendInfo['type'], managed?: boolean): BackendInfo {
    return {
        type,
        displayName: type,
        source: 'detected',
        processStatus: 'stopped',
        port: null,
        proxyPort: null,
        models: [],
        managed
    }
}

describe('backend update control targeting', () => {
    it('enables a remote update only for explicitly managed vLLM', () => {
        expect(engineUpdateTargetAllowed(backend('vllm', true), false)).toBe(true)
        expect(engineUpdateTargetAllowed(backend('vllm', false), false)).toBe(false)
        expect(engineUpdateTargetAllowed(backend('vllm'), false)).toBe(false)
        expect(engineUpdateTargetAllowed(backend('lm-studio', true), false)).toBe(false)
    })

    it('preserves local update targeting', () => {
        expect(engineUpdateTargetAllowed(backend('vllm', true), true)).toBe(true)
        expect(engineUpdateTargetAllowed(backend('ollama'), true)).toBe(true)
    })
})
