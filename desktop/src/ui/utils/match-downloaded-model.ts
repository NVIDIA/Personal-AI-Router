// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import type { ModelItem } from '@/ui/types/engine-info'
import type { ModelEntry } from '@/ui/types/model-hub'

/**
 * Returns true when a hub search result represents a model that is already
 * downloaded locally (per the installed `ModelItem[]` for this engine).
 *
 * Matching is engine-specific because the hub's id format and the engine's
 * stored model id format don't line up the same way:
 *
 * - Ollama lib hub:  `ModelItem.name` is `<base>:<tag>`; the hub id is the
 *                    bare `<base>`. Match on exact `<base>` or `<base>:`
 *                    prefix so every quantization collapses to "downloaded".
 * - LM Studio:       the download is keyed by `pullKey = <owner>/<repo>` (or
 *                    `<owner>/<repo>/<file>`); the hub id is `<owner>/<repo>`.
 */
type DownloadedMatcher = (hubEntry: ModelEntry, downloaded: ModelItem) => boolean

const matchOllama: DownloadedMatcher = (hubEntry, d) => {
    const hubId = hubEntry.id
    if (d.name === hubId) return true
    return d.name.startsWith(`${hubId}:`)
}

const matchHfPullKeyOrName: DownloadedMatcher = (hubEntry, d) => {
    const hubId = hubEntry.id
    const pullKey = d.pullKey ?? ''
    if (pullKey === hubId) return true
    if (pullKey.startsWith(`${hubId}/`)) return true
    if (d.name === hubId) return true
    if (d.name.startsWith(`${hubId}/`)) return true
    return false
}

const MATCHERS: Partial<Record<EngineType, DownloadedMatcher>> = {
    ollama: matchOllama,
    'lm-studio': matchHfPullKeyOrName
}

export function isHubEntryDownloaded(
    engine: EngineType,
    hubEntry: ModelEntry,
    downloaded: readonly ModelItem[]
): boolean {
    if (downloaded.length === 0) return false
    const matcher = MATCHERS[engine]
    if (!matcher) return false
    for (const d of downloaded) {
        if (!d.downloaded) continue
        if (matcher(hubEntry, d)) return true
    }
    return false
}
