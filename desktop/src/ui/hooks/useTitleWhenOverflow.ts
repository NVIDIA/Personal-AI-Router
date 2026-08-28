// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback } from 'react'

/**
 * Ref callback for a single-line truncating element: sets native `title` only when
 * horizontal overflow is detected (scrollWidth > clientWidth).
 */
export function useTitleWhenOverflow(fullLabel: string) {
    return useCallback(
        (el: HTMLElement | null) => {
            if (!el) return
            const overflow = el.scrollWidth > el.clientWidth
            el.title = overflow && fullLabel !== '' ? fullLabel : ''
        },
        [fullLabel]
    )
}
