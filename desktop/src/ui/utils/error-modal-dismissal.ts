// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ServiceError } from '@/shared/types/errors'

/**
 * Identity of the error set the modal is currently showing, used to latch a
 * dismissal so a re-delivered snapshot cannot re-open the dialog mid-close.
 *
 * `nvpair-errors` keys its store by `(nodeId, id)` and upserts a report with a
 * refreshed timestamp, so both parts are load-bearing:
 *
 * - `nodeId`, because producer ids are stable per subject
 *   (`engine-manager:install-failed:<engine>`), so two nodes failing the same
 *   way share one id and would otherwise collapse into a single entry.
 * - `timestamp`, because a dismissed error that happens again returns under the
 *   same id; only the timestamp distinguishes the new episode, and without it
 *   the latch would suppress the modal forever.
 *
 * Sorted so the key does not depend on the order the snapshot arrives in, and
 * JSON-encoded so an id or node id containing the separator cannot forge a
 * match.
 */
export function errorDismissalKey(errors: ServiceError[]): string {
    const entries = errors.map((err): [string, string, number] => [
        err.nodeId ?? '',
        err.id,
        err.timestamp
    ])
    entries.sort((a, b) => a[1].localeCompare(b[1]) || a[0].localeCompare(b[0]) || a[2] - b[2])
    return JSON.stringify(entries)
}
