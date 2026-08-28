// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { PanelPosition, SubmenuPosition, ViewportSize } from '@/ui/types/cascade-menu'
import { VIEWPORT_PADDING, MIN_PANEL_SIZE } from '@/ui/types/cascade-menu'

export function readViewport(): ViewportSize {
    return { vw: window.innerWidth, vh: window.innerHeight }
}

export function panelPositionEqual(a: PanelPosition | null, b: PanelPosition | null): boolean {
    if (a === b) return true
    if (!a || !b) return false
    return (
        a.top === b.top &&
        a.left === b.left &&
        a.maxWidth === b.maxWidth &&
        a.maxHeight === b.maxHeight &&
        a.side === b.side &&
        a.align === b.align
    )
}

export function submenuPositionEqual(
    a: SubmenuPosition | null,
    b: SubmenuPosition | null
): boolean {
    if (a === b) return true
    if (!a || !b) return false
    return (
        a.top === b.top &&
        a.left === b.left &&
        a.maxWidth === b.maxWidth &&
        a.maxHeight === b.maxHeight &&
        a.side === b.side
    )
}

export function computePanelPosition(
    triggerRect: DOMRect,
    vp: ViewportSize,
    preferredSide: 'top' | 'bottom' = 'bottom',
    preferredAlign: 'start' | 'end' = 'start'
): PanelPosition {
    const { vw, vh } = vp

    const spaceBelow = vh - triggerRect.bottom - VIEWPORT_PADDING
    const spaceAbove = triggerRect.top - VIEWPORT_PADDING
    const spaceRight = vw - triggerRect.left - VIEWPORT_PADDING
    const spaceLeft = triggerRect.right - VIEWPORT_PADDING

    const side =
        preferredSide === 'bottom'
            ? spaceBelow >= MIN_PANEL_SIZE
                ? 'bottom'
                : 'top'
            : spaceAbove >= MIN_PANEL_SIZE
              ? 'top'
              : 'bottom'

    const top = side === 'bottom' ? triggerRect.bottom : undefined
    const maxHeight = side === 'bottom' ? spaceBelow : spaceAbove

    const align =
        preferredAlign === 'start'
            ? spaceRight >= MIN_PANEL_SIZE
                ? 'start'
                : 'end'
            : spaceLeft >= MIN_PANEL_SIZE
              ? 'end'
              : 'start'

    const left = align === 'start' ? triggerRect.left : undefined
    const maxWidth = align === 'start' ? spaceRight : spaceLeft

    const finalTop = top ?? triggerRect.top - maxHeight
    const finalLeft = left ?? triggerRect.right - maxWidth

    return {
        top: Math.max(VIEWPORT_PADDING, finalTop),
        left: Math.max(VIEWPORT_PADDING, finalLeft),
        maxWidth: Math.min(maxWidth, vw - 2 * VIEWPORT_PADDING),
        maxHeight: Math.min(maxHeight, vh - 2 * VIEWPORT_PADDING),
        side,
        align
    }
}

/**
 * Max fraction of the parent panel width the submenu may overlap when it
 * needs more room than the gap between the parent edge and the viewport.
 */
const MAX_PARENT_OVERLAP_RATIO = 0.5

export function computeSubmenuPosition(
    parentItemRect: DOMRect,
    parentPanelRect: DOMRect,
    vp: ViewportSize,
    minWidth = 0
): SubmenuPosition {
    const { vw, vh } = vp

    const spaceRight = vw - parentPanelRect.right - VIEWPORT_PADDING
    const spaceLeft = parentPanelRect.left - VIEWPORT_PADDING

    const side =
        spaceRight >= MIN_PANEL_SIZE
            ? 'right'
            : spaceLeft >= MIN_PANEL_SIZE
              ? 'left'
              : spaceRight >= spaceLeft
                ? 'right'
                : 'left'

    const gapWidth = side === 'right' ? spaceRight : spaceLeft
    const maxOverlap = parentPanelRect.width * MAX_PARENT_OVERLAP_RATIO
    const overlap = minWidth > gapWidth ? Math.min(minWidth - gapWidth, maxOverlap) : 0

    let left: number
    let maxWidth: number

    if (side === 'right') {
        left = parentPanelRect.right - overlap
        maxWidth = gapWidth + overlap
    } else {
        const baseLeft = parentPanelRect.left - gapWidth
        left = Math.max(VIEWPORT_PADDING, baseLeft - overlap)
        maxWidth = gapWidth + overlap
    }

    maxWidth = Math.min(maxWidth, vw - 2 * VIEWPORT_PADDING)

    if (minWidth > maxWidth) {
        const extra = minWidth - maxWidth
        maxWidth = minWidth
        if (side === 'right') {
            left = Math.max(VIEWPORT_PADDING, left - extra)
        }
    }

    const rightEdge = left + maxWidth
    const viewportRight = vw - VIEWPORT_PADDING
    if (rightEdge > viewportRight) {
        left = Math.max(VIEWPORT_PADDING, left - (rightEdge - viewportRight))
    }

    maxWidth = Math.min(maxWidth, vw - 2 * VIEWPORT_PADDING)

    const spaceBelow = vh - parentItemRect.top - VIEWPORT_PADDING
    const spaceAbove = parentItemRect.bottom - VIEWPORT_PADDING

    let top: number
    let maxHeight: number

    if (spaceBelow >= MIN_PANEL_SIZE) {
        top = parentItemRect.top
        maxHeight = spaceBelow
    } else {
        top = Math.max(VIEWPORT_PADDING, parentItemRect.bottom - MIN_PANEL_SIZE)
        maxHeight = spaceAbove
    }

    return {
        top: Math.max(VIEWPORT_PADDING, top),
        left,
        maxWidth,
        maxHeight: Math.min(maxHeight, vh - 2 * VIEWPORT_PADDING),
        side
    }
}

export function nextEnabledIndex(
    items: { disabled?: boolean }[],
    current: number,
    direction: 1 | -1
): number {
    const len = items.length
    if (len === 0) return -1

    let idx = current + direction
    let attempts = 0
    while (attempts < len) {
        if (idx < 0) idx = len - 1
        if (idx >= len) idx = 0
        if (!items[idx].disabled) return idx
        idx += direction
        attempts++
    }
    return current
}

export function firstEnabledIndex(items: { disabled?: boolean }[]): number {
    return items.findIndex(i => !i.disabled)
}

export function lastEnabledIndex(items: { disabled?: boolean }[]): number {
    for (let i = items.length - 1; i >= 0; i--) {
        if (!items[i].disabled) return i
    }
    return -1
}
