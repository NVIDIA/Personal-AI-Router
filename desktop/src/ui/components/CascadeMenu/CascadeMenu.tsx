// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import type { CascadeMenuItem, CascadeMenuProps } from '@/ui/types/cascade-menu'
import { SUBMENU_OPEN_DELAY } from '@/ui/types/cascade-menu'
import { usePositionScheduler } from '@/ui/hooks/usePositionScheduler'
import { useMenuKeyboard } from '@/ui/hooks/useMenuKeyboard'
import MenuPanel from './MenuPanel'
import './CascadeMenu.css'

function getPortalContainer(trigger: HTMLElement | null): HTMLElement {
    if (!trigger) return document.body
    const modalContainer = trigger.closest('dialog, [role="dialog"], .nv-modal-content')
    if (modalContainer instanceof HTMLElement) return modalContainer
    return document.body
}

function portalOffset(container: HTMLElement): { top: number; left: number } {
    if (container === document.body) return { top: 0, left: 0 }
    const rect = container.getBoundingClientRect()
    return { top: rect.top, left: rect.left }
}

export default function CascadeMenu({
    items,
    checkboxMode = false,
    trigger,
    onOpenChange,
    side = 'bottom',
    align = 'start'
}: CascadeMenuProps) {
    const [open, setOpen] = useState(false)
    const triggerRef = useRef<HTMLDivElement>(null)
    const portalRef = useRef<HTMLDivElement>(null)
    const rootPanelRef = useRef<HTMLDivElement>(null)
    const subPanelRef = useRef<HTMLDivElement>(null)
    const hoverTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

    const [activeSubmenuItemId, setActiveSubmenuItemId] = useState<string | null>(null)

    const { rootPosition, submenuPosition, requestSubmenuUpdate } = usePositionScheduler(
        triggerRef,
        rootPanelRef,
        open,
        side,
        align
    )

    const activeSubmenuItem = useMemo(
        () => items.find(i => i.id === activeSubmenuItemId) ?? null,
        [items, activeSubmenuItemId]
    )

    const submenuItems = useMemo(() => activeSubmenuItem?.children ?? [], [activeSubmenuItem])

    const submenuCheckbox = activeSubmenuItem?.childrenCheckboxMode ?? false
    const submenuSlotBefore = activeSubmenuItem?.childrenSlotBefore
    const submenuSlotAfter = activeSubmenuItem?.childrenSlotAfter

    const getSubmenuItems = useCallback(() => submenuItems, [submenuItems])

    const doClose = useCallback(() => {
        setOpen(false)
        setActiveSubmenuItemId(null)
        requestSubmenuUpdate(null)
        onOpenChange?.(false)
    }, [onOpenChange, requestSubmenuUpdate])

    const handleSelect = useCallback(
        (item: CascadeMenuItem) => {
            item.onSelect?.()
            if (!checkboxMode && !submenuCheckbox) {
                doClose()
            }
        },
        [checkboxMode, submenuCheckbox, doClose]
    )

    const handleSubmenuSelect = useCallback(
        (item: CascadeMenuItem) => {
            item.onSelect?.()
            const parentItem = items.find(i => i.id === activeSubmenuItemId)
            if (!parentItem?.childrenCheckboxMode) {
                doClose()
            }
        },
        [items, activeSubmenuItemId, doClose]
    )

    const keyboard = useMenuKeyboard(items, getSubmenuItems, {
        checkboxMode,
        onSelect: handleSelect,
        onClose: doClose
    })

    const openSubmenuForIndex = useCallback(
        (index: number) => {
            const item = items[index]
            if (!item?.children?.length) {
                setActiveSubmenuItemId(null)
                requestSubmenuUpdate(null)
                return
            }
            setActiveSubmenuItemId(item.id)
            requestSubmenuUpdate(index, item.childrenMinWidth)
        },
        [items, requestSubmenuUpdate]
    )

    const handleRootItemPointerEnter = useCallback(
        (index: number) => {
            keyboard.setFocusedIndex(index)

            if (hoverTimerRef.current) clearTimeout(hoverTimerRef.current)
            hoverTimerRef.current = setTimeout(() => {
                openSubmenuForIndex(index)
            }, SUBMENU_OPEN_DELAY)
        },
        [keyboard, openSubmenuForIndex]
    )

    const handleRootItemPointerDown = useCallback(
        (index: number) => {
            keyboard.setFocusedIndex(index)
            const item = items[index]
            if (item?.children?.length) {
                openSubmenuForIndex(index)
            }
        },
        [keyboard, items, openSubmenuForIndex]
    )

    const handleRootItemClick = useCallback(
        (item: CascadeMenuItem) => {
            if (item.children?.length) return
            handleSelect(item)
        },
        [handleSelect]
    )

    const handleSubItemPointerEnter = useCallback(
        (index: number) => {
            keyboard.setSubmenuFocusedIndex(index)
        },
        [keyboard]
    )

    const handleSubItemPointerDown = useCallback((_index: number) => {
        // Click handles selection for submenus
    }, [])

    const handleSubItemClick = useCallback(
        (item: CascadeMenuItem) => {
            handleSubmenuSelect(item)
        },
        [handleSubmenuSelect]
    )

    const handleRootPanelPointerLeave = useCallback(() => {
        if (hoverTimerRef.current) clearTimeout(hoverTimerRef.current)
    }, [])

    // Sync keyboard submenu state with hover submenu state
    useEffect(() => {
        if (keyboard.state.submenuOpen && keyboard.state.focusedIndex >= 0) {
            openSubmenuForIndex(keyboard.state.focusedIndex)
        }
    }, [keyboard.state.submenuOpen, keyboard.state.focusedIndex, openSubmenuForIndex])

    const handleTriggerClick = useCallback(() => {
        const next = !open
        setOpen(next)
        onOpenChange?.(next)
        if (!next) {
            keyboard.reset()
            setActiveSubmenuItemId(null)
            requestSubmenuUpdate(null)
        }
    }, [open, onOpenChange, keyboard, requestSubmenuUpdate])

    // Window-level keyboard handler in capture phase so it fires before
    // any other keydown listener (e.g. modal Escape) and can stop propagation.
    useEffect(() => {
        if (!open) return
        const handler = keyboard.onKeyDown
        window.addEventListener('keydown', handler, true)
        return () => window.removeEventListener('keydown', handler, true)
    }, [open, keyboard.onKeyDown])

    // Close on outside click
    useEffect(() => {
        if (!open) return

        const handleMouseDown = (e: MouseEvent) => {
            const target = e.target as Node
            if (triggerRef.current?.contains(target)) return
            if (portalRef.current?.contains(target)) return
            doClose()
            keyboard.reset()
        }

        document.addEventListener('mousedown', handleMouseDown, true)
        return () => document.removeEventListener('mousedown', handleMouseDown, true)
    }, [open, doClose, keyboard])

    useEffect(() => {
        return () => {
            if (hoverTimerRef.current) clearTimeout(hoverTimerRef.current)
        }
    }, [])

    const triggerContent = typeof trigger === 'function' ? trigger({ open }) : trigger
    const portalContainer = getPortalContainer(triggerRef.current)
    const offset = portalOffset(portalContainer)

    const rootPanelStyle = useMemo(() => {
        if (!rootPosition) return { display: 'none' as const }
        return {
            position: 'fixed' as const,
            top: rootPosition.top - offset.top,
            left: rootPosition.left - offset.left,
            maxWidth: rootPosition.maxWidth,
            maxHeight: rootPosition.maxHeight
        }
    }, [rootPosition, offset.top, offset.left])

    const submenuMinWidth = activeSubmenuItem?.childrenMinWidth

    const subPanelStyle = useMemo(() => {
        if (!submenuPosition) return { display: 'none' as const }
        return {
            position: 'fixed' as const,
            top: submenuPosition.top - offset.top,
            left: submenuPosition.left - offset.left,
            maxWidth: submenuPosition.maxWidth,
            maxHeight: submenuPosition.maxHeight,
            minWidth: submenuMinWidth
        }
    }, [submenuPosition, submenuMinWidth, offset.top, offset.left])

    return (
        <>
            <div ref={triggerRef} className="inline-flex" onClick={handleTriggerClick}>
                {triggerContent}
            </div>

            {open &&
                rootPosition &&
                createPortal(
                    <div ref={portalRef} className="cascade-menu-portal">
                        <MenuPanel
                            items={items}
                            checkboxMode={checkboxMode}
                            focusedIndex={keyboard.state.focusedIndex}
                            style={rootPanelStyle}
                            onItemPointerEnter={handleRootItemPointerEnter}
                            onItemPointerDown={handleRootItemPointerDown}
                            onItemClick={handleRootItemClick}
                            onPointerLeave={handleRootPanelPointerLeave}
                            panelRef={rootPanelRef}
                        />

                        {activeSubmenuItemId && submenuPosition && submenuItems.length > 0 && (
                            <MenuPanel
                                items={submenuItems}
                                checkboxMode={submenuCheckbox}
                                focusedIndex={keyboard.state.submenuFocusedIndex}
                                style={subPanelStyle}
                                slotBefore={submenuSlotBefore}
                                slotAfter={submenuSlotAfter}
                                onItemPointerEnter={handleSubItemPointerEnter}
                                onItemPointerDown={handleSubItemPointerDown}
                                onItemClick={handleSubItemClick}
                                panelRef={subPanelRef}
                            />
                        )}
                    </div>,
                    portalContainer
                )}
        </>
    )
}
