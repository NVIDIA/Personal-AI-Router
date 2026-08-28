// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
    Button,
    Flex,
    ModalContent,
    ModalDialog,
    ModalRoot,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { InlineErrorBanner } from './InlineErrorBanner'
import { useBlurOnOpen } from '@/ui/hooks/useBlurOnOpen'
import getErrorString from '@/shared/utils/get-error-string'
import {
    formatClusterInviteError,
    INCORRECT_PIN_RECEIVER_MESSAGE
} from '@/ui/utils/cluster-invite-error'
import type { Invite } from '@/shared/types/cluster'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'

export function InviteApprovalModal() {
    const activeInviteId = useClusterInvitationsStore(state => state.activeInviteId)
    const pendingInvites = useClusterInvitationsStore(state => state.pendingInvites)
    const respondToInvite = useClusterInvitationsStore(state => state.respondToInvite)
    const clearActiveInvite = useClusterInvitationsStore(state => state.clearActiveInvite)

    const activeInvite = useMemo(
        () =>
            activeInviteId
                ? (pendingInvites.find(invite => invite.inviteId === activeInviteId) ?? null)
                : null,
        [activeInviteId, pendingInvites]
    )

    const [open, setOpen] = useState(false)
    // A latched copy of the invite being acted on, so a terminal "no longer
    // valid" message still renders after main prunes the invite from the set.
    const [shown, setShown] = useState<Invite | null>(null)
    // True once the invite reached a terminal, non-retryable state (a wrong PIN
    // evicts the backend session), so we show a Close-only message.
    const [terminal, setTerminal] = useState(false)
    const inputRef = useRef<HTMLInputElement>(null)
    useBlurOnOpen(open)
    const [pin, setPin] = useState('')
    const [processing, setProcessing] = useState(false)
    const [error, setError] = useState<string | null>(null)

    useEffect(() => {
        if (activeInvite) {
            setShown(activeInvite)
            setError(null)
            setPin('')
            setTerminal(false)
            setOpen(true)
            return
        }
        // The active invite was cleared externally (paired, declined, expired, or
        // TTL). Unless we are holding open a terminal message, close.
        if (!terminal) setOpen(false)
    }, [activeInvite, terminal])

    useEffect(() => {
        if (!open) return
        const id = requestAnimationFrame(() => inputRef.current?.focus())
        return () => cancelAnimationFrame(id)
    }, [open])

    const close = useCallback(() => {
        setOpen(false)
        setTerminal(false)
        // Clear the latched entry state so nothing carries into the next invite.
        // `shown` is intentionally left set so the modal can animate closed with
        // its content intact; the open effect re-hydrates it for a new invite.
        setPin('')
        setProcessing(false)
        setError(null)
        clearActiveInvite()
    }, [clearActiveInvite])

    // The PIN is a six-digit numeric code, so drop anything that isn't a digit
    // and cap the length at six as the user types or pastes.
    const handlePinChange = useCallback((value: string) => {
        setPin(value.replace(/\D/g, '').slice(0, 6))
    }, [])

    const handleAccept = useCallback(async () => {
        if (!shown) return
        if (pin.length !== 6) {
            setError('Enter the 6-digit PIN shown on the inviting node.')
            return
        }
        setProcessing(true)
        setError(null)
        try {
            const result = await respondToInvite(shown.inviteId, true, pin)
            if (result.state === 'failed') {
                // A failed completion after submitting a PIN is a wrong PIN in
                // practice (the backend confirms it with reason 'incorrect-pin').
                // It evicts the pairing session, so it cannot be retried here.
                setError(INCORRECT_PIN_RECEIVER_MESSAGE)
                setTerminal(true)
            } else {
                close()
            }
        } catch (err) {
            // A wrong PIN evicts the backend session, so a stale retry throws
            // `-32001: invite session evicted`. Normalize it (and any raw wrong-
            // PIN text) to friendly copy, and make it terminal so the user closes
            // and restarts instead of retrying into a dead session.
            const raw = getErrorString(err) ?? 'Failed to join cluster'
            setError(formatClusterInviteError(raw))
            if (raw.includes('-32001') || raw.toLowerCase().includes('invite session evicted')) {
                setTerminal(true)
            }
        }
        setProcessing(false)
    }, [shown, pin, respondToInvite, close])

    const handleDecline = useCallback(async () => {
        if (processing || !shown) {
            if (!shown) close()
            return
        }
        setProcessing(true)
        try {
            await respondToInvite(shown.inviteId, false)
        } catch {
            /* handled by service */
        }
        close()
        setProcessing(false)
    }, [processing, shown, respondToInvite, close])

    // Backdrop/Escape dismissal is disabled below; this only handles the explicit
    // header close control. When showing a terminal message there is nothing to
    // decline, so just close.
    const handleDismiss = useCallback(() => {
        if (terminal) {
            close()
            return
        }
        void handleDecline()
    }, [terminal, close, handleDecline])

    if (!shown) return null

    return (
        <ModalRoot open={open} onOpenChange={next => !next && handleDismiss()} hideCloseButton>
            <ModalDialog
                closeOnClickOutside={false}
                onEscapeKeyDown={event => event.preventDefault()}
            >
                <ModalContent className="no-drag-elements max-content-modal">
                    <DialogHeader onClose={handleDismiss}>Cluster invitation</DialogHeader>
                    <Stack gap="4" className="pt-2">
                        {error && (
                            <InlineErrorBanner message={error} onClose={() => setError(null)} />
                        )}
                        <Text kind="body/regular/sm">You have been invited to join a cluster:</Text>
                        <Flex align="center" gap="2">
                            <Text kind="body/semibold/sm" className="min-w-15">
                                From
                            </Text>
                            <Text kind="body/regular/sm">
                                {shown.fromNodeName || shown.fromNodeId}
                            </Text>
                        </Flex>
                        {!terminal && (
                            <Stack gap="1">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Enter the 6-digit PIN shown on{' '}
                                    {shown.fromNodeName || 'the inviting node'}.
                                </Text>
                                <TextInput
                                    ref={inputRef}
                                    value={pin}
                                    onValueChange={handlePinChange}
                                    placeholder="000000"
                                    inputMode="numeric"
                                    maxLength={6}
                                    disabled={processing}
                                    onKeyDown={event => {
                                        if (event.key === 'Enter') void handleAccept()
                                    }}
                                    style={{ fontFamily: 'monospace', letterSpacing: '0.3em' }}
                                />
                            </Stack>
                        )}
                        <Flex justify="end" gap="2">
                            {terminal ? (
                                <Button kind="primary" size="small" color="brand" onClick={close}>
                                    Close
                                </Button>
                            ) : (
                                <>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        onClick={handleDecline}
                                        disabled={processing}
                                    >
                                        Decline
                                    </Button>
                                    <Button
                                        kind="primary"
                                        size="small"
                                        color="brand"
                                        onClick={handleAccept}
                                        disabled={processing || pin.length !== 6}
                                    >
                                        {processing ? (
                                            <span
                                                className="spinner-element"
                                                role="status"
                                                aria-label=""
                                            />
                                        ) : (
                                            'Join cluster'
                                        )}
                                    </Button>
                                </>
                            )}
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
