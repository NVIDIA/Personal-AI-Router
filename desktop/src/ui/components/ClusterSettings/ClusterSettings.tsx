// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { MODULAR_CLUSTER_MANAGER_PORT } from '@/shared/constants/modular-runtime'
import type { ClusterRouterStatus } from '@/shared/types/ws-channels'
import getErrorString from '@/shared/utils/get-error-string'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import { useInvitablePeers } from '@/ui/hooks/useInvitablePeers'
import { isElectron } from '@/ui/api/bootstrap'
import ActiveNode from './ActiveNode'
import AvailableNode from './AvailableNode'
import ClusterAliasRoutesCard from './ClusterAliasRoutesCard'
import ClusterEndpointCard from './ClusterEndpointCard'
import PendingInviteCard from './PendingInviteCard'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'

export default function ClusterSettings() {
    const { connected, selfId, clusterId } = useConnectionStore()
    const members = useClusterInvitationsStore(state => state.members)
    const pendingInvites = useClusterInvitationsStore(state => state.pendingInvites)
    const selfNode = useNodesStore(state => (selfId ? (state.nodes.get(selfId) ?? null) : null))
    const [error, setError] = useState<string | null>(null)

    // `selfId` is the node UUID, so members correlate on `nodeUuid` (the
    // hostname `m.id` is display only).
    const connectedMembers = useMemo(
        () => members.filter(m => m.nodeUuid !== selfId && m.state === 'member'),
        [members, selfId]
    )

    // The backend exposes no cluster-level "created date"; the closest durable
    // signal is this node's own membership timestamp (set when it created or
    // joined the cluster). Read it from the self entry in the roster.
    const joinedAtLabel = useMemo(() => {
        const self = selfId ? members.find(m => m.nodeUuid === selfId) : null
        if (!self?.joinedAt) return null
        return new Date(self.joinedAt).toLocaleDateString(undefined, {
            year: 'numeric',
            month: 'short',
            day: 'numeric'
        })
    }, [members, selfId])

    const pendingMembers = useMemo(
        () => members.filter(m => m.nodeUuid !== selfId && m.state !== 'member'),
        [members, selfId]
    )

    const availableNodes = useInvitablePeers()

    // What the proxy reports as bound on this computer, or `null` when that has
    // not been answered — read once per mount, and again after every endpoint
    // write, accepted or refused, because the card's apply answers from the
    // live listener rather than from a restart. Both halves of the Model
    // routing section below work from this one answer, so an endpoint that is
    // not listening neither claims an address nor gets an alias editor above
    // it. A failed read is "could not tell", which is not evidence of a
    // listener either.
    const [routerStatus, setRouterStatus] = useState<ClusterRouterStatus | null>(null)

    const refreshRouterStatus = useCallback(async () => {
        if (!isElectron) return
        try {
            setRouterStatus(await window.pairApi.cluster.getRouterStatus())
        } catch {
            setRouterStatus(null)
        }
    }, [])

    useEffect(() => {
        void refreshRouterStatus()
    }, [refreshRouterStatus])

    const handleLeaveCluster = useCallback(async () => {
        if (!selfId || !clusterId) return
        try {
            const result = await window.pairApi.nodes.removeMember(selfId)
            if (!result.removed) {
                setError('Failed to leave the cluster. Please try again.')
                return
            }
            setError(null)
        } catch (err) {
            setError(getErrorString(err))
        }
    }, [selfId, clusterId])

    return (
        <Stack gap="6" className="relative py-8 px-3">
            {error && <InlineErrorBanner severity="error" message={error} />}

            <Flex wrap="wrap" align="start" justify="start" gap="4">
                <div className="settings-card pair-paper p-4">
                    <Flex justify="between" gap="4" className="mt-1">
                        <Stack gap="1">
                            <Text kind="body/semibold/md">IP</Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {selfNode?.ipAddress ?? ''}
                            </Text>
                        </Stack>
                        <Stack gap="1">
                            <Text kind="body/semibold/md">Pairing port</Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {MODULAR_CLUSTER_MANAGER_PORT}
                            </Text>
                        </Stack>
                    </Flex>
                </div>

                {clusterId && (
                    <div className="settings-card pair-paper p-4">
                        <Flex justify="between" align="center" gap="2">
                            <Stack gap="1">
                                <Text kind="body/semibold/md">Joined</Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {joinedAtLabel ?? '—'}
                                </Text>
                            </Stack>
                            <Button
                                kind="tertiary"
                                color="danger"
                                size="small"
                                disabled={!connected || !selfId}
                                onClick={handleLeaveCluster}
                            >
                                Leave
                            </Button>
                        </Flex>
                    </div>
                )}
            </Flex>

            {pendingInvites.length > 0 && (
                <Stack gap="2">
                    <Text kind="body/semibold/md" className="px-1">
                        Pending invites
                    </Text>

                    {pendingInvites.map(invite => (
                        <PendingInviteCard invite={invite} key={invite.inviteId} />
                    ))}
                </Stack>
            )}

            {connectedMembers.length > 0 && (
                <Stack gap="2">
                    <Text kind="body/semibold/md" className="px-1">
                        Connected nodes
                    </Text>

                    {connectedMembers.map(node => (
                        <ActiveNode node={node} key={node.nodeUuid} />
                    ))}
                </Stack>
            )}

            {pendingMembers.length > 0 && (
                <Stack gap="2">
                    <Text kind="body/semibold/md" className="px-1">
                        Pending
                    </Text>

                    {pendingMembers.map(node => (
                        <ActiveNode node={node} key={node.nodeUuid} />
                    ))}
                </Stack>
            )}

            {availableNodes.length > 0 && (
                <Stack gap="2">
                    <Text kind="body/semibold/md" className="px-1">
                        Available nodes to add
                    </Text>

                    {availableNodes.map(node => (
                        <AvailableNode node={node} key={node.id} />
                    ))}
                </Stack>
            )}

            {/* Both halves of cross-node model routing. The endpoint's own
                controls are always here because they decide whether an endpoint
                exists at all; what routes by name is listed once one answers,
                because a name with no endpoint to be typed at is not a route.
                Listing rather than editing is deliberate: a node's alias document
                is edited on that node's card, and an editor gated on the listener
                would leave an operator with the endpoint switched off unable to fix
                a name. */}
            <Stack gap="2">
                <Text kind="body/semibold/md" className="px-1">
                    Model routing
                </Text>
                <ClusterEndpointCard
                    status={routerStatus}
                    onChanged={() => void refreshRouterStatus()}
                />
                {routerStatus?.running === true && <ClusterAliasRoutesCard />}
            </Stack>
        </Stack>
    )
}
