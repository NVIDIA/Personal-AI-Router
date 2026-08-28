// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    memo,
    useCallback,
    useEffect,
    useLayoutEffect,
    useRef,
    useState,
    type ReactNode
} from 'react'
import type { CascadeMenuItem } from '@/ui/types/cascade-menu'
import { MENU_ITEM_HEIGHT } from '@/ui/types/cascade-menu'
import MenuItemRow from './MenuItemRow'
import { useVirtualList } from '@/ui/hooks/useVirtualList'

interface MenuPanelProps {
    items: CascadeMenuItem[]
    checkboxMode: boolean
    focusedIndex: number
    style: React.CSSProperties
    slotBefore?: ReactNode
    slotAfter?: ReactNode
    onItemPointerEnter: (index: number) => void
    onItemPointerDown: (index: number) => void
    onItemClick: (item: CascadeMenuItem) => void
    onPointerLeave?: () => void
    panelRef?: React.RefObject<HTMLDivElement | null>
}

function indexFromTarget(target: EventTarget | null): number {
    const el = (target as HTMLElement | null)?.closest?.('[data-menu-index]')
    if (!el) return -1
    return parseInt(el.getAttribute('data-menu-index')!, 10)
}

const MenuPanel = memo(function MenuPanel({
    items,
    checkboxMode,
    focusedIndex,
    style,
    slotBefore,
    slotAfter,
    onItemPointerEnter,
    onItemPointerDown,
    onItemClick,
    onPointerLeave,
    panelRef
}: MenuPanelProps) {
    const lastHoverRef = useRef(-1)
    const slotBeforeRef = useRef<HTMLDivElement | null>(null)
    const slotAfterRef = useRef<HTMLDivElement | null>(null)

    // ── Measure slot heights via ResizeObserver ──

    const [slotH, setSlotH] = useState(0)

    useEffect(() => {
        if (!slotBefore && !slotAfter) return
        const measure = () => {
            const bh = slotBeforeRef.current?.offsetHeight ?? 0
            const ah = slotAfterRef.current?.offsetHeight ?? 0
            const total = bh + ah
            setSlotH(prev => (prev !== total ? total : prev))
        }
        measure()
        const ro = new ResizeObserver(measure)
        if (slotBeforeRef.current) ro.observe(slotBeforeRef.current)
        if (slotAfterRef.current) ro.observe(slotAfterRef.current)
        return () => ro.disconnect()
    }, [slotBefore, slotAfter])

    const maxH =
        (typeof style.maxHeight === 'number'
            ? style.maxHeight
            : typeof style.maxHeight === 'string'
              ? parseInt(style.maxHeight, 10)
              : 400) - slotH

    // ── Virtual list ──

    const vlist = useVirtualList({
        count: items.length,
        estimateRowHeight: MENU_ITEM_HEIGHT,
        viewportHeight: maxH,
        threshold: 30,
        trackMaxWidth: true
    })

    // ── Keyboard focus → scroll to focused item (before paint for no jitter) ──

    useLayoutEffect(() => {
        if (focusedIndex >= 0) vlist.scrollToIndex(focusedIndex)
    }, [focusedIndex, vlist])

    // ── Sync panelRef via ref callback ──

    const outerRefCallback = useCallback(
        (el: HTMLDivElement | null) => {
            if (panelRef) panelRef.current = el
        },
        [panelRef]
    )

    // ── Event delegation ──

    const handlePointerOver = useCallback(
        (e: React.PointerEvent) => {
            const idx = indexFromTarget(e.target)
            if (idx >= 0 && idx !== lastHoverRef.current) {
                lastHoverRef.current = idx
                onItemPointerEnter(idx)
            }
        },
        [onItemPointerEnter]
    )

    const handlePointerDown = useCallback(
        (e: React.PointerEvent) => {
            const idx = indexFromTarget(e.target)
            if (idx >= 0) onItemPointerDown(idx)
        },
        [onItemPointerDown]
    )

    const handleClick = useCallback(
        (e: React.MouseEvent) => {
            const idx = indexFromTarget(e.target)
            if (idx >= 0 && items[idx] && !items[idx].disabled) {
                onItemClick(items[idx])
            }
        },
        [items, onItemClick]
    )

    const handlePointerLeave = useCallback(() => {
        lastHoverRef.current = -1
        onPointerLeave?.()
    }, [onPointerLeave])

    // ── Render items (virtualized or plain depending on count) ──

    const { virtualize, startIndex, endIndex, topSpacerHeight, bottomSpacerHeight } = vlist

    return (
        <div
            ref={outerRefCallback}
            role="menu"
            className="cascade-menu-panel"
            style={style}
            onPointerLeave={handlePointerLeave}
        >
            {slotBefore && (
                <div ref={slotBeforeRef} className="cascade-menu-slot cascade-menu-slot-before">
                    {slotBefore}
                </div>
            )}
            <div
                ref={vlist.scrollRef}
                className="cascade-menu-scroll"
                style={maxH > 0 ? { maxHeight: maxH } : undefined}
                onScroll={vlist.onScroll}
                onPointerOver={handlePointerOver}
                onPointerDown={handlePointerDown}
                onClick={handleClick}
            >
                {virtualize ? (
                    <>
                        {topSpacerHeight > 0 && <div style={{ height: topSpacerHeight }} />}
                        {items.slice(startIndex, endIndex).map((item, i) => {
                            const realIndex = startIndex + i
                            return (
                                <MenuItemRow
                                    key={item.id}
                                    item={item}
                                    index={realIndex}
                                    focused={realIndex === focusedIndex}
                                    checkboxMode={checkboxMode}
                                />
                            )
                        })}
                        {bottomSpacerHeight > 0 && <div style={{ height: bottomSpacerHeight }} />}
                    </>
                ) : (
                    items.map((item, i) => (
                        <MenuItemRow
                            key={item.id}
                            item={item}
                            index={i}
                            focused={i === focusedIndex}
                            checkboxMode={checkboxMode}
                        />
                    ))
                )}
            </div>
            {slotAfter && (
                <div ref={slotAfterRef} className="cascade-menu-slot cascade-menu-slot-after">
                    {slotAfter}
                </div>
            )}
        </div>
    )
})

export default MenuPanel
