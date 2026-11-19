// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type {
    EngineSettingsRequest,
    EngineSettingsSnapshot,
    EngineSettingsTarget
} from '@/shared/types/engine-settings'
import getErrorString from '@/shared/utils/get-error-string'

export const engineSettingsKey = (target: EngineSettingsTarget): string =>
    `${target.nodeId}:${target.engine}`

interface SettingsEntry {
    snapshot?: EngineSettingsSnapshot
    unavailable?: string
    arrival: number
}
interface SettingsStore {
    entries: Record<string, SettingsEntry>
    receive: (snapshot: EngineSettingsSnapshot) => void
    disconnect: (nodeId?: string) => void
    reload: (target: EngineSettingsTarget) => Promise<void>
    apply: (request: EngineSettingsRequest) => Promise<void>
}

export const useEngineSettingsStore = create<SettingsStore>((set, get) => ({
    entries: {},
    receive: snapshot => {
        const key = engineSettingsKey(snapshot)
        const current = get().entries[key]
        if (
            current?.snapshot?.epoch === snapshot.epoch &&
            (current.snapshot.sequence > snapshot.sequence ||
                (current.snapshot.sequence === snapshot.sequence && !current.unavailable))
        )
            return
        set(state => ({
            entries: {
                ...state.entries,
                [key]: { snapshot, arrival: (current?.arrival ?? 0) + 1 }
            }
        }))
    },
    disconnect: nodeId =>
        set(state => ({
            entries: Object.fromEntries(
                Object.entries(state.entries).map(([key, entry]) => [
                    key,
                    !nodeId || key.startsWith(`${nodeId}:`)
                        ? {
                              ...entry,
                              unavailable: 'Device disconnected. Reconnect to edit settings.',
                              arrival: entry.arrival + 1
                          }
                        : entry
                ])
            )
        })),
    reload: async target => {
        const key = engineSettingsKey(target)
        const arrival = get().entries[key]?.arrival ?? 0
        try {
            const snapshot = await window.pairApi.engines.getSettings(target)
            // A push/disconnect after this read began wins, including a new authority epoch.
            if ((get().entries[key]?.arrival ?? 0) !== arrival) return
            get().receive({ ...snapshot, nodeId: target.nodeId })
            set(state => ({
                entries: {
                    ...state.entries,
                    [key]: { ...state.entries[key], unavailable: undefined }
                }
            }))
        } catch (error) {
            if ((get().entries[key]?.arrival ?? 0) !== arrival) return
            set(state => ({
                entries: {
                    ...state.entries,
                    [key]: {
                        ...state.entries[key],
                        arrival,
                        unavailable: `Settings unavailable: ${getErrorString(error)}`
                    }
                }
            }))
        }
    },
    apply: async request => {
        try {
            // Receipts acknowledge operations; only a full snapshot updates
            // durable state, and engines:settings-changed delivers that on its
            // own. Re-reading here would just race the push.
            await window.pairApi.engines.applySettings(request)
        } catch (error) {
            // A transport failure leaves no push to wait for, so read back
            // whatever the device can still report — including nothing.
            await get().reload(request)
            throw error
        }
    }
}))

let consumers = 0
let unsubscribe: Array<() => void> = []
/** Subscribe before fetching a baseline. All open editors share one push listener. */
export function watchEngineSettings(): () => void {
    if (consumers++ === 0)
        unsubscribe = [
            window.pairApi.engines.onSettingsChanged(snapshot =>
                useEngineSettingsStore.getState().receive(snapshot)
            ),
            window.pairApi.engines.onSettingsDisconnected(({ nodeId }) =>
                useEngineSettingsStore.getState().disconnect(nodeId)
            )
        ]
    // Closing the last editor says nothing about any device, so cached
    // snapshots stay as they are. `disconnect` is reserved for a real
    // engines:settings-disconnected push and for losing the service
    // connection. An editor unmounts whenever its engine starts installing,
    // so flagging every node here would mislabel the whole cluster.
    return () => {
        if (--consumers === 0) {
            unsubscribe.forEach(stop => stop())
            unsubscribe = []
        }
    }
}
