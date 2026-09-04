// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'
import type { Invite } from '@/shared/types/cluster'
import type { ManualServicePorts } from '@/shared/types/manual-node'
import { MODULAR_INVITE_STATUS_POLL_INTERVAL_MS } from '@/shared/constants/modular-runtime'
import getErrorString from '@/shared/utils/get-error-string'
import { formatClusterInviteError } from '@/ui/utils/cluster-invite-error'

interface InvitePairing {
    /** The outbound invite (carries the PIN to display) once started; null before. */
    invite: Invite | null
    /** True while the initial `cluster:invite-node` request is in flight. */
    submitting: boolean
    error: string | null
    /**
     * Add a node by address and begin PIN pairing with it, then poll its status
     * until it resolves. `ports` overrides the assumed port of any single service
     * on that host.
     */
    start: (address: string, ports?: ManualServicePorts) => Promise<void>
    /**
     * Cancel a still-pending outbound invite: tell the backend to tear down the
     * pairing session (invalidating the PIN so a remote user can no longer
     * complete the join), dissolve any throwaway solo cluster, then clear local
     * state. Falls back to a local reset when there is no live invite.
     */
    cancel: () => Promise<void>
    /** Clear the invite and stop polling (local only; does not tell the backend). */
    reset: () => void
}

/**
 * Drives an outbound PIN-pairing session: fires `cluster:invite-node`, surfaces
 * the returned PIN, and polls `cluster:invite-status` until the invite leaves the
 * `pending` state (paired / declined / expired / failed).
 */
export function useInvitePairing(): InvitePairing {
    const [invite, setInvite] = useState<Invite | null>(null)
    const [submitting, setSubmitting] = useState(false)
    const [error, setError] = useState<string | null>(null)
    const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
    const inviteIdRef = useRef<string | null>(null)

    const stopPolling = useCallback(() => {
        if (pollRef.current) {
            clearInterval(pollRef.current)
            pollRef.current = null
        }
    }, [])

    const reset = useCallback(() => {
        stopPolling()
        inviteIdRef.current = null
        setInvite(null)
        setSubmitting(false)
        setError(null)
    }, [stopPolling])

    useEffect(() => () => stopPolling(), [stopPolling])

    const cancel = useCallback(async () => {
        const id = inviteIdRef.current
        stopPolling()
        if (id) {
            try {
                await window.pairApi.cluster.cancelInvite(id)
            } catch {
                // Best-effort: the backend's session TTL and the client sweep are
                // the backstops if this fails (e.g. it already resolved).
            }
            // The invite may have auto-created a throwaway solo cluster; drop it
            // if no peer joined (same as a terminal non-paired outcome).
            void window.pairApi.cluster.abandonIfSolo()
        }
        reset()
    }, [reset, stopPolling])

    const start = useCallback(
        async (address: string, ports?: ManualServicePorts) => {
            setSubmitting(true)
            setError(null)
            stopPolling()
            try {
                const result = await window.pairApi.cluster.inviteNode(address, ports)
                setInvite(result)
                if (result.state === 'pending' && result.inviteId) {
                    inviteIdRef.current = result.inviteId
                    pollRef.current = setInterval(() => {
                        const id = inviteIdRef.current
                        if (!id) return
                        void window.pairApi.cluster
                            .inviteStatus(id)
                            .then(status => {
                                setInvite(status)
                                if (status.state !== 'pending') {
                                    stopPolling()
                                    // A pairing that never completes leaves behind a
                                    // cluster that may have been auto-created just for
                                    // this invite; dissolve it if we are still solo.
                                    if (status.state !== 'paired') {
                                        void window.pairApi.cluster.abandonIfSolo()
                                    }
                                }
                            })
                            .catch(() => {
                                /* transient poll failure; keep trying */
                            })
                    }, MODULAR_INVITE_STATUS_POLL_INTERVAL_MS)
                } else if (result.state !== 'paired') {
                    // Any non-paired terminal initial result (failed / rejected /
                    // declined / expired). `rejected` (e.g. the target is already
                    // clustered, `reason: 'already-clustered'`) matched neither
                    // branch before, so its solo cluster was never dissolved. The
                    // panel renders the state/reason-specific copy; a bare `failed`
                    // also surfaces the generic error banner.
                    if (result.state === 'failed') setError(formatClusterInviteError(null))
                    void window.pairApi.cluster.abandonIfSolo()
                }
            } catch (err) {
                setError(formatClusterInviteError(getErrorString(err)))
                void window.pairApi.cluster.abandonIfSolo()
            } finally {
                setSubmitting(false)
            }
        },
        [stopPolling]
    )

    return { invite, submitting, error, start, cancel, reset }
}
