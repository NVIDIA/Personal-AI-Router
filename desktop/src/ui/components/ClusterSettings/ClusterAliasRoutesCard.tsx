// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useState } from 'react'
import { Panel, Spinner, Stack, Text } from '@nvidia/foundations-react-core'
import { isElectron } from '@/ui/api/bootstrap'
import {
    ALIAS_VIEW_REFRESH_MS,
    aliasViewAfterFailedRead,
    type ClusterAliasView,
    foreignAliasEntries
} from '@/ui/utils/cluster-alias-view'

/**
 * Which aliases this computer's cluster endpoint routes by that were written on
 * other machines — a diagnostic, and read-only by construction.
 *
 * There is no alias editor here. A node's list belongs to that node, so its card
 * in the Overview is where it is edited, this computer's included; a second editor
 * for one document would mean two operators replacing the same whole document
 * without seeing each other (decisions D20 and D21).
 *
 * What this card answers is a question no node card can: not what a given node
 * holds, but what a caller can send *this* endpoint right now, and which machine
 * wrote each of those names. It is the router's own merged view,
 * with this node's rows left out because they are on this computer's own card. A
 * definition whose model is offline contributes nothing, which leaves the name to
 * the machines that can serve it, so a row here means the route
 * works at the moment it was read.
 *
 * Mounted only while the endpoint is running, which is honest rather than
 * tidy — with no listener nothing routes from this computer, so an empty list
 * would be a wrong answer rather than a quiet one. It is why the section's copy
 * says so: the editor moved out of this card, and the gate no longer decides
 * whether an operator can edit anything.
 *
 * The list follows the cluster while the card is open, because the relay rewrites
 * the router's directory on a sweep and pushes nothing to the UI. Switching tabs
 * or closing Settings unmounts the card, which clears the timer.
 */

export default function ClusterAliasRoutesCard(): React.JSX.Element {
    const [aliasView, setAliasView] = useState<ClusterAliasView>({ state: 'loading' })

    const reloadView = useCallback(async (quiet = false) => {
        if (!isElectron) return
        if (!quiet) setAliasView({ state: 'loading' })
        try {
            setAliasView({
                state: 'ready',
                entries: await window.pairApi.cluster.getRouterAliases()
            })
        } catch {
            // A quiet failure keeps the rows already shown; only a first read
            // that fails says "could not be read".
            setAliasView(previous => aliasViewAfterFailedRead(previous, quiet))
        }
    }, [])

    useEffect(() => {
        void reloadView()
    }, [reloadView])

    useEffect(() => {
        const timer = setInterval(() => void reloadView(true), ALIAS_VIEW_REFRESH_MS)
        return () => clearInterval(timer)
    }, [reloadView])

    // Only names written elsewhere belong here: this node's own rows are on its
    // own card, and repeating them would read like somebody else's claim on a
    // name the operator is already looking at.
    const foreignEntries = useMemo(
        () => (aliasView.state === 'ready' ? foreignAliasEntries(aliasView.entries) : []),
        [aliasView]
    )

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="2">
                <Stack gap="1">
                    <Text kind="body/semibold/md">Aliases added on other nodes</Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Names this computer&apos;s cluster endpoint routes by that were written on
                        another node, listed while the model each stands for is reachable from here.
                        This machine&apos;s own aliases are on its card in the Overview, and so is
                        every other node&apos;s: edit a row on the node that added it.
                    </Text>
                </Stack>

                {aliasView.state === 'loading' && (
                    <Spinner size="small" aria-label="Loading cluster aliases" />
                )}
                {aliasView.state === 'unavailable' && (
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        The cluster alias view could not be read.
                    </Text>
                )}
                {aliasView.state === 'ready' && foreignEntries.length === 0 && (
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        No aliases from other nodes.
                    </Text>
                )}
                {aliasView.state === 'ready' &&
                    foreignEntries.map(entry => (
                        <Panel
                            key={`${entry.originNode}-${entry.name}`}
                            density="compact"
                            elevation="high"
                        >
                            <Stack gap="1">
                                <Text
                                    kind="body/regular/sm"
                                    className="font-mono pair-underscore-safe"
                                >
                                    {entry.name}
                                </Text>
                                <Text
                                    kind="body/regular/sm"
                                    className="text-subtle-color font-mono pair-underscore-safe"
                                >
                                    {entry.model}
                                </Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Added on {entry.originHost || 'another node'}
                                </Text>
                            </Stack>
                        </Panel>
                    ))}
            </Stack>
        </div>
    )
}
