// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { prepareLocalEnginesForShutdown } from '@/electron/service-bridge/modular-supervisor'

describe('engine shutdown intent', () => {
    it('uses the shutdown-specific RPC instead of an explicit engine stop', async () => {
        const call = vi.fn().mockResolvedValue(undefined)
        const broker = { call } as unknown as Parameters<typeof prepareLocalEnginesForShutdown>[0]

        await prepareLocalEnginesForShutdown(broker)

        expect(call).toHaveBeenCalledTimes(1)
        expect(call).toHaveBeenCalledWith('engine:prepare-shutdown', undefined, 30_000)
    })
})
