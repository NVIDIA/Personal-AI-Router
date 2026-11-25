// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useState } from 'react'
import { Button, Spinner, Stack, Text } from '@nvidia/foundations-react-core'
import type { ModelAlias } from '@/shared/types/cluster'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import ModelAliasEditor from '@/ui/components/ModelAliases/ModelAliasEditor'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'

/**
 * Inline model alias section for a single node, rendered inside the overview
 * node card's expandable "Model aliases" accordion.
 *
 * This is the only place an alias document is edited — this computer's own list
 * included. A node owns its list, so the card that names the node is the card
 * that edits it; a second editor for one document would mean two operators
 * replacing the same whole document without ever seeing each other's (decisions
 * D20 and D21). Saving either replaces the whole document on the node that owns
 * it, so the rows a node holds after a save are exactly the rows that were on
 * screen.
 *
 * Two sources, deliberately, one save. This computer's own list is read from this
 * node's settings service, because that service is authoritative for it and an
 * announcement can lag the edit that caused it. A peer's list comes from what that
 * peer announced on the node record, since asking it directly would need a read
 * path across the cluster for a value this machine already receives with the
 * peer's model inventory. Both are saved through the same call: the broker holds
 * this node's cluster id, so naming this machine writes here and naming a peer
 * carries the document there, where the peer's own settings service checks it.
 *
 * Whether a list is available has three answers per node, and they are kept
 * apart. A peer that has not announced one — an older build, a model read that
 * has not answered, a peer this machine cannot reach to ask — gets no editor at
 * all, because an empty editor over a silence is one Save away from erasing
 * aliases this computer never showed. A peer that announced an empty list does get
 * the editor: that is a stated answer, and adding the first row to it is a
 * legitimate thing to do from here. This computer's list is never a silence: a
 * read that has not answered yet shows a spinner, and a read that failed shows a
 * banner and a Retry rather than an empty editor, since this card is now the only
 * way to reach those rows.
 *
 * Nothing here waits for this computer's cluster endpoint to be running. The
 * document belongs to the node whose card it is, and a peer routes by its own
 * endpoint regardless of whether this machine's is answering anything.
 */

export default function NodeModelAliases({ nodeId }: { nodeId: string }): React.JSX.Element | null {
    const node = useNodesStore(state => state.nodes.get(nodeId))
    const selfId = useConnectionStore(state => state.selfId)
    const isLocal = selfId !== null && selfId === nodeId

    const [ownAliases, setOwnAliases] = useState<ModelAlias[] | null>(null)
    const [error, setError] = useState<string | null>(null)
    const [busy, setBusy] = useState(false)
    // Bumped by Retry, because the read below runs once per node: a banner whose
    // close left nothing to re-read would be a dead end on the only editor there
    // is for this document.
    const [readAttempt, setReadAttempt] = useState(0)
    // Set once a write here landed on the node and not yet cleared: the
    // announcement that proves it arrives on that node's next model sweep, so
    // without this the section would look untouched by a save that worked.
    const [writtenHere, setWrittenHere] = useState(false)

    useEffect(() => {
        if (!isLocal) return
        let cancelled = false
        void (async () => {
            try {
                const aliases = await window.pairApi.cluster.getModelAliases()
                if (!cancelled) setOwnAliases(aliases)
            } catch (err) {
                // Deliberately not shown as an empty list: this section is the
                // one place this node's own list is stated, and "I could not
                // read it" is a different fact from "it holds none".
                if (!cancelled) {
                    setOwnAliases(null)
                    setError(getErrorString(err))
                }
            }
        })()
        return () => {
            cancelled = true
        }
    }, [isLocal, readAttempt])

    const write = useCallback(
        async (aliases: ModelAlias[]): Promise<ModelAlias[] | null> => {
            setBusy(true)
            try {
                // The node that owns the document validates and stores it and
                // answers once it has written it; it does not hand the list back,
                // so what was written is what the node holds.
                await window.pairApi.cluster.setNodeModelAliases(nodeId, aliases)
                setError(null)
                setWrittenHere(true)
                return aliases
            } catch (err) {
                // The refusal is that node's own sentence, kept as it came: the
                // machine that owns the list is the one explaining it.
                setError(getErrorString(err))
                return null
            } finally {
                setBusy(false)
            }
        },
        [nodeId]
    )

    // One value answers "is there a list to edit here", and `null` means no. The
    // two reasons read differently, so they get different renderings below: a
    // peer that has said nothing at all, and this machine's own list that the
    // settings service has not answered about (or answered "could not read").
    const announced = node?.aliases
    const initial: ModelAlias[] | null = isLocal ? ownAliases : (announced ?? null)

    // One guard for both, because the two answers differ in what they say, not in
    // shape: a peer that has announced nothing shows no editor at all, since
    // saving replaces everything a node has and that should never be done to a
    // list this computer has not seen; this machine's own list shows a spinner
    // until its settings service answers, and a banner with a Retry when it
    // refuses, because this card is the only way to reach those rows.
    if (initial === null) {
        if (!isLocal) {
            return (
                <Stack gap="3" className="pt-1 pb-2">
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {node?.name ?? 'This node'} has not reported its model aliases.
                    </Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        A node announces its own list along with its model list, so it appears here
                        once that read has answered. Until then there is nothing to show and no list
                        to edit: saving replaces everything a node has, and that should never be
                        done to a list this computer has not seen.
                    </Text>
                </Stack>
            )
        }
        return (
            <Stack gap="3" className="pt-1 pb-2">
                {error && (
                    <InlineErrorBanner
                        severity="error"
                        message={error}
                        onClose={() => setError(null)}
                    />
                )}
                {error ? (
                    <Stack gap="1">
                        <Button
                            kind="secondary"
                            size="small"
                            disabled={busy}
                            onClick={() => {
                                setError(null)
                                setReadAttempt(attempt => attempt + 1)
                            }}
                        >
                            Retry
                        </Button>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Reads this computer&apos;s own list again. No rows are offered until
                            that read answers, because saving replaces the whole list.
                        </Text>
                    </Stack>
                ) : (
                    <Spinner size="small" aria-label="Loading this node's model aliases" />
                )}
            </Stack>
        )
    }

    return (
        <Stack gap="3" className="pt-1 pb-2">
            <ModelAliasEditor
                initial={initial}
                busy={busy}
                error={error}
                onDismissError={() => setError(null)}
                onSave={write}
                notice={
                    isLocal ? (
                        <>
                            Your own aliases. A caller sends one of these names to this
                            computer&apos;s cluster endpoint and it routes the request wherever that
                            model runs, on this machine or any paired one. This list is announced to
                            paired nodes, so they route by these names on their own engines too.
                            Saving replaces the whole list your settings service holds.
                            {writtenHere ? ' Saved; the rows above are what you sent.' : ''}
                        </>
                    ) : (
                        <>
                            {node?.name ?? 'This node'}&apos;s own aliases. A caller sends one of
                            these names to that node&apos;s cluster endpoint and it routes the
                            request wherever that model runs. Saving sends this whole list to{' '}
                            {node?.name ?? 'that node'}, where its own settings service checks it
                            and stores it, replacing what it held.
                            {writtenHere
                                ? ' Saved there; the rows above are the whole list that node now holds.'
                                : ''}
                        </>
                    )
                }
            />
        </Stack>
    )
}
