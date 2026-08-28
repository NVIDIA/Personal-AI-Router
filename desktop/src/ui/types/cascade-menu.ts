// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'

export interface CascadeMenuItem {
    id: string
    label: string | ReactNode
    children?: CascadeMenuItem[]
    childrenCheckboxMode?: boolean
    childrenMinWidth?: number
    childrenSlotBefore?: ReactNode
    childrenSlotAfter?: ReactNode
    checked?: boolean
    checkboxHidden?: boolean
    disabled?: boolean
    onSelect?: () => void
}

export interface CascadeMenuProps {
    items: CascadeMenuItem[]
    checkboxMode?: boolean
    trigger: ReactNode | ((props: { open: boolean }) => ReactNode)
    onOpenChange?: (open: boolean) => void
    side?: 'bottom' | 'top'
    align?: 'start' | 'end'
}

export interface PanelPosition {
    top: number
    left: number
    maxWidth: number
    maxHeight: number
    side: 'top' | 'bottom'
    align: 'start' | 'end'
}

export interface SubmenuPosition {
    top: number
    left: number
    maxWidth: number
    maxHeight: number
    side: 'left' | 'right'
}

export interface ViewportSize {
    vw: number
    vh: number
}

export const MENU_ITEM_HEIGHT = 32
export const VIEWPORT_PADDING = 8
export const MIN_PANEL_SIZE = 120
export const SUBMENU_OPEN_DELAY = 150
export const THROTTLE_MS = 16
