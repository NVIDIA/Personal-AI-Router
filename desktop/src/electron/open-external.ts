// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { shell } from 'electron'
import { createStructuredLogger } from '@/shared/utils/log'

const log = createStructuredLogger('window')

/**
 * Protocols we are willing to hand to the OS. Electron's security guidance
 * requires filtering `shell.openExternal` input: many scheme handlers execute
 * code or leak credentials (`file:`/UNC → NTLM hash leak, `smb:`, `ms-msdt:`,
 * arbitrary registered protocol handlers). A compromised/script-injected
 * renderer must not be able to reach those sinks.
 */
const ALLOWED_PROTOCOLS = new Set(['https:', 'http:', 'mailto:'])

/** True when `url` is a well-formed URL whose scheme is on the allowlist. */
function isAllowedExternalUrl(url: string): boolean {
    let parsed: URL
    try {
        parsed = new URL(url)
    } catch {
        return false
    }
    return ALLOWED_PROTOCOLS.has(parsed.protocol)
}

/**
 * Open an external URL only if it passes {@link isAllowedExternalUrl}. This is
 * the single sanctioned path to `shell.openExternal`; every renderer-influenced
 * URL (IPC `window:open-external`, `setWindowOpenHandler`, `will-navigate`) must
 * go through it. Rejected URLs are dropped with a log line, never opened.
 */
export function openExternalSafe(url: string): void {
    if (!isAllowedExternalUrl(url)) {
        log.warn({
            sublevel: 'open-external',
            message: 'Blocked external URL with disallowed scheme',
            data: { url }
        })
        return
    }
    void shell.openExternal(url)
}
