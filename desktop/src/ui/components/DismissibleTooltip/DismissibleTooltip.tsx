// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    useCallback,
    useEffect,
    useMemo,
    useRef,
    useState,
    type FocusEvent,
    type PointerEvent
} from 'react'
import { Tooltip, type TooltipProps } from '@nvidia/foundations-react-core'
import { useTooltipWindowDismissEffect } from '@/ui/hooks/useTooltipWindowDismissEffect'
import { DismissibleTooltipContext } from '@/ui/contexts/dismissible-tooltip-context'

interface DismissibleTooltipProps extends Omit<
    TooltipProps,
    'onOpenChange' | 'open' | 'slotContent'
> {
    slotContent: TooltipProps['slotContent']
    placement?: TooltipProps['side']
    open?: boolean
    onOpenChange?: (open: boolean) => void
    keepOpenOnContentHover?: boolean
}

const INTERACTIVE_TOOLTIP_CLOSE_DELAY_MS = 150

/**
 * Tooltip wrapper that dismisses on window blur/focus
 * and exposes {@link useDismissibleTooltipTrigger} (from its own module) for
 * click targets that open other UI (avoids stale `open` and focus-reopen issues).
 */
export function DismissibleTooltip({
    open: controlledOpen,
    onOpenChange: userOnOpenChange,
    slotContent,
    placement,
    children,
    keepOpenOnContentHover = false,
    onPointerEnter,
    onPointerLeave,
    onFocus,
    onBlur,
    attributes,
    ...rest
}: DismissibleTooltipProps) {
    const [uncontrolledOpen, setUncontrolledOpen] = useState(false)
    const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
    const contentHasFocusRef = useRef(false)
    const triggerElRef = useRef<Element | null>(null)
    const contentElRef = useRef<Element | null>(null)
    const isControlled = controlledOpen !== undefined
    const open = isControlled ? controlledOpen : uncontrolledOpen

    const clearCloseTimer = useCallback(() => {
        if (!closeTimerRef.current) return
        clearTimeout(closeTimerRef.current)
        closeTimerRef.current = null
    }, [])

    const commitOpen = useCallback(
        (next: boolean) => {
            if (!isControlled) setUncontrolledOpen(next)
            userOnOpenChange?.(next)
        },
        [isControlled, userOnOpenChange]
    )

    const setOpen = useCallback(
        (next: boolean) => {
            if (next) {
                clearCloseTimer()
                commitOpen(true)
                return
            }

            if (!keepOpenOnContentHover) {
                commitOpen(false)
                return
            }

            clearCloseTimer()
            closeTimerRef.current = setTimeout(() => {
                closeTimerRef.current = null
                commitOpen(false)
            }, INTERACTIVE_TOOLTIP_CLOSE_DELAY_MS)
        },
        [clearCloseTimer, commitOpen, keepOpenOnContentHover]
    )

    const dismiss = useCallback(() => {
        clearCloseTimer()
        commitOpen(false)
    }, [clearCloseTimer, commitOpen])

    useTooltipWindowDismissEffect(dismiss)

    useEffect(() => clearCloseTimer, [clearCloseTimer])

    // Safety net: the foundations Tooltip occasionally drops its own
    // pointerleave (fast movement, a transformed/animated trigger, or a
    // mid-render swap), leaving a hover tooltip stuck open. While open, watch
    // pointerover globally and close as soon as the pointer is over an element
    // that is neither the trigger nor (for hoverable tooltips) the tooltip
    // content. Controlled tooltips own their own open state, so this is skipped.
    useEffect(() => {
        if (!open || isControlled) return

        const handleGlobalPointerOver = (event: Event) => {
            const target = event.target
            if (!(target instanceof Node)) return

            const trigger = triggerElRef.current
            if (trigger && trigger.contains(target)) return

            if (keepOpenOnContentHover) {
                const content = contentElRef.current
                if (content && content.contains(target)) return
                if (target instanceof Element && target.closest('.nv-tooltip-content')) {
                    return
                }
            }

            setOpen(false)
        }

        document.addEventListener('pointerover', handleGlobalPointerOver)
        return () => {
            document.removeEventListener('pointerover', handleGlobalPointerOver)
        }
    }, [open, isControlled, keepOpenOnContentHover, setOpen])

    const ctx = useMemo(() => ({ dismiss }), [dismiss])

    const handleContentPointerEnter = useCallback(
        (event: PointerEvent<HTMLDivElement>) => {
            contentElRef.current = event.currentTarget
            if (keepOpenOnContentHover) setOpen(true)
            onPointerEnter?.(event)
        },
        [keepOpenOnContentHover, onPointerEnter, setOpen]
    )

    const handleContentPointerLeave = useCallback(
        (event: PointerEvent<HTMLDivElement>) => {
            if (keepOpenOnContentHover && !contentHasFocusRef.current) setOpen(false)
            onPointerLeave?.(event)
        },
        [keepOpenOnContentHover, onPointerLeave, setOpen]
    )

    const handleContentFocus = useCallback(
        (event: FocusEvent<HTMLDivElement>) => {
            if (keepOpenOnContentHover) {
                contentElRef.current = event.currentTarget
                contentHasFocusRef.current = true
                setOpen(true)
            }
            onFocus?.(event)
        },
        [keepOpenOnContentHover, onFocus, setOpen]
    )

    const handleTriggerPointerEnter = useCallback(
        (event: PointerEvent<HTMLButtonElement>) => {
            triggerElRef.current = event.currentTarget
            if (keepOpenOnContentHover) setOpen(true)
            attributes?.TooltipTrigger?.onPointerEnter?.(event)
        },
        [attributes?.TooltipTrigger, keepOpenOnContentHover, setOpen]
    )

    const handleTriggerFocus = useCallback(
        (event: FocusEvent<HTMLButtonElement>) => {
            triggerElRef.current = event.currentTarget
            if (keepOpenOnContentHover) setOpen(true)
            attributes?.TooltipTrigger?.onFocus?.(event)
        },
        [attributes?.TooltipTrigger, keepOpenOnContentHover, setOpen]
    )

    const handleContentBlur = useCallback(
        (event: FocusEvent<HTMLDivElement>) => {
            const focusRemainsInContent =
                event.relatedTarget instanceof Node &&
                event.currentTarget.contains(event.relatedTarget)

            if (keepOpenOnContentHover && !focusRemainsInContent) {
                contentHasFocusRef.current = false
                setOpen(false)
            }

            onBlur?.(event)
        },
        [keepOpenOnContentHover, onBlur, setOpen]
    )

    return (
        <DismissibleTooltipContext.Provider value={ctx}>
            <Tooltip
                open={open}
                onOpenChange={setOpen}
                slotContent={slotContent}
                side={placement ?? rest.side}
                onPointerEnter={handleContentPointerEnter}
                onPointerLeave={handleContentPointerLeave}
                onFocus={handleContentFocus}
                onBlur={handleContentBlur}
                attributes={{
                    ...attributes,
                    TooltipTrigger: {
                        ...attributes?.TooltipTrigger,
                        onPointerEnter: handleTriggerPointerEnter,
                        onFocus: handleTriggerFocus
                    }
                }}
                {...rest}
            >
                {children}
            </Tooltip>
        </DismissibleTooltipContext.Provider>
    )
}
