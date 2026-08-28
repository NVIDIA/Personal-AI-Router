// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ConnectorStatus } from '@/shared/types/ipc-channels'

/** What a window shows in place of its node content, if anything. */
type ShellView = 'service-stopped' | 'loading' | 'content'

/**
 * Decide what Overview and the tray render while the service is unavailable.
 *
 * A stopped service outranks the connection snapshot. That snapshot only
 * refreshes while the bridge is alive, so it still reads `connected` after the
 * service goes down under an open window, and it reads `false` in a window
 * opened while the service was down — which is indistinguishable from a slow
 * start unless the connector status breaks the tie. Getting this wrong is what
 * left a stopped service showing a spinner that never resolved.
 */
export function resolveShellView(state: {
    connectorStatus: ConnectorStatus
    connected: boolean
    fetchedNodes: boolean
}): ShellView {
    if (state.connectorStatus === 'disconnected') return 'service-stopped'
    if (!state.connected || !state.fetchedNodes) return 'loading'
    return 'content'
}
