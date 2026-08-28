// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import {
    Button,
    Flex,
    ModalContent,
    ModalDialog,
    ModalHeading,
    ModalRoot,
    Stack,
    Text
} from '@nvidia/foundations-react-core'

export function ConfirmModal({
    open,
    onOpenChange,
    title,
    message,
    confirmLabel,
    confirmColor = 'brand',
    onConfirm
}: {
    open: boolean
    onOpenChange: (open: boolean) => void
    title: string
    message: ReactNode
    confirmLabel: string
    confirmColor?: 'brand' | 'danger'
    onConfirm: () => void
}) {
    const handleClose = () => onOpenChange(false)

    return (
        <ModalRoot open={open} onOpenChange={onOpenChange}>
            <ModalDialog>
                <ModalContent className="no-drag-elements max-content-modal">
                    <ModalHeading>{title}</ModalHeading>
                    <Stack gap="4" className="-mt-2">
                        <Text kind="body/regular/sm" asChild>
                            <div>{message}</div>
                        </Text>
                        <Flex justify="end" gap="2">
                            <Button kind="secondary" size="small" onClick={handleClose}>
                                Cancel
                            </Button>
                            <Button
                                kind="primary"
                                size="small"
                                color={confirmColor}
                                onClick={() => {
                                    onOpenChange(false)
                                    onConfirm()
                                }}
                            >
                                {confirmLabel}
                            </Button>
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
