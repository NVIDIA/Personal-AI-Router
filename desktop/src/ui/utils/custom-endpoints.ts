// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { CustomEndpointInfo } from '@/shared/types/custom-endpoints'

/**
 * Pure logic behind the per-node Custom endpoints surfaces. Kept free of
 * React so the form rules (port is mandatory and never inferred, url/port
 * must agree) and the list-line labels are pinned by unit tests; the
 * components below it are presentation only.
 */

/**
 * Raw field text of the add form and its error shape. Not exported on
 * purpose: the form and the tests pass `{url, port}` literals and read the
 * error keys structurally, and an export nobody imports fails
 * `dead-code:check`. The pair-api + its test keep `CustomEndpointDraft`.
 */
interface CustomEndpointFormValues {
    url: string
    port: string
}

interface CustomEndpointFormErrors {
    url?: string
    port?: string
    /** The url embeds a port that contradicts the Port field — a pair-level error, shown above the form. */
    mismatch?: string
}

// Echoes the broker's "never inferred" rule on the client: the form must not
// send a half-guessed endpoint for the broker to reject after a round trip.
const PORT_REQUIRED = 'Port is required — PAIR never infers it. Use the endpoint’s own port.'

export function validateCustomEndpointForm(
    values: CustomEndpointFormValues
): CustomEndpointFormErrors {
    const errors: CustomEndpointFormErrors = {}

    let port: number | null = null
    const portText = values.port.trim()
    if (/^\d+$/.test(portText)) {
        const parsed = Number(portText)
        if (parsed >= 1 && parsed <= 65535) port = parsed
    }
    if (port === null) errors.port = PORT_REQUIRED

    const url = values.url.trim()
    if (url === '') {
        errors.url = 'Enter the endpoint url, for example https://api.example.com/v1'
        return errors
    }

    const schemeEnd = url.indexOf('://')
    const scheme = schemeEnd > 0 ? url.slice(0, schemeEnd).toLowerCase() : ''
    if (scheme !== 'http' && scheme !== 'https') {
        errors.url = 'The url must start with http:// or https://'
        return errors
    }

    let host = ''
    let urlPort: string | null = null
    try {
        const parsed = new URL(url)
        host = parsed.hostname
        urlPort = parsed.port === '' ? null : parsed.port
    } catch {
        host = ''
    }
    if (!host) {
        errors.url = 'The url has no host to dial'
        return errors
    }

    if (urlPort !== null && port !== null && Number(urlPort) !== port) {
        errors.mismatch =
            'The Port field disagrees with the port inside the url. Remove one so they match.'
    }
    return errors
}

/** Name the user gave, else the dialable host:port, else the raw baseUrl. */
export function customEndpointDisplayLabel(entry: CustomEndpointInfo): string {
    if (entry.name) return entry.name
    try {
        const parsed = new URL(entry.baseUrl)
        if (parsed.hostname) {
            return parsed.port ? `${parsed.hostname}:${parsed.port}` : parsed.hostname
        }
    } catch {
        /* unparseable baseUrl falls through to the raw string */
    }
    return entry.baseUrl
}

interface CustomEndpointKeyFieldErrors {
    key?: string
}

/**
 * The card status line: verdict first, then (online endpoints only) the
 * served-model count, then the age of the verdict that produced the state.
 * Offline and unchecked states carry no models suffix — a failed probe's
 * list is stale noise — and a zero-model count must never read as a status.
 * "Online"/"Offline" state whether the endpoint answers, which is what the
 * verdict means; credential presence is not a status and reads in the
 * editor instead. `healthy` arrives only as a real boolean; its absence is
 * "Not checked yet", the same third state the wire keeps distinct from
 * `false`.
 */
export function customEndpointSummaryLabel(entry: CustomEndpointInfo, now: Date): string {
    if (typeof entry.healthy !== 'boolean') return 'Not checked yet'
    let label = entry.healthy ? 'Online' : 'Offline'
    if (entry.healthy && entry.models && entry.models.length > 0) {
        label += entry.models.length === 1 ? ' · 1 model' : ` · ${entry.models.length} models`
    }
    if (entry.lastChecked) {
        const checked = new Date(entry.lastChecked)
        if (!Number.isNaN(checked.getTime())) {
            const ageMs = Math.max(0, now.getTime() - checked.getTime())
            if (ageMs < 60_000) label += ' · checked just now'
            else if (ageMs < 3_600_000) label += ` · checked ${Math.floor(ageMs / 60_000)} min ago`
            else label += ` · checked ${Math.floor(ageMs / 3_600_000)} h ago`
        }
    }
    return label
}

/**
 * The edit form's key-field rule: typing a new key and checking Remove saved
 * key cannot ride one submit — there is no safe reading, and the user has to
 * pick. Everything else is a valid intent: typed key replaces, checkbox
 * clears, blank field keeps what is stored.
 */
export function validateCustomEndpointKeyFields(input: {
    keySet: boolean
    apiKey: string
    clearKey: boolean
}): CustomEndpointKeyFieldErrors {
    const errors: CustomEndpointKeyFieldErrors = {}
    if (input.clearKey && input.apiKey !== '') {
        errors.key = 'Enter a new key or remove the saved key, not both'
    }
    return errors
}

/**
 * The Custom proxy-port field's rule, mirroring the broker's: a port is typed,
 * never inferred, and only a whole number in 1-65535 is one. Returning the
 * reason rather than a boolean lets the field say why Apply stays off while the
 * text is still being typed, instead of spending a call — or a peer relay's whole
 * budget — to be told the same thing.
 */
export function validateCustomProxyPort(value: string): string | undefined {
    const text = value.trim()
    if (!/^\d+$/.test(text)) return 'Enter the proxy port as a number, for example 8000'
    const port = Number(text)
    if (port < 1 || port > 65535) return 'The proxy port must be between 1 and 65535'
    return undefined
}

/**
 * Version-skew reading for a peer that cannot answer the Custom plane at all.
 * Three wordings mean one thing by the time they reach this surface, so they
 * read as one sentence: the pinned relay's unified answer ("device is offline
 * or does not support custom endpoint management"), and the bridge's two
 * empty-reply guards — "the device returned no endpoint list" for the endpoint
 * reads and "the device returned no proxy port" for the facade-port pair, the
 * shape a peer shows when it has no Custom routes at all. A peer answering 404
 * for `custom/get-port` is a peer predating custom-endpoint management: the
 * node predates the plane, and the sentence names the fix (update PAIR on that
 * node) where the wire strings only name a class of failure. Offline nodes never
 * get here — the whole Custom section hides while a node reads disconnected —
 * so "older version" is the truthful reading. Every other reason passes through
 * verbatim: a trust refusal or a timeout is not version skew, and rewriting
 * those would lie.
 */
export function customEndpointsUnavailableLabel(reason: string): string {
    if (
        reason.includes('does not support custom endpoint management') ||
        reason.includes('the device returned no endpoint list') ||
        reason.includes('the device returned no proxy port')
    ) {
        return (
            'This node is running an older version of NVIDIA PAIR that doesn’t support ' +
            'custom inference engines. Update NVIDIA PAIR on that node to add, edit, or ' +
            'check custom endpoints.'
        )
    }
    return reason
}
