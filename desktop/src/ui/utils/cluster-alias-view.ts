// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ClusterModelAliasViewEntry } from '@/shared/types/cluster'

/**
 * What the cluster alias section of the model-aliases card shows. `unavailable`
 * is deliberately distinct from a ready-and-empty view — "cannot read" must
 * never be drawn as "the cluster has none".
 */
export type ClusterAliasView =
    | { state: 'loading' }
    | { state: 'unavailable' }
    | { state: 'ready'; entries: ClusterModelAliasViewEntry[] }

/**
 * How often an open card re-reads the cluster alias view. The router learns of
 * a peer's alias edits from the relay on a 15 s sweep and pushes nothing to the
 * UI, so this timer is the difference between the section following the cluster
 * and the operator leaving the tab and coming back to force a read. Ten
 * seconds tracks the sweep closely enough to feel live without inventing a
 * second, push-shaped contract for one card.
 */
export const ALIAS_VIEW_REFRESH_MS = 10_000

/**
 * The rows the section shows from one router reply: only the definitions a peer
 * wrote. This node's own are the editable rows above the section, and repeating
 * them as read-only rows would read like somebody else's claim on a name the
 * operator is already looking at.
 *
 * The router answers with a row for every definition of a name that resolves,
 * so a name several machines serve appears several times — the same name in both
 * halves of the card is that name being served from both machines, and two peer
 * rows for one name are two other machines serving it. A
 * definition whose model is offline contributes no row, which is how a name
 * reaches the machines that can still serve it.
 */
export function foreignAliasEntries(
    entries: ClusterModelAliasViewEntry[]
): ClusterModelAliasViewEntry[] {
    return entries.filter(entry => entry.originNode !== '')
}

/**
 * What to show after a read fails. A background refresh keeps the rows it
 * already had: the router answers from a directory the relay rewrites on every
 * sweep, so one failed read usually means a peer mid-handshake and the next
 * refresh corrects it, and flashing "could not be read" every so often would
 * train the operator to ignore the section. A read with nothing to keep — the
 * first one, or one after an earlier failure — has no honest stale answer to
 * show and says so.
 */
export function aliasViewAfterFailedRead(
    previous: ClusterAliasView,
    quiet: boolean
): ClusterAliasView {
    if (quiet && previous.state === 'ready') return previous
    return { state: 'unavailable' }
}
