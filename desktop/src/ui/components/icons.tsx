// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ForwardRefExoticComponent, RefAttributes } from 'react'
import {
    ArrowDown,
    ArrowUp,
    ChartNoAxesColumn,
    Check as LucideCheck,
    ChevronDown,
    ChevronRight,
    CirclePlus,
    Copy,
    Download as LucideDownload,
    Ellipsis,
    ExternalLink,
    Filter,
    Globe,
    Minus,
    Search as LucideSearch,
    Settings,
    Square as LucideSquare,
    X,
    type LucideProps
} from 'lucide-react'

type LucideIconComponent = ForwardRefExoticComponent<
    Omit<LucideProps, 'ref'> & RefAttributes<SVGSVGElement>
>

function appIcon(Icon: LucideIconComponent) {
    return function AppIcon({ size = '1em', strokeWidth = 2.25, ...props }: LucideProps) {
        return (
            <Icon
                aria-hidden="true"
                focusable="false"
                size={size}
                strokeWidth={strokeWidth}
                {...props}
            />
        )
    }
}

export const AddCircle = appIcon(CirclePlus)
export const ArrowDownward = appIcon(ArrowDown)
export const ArrowUpward = appIcon(ArrowUp)
export const BarChartOutlined = appIcon(ChartNoAxesColumn)
export const Check = appIcon(LucideCheck)
export const Close = appIcon(X)
export const ContentCopy = appIcon(Copy)
export const Square = appIcon(LucideSquare)
export const Download = appIcon(LucideDownload)
export const ExpandMore = appIcon(ChevronDown)
export const FilterList = appIcon(Filter)
export const KeyboardArrowRight = appIcon(ChevronRight)
export const Minimize = appIcon(Minus)
export const MoreHoriz = appIcon(Ellipsis)
export const OpenInNew = appIcon(ExternalLink)
export const Public = appIcon(Globe)
export const Search = appIcon(LucideSearch)
export const SettingsOutlined = appIcon(Settings)

/**
 * Classic Windows "restore down" glyph (a front square with the back square's
 * top-right corner). lucide has no true restore icon, so this is a small custom
 * SVG drawn to match the native maximize/restore control.
 */
export function RestoreWindow({ size = '1em', strokeWidth = 2.25, ...props }: LucideProps) {
    return (
        <svg
            viewBox="0 0 24 24"
            width={size}
            height={size}
            fill="none"
            stroke="currentColor"
            strokeWidth={strokeWidth}
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
            focusable="false"
            {...props}
        >
            <rect x="4" y="8" width="12" height="12" rx="1.5" />
            <path d="M8 8V6.5A1.5 1.5 0 0 1 9.5 5H18a1 1 0 0 1 1 1v8.5a1.5 1.5 0 0 1-1.5 1.5H16" />
        </svg>
    )
}
