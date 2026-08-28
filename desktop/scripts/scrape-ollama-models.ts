// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Regenerate the committed Ollama model list (`ollama-models.json`).
 *
 *   npm run scrape:ollama-models
 *
 * PAIR ships a **locked** list of Ollama models rather than scraping
 * ollama.com from the running app (the live scrape was fragile and broke
 * whenever Ollama tweaked their markup). This dev-only script performs the
 * scrape on demand: run it when Ollama's catalog visibly changes, review the
 * diff, and commit the regenerated JSON.
 *
 * There is no official JSON API for the library page (ollama/ollama#9142),
 * but the page is server-rendered and every model card is anchored by stable
 * `x-test-*` attributes used by Ollama's own test suite — so a narrow regex
 * parser is reliable. A second pass fetches each model's `/tags` page for real
 * GGUF byte sizes and short digests (what Ollama Desktop itself displays).
 * `/api/tags` is a last-resort fallback if the index scrape yields nothing.
 */
import { writeFileSync } from 'node:fs'
import path from 'node:path'
import axios, { isAxiosError } from 'axios'
import type { OllamaTagsModel } from '@/electron/model-hub/ollama-library'

const OLLAMA_LIBRARY_URL = 'https://ollama.com/library'
const OLLAMA_DETAIL_URL = (base: string): string =>
    `https://ollama.com/library/${encodeURIComponent(base)}/tags`
const OLLAMA_TAGS_API = 'https://ollama.com/api/tags'
const HTTP_TIMEOUT_MS = 20_000
/** Per-page timeout for the detail-page enrichment pass. */
const DETAIL_TIMEOUT_MS = 10_000
/**
 * Bounded in-flight detail-page fetches. Four is polite — Ollama serves the
 * library page via a CDN, but we still never want to hammer it.
 */
const DETAIL_CONCURRENCY = 4
const REQUEST_HEADERS: Record<string, string> = {
    // Scraping without a UA string gets served cached/edge garbage on some CDNs.
    'User-Agent': 'PAIR/1.0 (+https://github.com/ollama/ollama)',
    Accept: 'text/html,application/xhtml+xml'
}

/** Destination for the committed list, resolved from the repo root. */
const OUTPUT_PATH = path.resolve(
    __dirname,
    '..',
    'src',
    'electron',
    'model-hub',
    'ollama-models.json'
)

interface OllamaTagsResponse {
    models?: unknown
}

/**
 * The subset of a library card we actually persist: the pull-ready base
 * `name`, the capability chips (first becomes `family`), the size chips, and
 * the absolute "updated" timestamp. Pulls / tag counts / descriptions are
 * rendered on the page but unused downstream, so we don't scrape them.
 */
interface ScrapedModelCard {
    name: string
    capabilities: string[]
    sizes: string[]
    updatedAtIso: string
}

function firstMatch(text: string, re: RegExp): string | undefined {
    const m = re.exec(text)
    return m ? m[1] : undefined
}

function allMatches(text: string, re: RegExp): string[] {
    const out: string[] = []
    let m: RegExpExecArray | null
    while ((m = re.exec(text)) !== null) {
        out.push(m[1].trim())
    }
    return out
}

function decodeHtmlEntities(s: string): string {
    return s
        .replace(/&amp;/g, '&')
        .replace(/&lt;/g, '<')
        .replace(/&gt;/g, '>')
        .replace(/&quot;/g, '"')
        .replace(/&#39;/g, "'")
        .replace(/&nbsp;/g, ' ')
}

/**
 * Ollama's hosted cloud-only variants (e.g. `qwen3-vl:235b-cloud`,
 * `gpt-oss:120b-cloud`, `qwen3-vl:cloud`, `…:cloud-preview`). They cannot
 * be `ollama pull`-ed, have no local byte size, and are meaningless for a
 * self-hosted cluster — so we drop them unconditionally from both the
 * index and the detail-page passes.
 *
 * We match `cloud` as a whole dash-/colon-delimited token so every
 * position is caught: `:cloud`, `:cloud-preview`, `-cloud`, `-cloud-…`,
 * and tag-only strings like `cloud` or `cloud-q4`. Unrelated names like
 * `icloud-*` or `clouds-*` are unaffected because of the boundary check.
 */
function isCloudTagName(name: string): boolean {
    return /(^|[-:])cloud($|[-:])/i.test(name)
}

/**
 * Index-level defensive filter. Ollama currently does not publish any
 * "cloud" capability chip on the library index (verified), but the
 * capability appears in their design system, so we filter pre-emptively
 * to keep parity with the tag-level filter.
 */
function isCloudOnlyCard(card: ScrapedModelCard): boolean {
    return card.capabilities.some(c => c.toLowerCase() === 'cloud')
}

/**
 * Parse the ollama.com/library HTML. Ollama removed the `x-test-*` anchors
 * their test suite once used, so we anchor on each card's link:
 * `<a href="/library/<name>" class="group w-full …">`. The base name is the
 * URL slug (exactly the `ollama pull` id). Inside each card:
 *
 * - capability chips are indigo (`text-indigo-600`);
 * - size chips are blue (`text-blue-600`);
 * - the "Updated" stat span carries an absolute timestamp in its `title`.
 *
 * These Tailwind color classes are the most stable structure the page still
 * exposes. When Ollama changes them again, this parser is what a dev updates
 * before re-running the script.
 */
function parseHtmlLibrary(html: string): ScrapedModelCard[] {
    const out: ScrapedModelCard[] = []
    // `class="group w-full …"` (with the space) is the card link; the small
    // in-card name links use `class="group-hover:…"`, so this never matches
    // them. No nested <a> inside a card, so a lazy `</a>` is safe.
    const cardRe =
        /<a\b[^>]*\bhref="\/library\/([^"/?#]+)"[^>]*\bclass="group w-full[^"]*"[^>]*>([\s\S]*?)<\/a>/g

    let cardMatch: RegExpExecArray | null
    while ((cardMatch = cardRe.exec(html)) !== null) {
        const name = decodeHtmlEntities(cardMatch[1]).trim()
        if (!name) continue
        const block = cardMatch[2]

        const capabilities = allMatches(
            block,
            /<span\b[^>]*\btext-indigo-600\b[^>]*>([^<]+)<\/span>/g
        )
        const sizes = allMatches(block, /<span\b[^>]*\btext-blue-600\b[^>]*>([^<]+)<\/span>/g)

        // Absolute timestamp lives in the `title` of the stats span that also
        // renders `Updated&nbsp;<relative>`. Bound the gap so we only match the
        // updated span's own title, never the card's `<div title="<name>">`.
        const updatedAbs = firstMatch(
            block,
            /<span\b[^>]*\btitle="([^"]+)"[^>]*>[\s\S]{0,600}?Updated&nbsp;/
        )
        let updatedAtIso = ''
        if (updatedAbs) {
            const parsed = new Date(updatedAbs)
            if (!Number.isNaN(parsed.getTime())) {
                updatedAtIso = parsed.toISOString()
            }
        }

        out.push({ name, capabilities, sizes, updatedAtIso })
    }

    return out
}

/**
 * Expand a parsed library card into one `OllamaTagsModel` per advertised
 * size. Cards without any `x-test-size` (e.g. newly-added preview models)
 * emit a single entry with the bare base name so they still show up.
 */
function cardToTags(card: ScrapedModelCard): OllamaTagsModel[] {
    const modifiedAt = card.updatedAtIso || new Date().toISOString()
    const family = card.capabilities[0] ?? ''

    const emit = (tag: string | null): OllamaTagsModel => {
        const fullName = tag ? `${card.name}:${tag}` : card.name
        return {
            name: fullName,
            model: fullName,
            modified_at: modifiedAt,
            size: 0,
            digest: '',
            details: {
                parent_model: '',
                format: '',
                family,
                families: null,
                parameter_size: tag ?? '',
                quantization_level: ''
            }
        }
    }

    if (card.sizes.length === 0) {
        return [emit(null)]
    }
    return card.sizes.map(emit)
}

function isRecord(v: unknown): v is Record<string, unknown> {
    return v !== null && typeof v === 'object'
}

function readStr(o: Record<string, unknown>, key: string): string {
    const v = o[key]
    return typeof v === 'string' ? v : ''
}

function readStringArrayOrNull(o: Record<string, unknown>, key: string): string[] | null {
    const v = o[key]
    if (!Array.isArray(v)) return null
    const arr: string[] = []
    for (const item of v) {
        if (typeof item === 'string') arr.push(item)
    }
    return arr
}

function normalizeApiTagsEntry(raw: unknown): OllamaTagsModel | null {
    if (!isRecord(raw)) return null
    const o = raw
    const rawName = readStr(o, 'name') || readStr(o, 'model')
    const name = rawName.trim()
    if (!name) return null
    const model = readStr(o, 'model') || name
    const sizeVal = o.size
    const size = typeof sizeVal === 'number' && sizeVal >= 0 ? sizeVal : 0
    const d: Record<string, unknown> = isRecord(o.details) ? o.details : {}
    return {
        name,
        model,
        modified_at: readStr(o, 'modified_at'),
        size,
        digest: readStr(o, 'digest'),
        details: {
            parent_model: readStr(d, 'parent_model'),
            format: readStr(d, 'format'),
            family: readStr(d, 'family'),
            families: readStringArrayOrNull(d, 'families'),
            parameter_size: readStr(d, 'parameter_size'),
            quantization_level: readStr(d, 'quantization_level')
        }
    }
}

function dedupeByName(models: OllamaTagsModel[]): OllamaTagsModel[] {
    const byName = new Map<string, OllamaTagsModel>()
    for (const m of models) byName.set(m.name, m)
    return Array.from(byName.values())
}

// ---------------------------------------------------------------------------
// Detail-page enrichment — fetches /library/<base>/tags per model to get real
// GGUF byte sizes, short digests, and context lengths that the index page
// does not expose. This is what Ollama Desktop itself displays.
// ---------------------------------------------------------------------------

/**
 * Parse a "6.1GB" / "250MB" / "143GB" size string as rendered on the tags
 * page. Ollama publishes sizes using **binary** units (1 GB = 1024³ bytes)
 * matching the raw GGUF byte count, so round-tripping `size -> formatBytes`
 * reproduces the exact string the page shows. Returns 0 for unparseable
 * input so we don't emit garbage.
 */
function parseSizeToBytes(raw: string): number {
    const m = /^\s*([0-9]+(?:\.[0-9]+)?)\s*(TB|GB|MB|KB|B)\s*$/i.exec(raw)
    if (!m) return 0
    const value = parseFloat(m[1])
    if (!Number.isFinite(value) || value < 0) return 0
    const unit = m[2].toUpperCase()
    const factor =
        unit === 'TB'
            ? 1024 ** 4
            : unit === 'GB'
              ? 1024 ** 3
              : unit === 'MB'
                ? 1024 ** 2
                : unit === 'KB'
                  ? 1024
                  : 1
    return Math.round(value * factor)
}

/**
 * One row on a model's `/library/<base>/tags` page. Anchored on the mobile
 * inline bullet string `<digest></span> • <size> • <context> context
 * window` which is rendered for every tag on both the desktop and mobile
 * layouts — the surrounding CSS grid classes change more often than this
 * pattern does.
 */
interface DetailTagRow {
    fullName: string
    digest: string
    sizeBytes: number
    context: string
}

/**
 * Parse every tag row on `/library/<base>/tags`.
 *
 * Each tag is rendered twice: a self-contained **mobile card**
 * (`<a href="/library/<base>[:<tag>]" class="md:hidden …">`) whose bullet
 * string holds `<span class="font-mono">DIGEST</span> • SIZE • CONTEXT context
 * window` together, and a **desktop block** that splits digest/size/context
 * across sibling elements using `·` separators. We anchor on the mobile card
 * and extract digest/size/context **from within that card's own block** — the
 * old "find the next `font-mono` after any href" approach skipped across the
 * `·`-separated desktop layout and mis-associated a tag with the next row's
 * (larger) size.
 */
function parseHtmlTagsPage(html: string, base: string): DetailTagRow[] {
    const rows: DetailTagRow[] = []
    const tagPrefix = `/library/${base}`
    const cardRe = new RegExp(
        `<a\\b[^>]*\\bhref="(${escapeRegex(tagPrefix)}(?::[^"?#]+)?)"[^>]*\\bclass="[^"]*\\bmd:hidden\\b[^"]*"[^>]*>([\\s\\S]*?)<\\/a>`,
        'g'
    )
    const detailRe =
        /<span class="font-mono">\s*([0-9a-f]{8,})\s*<\/span>[^\u2022]*?\u2022\s*([^\u2022]+?)\s*\u2022\s*([^\u2022]+?)\s*context/
    let m: RegExpExecArray | null
    while ((m = cardRe.exec(html)) !== null) {
        const href = m[1]
        const cardBlock = m[2]
        const detail = detailRe.exec(cardBlock)
        if (!detail) continue
        const digest = detail[1].trim()
        const sizeStr = detail[2].trim()
        const ctxStr = detail[3].trim()
        // `/library/<base>` (no colon) → name = `<base>`; `/library/<base>:<tag>` → `<base>:<tag>`.
        const suffix = href.slice(tagPrefix.length)
        const fullName = suffix.startsWith(':') ? `${base}${suffix}` : base
        rows.push({
            fullName,
            digest,
            sizeBytes: parseSizeToBytes(sizeStr),
            context: ctxStr
        })
    }
    return rows
}

function escapeRegex(s: string): string {
    return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function detailRowToTagsModel(
    row: DetailTagRow,
    card: ScrapedModelCard | undefined
): OllamaTagsModel {
    const modifiedAt =
        card?.updatedAtIso && card.updatedAtIso.length > 0
            ? card.updatedAtIso
            : new Date().toISOString()
    const family = card?.capabilities[0] ?? ''
    // `fullName` is either `<base>` or `<base>:<tag>`. `parameter_size`
    // only gets a value when there's a tag suffix.
    const colon = row.fullName.indexOf(':')
    const parameterSize = colon >= 0 ? row.fullName.slice(colon + 1) : ''
    return {
        name: row.fullName,
        model: row.fullName,
        modified_at: modifiedAt,
        size: row.sizeBytes,
        digest: row.digest,
        details: {
            parent_model: '',
            format: '',
            family,
            families: null,
            parameter_size: parameterSize,
            quantization_level: ''
        }
    }
}

/**
 * Bounded concurrent map. Runs `fn` over `items` with at most `limit`
 * promises in flight at once. Results preserve input order.
 */
async function mapWithConcurrency<T, R>(
    items: readonly T[],
    limit: number,
    fn: (item: T, index: number) => Promise<R>
): Promise<R[]> {
    const results = new Array<R>(items.length)
    let next = 0
    const workers: Promise<void>[] = []
    const worker = async (): Promise<void> => {
        for (;;) {
            const i = next
            next += 1
            if (i >= items.length) return
            results[i] = await fn(items[i], i)
        }
    }
    const n = Math.max(1, Math.min(limit, items.length))
    for (let i = 0; i < n; i += 1) workers.push(worker())
    await Promise.all(workers)
    return results
}

/** Phase A fetch: the `/library` index HTML parsed into per-card descriptors. */
async function fetchLibraryIndex(): Promise<ScrapedModelCard[] | null> {
    try {
        console.log(`GET ${OLLAMA_LIBRARY_URL}`)
        const { data, status } = await axios.get<string>(OLLAMA_LIBRARY_URL, {
            timeout: HTTP_TIMEOUT_MS,
            headers: REQUEST_HEADERS,
            responseType: 'text',
            transformResponse: [(v: string) => v]
        })
        if (typeof data !== 'string' || data.length === 0) {
            console.warn(`library scrape: empty body (status ${status})`)
            return null
        }
        return parseHtmlLibrary(data)
    } catch (err) {
        const status = isAxiosError(err) ? err.response?.status : undefined
        console.warn(`library scrape failed: ${status ?? ''} ${describeError(err)}`.trim())
        return null
    }
}

/** Fetch and parse a single model's `/library/<base>/tags` page. */
async function fetchDetailTags(base: string): Promise<DetailTagRow[] | null> {
    try {
        const url = OLLAMA_DETAIL_URL(base)
        const { data } = await axios.get<string>(url, {
            timeout: DETAIL_TIMEOUT_MS,
            headers: REQUEST_HEADERS,
            responseType: 'text',
            transformResponse: [(v: string) => v]
        })
        if (typeof data !== 'string' || data.length === 0) return null
        return parseHtmlTagsPage(data, base)
    } catch (err) {
        const status = isAxiosError(err) ? err.response?.status : undefined
        console.warn(
            `detail scrape failed for ${base}: ${status ?? ''} ${describeError(err)}`.trim()
        )
        return null
    }
}

/** Last-resort fallback: the curated `/api/tags` "what's new" feed. */
async function fetchApiTags(): Promise<OllamaTagsModel[] | null> {
    try {
        console.log(`GET ${OLLAMA_TAGS_API}`)
        const { data } = await axios.get<OllamaTagsResponse>(OLLAMA_TAGS_API, {
            timeout: HTTP_TIMEOUT_MS,
            headers: REQUEST_HEADERS
        })
        const raw = Array.isArray(data?.models) ? data.models : []
        return raw
            .map(normalizeApiTagsEntry)
            .filter((m): m is OllamaTagsModel => m != null)
            .filter(m => !isCloudTagName(m.name))
    } catch (err) {
        const status = isAxiosError(err) ? err.response?.status : undefined
        console.warn(`/api/tags fetch failed: ${status ?? ''} ${describeError(err)}`.trim())
        return null
    }
}

/**
 * Phase B: for every parsed index card, load its tags page and parse the
 * per-tag rows. Enriched bases contribute their full tag set; bases that
 * fail to enrich fall back to the index-level entries so a model never
 * disappears from the list.
 */
async function enrichFromDetailPages(
    cards: readonly ScrapedModelCard[],
    indexEntries: readonly OllamaTagsModel[]
): Promise<OllamaTagsModel[]> {
    const enrichedByBase = new Map<string, OllamaTagsModel[]>()
    let okCount = 0
    let failCount = 0
    let skippedCloudRows = 0

    await mapWithConcurrency(cards, DETAIL_CONCURRENCY, async (card, i) => {
        const rows = await fetchDetailTags(card.name)
        if (rows && rows.length > 0) {
            const kept: OllamaTagsModel[] = []
            for (const r of rows) {
                if (isCloudTagName(r.fullName)) {
                    skippedCloudRows += 1
                    continue
                }
                kept.push(detailRowToTagsModel(r, card))
            }
            if (kept.length > 0) {
                enrichedByBase.set(card.name, kept)
                okCount += 1
            } else {
                failCount += 1
            }
        } else {
            failCount += 1
        }
        if ((i + 1) % 25 === 0) {
            console.log(
                `detail-scrape progress: ${i + 1}/${cards.length} (ok=${okCount}, fail=${failCount})`
            )
        }
    })

    const indexByBase = new Map<string, OllamaTagsModel[]>()
    for (const entry of indexEntries) {
        const base = entry.name.includes(':') ? entry.name.split(':')[0] : entry.name
        const arr = indexByBase.get(base) ?? []
        arr.push(entry)
        indexByBase.set(base, arr)
    }

    const final: OllamaTagsModel[] = []
    for (const card of cards) {
        const enriched = enrichedByBase.get(card.name)
        if (enriched && enriched.length > 0) {
            final.push(...enriched)
        } else {
            final.push(...(indexByBase.get(card.name) ?? []))
        }
    }

    const cloudNote = skippedCloudRows > 0 ? `, skipped-cloud=${skippedCloudRows}` : ''
    console.log(
        `detail-scrape done: ${final.length} entries (enriched=${okCount} bases, fallback=${failCount} bases${cloudNote})`
    )
    return final
}

/**
 * Two-phase scrape returning the full, deduplicated model list. Phase A is
 * the cheap index scrape; Phase B enriches each model with real byte sizes.
 * Falls back to `/api/tags` only if Phase A itself fails.
 */
async function scrapeOllamaLibrary(): Promise<OllamaTagsModel[]> {
    const rawCards = await fetchLibraryIndex()
    if (rawCards && rawCards.length > 0) {
        const cloudCardCount = rawCards.filter(isCloudOnlyCard).length
        const cards = rawCards.filter(c => !isCloudOnlyCard(c))
        if (cloudCardCount > 0) {
            console.log(`filtered ${cloudCardCount} cloud-only card(s) from index`)
        }
        const indexEntries = cards.flatMap(cardToTags).filter(m => !isCloudTagName(m.name))
        console.log(
            `library scrape ok: ${indexEntries.length} entries across ${cards.length} models (phase A)`
        )
        const enriched = await enrichFromDetailPages(cards, indexEntries)
        return dedupeByName(enriched)
    }

    console.warn('library scrape returned 0 entries — falling back to /api/tags')
    const apiTags = await fetchApiTags()
    if (apiTags && apiTags.length > 0) {
        console.log(`api/tags fallback ok: ${apiTags.length} entries`)
        return dedupeByName(apiTags)
    }
    return []
}

function describeError(err: unknown): string {
    if (isAxiosError(err)) return err.message || 'request failed'
    if (err instanceof Error) return err.message
    return String(err)
}

async function main(): Promise<void> {
    const started = Date.now()
    const models = await scrapeOllamaLibrary()
    if (models.length === 0) {
        console.error('scrape produced 0 models; refusing to overwrite ollama-models.json')
        process.exit(1)
    }

    const payload = {
        scrapedAt: new Date().toISOString(),
        source: OLLAMA_LIBRARY_URL,
        count: models.length,
        models
    }
    writeFileSync(OUTPUT_PATH, `${JSON.stringify(payload, null, 4)}\n`)
    console.log(`wrote ${models.length} models to ${OUTPUT_PATH} in ${Date.now() - started}ms`)
}

void main().catch(err => {
    console.error(describeError(err))
    process.exit(1)
})
