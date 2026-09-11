// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineType } from '@/shared/types/engines'

/**
 * Decides whether a search that matched nothing is worth resolving against the
 * engine's registry.
 *
 * The Ollama hub browses a committed snapshot, so a model published after that
 * snapshot was taken is missing from the list even though the engine can pull
 * it; `engine:lookup-hub-model` closes that gap for one exact name. A lookup
 * costs a request, so the pointless ones are stopped here rather than sent.
 */

/**
 * Longest name worth asking about. Ollama references are far shorter in
 * practice; the cap keeps a paste of unbounded text away from the matcher, the
 * same reason `formatModelDisplayName` caps its input.
 */
const MAX_NAME_LENGTH = 256

/**
 * `[namespace/]name[:tag]`, the form `ollama pull` accepts. Neither `/` nor `:`
 * is in the segment character class, so each separator is the only place a
 * repetition can start and matching stays linear.
 */
const OLLAMA_MODEL_REFERENCE =
    /^[a-zA-Z0-9][a-zA-Z0-9._-]*(?:\/[a-zA-Z0-9][a-zA-Z0-9._-]*)*(?::[a-zA-Z0-9][a-zA-Z0-9._-]*)?$/

/**
 * Ollama's hosted cloud-only tags (`qwen3-vl:235b-cloud`, `gpt-oss:120b-cloud`,
 * `…:cloud-preview`) cannot run on a local engine, so resolving one would only
 * add a row nobody can use. The catalog scraper drops them for the same reason.
 * `cloud` is matched as a whole delimited token, leaving `icloud-*` alone.
 */
function isOllamaCloudTag(name: string): boolean {
    return /(^|[-:])cloud($|[-:])/i.test(name)
}

function isOllamaLookupCandidate(name: string): boolean {
    return OLLAMA_MODEL_REFERENCE.test(name) && !isOllamaCloudTag(name)
}

/**
 * Per-engine gate, keyed like the matchers in `match-downloaded-model.ts`. An
 * engine missing from this map is never looked up, which is why LM Studio is
 * absent: its catalog is fetched live on a six-hour TTL, so it does not fall
 * behind between releases and has no gap to close.
 */
const LOOKUP_CANDIDATES: Partial<Record<EngineType, (name: string) => boolean>> = {
    ollama: isOllamaLookupCandidate
}

/**
 * The exact model name to resolve, or null when the query is not worth a
 * request — an engine with no registry to ask, a query that is not a model
 * reference, or a cloud-only tag.
 */
export function resolveLookupQuery(engine: EngineType | null, query: string): string | null {
    if (!engine) return null
    const isCandidate = LOOKUP_CANDIDATES[engine]
    if (!isCandidate) return null

    const name = query.trim()
    if (!name || name.length > MAX_NAME_LENGTH) return null
    return isCandidate(name) ? name : null
}
