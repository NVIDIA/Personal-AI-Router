// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo } from 'react'
import { Checkbox, Text } from '@nvidia/foundations-react-core'
import { KeyboardArrowRight } from '@/ui/components/icons'
import type { CascadeMenuItem } from '@/ui/types/cascade-menu'
import { useTitleWhenOverflow } from '@/ui/hooks/useTitleWhenOverflow'

interface MenuItemRowProps {
    item: CascadeMenuItem
    index: number
    focused: boolean
    checkboxMode: boolean
    className?: string
}

const MenuItemRow = memo(function MenuItemRow({
    item,
    index,
    focused,
    checkboxMode,
    className
}: MenuItemRowProps) {
    const hasChildren = !!item.children?.length
    const nameTitleRef = useTitleWhenOverflow(typeof item.label === 'string' ? item.label : '')

    return (
        <div
            role={checkboxMode && !item.checkboxHidden ? 'menuitemcheckbox' : 'menuitem'}
            aria-checked={checkboxMode && !item.checkboxHidden ? item.checked : undefined}
            aria-disabled={item.disabled || undefined}
            aria-haspopup={hasChildren ? 'menu' : undefined}
            data-menu-index={index}
            data-vlist-index={index}
            className={`${className ? className : ''} cascade-menu-item${focused ? ' cascade-menu-item-focused' : ''}${item.disabled ? ' cascade-menu-item-disabled' : ''}`}
        >
            {checkboxMode && !item.checkboxHidden && (
                <Checkbox
                    checked={!!item.checked}
                    tabIndex={-1}
                    className="pointer-events-none shrink-0"
                    aria-label={typeof item.label === 'string' ? item.label : 'Menu item'}
                />
            )}
            <Text kind="body/regular/sm" className="truncate min-w-0 grow" ref={nameTitleRef}>
                {item.label}
            </Text>
            {hasChildren && (
                <KeyboardArrowRight style={{ fontSize: 14 }} className="shrink-0 opacity-60" />
            )}
        </div>
    )
})

export default MenuItemRow
