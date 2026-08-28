// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { ServiceStatus } from '@/shared/types/ipc-channels'

/**
 * Renderer mirror of the Electron connector lifecycle (`service:status`).
 *
 * Domain data still arrives over `window.pairApi`; this store carries only the
 * Electron-native lifecycle fact, which is the one thing the service bridge
 * cannot report — a stopped broker sends nothing at all. Without it the shell
 * cannot tell "still starting" from "stopped", so a stopped service renders an
 * indefinite loading state with no way back.
 */
interface ServiceStatusStore {
    status: ServiceStatus
    /** Seeds from the main process, then follows `service:status` pushes. */
    initialize: () => Promise<void>
    cleanup: () => void
}

/**
 * Electron starts the connector before the first window exists, so a renderer
 * that has not read the real status yet is truthfully still connecting.
 * Defaulting to `disconnected` would flash the stopped notice on every launch.
 */
const INITIAL_STATUS: ServiceStatus = { connectorStatus: 'connecting', weSpawned: false }

let unsub: (() => void) | null = null

/** A push always beats the initial read, which may resolve after it. */
let pushObserved = false

export const useServiceStatusStore = create<ServiceStatusStore>((set, get) => ({
    status: INITIAL_STATUS,

    initialize: async () => {
        get().cleanup()
        if (!window.windowApi?.service) return

        // Subscribe before the read so a status change during the round trip is
        // not lost.
        pushObserved = false
        unsub = window.windowApi.service.onStatusChanged(status => {
            pushObserved = true
            set({ status })
        })

        try {
            const seeded = await window.windowApi.service.getStatus()
            if (!pushObserved) set({ status: seeded })
        } catch {
            // Main is the only source for this; keep following pushes.
        }
    },

    cleanup: () => {
        unsub?.()
        unsub = null
        pushObserved = false
    }
}))
