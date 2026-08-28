// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    useCallback,
    useContext,
    type MouseEvent as ReactMouseEvent,
    type MouseEventHandler
} from 'react'
import { DismissibleTooltipContext } from '@/ui/contexts/dismissible-tooltip-context'

/**
 * Merge with a trigger `onClick`: closes the parent DismissibleTooltip,
 * blurs the target (reduces Radix focus-reopen), then runs the handler.
 * Pass `() => void` to avoid an extra `useCallback` wrapper when you do not need the event.
 */
export function useDismissibleTooltipTrigger<T extends HTMLElement>(
    userOnClick?: MouseEventHandler<T> | (() => void)
): MouseEventHandler<T> {
    const ctx = useContext(DismissibleTooltipContext)

    return useCallback(
        (e: ReactMouseEvent<T>) => {
            ctx?.dismiss()
            e.currentTarget.blur()
            if (userOnClick === undefined) return
            ;(userOnClick as (ev: ReactMouseEvent<T>) => void)(e)
        },
        [ctx, userOnClick]
    )
}
