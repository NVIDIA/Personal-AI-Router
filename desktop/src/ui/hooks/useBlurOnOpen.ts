// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect } from 'react'

/**
 * Blurs the active element when `open` transitions to true.
 * Prevents dialogs from auto-focusing buttons and showing tooltips on open.
 */
export function useBlurOnOpen(open: boolean): void {
    useEffect(() => {
        if (open) {
            requestAnimationFrame(() => {
                const el = document.activeElement
                if (
                    el instanceof HTMLElement &&
                    el.tagName !== 'INPUT' &&
                    el.tagName !== 'TEXTAREA'
                ) {
                    el.blur()
                }
            })
        }
    }, [open])
}
