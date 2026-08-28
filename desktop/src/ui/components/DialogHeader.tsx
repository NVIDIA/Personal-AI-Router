// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { Button, Divider, Flex, Text } from '@nvidia/foundations-react-core'
import { Close } from './icons'

interface DialogHeaderProps {
    children: ReactNode
    onClose: () => void
    className?: string
    divider?: boolean
}

export function DialogHeader({ children, onClose, className, divider = true }: DialogHeaderProps) {
    return (
        <>
            <div className={className}>
                <Flex align="start" gap="4">
                    <Text kind="body/bold/lg" className="grow min-w-0">
                        {children}
                    </Text>
                    <Button
                        type="button"
                        kind="tertiary"
                        size="small"
                        onClick={onClose}
                        title="Close"
                        aria-label="Close"
                        className="shrink-0 -mt-1 -mr-2"
                    >
                        <Close style={{ fontSize: 16 }} />
                    </Button>
                </Flex>
            </div>
            {divider && <Divider className="w-[calc(100%+32px)] mx-[-16px]" />}
        </>
    )
}
