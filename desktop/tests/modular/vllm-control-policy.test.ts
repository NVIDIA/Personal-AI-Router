// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    engineEnabled,
    engineInstallAllowed,
    engineInstallUnavailableReason,
    engineLifecycleAllowed
} from '@/ui/utils/engine-control-policy'
import {
    canAutoInstallBackendForOs,
    canAutoInstallBackendForTarget
} from '@/ui/utils/backend-target-os'

describe('vLLM control policy', () => {
    it('fails closed when ownership and enabled state are absent', () => {
        const facts = { type: 'vllm' as const, processStatus: 'running' as const }
        expect(engineEnabled(facts)).toBe(false)
        expect(engineLifecycleAllowed(facts)).toBe(false)
        expect(engineInstallAllowed(facts)).toBe(false)
    })

    it('requires explicit managed facts for lifecycle controls', () => {
        const facts = {
            type: 'vllm' as const,
            processStatus: 'stopped' as const,
            enabled: true,
            managed: true,
            installSupported: true
        }
        expect(engineEnabled(facts)).toBe(true)
        expect(engineLifecycleAllowed(facts)).toBe(true)
        expect(engineInstallAllowed(facts)).toBe(false)
    })

    it('offers install only for an explicitly non-adopted supported instance', () => {
        const base = {
            type: 'vllm' as const,
            processStatus: 'not-installed' as const,
            installSupported: true
        }
        expect(engineInstallAllowed(base)).toBe(false)
        expect(engineInstallAllowed({ ...base, adopted: true })).toBe(false)
        expect(engineInstallAllowed({ ...base, adopted: false })).toBe(true)
        expect(engineInstallAllowed({ ...base, adopted: false, installSupported: false })).toBe(
            false
        )
    })

    it('surfaces an explicit backend prerequisite only for an unsupported missing install', () => {
        const reason =
            'Managed vLLM has no native Windows recipe. WSL2 ownership is not implemented.'
        const base = {
            type: 'vllm' as const,
            processStatus: 'not-installed' as const,
            installSupported: false,
            installReason: reason
        }
        expect(engineInstallUnavailableReason(base)).toBe(reason)
        expect(engineInstallUnavailableReason({ ...base, installSupported: true })).toBe('')
        expect(engineInstallUnavailableReason({ ...base, processStatus: 'stopped' })).toBe('')
        expect(engineInstallUnavailableReason({ ...base, installReason: '   ' })).toMatch(
            /unavailable on this host/
        )
    })

    it('exposes the managed installer only on Linux', () => {
        expect(canAutoInstallBackendForOs('vllm', 'Linux')).toBe(true)
        expect(canAutoInstallBackendForOs('vllm', 'Windows')).toBe(false)
        expect(canAutoInstallBackendForOs('vllm', 'MacOS')).toBe(false)
    })

    it('trusts explicit remote install support and fails closed when legacy target OS is unknown', () => {
        const supportedRemote = {
            type: 'vllm' as const,
            processStatus: 'not-installed' as const,
            adopted: false,
            installSupported: true
        }
        expect(
            engineInstallAllowed(supportedRemote) &&
                canAutoInstallBackendForTarget('vllm', undefined, true)
        ).toBe(true)

        const unknownLegacyRemote = {
            type: 'vllm' as const,
            processStatus: 'not-installed' as const,
            adopted: false
        }
        expect(
            engineInstallAllowed(unknownLegacyRemote) &&
                canAutoInstallBackendForTarget('vllm', undefined)
        ).toBe(false)
        expect(canAutoInstallBackendForTarget('vllm', 'Linux', false)).toBe(false)
    })

    it('preserves legacy running fallback for existing engines', () => {
        for (const type of ['ollama', 'lm-studio'] as const) {
            expect(engineEnabled({ type, processStatus: 'running' })).toBe(true)
            expect(engineEnabled({ type, processStatus: 'running', enabled: false })).toBe(false)
            expect(engineLifecycleAllowed({ type, processStatus: 'running' })).toBe(true)
            expect(engineInstallAllowed({ type, processStatus: 'running' })).toBe(true)
        }
    })

    it('preserves adopted and unsupported legacy restrictions', () => {
        const base = { type: 'lm-studio' as const, processStatus: 'running' as const }
        expect(engineLifecycleAllowed({ ...base, adopted: true })).toBe(false)
        expect(engineInstallAllowed({ ...base, adopted: true, installSupported: true })).toBe(false)
        expect(engineInstallAllowed({ ...base, installSupported: false })).toBe(false)
    })
})
