// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type { ClusterModelAliasViewEntry } from '@/shared/types/cluster'
import {
    aliasViewAfterFailedRead,
    type ClusterAliasView,
    foreignAliasEntries
} from '@/ui/utils/cluster-alias-view'

// The router replies with every name this node routes by, its own included; the
// card's cluster section shows the peer-written rows beside the editable local
// ones. A local row has an empty origin — that is what distinguishes "mine" from
// "written elsewhere".
function localEntry(name: string, model: string): ClusterModelAliasViewEntry {
    return { name, model, originNode: '', originHost: '' }
}

function peerEntry(
    name: string,
    model: string,
    originNode = 'host-b',
    originHost = 'peer-one'
): ClusterModelAliasViewEntry {
    return { name, model, originNode, originHost }
}

function readyEntries(entries: ClusterModelAliasViewEntry[]): ClusterAliasView {
    return { state: 'ready', entries }
}

function described(view: ClusterAliasView): string {
    return view.state === 'ready'
        ? `ready:${view.entries.map(entry => entry.name).join(',')}`
        : view.state
}

describe('foreignAliasEntries', () => {
    it('keeps only the aliases a peer added', () => {
        const kept = foreignAliasEntries([
            localEntry('ghostly', 'google/gemma-4-12b'),
            peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b')
        ])

        expect(kept).toEqual([peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b')])
    })

    it('keeps nothing when every alias was written here', () => {
        expect(foreignAliasEntries([localEntry('ghostly', 'no-such-model')])).toEqual([])
    })

    it('keeps both rows when this node and a peer both serve the name', () => {
        // A name several machines define is served by all of them that can, so
        // the router answers with a row per definition. The card keeps only the
        // peer's, and the name showing in both halves is the point: the editable
        // row is this machine's configuration, the read-only row is the other
        // machine's, and a request for that name can be answered by either.
        const kept = foreignAliasEntries([
            localEntry('qwen38-flash-next', 'google/gemma-4-12b'),
            peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b')
        ])

        expect(kept).toEqual([peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b')])
    })

    it('keeps every peer row when one name has several peer definitions', () => {
        // Two other machines each packing the same weights under their own name
        // is two answers for one name, and the section says so twice, each row
        // naming the machine to edit. Collapsing them would hide half of what a
        // request for the name can reach.
        const kept = foreignAliasEntries([
            peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b', 'host-b', 'peer-one'),
            peerEntry('qwen38-flash-next', 'qwen38-flash-next-q4:latest', 'host-d', 'peer-two')
        ])

        expect(kept).toHaveLength(2)
        expect(kept.map(entry => entry.originHost)).toEqual(['peer-one', 'peer-two'])
    })
})

describe('aliasViewAfterFailedRead', () => {
    it('keeps the last cluster aliases when a background refresh fails', () => {
        const shown = readyEntries([peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b')])

        expect(described(aliasViewAfterFailedRead(shown, true))).toBe(described(shown))
    })

    it('says the view could not be read when the first read fails', () => {
        expect(aliasViewAfterFailedRead({ state: 'loading' }, false)).toEqual({
            state: 'unavailable'
        })
    })

    it('says the view could not be read when a refresh fails with nothing to keep', () => {
        // A quiet refresh that finds the section still loading, or already
        // failing, has no honest stale answer to hold onto.
        expect(aliasViewAfterFailedRead({ state: 'loading' }, true)).toEqual({
            state: 'unavailable'
        })
        expect(aliasViewAfterFailedRead({ state: 'unavailable' }, true)).toEqual({
            state: 'unavailable'
        })
    })

    it('says the view could not be read when the operator asks for it again', () => {
        // A read the operator caused — a save, or Retry — answers with the truth
        // of this attempt rather than with rows that may be minutes old.
        const shown = readyEntries([peerEntry('qwen38-flash-next', 'qwen/qwen3.6-35b-a3b')])

        expect(aliasViewAfterFailedRead(shown, false)).toEqual({ state: 'unavailable' })
    })
})
