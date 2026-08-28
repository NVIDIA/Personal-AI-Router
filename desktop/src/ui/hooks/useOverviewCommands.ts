// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import type { OverviewMessage } from '@/shared/types/overview'

/**
 * Subscribes the Overview window to commands pushed from Electron main
 * (`overview:command`): focus a node's engine settings or surface a message
 * modal. Signals readiness (`overview:ready`) on mount so main flushes any
 * commands it queued before the renderer subscribed.
 *
 * Returns the queue of pending message modals plus a dismisser.
 */
export function useOverviewCommands(): {
    messages: OverviewMessage[]
    dismissMessage: (id: string) => void
} {
    const [messages, setMessages] = useState<OverviewMessage[]>([])

    useEffect(() => {
        if (!window.windowApi) return

        const { focusNodeEngineSettings } = useOverviewUiStore.getState()

        const unsubscribe = window.windowApi.window.onOverviewCommand(command => {
            if (command.type === 'focus-node') {
                focusNodeEngineSettings(command.nodeId)
            } else {
                setMessages(prev =>
                    prev.some(m => m.id === command.message.id) ? prev : [...prev, command.message]
                )
            }
        })

        void window.windowApi.window.overviewReady()

        return unsubscribe
    }, [])

    const dismissMessage = (id: string): void => {
        setMessages(prev => prev.filter(m => m.id !== id))
    }

    return { messages, dismissMessage }
}
