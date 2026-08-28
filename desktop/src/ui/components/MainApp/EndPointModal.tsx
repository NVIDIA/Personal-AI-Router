// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ModalContent, ModalDialog, ModalRoot } from '@nvidia/foundations-react-core'
import EndpointContent from '@/ui/components/EndpointContent'
import { DialogHeader } from '@/ui/components/DialogHeader'
import { useConnectionStore } from '@/ui/stores/connection.store'

export default function EndPointModal({
    open,
    onOpenChange
}: {
    open: boolean
    onOpenChange: (open: boolean) => void
}) {
    const selfId = useConnectionStore(state => state.selfId)

    if (!selfId) return null

    return (
        <ModalRoot open={open} onOpenChange={onOpenChange} hideCloseButton>
            <ModalDialog>
                <ModalContent className="no-drag-elements max-content-modal">
                    <DialogHeader
                        className="no-drag-elements"
                        divider={true}
                        onClose={() => onOpenChange(false)}
                    >
                        API endpoints
                    </DialogHeader>
                    <EndpointContent selfId={selfId} />
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
