// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'
import type { CascadeMenuItem } from '@/ui/types/cascade-menu'
import { firstEnabledIndex, lastEnabledIndex, nextEnabledIndex } from '@/ui/utils/cascade-menu'

interface MenuKeyboardState {
    focusedIndex: number
    submenuOpen: boolean
    submenuFocusedIndex: number
}

interface UseMenuKeyboardOpts {
    checkboxMode: boolean
    onSelect: (item: CascadeMenuItem) => void
    onClose: () => void
}

interface UseMenuKeyboardReturn {
    state: MenuKeyboardState
    setFocusedIndex: (index: number) => void
    setSubmenuFocusedIndex: (index: number) => void
    openSubmenu: () => void
    closeSubmenu: () => void
    onKeyDown: (e: KeyboardEvent) => void
    reset: () => void
}

const HANDLED_KEYS = new Set([
    'ArrowDown',
    'ArrowUp',
    'ArrowLeft',
    'ArrowRight',
    'Escape',
    'Home',
    'End',
    'Enter',
    ' '
])

export function useMenuKeyboard(
    items: CascadeMenuItem[],
    getSubmenuItems: () => CascadeMenuItem[],
    opts: UseMenuKeyboardOpts
): UseMenuKeyboardReturn {
    const [focusedIndex, setFocusedIndex] = useState(-1)
    const [submenuOpen, setSubmenuOpen] = useState(false)
    const [submenuFocusedIndex, setSubmenuFocusedIndex] = useState(-1)
    const submenuOpenRef = useRef(false)

    const itemsRef = useRef(items)
    itemsRef.current = items
    const getSubmenuItemsRef = useRef(getSubmenuItems)
    getSubmenuItemsRef.current = getSubmenuItems
    const optsRef = useRef(opts)
    optsRef.current = opts

    const openSubmenu = useCallback(() => {
        setSubmenuOpen(true)
        submenuOpenRef.current = true
        const sub = getSubmenuItemsRef.current()
        setSubmenuFocusedIndex(firstEnabledIndex(sub))
    }, [])

    const closeSubmenu = useCallback(() => {
        setSubmenuOpen(false)
        submenuOpenRef.current = false
        setSubmenuFocusedIndex(-1)
    }, [])

    const reset = useCallback(() => {
        setFocusedIndex(-1)
        setSubmenuOpen(false)
        submenuOpenRef.current = false
        setSubmenuFocusedIndex(-1)
    }, [])

    const openSubmenuRef = useRef(openSubmenu)
    openSubmenuRef.current = openSubmenu

    const onKeyDown = useCallback(
        (e: KeyboardEvent) => {
            if (!HANDLED_KEYS.has(e.key)) return

            e.preventDefault()
            e.stopImmediatePropagation()

            const key = e.key
            const currentItems = itemsRef.current
            const currentOpts = optsRef.current

            if (submenuOpenRef.current) {
                const subItems = getSubmenuItemsRef.current()
                switch (key) {
                    case 'ArrowDown':
                        setSubmenuFocusedIndex(prev => nextEnabledIndex(subItems, prev, 1))
                        return
                    case 'ArrowUp':
                        setSubmenuFocusedIndex(prev => nextEnabledIndex(subItems, prev, -1))
                        return
                    case 'ArrowLeft':
                        closeSubmenu()
                        return
                    case 'Escape':
                        closeSubmenu()
                        return
                    case 'Home':
                        setSubmenuFocusedIndex(firstEnabledIndex(subItems))
                        return
                    case 'End':
                        setSubmenuFocusedIndex(lastEnabledIndex(subItems))
                        return
                    case 'Enter':
                    case ' ':
                        setSubmenuFocusedIndex(prev => {
                            if (prev >= 0 && prev < subItems.length) {
                                const item = subItems[prev]
                                if (!item.disabled) queueMicrotask(() => currentOpts.onSelect(item))
                            }
                            return prev
                        })
                        return
                }
                return
            }

            switch (key) {
                case 'ArrowDown':
                    setFocusedIndex(prev => {
                        if (prev === -1) return firstEnabledIndex(currentItems)
                        return nextEnabledIndex(currentItems, prev, 1)
                    })
                    return
                case 'ArrowUp':
                    setFocusedIndex(prev => {
                        if (prev === -1) return lastEnabledIndex(currentItems)
                        return nextEnabledIndex(currentItems, prev, -1)
                    })
                    return
                case 'ArrowRight':
                    setFocusedIndex(prev => {
                        const item = currentItems[prev]
                        if (item?.children?.length) {
                            queueMicrotask(() => openSubmenuRef.current())
                        }
                        return prev
                    })
                    return
                case 'Escape':
                    currentOpts.onClose()
                    return
                case 'Home':
                    setFocusedIndex(firstEnabledIndex(currentItems))
                    return
                case 'End':
                    setFocusedIndex(lastEnabledIndex(currentItems))
                    return
                case 'Enter':
                case ' ':
                    setFocusedIndex(prev => {
                        const item = currentItems[prev]
                        if (!item) return prev
                        if (item.children?.length) {
                            queueMicrotask(() => openSubmenuRef.current())
                        } else if (!item.disabled) {
                            queueMicrotask(() => currentOpts.onSelect(item))
                        }
                        return prev
                    })
                    return
            }
        },
        [closeSubmenu]
    )

    // Stable ref so CascadeMenu's useEffect doesn't churn
    const onKeyDownRef = useRef(onKeyDown)
    useEffect(() => {
        onKeyDownRef.current = onKeyDown
    }, [onKeyDown])

    const stableOnKeyDown = useCallback((e: KeyboardEvent) => {
        onKeyDownRef.current(e)
    }, [])

    return {
        state: { focusedIndex, submenuOpen, submenuFocusedIndex },
        setFocusedIndex,
        setSubmenuFocusedIndex,
        openSubmenu,
        closeSubmenu,
        onKeyDown: stableOnKeyDown,
        reset
    }
}
