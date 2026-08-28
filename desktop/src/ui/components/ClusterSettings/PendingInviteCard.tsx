// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { Invite } from '@/shared/types/cluster'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'

/**
 * A single inbound (receiver-side) pending invite in Settings -> Cluster. Gives
 * the user a durable way to reopen the PIN prompt for an invite that was
 * dismissed or arrived while another was open (the fix for NVBugs 6459451),
 * plus an inline decline.
 */
export default function PendingInviteCard({ invite }: { invite: Invite }) {
    const setActiveInvite = useClusterInvitationsStore(state => state.setActiveInvite)
    const respondToInvite = useClusterInvitationsStore(state => state.respondToInvite)
    const [declining, setDeclining] = useState(false)

    const handleRespond = useCallback(() => {
        setActiveInvite(invite.inviteId)
    }, [setActiveInvite, invite.inviteId])

    const handleDecline = useCallback(async () => {
        setDeclining(true)
        try {
            await respondToInvite(invite.inviteId, false)
        } catch {
            /* main prunes the invite on resolution; nothing to surface here */
        }
        setDeclining(false)
    }, [respondToInvite, invite.inviteId])

    const receivedAt = new Date(invite.createdAt).toLocaleTimeString(undefined, {
        hour: 'numeric',
        minute: '2-digit'
    })

    return (
        <div className="settings-card node-item-card pair-paper p-4">
            <Flex align="center" justify="between" gap="2">
                <Stack gap="1">
                    <Text kind="body/bold/sm" className="uppercase">
                        {invite.fromNodeName || invite.fromNodeId}
                    </Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Invited you to {invite.clusterFriendlyName || 'their cluster'} ·{' '}
                        {receivedAt}
                    </Text>
                </Stack>

                <Flex align="center" gap="2">
                    <Button
                        kind="secondary"
                        size="small"
                        onClick={handleDecline}
                        disabled={declining}
                    >
                        Decline
                    </Button>
                    <Button
                        kind="primary"
                        size="small"
                        color="brand"
                        onClick={handleRespond}
                        disabled={declining}
                    >
                        Enter PIN
                    </Button>
                </Flex>
            </Flex>
        </div>
    )
}
