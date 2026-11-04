// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'
import { Divider, Stack, Text } from '@nvidia/foundations-react-core'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { CustomEndpointAddForm } from './CustomEndpointAddForm'
import { CustomEndpointCard } from './CustomEndpointCard'
import { CustomProxyPortField } from './CustomProxyPortField'
import { customEndpointsUnavailableLabel } from '@/ui/utils/custom-endpoints'
import type {
    CustomEndpointInfo,
    CustomProxyPortState
} from '@/shared/types/custom-endpoints'
import getErrorString from '@/shared/utils/get-error-string'

/**
 * Per-node "Custom" section: one card per endpoint the addressed node's
 * broker has configured, plus the add form below them. This stays a pull
 * surface: `custom:list` is read when it opens and re-read on a 30 s timer
 * while it stays open, and a card's Refresh or a successful save re-reads
 * through `custom:refresh`, whose reply re-probes the endpoint just touched
 * instead of trusting the cache — health is only truthful at read time, and
 * a stale flag behind a closed window is worse than none. What the timer
 * fixes is staleness inside an open window: the prober re-probes every 10 s
 * anyway, so a "checked 1 h ago" verdict was a stale read, not a stale
 * endpoint, and the same page keeps engine telemetry current by polling.
 * Removing is a plain button (no confirm modal): endpoint config is cheap to
 * re-type, unlike model deletion.
 *
 * `nodeId` addresses which broker answers — this node's, or, when it names a
 * paired peer, the peer's over the pinned control channel. A peer that
 * predates custom-endpoint management answers with a reason instead of a
 * list; the section then stays read-only and shows the reason, rather than
 * pretending an unknown configuration is an empty one. The two "cannot
 * serve" wordings (the relay's unified offline-or-unsupported answer and the
 * bridge's empty-reply guard) name a version the operator can act on, so
 * they render as the older-version sentence with the raw string kept as the
 * element's tooltip; every other refusal — trust, timeout — shows verbatim
 * because rewriting those would misname the trouble. No endpoint can be
 * added or removed through a device that cannot take the edit either way.
 *
 * The facade-port row rides the same read on the same timer: the port is one
 * number per node, shared by every endpoint below it, and a node that cannot
 * answer for its endpoints cannot answer for its proxy either. That is why one
 * failed read takes the whole section read-only — a panel that never reached the
 * node would otherwise offer to move a proxy it knows nothing about. A read that
 * answers no port (no Custom proxy on that node, or a peer too old for the port
 * plane) simply shows no field.
 */

/**
 * Cadence for re-reading the endpoint snapshot while the section is open.
 * Timed against the prober's own 10 s cycle: the read only reports what the
 * last probe saw, so anything faster than a third of that cycle would
 * re-report the same snapshot, and anything much slower would let verdicts
 * age back into the "this reads as broken" range the timer exists to kill.
 */
const CUSTOM_SECTION_AUTO_REFRESH_MS = 30_000

export function CustomEndpointSection({ nodeId }: { nodeId: string }) {
    // null until the first list lands: distinguishes "loading" from "none".
    const [entries, setEntries] = useState<CustomEndpointInfo[] | null>(null)
    const [removingId, setRemovingId] = useState<string | null>(null)
    const [error, setError] = useState<string | null>(null)
    // Only a peer-addressed read can land here: the local bridge answers a
    // broker that cannot respond with an empty list. While it is set, the
    // section shows the reason and hides every control.
    const [unavailable, setUnavailable] = useState<string | null>(null)
    // True while a section-level read is in flight, so auto ticks never
    // stack: a peer relay can hold one call for its whole 45 s budget, and
    // a second dial underneath it answers nothing new. User-initiated reads
    // (remove, saved edit) run through the same flag but are not gated by
    // it — a re-read after a config change answers a question the pending
    // read was not asking.
    const readInFlight = useRef(false)
    // The Custom facade's port, read with the endpoint snapshot. null means the
    // addressed node reported no proxy port at all, which renders no field:
    // this node's own 8000 says nothing about a peer's proxy.
    const [proxyPort, setProxyPort] = useState<CustomProxyPortState | null>(null)

    const refresh = useCallback(async () => {
        readInFlight.current = true
        try {
            // One snapshot, two questions: which endpoints this node routes, and
            // the port its Custom proxy fronts them on. Both are awaited
            // together so a peer round trip costs one relay pair instead of two,
            // and either answer missing means the section never reached this
            // node — the one state that has to go read-only, rather than offer
            // to add, remove, or re-port a configuration nobody read.
            const [snapshot, port] = await Promise.all([
                window.pairApi.customEndpoints.list(nodeId),
                window.pairApi.customEndpoints.getPort(nodeId)
            ])
            setEntries(snapshot)
            setProxyPort(port)
            setUnavailable(null)
        } catch (cause) {
            // The bridge forwards the peer's refusal verbatim ("device is
            // offline or does not support custom endpoint management", "not
            // clustered"), which is the actionable sentence here. The banner
            // translates only the version-skew wordings (see
            // customEndpointsUnavailableLabel); this state keeps the raw
            // string as the tooltip.
            setUnavailable(getErrorString(cause))
        } finally {
            readInFlight.current = false
        }
    }, [nodeId])

    useEffect(() => {
        void refresh()
        // Keep the whole snapshot current while the section is open, mirroring
        // how the node-info poller keeps engine status and model lists fresh.
        // It also keeps a peer that went quiet — or gets updated to a version
        // that supports the Custom plane — from staying pinned in the
        // read-only banner until the panel reopens: a recovered read clears
        // `unavailable` and the controls with it.
        const timer = setInterval(() => {
            if (readInFlight.current) return
            void refresh()
        }, CUSTOM_SECTION_AUTO_REFRESH_MS)
        return () => clearInterval(timer)
    }, [refresh])

    /**
     * After an add or a saved edit, re-read with the stored endpoint as the
     * refresh scope so its card shows a live verdict right away instead of
     * "Not checked yet" until the next background probe cycle. If that read
     * fails, the entries stay at their last truth — the endpoint was stored
     * either way, and a snapshot that could not be taken says nothing about
     * what is configured.
     */
    const applySnapshot = useCallback(
        async (scopeId: string) => {
            setError(null)
            try {
                setEntries(await window.pairApi.customEndpoints.refresh(scopeId, nodeId))
            } catch (cause) {
                setError(getErrorString(cause))
            }
        },
        [nodeId]
    )

    const remove = useCallback(
        async (entry: CustomEndpointInfo) => {
            setRemovingId(entry.id)
            setError(null)
            try {
                await window.pairApi.customEndpoints.remove(entry.id, nodeId)
                await refresh()
            } catch (cause) {
                setError(getErrorString(cause))
            } finally {
                setRemovingId(null)
            }
        },
        [nodeId, refresh]
    )

    return (
        <Stack gap="3" className="pt-2">
            <Divider />
            <Stack gap="1">
                <Text kind="body/semibold/sm">Custom</Text>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    External OpenAI-API-compatible endpoints, routed through this node’s proxy while
                    they answer health checks.
                </Text>
            </Stack>
            {error ? <InlineErrorBanner message={error} onClose={() => setError(null)} /> : null}
            {unavailable ? (
                <div title={unavailable}>
                    <InlineErrorBanner
                        severity="warning"
                        message={customEndpointsUnavailableLabel(unavailable)}
                    />
                </div>
            ) : null}
            {unavailable ? null : proxyPort ? (
                <CustomProxyPortField
                    state={proxyPort}
                    nodeId={nodeId}
                    onPort={state => setProxyPort(state)}
                />
            ) : null}
            {unavailable ? null : entries === null ? (
                <span
                    className="spinner-element-medium"
                    role="status"
                    aria-label="Loading custom endpoints"
                />
            ) : (
                entries.map(entry => (
                    <CustomEndpointCard
                        key={entry.id}
                        entry={entry}
                        nodeId={nodeId}
                        removing={removingId === entry.id}
                        onSnapshot={entries => setEntries(entries)}
                        onRemove={entry => void remove(entry)}
                    />
                ))
            )}
            {unavailable ? null : (
                <CustomEndpointAddForm
                    nodeId={nodeId}
                    onDone={stored => void applySnapshot(stored.id)}
                />
            )}
        </Stack>
    )
}
