// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * One user-configured custom endpoint as the broker reports it on the
 * `custom/list` / `custom/add` wire. Mirrors the Go `customEndpointView`
 * shape. It never carries an API key: the broker's view has no key field, and
 * the bridge parser rebuilds entries from these known fields only, so a key
 * smuggled into a reply can never reach the renderer.
 */
export interface CustomEndpointInfo {
    /** Stable identity: a user-assigned id, else `"custom:" + baseUrl`. */
    id: string
    /** Optional display label; absent when the user gave none. */
    name?: string
    /** Canonical endpoint base, e.g. `https://host:8000/v1`. */
    baseUrl: string
    /** Whether an API key is configured — the fact, never the key. */
    keySet: boolean
    /** Last probe verdict; only present when the reply reported a boolean. */
    healthy?: boolean
    /** RFC3339 timestamp of the last probe, when reported. */
    lastChecked?: string
    /**
     * Served model ids, and only ever present alongside a healthy verdict
     * that reported models — the broker omits the field for unchecked and
     * for checked-but-serving-nothing endpoints. Absence therefore pairs
     * with `healthy`/`lastChecked`: no verdict at all means "not checked",
     * a verdict without models means "checked, nothing served", which the
     * cards render differently. The bridge filters non-string entries, so
     * this is always a plain string list when present.
     */
    models?: string[]
}

/**
 * The Custom facade's own listen port on the node that answers, as the broker
 * reports it on `custom:get-port` / `custom:set-port`. One number per node,
 * shared by every custom endpoint there — the endpoints' own ports belong to
 * the upstreams they forward to, not to this facade.
 *
 * `running` says which kind of number `port` is: true while a live facade
 * listens there, false when it is only where the next bring-up is planned to
 * land. A peer that predates this release answers neither method, so a read can
 * report nothing at all; the Custom section then shows no port field rather than
 * this node's own default, which would describe a different machine.
 */
export interface CustomProxyPortState {
    /** The facade's listen port: live, or planned while nothing listens. */
    port: number
    /** Whether a proxy is actually listening on `port` right now. */
    running: boolean
}
