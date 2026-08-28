// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** Top-level content tab of the consolidated Overview window. */
export type OverviewTab = 'overview' | 'settings'

/** What an Overview message modal's action button does when clicked. */
export type OverviewMessageAction = 'open-update' | 'open-service'

/**
 * A one-off message surfaced as a modal on the Overview window. Replaces the
 * unreliable OS notifications for update-available and broker-crash.
 */
export interface OverviewMessage {
    id: string
    kind: 'update' | 'service' | 'info'
    title: string
    body: string
    actionLabel?: string
    action?: OverviewMessageAction
}

/**
 * Commands pushed from Electron main to the Overview renderer over the
 * `overview:command` channel. Main enqueues these and flushes once the renderer
 * reports ready (`overview:ready`), so a freshly opened Overview never misses one.
 */
export type OverviewCommand =
    | { type: 'focus-node'; nodeId: string }
    | { type: 'message'; message: OverviewMessage }
