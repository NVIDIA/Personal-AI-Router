// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Cluster-layer data types shared by service transports and the UI.
 *
 * These shapes cross the renderer service bridge, so they must remain
 * JSON-serializable. They mirror the `nvpair-cluster-manager` contract
 * (`services/nvpair-cluster-manager/spec.md` §6/§7): membership and the
 * interactive PIN-pairing invite handshake.
 */

export type NodeIdentityVerification = 'local' | 'pinned' | 'unverified'

export interface AvailableNode {
    /** Discovery record identity, independent of a claimed host UUID. */
    discoveryRecordId?: string
    /** How the service established this record's identity. */
    identityVerification?: NodeIdentityVerification
    /**
     * UI row key, matching `NodeItem.id`: host UUID for local/pinned records,
     * discovery record ID for unverified records. Proxy-only rows retain the
     * operational node key reported by the proxy.
     */
    id: string
    /** Display hostname. */
    name: string
    ipAddress: string
    /**
     * Every address the node published, in its own ranked order with `ipAddress`
     * first. Absent when the node published a single address.
     *
     * A multi-homed node has no single address every peer can reach: a
     * direct-connect link between two machines is reachable only from the machine
     * on its far end. The node ranks its own addresses from evidence no observer
     * has, so anything that must connect walks this list in order rather than
     * treating `ipAddress` as the only answer.
     */
    ipAddresses?: string[]
    port: number
    lastSeen: number
    /** Whether this node is already a locally-trusted (pinned) peer. */
    trusted: boolean
    /**
     * Whether this node already belongs to a cluster. An already-clustered node
     * cannot be invited into another, so invite UI treats it as non-invitable.
     */
    clustered: boolean
    /**
     * That node's own cluster model alias document, as it announced it: the list
     * its owner edits, not the merged list this computer routes by
     * (`cluster:get-router-aliases`). It is announced by the node's engine
     * manager, so it survives that node's engines being stopped.
     *
     * Absent means the node has not reported a document at all — an older build,
     * or a model read that has not answered — which is deliberately not the same
     * as an empty list. The editor behind this replaces the whole list it shows,
     * so it will edit a stated empty and never a silence.
     */
    aliases?: ModelAlias[]
}

/**
 * Membership state of a `ClusterNode` (cluster-manager `MembershipState`).
 * - `member` — join confirmed on both sides.
 * - `pending-outbound` — we invited them, awaiting their PIN entry.
 * - `pending-inbound` — they invited us, awaiting the local user's PIN entry.
 */
export type MembershipState = 'member' | 'pending-outbound' | 'pending-inbound'

/** A cluster member (or pending invitee) as reported by the cluster-manager. */
export interface ClusterNode {
    /** Logical node id (hostname). Display identity only. */
    id: string
    /**
     * Stable cryptographic identity; the trusted-node-store key and cert subject.
     * This matches a verified host's `NodeItem.id` and the local `selfId`.
     * Unverified discoveries use separate record IDs even when they claim this
     * UUID. Member-to-node matching uses this field, never `id` (hostname).
     */
    nodeUuid: string
    name: string
    ipAddress: string
    port: number
    clusterId: string
    state: MembershipState
    /** Epoch ms when membership was confirmed; null while pending. */
    joinedAt: number | null
    /** Epoch ms of last successful contact; null if never. */
    lastSeen: number | null
}

/** This node's own identity (`cluster:get-node-id`). */
export interface ClusterNodeIdentity {
    nodeUuid: string
    nodeId: string
    name: string
    certFingerprint: string
    /** `""` until the node creates a cluster or adopts one by accepting an invite. */
    clusterId: string
}

/**
 * Terminal/transient state of a pairing session (cluster-manager `InviteState`).
 * - `pending` — Initial Exchange done; awaiting the user's PIN entry.
 * - `paired` — Completion Exchange succeeded; certs pinned, membership recorded.
 * - `declined` — the invitee's user declined.
 * - `canceled` — the inviter aborted the invite before it completed
 *   (`cluster:cancel-invite`); on the joiner, set when the cancel signal arrives.
 * - `expired` — TTL elapsed with no PIN entry.
 * - `failed` — pairing could not complete (peer unreachable, wrong PIN,
 *   malformed); a wrong PIN carries `reason: 'incorrect-pin'` (see
 *   {@link Invite.reason}) and is mirrored on both the inviter and the joiner.
 * - `rejected` — the target refused the invite outright (e.g. it is already in a
 *   cluster); see {@link Invite.reason}. No PIN is issued.
 */
export type InviteState =
    | 'pending'
    | 'paired'
    | 'declined'
    | 'canceled'
    | 'expired'
    | 'failed'
    | 'rejected'

/**
 * Broker-facing view of a pairing session. The six-digit `pin` is present only
 * in the inviter's `cluster:invite-node` result (the inviter displays it); it is
 * carried to the joiner by the user out of band and never sent on the wire.
 */
export interface Invite {
    inviteId: string
    fromNodeId: string
    fromNodeUuid: string
    fromNodeName: string
    toNodeId: string | null
    clusterId: string
    clusterFriendlyName: string
    pin: string | null
    state: InviteState
    /**
     * Machine-readable reason for a non-successful terminal state, when the
     * backend supplies one. Known values:
     * - `'already-clustered'` — paired with `state: 'rejected'`.
     * - `'incorrect-pin'` — paired with `state: 'failed'`; the joiner entered a
     *   wrong PIN. Set on both the joiner and inviter so the wrong-PIN error is
     *   mirrored on both sides. Empty/absent for other `failed` causes.
     */
    reason: string
    createdAt: number
    respondedAt: number | null
}

export interface ClusterInfo {
    clusterId: string | null
    isClustered: boolean
    clusterFriendlyName: string
}

/** Pushed when the local cluster identity changes (localhost UI). */
export interface ClusterIdentityPayload {
    clusterId: string | null
    clusterFriendlyName: string
}

/**
 * One configured cluster model alias: a caller-side name and the model that
 * name stands for. Nothing here names a machine or an engine — every
 * computer's cluster router derives the providers from what its own catalog
 * sees, which is what lets a synonym typed on one machine route on another.
 *
 * The same two fields are what `nvpair-node-settings` stores (under
 * `model_aliases`) and what travels to the cluster router, so no layer
 * translates a spelling any more. The service bridge still decodes instead of
 * forwarding raw JSON (`service-bridge/model-aliases.ts`).
 */
export interface ModelAlias {
    name: string
    model: string
}

/**
 * One row of the cluster alias view: what this computer's cluster router
 * actually routes by, with the origin of the definition this row carries. A
 * name several machines define and can serve appears once per machine, each
 * row holding that machine's own synonym and the machine that wrote it,
 * because all of them answer for the name. The pair is the same synonym a
 * `ModelAlias` carries; the origin says where it was written, which is the
 * machine its editor lives on — write authority stays with the writer, so a
 * peer's entry is shown read-only here.
 *
 * `originNode` and `originHost` are empty for this node's own configured
 * entries. `originNode` is the peer's routing id (for keys and dedupe);
 * `originHost` is its display name, the one an operator can act on.
 */
export interface ClusterModelAliasViewEntry {
    name: string
    model: string
    originNode: string
    originHost: string
}
