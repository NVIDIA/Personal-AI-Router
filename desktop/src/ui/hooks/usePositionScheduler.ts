// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'
import type { PanelPosition, SubmenuPosition, ViewportSize } from '@/ui/types/cascade-menu'
import { THROTTLE_MS } from '@/ui/types/cascade-menu'
import {
    computePanelPosition,
    computeSubmenuPosition,
    panelPositionEqual,
    submenuPositionEqual,
    readViewport
} from '@/ui/utils/cascade-menu'

interface SchedulerOutput {
    rootPosition: PanelPosition | null
    submenuPosition: SubmenuPosition | null
    requestSubmenuUpdate: (itemIndex: number | null, minWidth?: number) => void
}

/**
 * Centralised layout scheduler that owns all DOM geometry reads for the menu.
 *
 * - Single RAF loop — all getBoundingClientRect calls happen in one read phase
 *   before any React state writes, preventing read-write-read thrashing.
 * - Dirty flag + timestamp throttle — events mark dirty but the tick only runs
 *   at most once per THROTTLE_MS, and only when something actually changed.
 * - Cached output — skips React setState when the computed positions are
 *   structurally equal to the previous frame, avoiding unnecessary renders.
 */
export function usePositionScheduler(
    triggerRef: React.RefObject<HTMLElement | null>,
    rootPanelRef: React.RefObject<HTMLElement | null>,
    open: boolean,
    preferredSide: 'top' | 'bottom',
    preferredAlign: 'start' | 'end'
): SchedulerOutput {
    const [rootPosition, setRootPosition] = useState<PanelPosition | null>(null)
    const [submenuPosition, setSubmenuPosition] = useState<SubmenuPosition | null>(null)

    const dirtyRef = useRef(false)
    const rafRef = useRef(0)
    const lastTickRef = useRef(0)
    const throttleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
    const submenuItemIndexRef = useRef<number | null>(null)
    const submenuMinWidthRef = useRef(0)

    const cachedRootRef = useRef<PanelPosition | null>(null)
    const cachedSubRef = useRef<SubmenuPosition | null>(null)
    const cachedViewportRef = useRef<ViewportSize>({ vw: 0, vh: 0 })

    const prefsRef = useRef({ side: preferredSide, align: preferredAlign })
    prefsRef.current = { side: preferredSide, align: preferredAlign }

    const tick = useCallback(() => {
        rafRef.current = 0
        dirtyRef.current = false
        lastTickRef.current = performance.now()

        if (!triggerRef.current) return

        // ── Read phase: all DOM reads batched here ──
        const vp = readViewport()
        const triggerRect = triggerRef.current.getBoundingClientRect()

        let itemRect: DOMRect | null = null
        let panelRect: DOMRect | null = null

        if (submenuItemIndexRef.current !== null && rootPanelRef.current) {
            panelRect = rootPanelRef.current.getBoundingClientRect()
            const rowEl = rootPanelRef.current.querySelector<HTMLElement>(
                `[data-menu-index="${submenuItemIndexRef.current}"]`
            )
            if (rowEl) itemRect = rowEl.getBoundingClientRect()
        }

        cachedViewportRef.current = vp

        // ── Compute phase: pure math, no DOM access ──
        const { side, align } = prefsRef.current
        const nextRoot = computePanelPosition(triggerRect, vp, side, align)

        const subMinWidth = submenuMinWidthRef.current

        let nextSub: SubmenuPosition | null = null
        if (itemRect && panelRect) {
            const dx = nextRoot.left - panelRect.left
            const dy = nextRoot.top - panelRect.top
            if (dx !== 0 || dy !== 0) {
                const adjPanel = new DOMRect(
                    panelRect.x + dx,
                    panelRect.y + dy,
                    panelRect.width,
                    panelRect.height
                )
                const adjItem = new DOMRect(
                    itemRect.x + dx,
                    itemRect.y + dy,
                    itemRect.width,
                    itemRect.height
                )
                nextSub = computeSubmenuPosition(adjItem, adjPanel, vp, subMinWidth)
            } else {
                nextSub = computeSubmenuPosition(itemRect, panelRect, vp, subMinWidth)
            }
        }

        // ── Write phase: only setState when changed ──
        if (!panelPositionEqual(nextRoot, cachedRootRef.current)) {
            cachedRootRef.current = nextRoot
            setRootPosition(nextRoot)
        }

        if (!submenuPositionEqual(nextSub, cachedSubRef.current)) {
            cachedSubRef.current = nextSub
            setSubmenuPosition(nextSub)
        }
    }, [triggerRef, rootPanelRef])

    const scheduleTick = useCallback(() => {
        dirtyRef.current = true

        if (rafRef.current) return

        const elapsed = performance.now() - lastTickRef.current
        if (elapsed >= THROTTLE_MS) {
            rafRef.current = requestAnimationFrame(tick)
        } else {
            if (throttleTimerRef.current) return
            throttleTimerRef.current = setTimeout(() => {
                throttleTimerRef.current = null
                if (dirtyRef.current && !rafRef.current) {
                    rafRef.current = requestAnimationFrame(tick)
                }
            }, THROTTLE_MS - elapsed)
        }
    }, [tick])

    const requestSubmenuUpdate = useCallback(
        (itemIndex: number | null, minWidth = 0) => {
            submenuItemIndexRef.current = itemIndex
            submenuMinWidthRef.current = minWidth
            if (itemIndex === null) {
                cachedSubRef.current = null
                setSubmenuPosition(null)
                return
            }
            scheduleTick()
        },
        [scheduleTick]
    )

    // Initial computation + event listeners while open
    useEffect(() => {
        if (!open) {
            cachedRootRef.current = null
            cachedSubRef.current = null
            submenuItemIndexRef.current = null
            setRootPosition(null)
            setSubmenuPosition(null)
            return
        }

        // Immediate first tick (no throttle delay on open)
        dirtyRef.current = true
        rafRef.current = requestAnimationFrame(tick)

        const onViewportChange = () => scheduleTick()

        window.addEventListener('resize', onViewportChange, { passive: true })
        window.addEventListener('scroll', onViewportChange, { passive: true, capture: true })

        // Observe the trigger's positioned ancestor chain so that any
        // parent resize (modal reflow, sidebar collapse, etc.) triggers
        // a position recalculation.
        const ro = new ResizeObserver(onViewportChange)
        let ancestor: HTMLElement | null = triggerRef.current
        while (ancestor) {
            ro.observe(ancestor)
            ancestor = ancestor.offsetParent as HTMLElement | null
        }

        return () => {
            cancelAnimationFrame(rafRef.current)
            rafRef.current = 0
            if (throttleTimerRef.current) {
                clearTimeout(throttleTimerRef.current)
                throttleTimerRef.current = null
            }
            dirtyRef.current = false
            window.removeEventListener('resize', onViewportChange)
            window.removeEventListener('scroll', onViewportChange, { capture: true })
            ro.disconnect()
        }
    }, [open, tick, scheduleTick, triggerRef])

    return { rootPosition, submenuPosition, requestSubmenuUpdate }
}
