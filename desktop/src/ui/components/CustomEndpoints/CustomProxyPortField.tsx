// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import { Button, Flex, FormField, Stack, Text, TextInput } from '@nvidia/foundations-react-core'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { validateCustomProxyPort } from '@/ui/utils/custom-endpoints'
import type { CustomProxyPortState } from '@/shared/types/custom-endpoints'
import getErrorString from '@/shared/utils/get-error-string'

/**
 * The Custom facade's own port, edited the way an inference engine's proxy port
 * is edited: one field, one Apply, and the owning node's answer replaces
 * whatever was typed. The number belongs to the node rather than to any one
 * endpoint — every custom endpoint configured there is reached through this one
 * facade — so the field sits above the cards and edits whichever node the
 * section addresses: a peer's field moves that peer's proxy.
 *
 * Apply stays off, with the reason in the field's helper slot, until the text
 * names a port that would do something: not a number, outside 1-65535, or the
 * port already in use. That holds back a call which could only come back as a
 * refusal, and for a peer it holds back that call's whole relay budget too.
 *
 * Nothing listening (`running: false`) is reported as one sentence rather than a
 * locked field. The port is still worth reading — that is where the facade will
 * land, and where an external client would be pointed — but with no live proxy
 * there is nothing to rebind, and the owning node refuses the edit rather than
 * keeping a second copy of the number its proxy would ignore on the next start.
 * Saying so here saves a round trip whose only possible answer is "no".
 *
 * An untouched field follows the node: the section re-reads the live port with
 * the endpoint snapshot, so a facade that moved by itself — a lost bind race at
 * startup — shows up without a reload. A typed value stays until it is applied,
 * and a refused Apply keeps it so the next port can be typed over it.
 */

interface CustomProxyPortFieldProps {
    /** The addressed node's facade port as of the last snapshot read. */
    state: CustomProxyPortState
    /** Whose proxy this is: this node's id, or the peer the section addresses. */
    nodeId: string
    /** Hands the node's answer to the section, which is what the field then
     *  shows — where the proxy landed, not what was typed. */
    onPort: (state: CustomProxyPortState) => void
}

const HELP_TEXT = 'Every custom endpoint on this node is reached through this port.'

export function CustomProxyPortField({ state, nodeId, onPort }: CustomProxyPortFieldProps) {
    // null means untouched: show the node's live port. A string is a draft, and
    // only a draft can differ from it.
    const [draft, setDraft] = useState<string | null>(null)
    const [submitting, setSubmitting] = useState(false)
    const [error, setError] = useState<string | null>(null)

    const live = String(state.port)
    const shown = draft ?? live
    const refusal = validateCustomProxyPort(shown)
    const changed = shown !== live

    const apply = useCallback(async () => {
        if (submitting) return
        setSubmitting(true)
        setError(null)
        try {
            // The node's answer replaces the draft, so the field ends on where
            // the proxy actually landed.
            onPort(await window.pairApi.customEndpoints.setPort(Number(shown.trim()), nodeId))
            setDraft(null)
        } catch (cause) {
            // The owning node's reason names the port and who holds it, which is
            // the part worth acting on, so it is shown instead of swallowed — and
            // the draft stays, so the next port is typed over it rather than
            // retyped from memory.
            setError(getErrorString(cause))
        } finally {
            setSubmitting(false)
        }
    }, [shown, submitting, nodeId, onPort])

    if (!state.running) {
        return (
            <Text kind="body/regular/sm" className="text-subtle-color">
                Custom endpoints are reached through port {state.port} on this node, which nothing
                is listening on right now. Start the Custom proxy to change its port.
            </Text>
        )
    }

    return (
        <Stack gap="2">
            {error ? <InlineErrorBanner message={error} onClose={() => setError(null)} /> : null}
            <Flex gap="3" align="end">
                <FormField slotLabel="Proxy port" slotHelp={refusal ?? HELP_TEXT}>
                    <TextInput
                        size="small"
                        aria-label="Custom proxy port"
                        value={shown}
                        onValueChange={value => setDraft(value)}
                        inputMode="numeric"
                        disabled={submitting}
                    />
                </FormField>
                <Button
                    kind="primary"
                    color="brand"
                    size="small"
                    className="relative"
                    aria-label="Apply proxy port"
                    aria-busy={submitting}
                    disabled={!changed || !!refusal || submitting}
                    onClick={() => void apply()}
                >
                    <span className={submitting ? 'invisible' : undefined}>Apply</span>
                    {submitting && (
                        <span
                            className="absolute inset-0 flex items-center justify-center"
                            aria-hidden="true"
                        >
                            <span className="spinner-element-small" />
                        </span>
                    )}
                </Button>
            </Flex>
        </Stack>
    )
}
