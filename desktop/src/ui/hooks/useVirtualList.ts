// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useLayoutEffect, useReducer, useRef, useState } from 'react'

interface UseVirtualListOptions {
    /** Total number of items in the list. */
    count: number
    /** Best-guess row height used for items not yet measured. */
    estimateRowHeight: number
    /** Extra rows rendered above/below the visible window. Default 6. */
    overscan?: number
    /** Minimum item count before virtualization kicks in. Default 50. */
    threshold?: number
    /** Pixel height of the scroll viewport (the container's visible area). */
    viewportHeight: number
    /**
     * When this value changes (by reference), the height cache is cleared and
     * scroll resets to top. Use the list array identity (e.g. `sortedModels`).
     */
    resetKey?: unknown
    /**
     * When true, tracks the widest `scrollWidth` seen and applies it as
     * `min-width` on the scroll container so the panel never shrinks.
     * Resets on count/resetKey change. Default false.
     */
    trackMaxWidth?: boolean
}

interface UseVirtualListResult {
    /** Whether the list is being virtualized (count >= threshold). */
    virtualize: boolean
    /** Attach to the scrollable container element. */
    scrollRef: React.RefObject<HTMLDivElement | null>
    /** Average measured row height (or estimate if nothing measured yet). */
    rowHeight: number
    /** First index to render (inclusive). */
    startIndex: number
    /** Last index to render (exclusive). */
    endIndex: number
    /** Height in px for the top spacer div. */
    topSpacerHeight: number
    /** Height in px for the bottom spacer div. */
    bottomSpacerHeight: number
    /** Total scrollable height of all items. */
    totalHeight: number
    /** Attach as `onScroll` on the scrollable container. */
    onScroll: (e: React.UIEvent<HTMLDivElement>) => void
    /** Programmatically scroll so `index` is visible. Updates DOM + state in sync. */
    scrollToIndex: (index: number) => void
}

const DEFAULT_OVERSCAN = 6
const DEFAULT_THRESHOLD = 50

/**
 * Virtual list hook supporting variable-height items.
 *
 * Items MUST render a `data-vlist-index={realIndex}` attribute on their
 * outermost element so the hook can measure heights after each paint.
 *
 * Heights are cached per-index; unmeasured items use `estimateRowHeight`.
 * After mount/scroll the hook measures visible items in a `useLayoutEffect`
 * and triggers a synchronous re-render (before paint) when measurements
 * differ from the cache, so spacers are correct on the first visible frame.
 */
export function useVirtualList(opts: UseVirtualListOptions): UseVirtualListResult {
    const {
        count,
        estimateRowHeight,
        overscan = DEFAULT_OVERSCAN,
        threshold = DEFAULT_THRESHOLD,
        viewportHeight,
        resetKey,
        trackMaxWidth = false
    } = opts

    const scrollRef = useRef<HTMLDivElement | null>(null)
    const virtualize = count >= threshold

    // ── Per-item height cache ──

    const heightCacheRef = useRef(new Map<number, number>())
    const [, forceRender] = useReducer((x: number) => x + 1, 0)

    // ── Width tracking ──

    const maxWidthRef = useRef(0)

    useEffect(() => {
        if (!trackMaxWidth || !scrollRef.current) return
        const id = requestAnimationFrame(() => {
            if (!scrollRef.current) return
            const sw = scrollRef.current.scrollWidth
            if (sw > maxWidthRef.current) {
                maxWidthRef.current = sw
                scrollRef.current.style.minWidth = `${sw}px`
            }
        })
        return () => cancelAnimationFrame(id)
    }, [trackMaxWidth, count])

    // ── Scroll tracking ──

    const [scrollTop, setScrollTop] = useState(0)
    const prevCountRef = useRef(count)
    const prevResetKeyRef = useRef(resetKey)

    if (prevCountRef.current !== count || prevResetKeyRef.current !== resetKey) {
        prevCountRef.current = count
        prevResetKeyRef.current = resetKey
        heightCacheRef.current.clear()
        maxWidthRef.current = 0
        if (scrollRef.current) {
            scrollRef.current.scrollTop = 0
            if (trackMaxWidth) scrollRef.current.style.minWidth = ''
        }
        if (scrollTop !== 0) setScrollTop(0)
    }

    const onScroll = useCallback(
        (e: React.UIEvent<HTMLDivElement>) => {
            if (!virtualize) return
            const el = e.currentTarget
            setScrollTop(el.scrollTop)
            if (trackMaxWidth) {
                const sw = el.scrollWidth
                if (sw > maxWidthRef.current) {
                    maxWidthRef.current = sw
                    el.style.minWidth = `${sw}px`
                }
            }
        },
        [virtualize, trackMaxWidth]
    )

    // ── Prefix sums (O(N) per render, trivial for N ≤ a few thousand) ──

    const cache = heightCacheRef.current
    const offsets = new Array<number>(count + 1)
    offsets[0] = 0
    for (let i = 0; i < count; i++) {
        offsets[i + 1] = offsets[i] + (cache.get(i) ?? estimateRowHeight)
    }
    const totalHeight = offsets[count] ?? 0
    const offsetsRef = useRef(offsets)
    offsetsRef.current = offsets

    // ── Window computation ──

    let startIndex = 0
    let endIndex = count

    if (virtualize && count > 0) {
        let lo = 0
        let hi = count
        while (lo < hi) {
            const mid = (lo + hi) >>> 1
            if (offsets[mid + 1] <= scrollTop) lo = mid + 1
            else hi = mid
        }
        startIndex = Math.max(0, lo - overscan)

        let end = lo
        while (end < count && offsets[end] < scrollTop + viewportHeight) end++
        endIndex = Math.min(count, end + overscan)
    }

    const topSpacerHeight = offsets[startIndex] ?? 0
    const bottomSpacerHeight = totalHeight - (offsets[endIndex] ?? totalHeight)

    const rowHeight = cache.size > 0 ? totalHeight / count : estimateRowHeight

    // ── Measure visible items (useLayoutEffect → correct before first paint) ──

    useLayoutEffect(() => {
        if (!virtualize || !scrollRef.current) return
        const c = heightCacheRef.current
        const items = scrollRef.current.querySelectorAll<HTMLElement>('[data-vlist-index]')
        let changed = false
        items.forEach(el => {
            const attr = el.getAttribute('data-vlist-index')
            if (attr === null) return
            const idx = parseInt(attr, 10)
            const h = el.offsetHeight
            if (h > 0 && c.get(idx) !== h) {
                c.set(idx, h)
                changed = true
            }
        })
        if (changed) forceRender()
    }, [virtualize, startIndex, endIndex, forceRender])

    // ── Scroll-to-index (uses per-item offsets for accuracy) ──

    const scrollToIndex = useCallback(
        (index: number) => {
            if (!scrollRef.current || index < 0) return
            const offs = offsetsRef.current
            const targetTop = offs[index] ?? 0
            const itemH =
                offs[index + 1] !== undefined ? offs[index + 1] - offs[index] : estimateRowHeight
            const el = scrollRef.current
            const current = el.scrollTop

            if (targetTop < current) {
                el.scrollTop = targetTop
                setScrollTop(targetTop)
            } else if (targetTop + itemH > current + viewportHeight) {
                const newTop = targetTop + itemH - viewportHeight
                el.scrollTop = newTop
                setScrollTop(newTop)
            }
        },
        [viewportHeight, estimateRowHeight]
    )

    return {
        virtualize,
        scrollRef,
        rowHeight,
        startIndex,
        endIndex,
        topSpacerHeight,
        bottomSpacerHeight,
        totalHeight,
        onScroll,
        scrollToIndex
    }
}
