// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { Banner, Text } from '@nvidia/foundations-react-core'

type InlineBannerSeverity = 'info' | 'warning' | 'error'

interface InlineErrorBannerProps {
    severity?: InlineBannerSeverity
    message: ReactNode
    onClose?: () => void
    children?: ReactNode
    className?: string
    style?: React.CSSProperties
}

export function InlineErrorBanner({
    severity = 'error',
    message,
    onClose,
    className,
    style,
    children
}: InlineErrorBannerProps) {
    return (
        <Banner
            kind="inline"
            status={severity}
            role={severity === 'error' ? 'alert' : 'status'}
            className={className}
            style={style}
            onClose={onClose}
            actionsPosition={children ? 'bottom' : 'right'}
            slotActions={children}
            attributes={{
                BannerHeader: { className: 'min-w-0 flex-1' }
            }}
        >
            <Text kind="body/regular/sm" className="whitespace-pre-wrap wrap-break-word">
                {message}
            </Text>
        </Banner>
    )
}
