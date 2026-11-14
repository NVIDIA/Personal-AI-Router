// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { CustomEndpointAddForm } from './CustomEndpointAddForm'
import { customEndpointDisplayLabel, customEndpointSummaryLabel } from '@/ui/utils/custom-endpoints'
import type { CustomEndpointInfo } from '@/shared/types/custom-endpoints'
import getErrorString from '@/shared/utils/get-error-string'

/**
 * One custom endpoint as a card: address, live verdict, and controls in the
 * header; the served models behind a read-only accordion; and — while
 * editing — the generalized add form behind a Settings accordion, mirroring
 * how managed engines expose their settings for the same address.
 *
 * Refresh re-reads through `custom:refresh` so the answer carries a fresh
 * probe verdict rather than the cache, and it replaces the whole list, not
 * just this card: the reply is a full snapshot. A failed refresh (a peer
 * that went quiet mid-session) shows a banner and changes nothing else — the
 * entries are still the last truth, and the 10 s background probe keeps
 * running regardless of what this card does.
 *
 * Saving edits lives in `CustomEndpointAddForm`; this component owns only
 * whether the editor is open. After a successful save the editor closes and
 * the card demands a fresh verdict for the id the add reply reported — for a
 * same-address edit that is this endpoint, and for an address change it is
 * the replacement entry that just took its place.
 */

interface CustomEndpointCardProps {
    entry: CustomEndpointInfo
    /** Which node's broker answers: this node, or the peer whose name this
     *  card's section addressed — the calls then ride the control channel. */
    nodeId?: string
    /** True while the section is removing this endpoint. */
    removing: boolean
    /** Replace the whole list with a refreshed snapshot. */
    onSnapshot: (entries: CustomEndpointInfo[]) => void
    onRemove: (entry: CustomEndpointInfo) => void
}

export function CustomEndpointCard({
    entry,
    nodeId,
    removing,
    onSnapshot,
    onRemove
}: CustomEndpointCardProps) {
    const [refreshing, setRefreshing] = useState(false)
    const [editing, setEditing] = useState(false)
    const [error, setError] = useState<string | null>(null)

    const runRefresh = useCallback(
        async (scopeId: string) => {
            setRefreshing(true)
            setError(null)
            try {
                onSnapshot(await window.pairApi.customEndpoints.refresh(scopeId, nodeId))
            } catch (cause) {
                setError(getErrorString(cause))
            } finally {
                setRefreshing(false)
            }
        },
        [nodeId, onSnapshot]
    )

    const handleSaved = useCallback(
        (stored: CustomEndpointInfo) => {
            setEditing(false)
            void runRefresh(stored.id)
        },
        [runRefresh]
    )

    const label = customEndpointDisplayLabel(entry)

    const modelsBody =
        entry.healthy === true && entry.models && entry.models.length > 0 ? (
            entry.models.map(model => (
                <Text key={model} kind="body/regular/sm" className="font-mono">
                    {model}
                </Text>
            ))
        ) : entry.healthy === true ? (
            <Text kind="body/regular/sm" className="text-subtle-color">
                The endpoint answered but served no models.
            </Text>
        ) : entry.healthy === false ? (
            <Text kind="body/regular/sm" className="text-subtle-color">
                The endpoint is not answering health checks, so its models are unknown.
            </Text>
        ) : (
            <Text kind="body/regular/sm" className="text-subtle-color">
                No health check has run yet. Press Refresh to check now.
            </Text>
        )

    return (
        <div className="pair-paper w-full p-4">
            <Stack gap="0" className="min-w-0">
                <Flex align="center" justify="between" gap="2">
                    <Stack gap="0" className="min-w-0">
                        <Text kind="body/semibold/sm" className="wrap-break-word">
                            {label}
                        </Text>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {customEndpointSummaryLabel(entry, new Date())}
                        </Text>
                    </Stack>
                    <Flex align="center" gap="1">
                        <Button
                            kind="secondary"
                            size="tiny"
                            onClick={() => void runRefresh(entry.id)}
                            disabled={refreshing}
                            aria-label={`Refresh ${label}`}
                        >
                            {refreshing ? (
                                <span
                                    className="spinner-element-small"
                                    role="status"
                                    aria-label=""
                                />
                            ) : (
                                'Refresh'
                            )}
                        </Button>
                        <Button
                            kind="secondary"
                            size="tiny"
                            onClick={() => setEditing(value => !value)}
                            disabled={refreshing}
                            aria-label={editing ? `Close editor for ${label}` : `Edit ${label}`}
                        >
                            {editing ? 'Done' : 'Edit'}
                        </Button>
                        <Button
                            kind="secondary"
                            size="tiny"
                            onClick={() => onRemove(entry)}
                            disabled={removing}
                            aria-label={`Remove ${label}`}
                        >
                            {removing ? (
                                <span
                                    className="spinner-element-small"
                                    role="status"
                                    aria-label=""
                                />
                            ) : (
                                'Remove'
                            )}
                        </Button>
                    </Flex>
                </Flex>
                {error ? (
                    <div className="pt-2">
                        <InlineErrorBanner message={error} onClose={() => setError(null)} />
                    </div>
                ) : null}
                <details className="pair-accordion translucent-bg-accordion">
                    <summary className="pair-accordion-summary">
                        <Text kind="body/semibold/sm">Models</Text>
                    </summary>
                    <Stack gap="1" className="p-3">
                        {modelsBody}
                    </Stack>
                </details>
                {editing ? (
                    <details open className="pair-accordion translucent-bg-accordion">
                        <summary className="pair-accordion-summary">
                            <Text kind="body/semibold/sm">Settings</Text>
                        </summary>
                        <div className="p-3">
                            <CustomEndpointAddForm
                                nodeId={nodeId}
                                editing={entry}
                                onDone={stored => void handleSaved(stored)}
                            />
                        </div>
                    </details>
                ) : null}
            </Stack>
        </div>
    )
}
