// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef } from 'react'

/**
 * Subscribes to `window` `blur` and `focus` and calls `onDismiss` so floating
 * tooltips do not stay open or reopen incorrectly after OS focus changes (alt-tab,
 * child BrowserWindow, in-app surfaces that do not blur the window).
 */
export function useTooltipWindowDismissEffect(onDismiss: () => void): void {
    const onDismissRef = useRef(onDismiss)
    onDismissRef.current = onDismiss

    useEffect(() => {
        const close = () => onDismissRef.current()
        window.addEventListener('blur', close)
        window.addEventListener('focus', close)
        return () => {
            window.removeEventListener('blur', close)
            window.removeEventListener('focus', close)
        }
    }, [])
}
