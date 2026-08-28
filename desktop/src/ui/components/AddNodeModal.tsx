// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import {
    Button,
    Divider,
    Flex,
    FormField,
    ModalContent,
    ModalDialog,
    ModalRoot,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { InvitePairingPanel } from './InvitePairingPanel'
import { useBlurOnOpen } from '@/ui/hooks/useBlurOnOpen'
import { useInvitePairing } from '@/ui/hooks/useInvitePairing'
import { useInvitablePeers } from '@/ui/hooks/useInvitablePeers'

interface AddNodeModalProps {
    open: boolean
    onOpenChange: (open: boolean) => void
}

export function AddNodeModal({ open, onOpenChange }: AddNodeModalProps) {
    useBlurOnOpen(open)
    const [manualIp, setManualIp] = useState('')
    const pairing = useInvitePairing()
    const nodesThatCanBeAdded = useInvitablePeers()

    const handleOpenChange = useCallback(
        (next: boolean) => {
            setManualIp('')
            pairing.reset()
            onOpenChange(next)
        },
        [onOpenChange, pairing]
    )

    const handleManualInvite = useCallback(() => {
        const ip = manualIp.trim()
        if (!ip) return
        void pairing.start(ip)
    }, [manualIp, pairing])

    const showPairing = pairing.invite !== null || pairing.error !== null
    const inviteInFlight = pairing.submitting || pairing.invite?.state === 'pending'

    return (
        <ModalRoot open={open} onOpenChange={handleOpenChange} hideCloseButton>
            <ModalDialog>
                <ModalContent className="no-drag-elements max-content-modal">
                    <DialogHeader onClose={() => handleOpenChange(false)}>
                        <Flex align="center" gap="2">
                            <span>Add node</span>
                            {pairing.submitting && (
                                <span className="spinner-element" role="status" aria-label="" />
                            )}
                        </Flex>
                    </DialogHeader>
                    <Stack gap="4" className="pt-2">
                        {showPairing ? (
                            <InvitePairingPanel
                                invite={pairing.invite}
                                error={pairing.error}
                                onReset={pairing.reset}
                                onCancel={() => void pairing.cancel()}
                                onDone={() => handleOpenChange(false)}
                            />
                        ) : (
                            <>
                                <Flex align="end" gap="2">
                                    <FormField slotLabel="IP address" className="flex-1">
                                        <TextInput
                                            value={manualIp}
                                            onValueChange={setManualIp}
                                            placeholder="192.168.1.100"
                                            onKeyDown={event => {
                                                if (event.key === 'Enter') handleManualInvite()
                                            }}
                                            disabled={inviteInFlight}
                                        />
                                    </FormField>
                                    <Button
                                        kind="primary"
                                        color="brand"
                                        onClick={handleManualInvite}
                                        disabled={!manualIp.trim() || inviteInFlight}
                                    >
                                        Invite
                                    </Button>
                                </Flex>

                                {nodesThatCanBeAdded.length > 0 && (
                                    <Stack gap="4" className="mt-1">
                                        <Divider />

                                        <Stack gap="1">
                                            {nodesThatCanBeAdded.map(node => (
                                                <Flex
                                                    key={node.id}
                                                    align="center"
                                                    justify="between"
                                                    gap="2"
                                                    className="py-1"
                                                >
                                                    <Stack gap="0">
                                                        <Text
                                                            kind="body/semibold/sm"
                                                            className="uppercase"
                                                        >
                                                            {node.name || node.ipAddress}
                                                        </Text>
                                                        <Text
                                                            kind="body/regular/sm"
                                                            className="text-subtle-color"
                                                        >
                                                            {node.clustered
                                                                ? 'In another cluster'
                                                                : `${node.ipAddress}:${node.port}`}
                                                        </Text>
                                                    </Stack>
                                                    <Button
                                                        kind="primary"
                                                        color="brand"
                                                        size="small"
                                                        onClick={() =>
                                                            void pairing.start(node.ipAddress)
                                                        }
                                                        // A node already in a cluster cannot join
                                                        // another; the backend would reject the
                                                        // invite (`rejected` / `already-clustered`).
                                                        disabled={inviteInFlight || node.clustered}
                                                    >
                                                        Invite
                                                    </Button>
                                                </Flex>
                                            ))}
                                        </Stack>
                                    </Stack>
                                )}
                            </>
                        )}
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
