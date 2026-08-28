// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { APP_DISPLAY_NAME } from '@/shared/constants/app'

const DEFAULT_INVITE_ERROR = `Could not invite that node. Check the IP address, port, and that ${APP_DISPLAY_NAME} is running on the other machine, then try again.`

/**
 * Shown when a pairing session was torn down and can no longer be answered
 * (a wrong PIN evicts the backend session, `-32001`). The invite is terminal;
 * the user must restart the flow with a fresh invite.
 */
const INVITE_SESSION_ENDED_MESSAGE =
    'This invitation is no longer valid — ask the inviting node to send a new one.'

/** Wrong-PIN copy for the joiner (the node that entered the PIN). */
export const INCORRECT_PIN_RECEIVER_MESSAGE = `Incorrect PIN. ${INVITE_SESSION_ENDED_MESSAGE}`

/** Wrong-PIN copy mirrored on the inviter (the node that issued the PIN). */
export const INCORRECT_PIN_SENDER_MESSAGE =
    'Incorrect PIN entered on the other node. Send a new invite to try again.'

export function formatClusterInviteError(message: string | null | undefined): string {
    const normalizedMessage = message ?? DEFAULT_INVITE_ERROR
    const normalized = normalizedMessage.toLowerCase()

    // A wrong PIN evicts the backend pairing session, so a stale retry surfaces
    // `-32001: invite session evicted`. Normalize it (and any raw wrong-PIN
    // text) to the terminal "no longer valid" copy instead of leaking the code.
    if (
        normalized.includes('-32001') ||
        normalized.includes('invite session evicted') ||
        normalized.includes('incorrect pin')
    ) {
        return INVITE_SESSION_ENDED_MESSAGE
    }

    if (normalized.includes('deadline_exceeded') || normalized.includes('deadline exceeded')) {
        return `Could not reach that node before the connection timed out. Check the IP address, port, and that ${APP_DISPLAY_NAME} is running on the other machine, then try again.`
    }

    if (
        normalized.includes('unavailable') ||
        normalized.includes('econnrefused') ||
        normalized.includes('connection refused') ||
        normalized.includes('name resolution')
    ) {
        return 'Could not connect to that node. Check the IP address, join port, and that the other machine is online.'
    }

    return normalizedMessage
}
