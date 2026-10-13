// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState } from 'react'
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
import type { ManualServicePorts } from '@/shared/types/manual-node'

/**
 * The advanced port fields, in the order they are shown. A node on its default
 * ports needs none of them; a node behind a forwarded range, or one running an
 * engine somewhere else, needs exactly the one it moved.
 */
const PORT_FIELDS = [
    { key: 'nodeInfo', label: 'Node info', placeholder: '14318' },
    { key: 'cluster', label: 'Pairing', placeholder: '14321' },
    { key: 'ollama', label: 'Ollama', placeholder: '11434' },
    { key: 'lmstudio', label: 'LM Studio', placeholder: '1234' }
] as const

type PortField = (typeof PORT_FIELDS)[number]['key']

type PortDrafts = Partial<Record<PortField, string>>

/**
 * Reads the typed ports, dropping anything that is not a usable TCP port so a
 * half-typed field never travels as an override. Returns undefined when nothing
 * was set: an empty overrides object is not the same as none.
 */
function draftPorts(drafts: PortDrafts): ManualServicePorts | undefined {
    const ports: ManualServicePorts = {}
    let any = false
    for (const field of PORT_FIELDS) {
        const raw = drafts[field.key]?.trim()
        if (!raw) continue
        const port = Number(raw)
        if (!Number.isInteger(port) || port < 1 || port > 65535) continue
        ports[field.key] = port
        any = true
    }
    return any ? ports : undefined
}

interface AddNodeModalProps {
    open: boolean
    onOpenChange: (open: boolean) => void
}

export function AddNodeModal({ open, onOpenChange }: AddNodeModalProps) {
    useBlurOnOpen(open)
    const [manualIp, setManualIp] = useState('')
    const [showPorts, setShowPorts] = useState(false)
    const [portDrafts, setPortDrafts] = useState<PortDrafts>({})
    const pairing = useInvitePairing()
    const nodesThatCanBeAdded = useInvitablePeers()

    const ports = useMemo(() => draftPorts(portDrafts), [portDrafts])

    const handleOpenChange = useCallback(
        (next: boolean) => {
            setManualIp('')
            setShowPorts(false)
            setPortDrafts({})
            pairing.reset()
            onOpenChange(next)
        },
        [onOpenChange, pairing]
    )

    const handleManualInvite = useCallback(() => {
        const address = manualIp.trim()
        if (!address) return
        void pairing.start(address, ports)
    }, [manualIp, ports, pairing])

    const setPortDraft = useCallback((field: PortField, value: string) => {
        setPortDrafts(previous => ({ ...previous, [field]: value }))
    }, [])

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
                                    <FormField slotLabel="Address or host name" className="flex-1">
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
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    On a VPN or overlay network such as Tailscale, use the host name
                                    — for example gpu-box.tail1234.ts.net. A name is re-resolved on
                                    every check, so the node keeps working after it changes address.
                                </Text>

                                <Stack gap="2">
                                    <Button
                                        kind="tertiary"
                                        size="small"
                                        onClick={() => setShowPorts(open => !open)}
                                        disabled={inviteInFlight}
                                    >
                                        {showPorts ? 'Hide service ports' : 'Service ports'}
                                    </Button>
                                    {showPorts && (
                                        <Stack gap="2">
                                            <Text
                                                kind="body/regular/sm"
                                                className="text-subtle-color"
                                            >
                                                Leave a field empty to use this service&apos;s
                                                default port. Enter the address on its own above —
                                                ports belong here, not after a colon.
                                            </Text>
                                            <Flex gap="2" wrap="wrap">
                                                {PORT_FIELDS.map(field => (
                                                    <FormField
                                                        key={field.key}
                                                        slotLabel={field.label}
                                                        className="flex-1"
                                                    >
                                                        <TextInput
                                                            value={portDrafts[field.key] ?? ''}
                                                            onValueChange={value =>
                                                                setPortDraft(field.key, value)
                                                            }
                                                            placeholder={field.placeholder}
                                                            inputMode="numeric"
                                                            disabled={inviteInFlight}
                                                        />
                                                    </FormField>
                                                ))}
                                            </Flex>
                                        </Stack>
                                    )}
                                </Stack>

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
