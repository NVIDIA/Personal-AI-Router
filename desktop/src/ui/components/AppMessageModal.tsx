// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    Button,
    Flex,
    ModalContent,
    ModalDialog,
    ModalRoot,
    Stack,
    Text
} from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import type { OverviewMessage } from '@/shared/types/overview'

/**
 * In-Overview replacement for OS notifications. Shows a one-off message (e.g.
 * "Update available", "Service stopped unexpectedly") with an optional action
 * that navigates to the relevant Settings sub-tab.
 */
export function AppMessageModal({
    message,
    onClose
}: {
    message: OverviewMessage
    onClose: () => void
}) {
    const openSettings = useOverviewUiStore(s => s.openSettings)

    const handleAction = (): void => {
        if (message.action === 'open-update' || message.action === 'open-service') {
            openSettings('service')
        }
        onClose()
    }

    return (
        <ModalRoot
            open
            onOpenChange={next => {
                if (!next) onClose()
            }}
            hideCloseButton
        >
            <ModalDialog>
                <ModalContent className="no-drag-elements max-w-md">
                    <DialogHeader onClose={onClose}>
                        <Text kind="title/sm" className="pr-6">
                            {message.title}
                        </Text>
                    </DialogHeader>
                    <Stack gap="4" className="pt-2">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {message.body}
                        </Text>
                        <Flex justify="end" gap="2">
                            <Button kind="secondary" onClick={onClose}>
                                Dismiss
                            </Button>
                            {message.actionLabel && (
                                <Button kind="primary" color="brand" onClick={handleAction}>
                                    {message.actionLabel}
                                </Button>
                            )}
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
