// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Custom endpoints API — user-configured external OpenAI-API-compatible
 * endpoints, read and edited through the addressed node's broker (the
 * configuration's source of truth). Unlike the engine command API there are
 * no push subscriptions: the Custom section re-invokes `custom:list` when it
 * opens or the list changes, and a card's Refresh invokes `custom:refresh` to
 * demand a fresh probe verdict right now, because a cached health flag that
 * goes stale behind a closed window is worse than none.
 *
 * The optional `nodeId` addresses another node's broker. A missing or
 * self-addressed read runs the unchanged local path and yields an empty list
 * when it cannot be answered; a peer-addressed read rejects so the
 * editor can go read-only with the reason instead of showing an unknown
 * peer's configuration as an empty one. The API key rides the add request —
 * local IPC, or the pinned control channel when a peer is addressed — and it
 * never returns on a response and is never logged.
 *
 * `getPort`/`setPort` move the proxy that fronts those endpoints: the Custom
 * facade's listen port, which every endpoint on a node shares. It is read with
 * the endpoint snapshot on the same timer and edited in the same panel, because
 * the number only means anything beside the endpoints routed through it.
 */
import type { PreloadServiceTransport as ServiceTransport } from '@/shared/types/service-bridge'
import type {
    CustomEndpointInfo,
    CustomProxyPortState
} from '@/shared/types/custom-endpoints'

/**
 * A validated add- or edit-form submission. Port is mandatory — never
 * defaulted here. With `replaceId` the same request edits the named entry in
 * place instead of forking a second one for the same address, and
 * `keyAction:"clear"` removes that entry's stored credential; an empty/absent
 * `apiKey` keeps whatever key is stored.
 */
export interface CustomEndpointDraft {
    url: string
    port: number
    name?: string
    apiKey?: string
    replaceId?: string
    keyAction?: 'clear'
}

export interface ICustomEndpointsApi {
    /** Snapshot of the addressed node's configured endpoints; empty when none
     *  or, locally, when the broker is down. Peer-addressed failures reject. */
    list(nodeId?: string): Promise<CustomEndpointInfo[]>
    /** Re-probe now, then return the same snapshot shape as `list`: one
     *  endpoint when `id` is given, every tracked endpoint when it is not.
     *  Local degradation matches `list` (empty, not an error); peer-addressed
     *  failures reject. */
    refresh(id?: string, nodeId?: string): Promise<CustomEndpointInfo[]>
    /** Store the endpoint; resolves with the stored entry or rejects with the
     *  owning broker's reason. */
    add(draft: CustomEndpointDraft, nodeId?: string): Promise<CustomEndpointInfo>
    /** Idempotent: removing an id the owning node never configured resolves
     *  `{removed: false}`. */
    remove(id: string, nodeId?: string): Promise<{ removed: boolean }>
    /** Where the addressed node's Custom proxy listens — or would listen, when
     *  nothing does. `null` means that node reported no proxy port at all,
     *  which is not the same as a port of zero: no field is rendered for it. */
    getPort(nodeId?: string): Promise<CustomProxyPortState | null>
    /** Move the Custom proxy to `port` and report where it landed. Rejects with
     *  the owning node's reason when the port is refused or the proxy could not
     *  take it, so the field can show what to type instead. */
    setPort(port: number, nodeId?: string): Promise<CustomProxyPortState>
}

export function createCustomEndpointsApi(transport: ServiceTransport): ICustomEndpointsApi {
    return {
        // Local reads carry an empty payload: `nodeId` is addressing, not
        // configuration, and the unchanged local path must not grow a field.
        list: (nodeId?: string) => transport.invoke('custom:list', nodeId ? { nodeId } : {}),
        // Same rule for the refresh scope: `id` narrows which endpoint is
        // re-probed, `nodeId` only travels when a peer is addressed.
        refresh: (id?: string, nodeId?: string) => {
            const scope = id ? { id } : {}
            return transport.invoke('custom:refresh', nodeId ? { ...scope, nodeId } : scope)
        },
        add: (draft: CustomEndpointDraft, nodeId?: string) =>
            transport.invoke('custom:add', nodeId ? { ...draft, nodeId } : draft),
        remove: (id: string, nodeId?: string) =>
            transport.invoke('custom:remove', nodeId ? { id, nodeId } : { id }),
        // The facade port is one number per node, not per endpoint, so the read
        // carries nothing but the optional peer id. A peer that cannot answer
        // rejects instead of yielding this node's own default: a port read from
        // the wrong machine would render a stranger's proxy as editable here.
        getPort: (nodeId?: string) =>
            transport.invoke('custom:get-port', nodeId ? { nodeId } : {}),
        setPort: (port: number, nodeId?: string) =>
            transport.invoke('custom:set-port', nodeId ? { port, nodeId } : { port })
    }
}
