// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi, beforeEach } from 'vitest'

// vi.mock is hoisted above module-level consts, so the spy has to be too.
const { refresh } = vi.hoisted(() => ({ refresh: vi.fn() }))

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/model-hub/lmstudio-catalog', () => ({
    lmStudioCatalogCache: { refresh, ensureLoaded: vi.fn(), list: () => [] }
}))

import { warmEngineHubs } from '@/electron/model-hub'
import type { EngineType } from '@/shared/types/engines'

const installed =
    (...engines: EngineType[]) =>
    (engine: EngineType) =>
        engines.includes(engine)

describe('engine hub warming', () => {
    beforeEach(() => refresh.mockClear())

    // The LM Studio catalogue fetch is the only unprompted outbound request PAIR
    // makes. A node running Ollama or MLX has no reason to announce itself to
    // huggingface.co at every launch, so the warm is gated on the engine that
    // needs it actually being present.
    it('does not fetch the LM Studio catalogue when LM Studio is not installed', () => {
        warmEngineHubs(installed('ollama', 'mlx'))
        expect(refresh).not.toHaveBeenCalled()
    })

    it('fetches it when LM Studio is installed', () => {
        warmEngineHubs(installed('lm-studio'))
        expect(refresh).toHaveBeenCalledTimes(1)
    })

    // An engine-manager that has not reported yet reads as "not installed". The
    // cost is a slower first modal open, because getEngineHubModels awaits
    // ensureLoaded() regardless -- never a missing catalogue.
    it('stays silent when no engine facts have arrived', () => {
        warmEngineHubs(() => false)
        expect(refresh).not.toHaveBeenCalled()
    })
})
