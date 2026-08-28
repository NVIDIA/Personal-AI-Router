// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'

interface DropdownClickableInstance {
    open: boolean
    onOpenChange: (open: boolean) => void
    dropDownContentClassName: string
}

/**
 * Hook so the opening click does not immediately activate the first menu item.
 * Applies `pointer-events-none` on menu content until after the current
 * event turn (`setTimeout(0)`), then `pointer-events-auto`.
 *
 * Pass `dropDownContentClassName` to the menu surface rather than individual
 * menu items. Do not put `pointer-events-auto` on individual items; it overrides
 * the content wrapper and defeats this hook.
 */
export function useDropdownClickable(): { get: (key: string) => DropdownClickableInstance } {
    const [openMap, setOpenMap] = useState<Record<string, boolean>>({})
    const [clickableMap, setClickableMap] = useState<Record<string, boolean>>({})
    const timeoutRefs = useRef<Record<string, ReturnType<typeof setTimeout>>>({})
    /** Stable per-key handlers so menus are not re-triggered every render. */
    const onOpenChangeByKeyRef = useRef<Record<string, (open: boolean) => void>>({})

    useEffect(() => {
        const refs = timeoutRefs.current
        return () => {
            for (const id of Object.values(refs)) {
                clearTimeout(id)
            }
        }
    }, [])

    const get = useCallback(
        (key: string): DropdownClickableInstance => {
            const isOpen = openMap[key] ?? false
            const isClickable = clickableMap[key] ?? true

            if (!onOpenChangeByKeyRef.current[key]) {
                onOpenChangeByKeyRef.current[key] = (open: boolean) => {
                    setOpenMap(prev => ({ ...prev, [key]: open }))
                    if (open) {
                        setClickableMap(prev => ({ ...prev, [key]: false }))
                        const prevT = timeoutRefs.current[key]
                        if (prevT) clearTimeout(prevT)
                        timeoutRefs.current[key] = setTimeout(() => {
                            setClickableMap(prev => ({ ...prev, [key]: true }))
                            delete timeoutRefs.current[key]
                        }, 0)
                    } else {
                        const t = timeoutRefs.current[key]
                        if (t) {
                            clearTimeout(t)
                            delete timeoutRefs.current[key]
                        }
                        setClickableMap(prev => ({ ...prev, [key]: true }))
                    }
                }
            }

            return {
                open: isOpen,
                onOpenChange: onOpenChangeByKeyRef.current[key],
                dropDownContentClassName: `pointer-events-${isClickable ? 'auto' : 'none'}`
            }
        },
        [openMap, clickableMap]
    )

    return { get }
}
