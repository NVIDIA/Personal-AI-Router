// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react'
import type { CustomEndpointInfo } from '@/shared/types/custom-endpoints'
import type { NodeItem } from '@/shared/types/nodes'

/**
 * Custom-engine snapshot per node id, collected by the tray's own
 * `customEndpoints.list` pulls. Absent key means no snapshot yet for that
 * node; an empty array means the node has no configured Custom endpoints.
 */
export type TrayCustomSnapshot = ReadonlyMap<string, readonly CustomEndpointInfo[]>

/**
 * Pull cadence while the tray could be visible. The window is persistent and
 * hidden on blur rather than destroyed, so a timer created per tray open
 * would keep firing while hidden; the 30s cadence also matches the Custom
 * endpooint section's health-poll frequency.
 */
const TRAY_CUSTOM_POLL_MS = 30_000

type NodeTarget = { nodeId: string; ip?: string }

function parseNodeTargets(targetKey: string): NodeTarget[] {
    if (targetKey.length === 0) return []
    return targetKey.split(';').map(entry => {
        const [nodeId, ip] = entry.split('|')
        return ip !== undefined && ip.length > 0 ? { nodeId, ip } : { nodeId }
    })
}

/**
 * Per-node Custom engine endpoints for the tray's cluster-status rows.
 *
 * Custom endpoint health has no push feed (unlike the other tray cluster
 * data), so the tray pulls `customEndpoints.list` itself: once when the
 * online node set changes, once when the tray window regains focus, and on a
 * 30s cadence only while the surface could be visible (focus + page visible).
 * A hidden tray is a background poller whose peer-addressed reads each hold a
 * control-channel lane for their whole timeout, so polling pauses while
 * hidden and resumes with a full cycle on the next focus.
 *
 * The effect keys on a serialized node/ip target string rather than the node
 * array identity: NodeItem fields change with each 5s metrics tick, and keying
 * on the array would burst-poll the relay on every tick. The IP rides along
 * because a re-discovered address changes which machine `custom:list` reads.
 *
 * A failed read keeps the previous snapshot: an error means "unknown", not
 * "no custom engines", and listing a peer can fail for the whole duration its
 * relayed read times out. Entries for nodes that left the tray's node set are
 * dropped.
 */
export function useTrayCustomEndpoints(onlineNodes: readonly NodeItem[]): TrayCustomSnapshot {
    const [snapshot, setSnapshot] = useState<TrayCustomSnapshot>(new Map())
    const snapshotRef = useRef(snapshot)
    snapshotRef.current = snapshot

    // Focus is the tray's proxy for "could be visible": macOS and Windows
    // hide the window on blur and focus it on open. [id|ip] joined, stable
    // across metrics ticks.
    const targetKey = onlineNodes.map(n => `${n.id}|${n.ipAddress}`).join(';')
    const [focused, setFocused] = useState(() =>
        typeof document === 'undefined' ? true : document.hasFocus()
    )

    useEffect(() => {
        const onFocus = (): void => setFocused(true)
        const onBlur = (): void => setFocused(false)
        const onVisibility = (): void => setFocused(document.hasFocus() && !document.hidden)
        window.addEventListener('focus', onFocus)
        window.addEventListener('blur', onBlur)
        document.addEventListener('visibilitychange', onVisibility)
        return () => {
            window.removeEventListener('focus', onFocus)
            window.removeEventListener('blur', onBlur)
            document.removeEventListener('visibilitychange', onVisibility)
        }
    }, [])

    useEffect(() => {
        const targets = parseNodeTargets(targetKey)
        let cancelled = false
        let inFlight = false

        const listCustom = (): void => {
            if (inFlight) return
            inFlight = true
            void (async () => {
                const next = new Map<string, readonly CustomEndpointInfo[]>()
                for (const target of targets) {
                    try {
                        const entries = await window.pairApi.customEndpoints.list(target.nodeId)
                        if (cancelled) return
                        // One member's offline or unreachable peer keeps its previous
                        // snapshot instead of dropping its rows to an unknown state.
                        const resolved = entries ?? snapshotRef.current.get(target.nodeId)
                        if (resolved !== undefined) next.set(target.nodeId, resolved)
                    } catch (error) {
                        if (cancelled) return
                        console.error('[tray] custom:list failed:', error)
                        const previous = snapshotRef.current.get(target.nodeId)
                        if (previous !== undefined) next.set(target.nodeId, previous)
                    }
                }
                inFlight = false
                if (!cancelled) setSnapshot(next)
            })()
        }

        listCustom()
        if (targets.length === 0 || !focused)
            return (): void => {
                cancelled = true
            }

        const timer = setInterval(listCustom, TRAY_CUSTOM_POLL_MS)
        return (): void => {
            cancelled = true
            clearInterval(timer)
        }
    }, [targetKey, focused])

    return snapshot
}
