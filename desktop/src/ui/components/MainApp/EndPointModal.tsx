// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { ModalContent, ModalDialog, ModalRoot } from '@nvidia/foundations-react-core'
import EndpointContent from '@/ui/components/EndpointContent'
import { DialogHeader } from '@/ui/components/DialogHeader'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { isElectron } from '@/ui/api/bootstrap'

export default function EndPointModal({
    open,
    onOpenChange
}: {
    open: boolean
    onOpenChange: (open: boolean) => void
}) {
    const selfId = useConnectionStore(state => state.selfId)
    /** Port the cluster routing endpoint answers on, or null when it does not. */
    const [clusterPort, setClusterPort] = useState<number | null>(null)

    // This modal stays mounted once opened, so the read hangs off the opening
    // rather than a mount: an endpoint started or lost while the window sat
    // closed should be right the moment somebody looks at it. A read that fails
    // lists nothing — the Endpoints list describes what is bound, and "could not
    // tell" is not evidence of a listener worth copying a URL for.
    useEffect(() => {
        if (!open || !isElectron) return

        let ignored = false
        const show = (port: number | null): void => {
            if (!ignored) setClusterPort(port)
        }

        window.pairApi.cluster
            .getRouterStatus()
            .then(status => show(status.running ? status.port : null))
            .catch(() => show(null))

        return () => {
            ignored = true
        }
    }, [open])

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
                    <EndpointContent selfId={selfId} clusterPort={clusterPort} />
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
