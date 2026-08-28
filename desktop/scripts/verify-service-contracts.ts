#!/usr/bin/env tsx
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Desktop ↔ services contract verifier for the monorepo.
 *
 * Parses sibling `services/` Go sources for JSON-RPC methods each binary EMITS
 * and HANDLES, then cross-checks against the Electron service bridge. Also
 * verifies runtime binary inventory, macOS firewall helper parity, and keeps
 * `docs/services-api.md` in sync with the current tree.
 *
 * Usage:
 *   npm run service-contracts           Read-only report.
 *   npm run service-contracts:write       Regenerate docs/services-api.md.
 *   npm run service-contracts:check       CI: exit 1 on drift or stale API doc.
 *
 * Flags:
 *   --write   Regenerate docs/services-api.md.
 *   --check   Exit 1 on unignored drift or stale generated doc.
 */

import { existsSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import {
    modularFirewallBinaryBaseNames,
    modularShippedBinaryBaseNames
} from '@/shared/constants/modular-binaries'

type JsonPrimitive = string | number | boolean | null
type JsonValue = JsonPrimitive | JsonObject | JsonValue[]
interface JsonObject {
    [key: string]: JsonValue
}

function isJsonObject(value: JsonValue): value is JsonObject {
    return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function stringRecord(value: JsonValue | undefined): Record<string, string> {
    const out: Record<string, string> = {}
    if (value && isJsonObject(value)) {
        for (const [key, v] of Object.entries(value)) {
            if (typeof v === 'string') out[key] = v
        }
    }
    return out
}

const ROOT = process.cwd()
const SERVICES_ROOT = path.resolve(ROOT, '..', 'services')
const EXCEPTIONS_PATH = path.resolve(ROOT, 'docs/service-contract-exceptions.json')
const API_DOC_PATH = path.resolve(ROOT, 'docs/services-api.md')
const BACKEND_DOC_PATH = path.resolve(ROOT, 'docs/services-backend.md')
const PARITY_DOC_PATH = path.resolve(ROOT, 'docs/services-parity.md')
const PRIVILEGED_HELPER_PATH = path.resolve(ROOT, 'native/PrivilegedHelper/main.swift')

const UI_SCAN_TARGETS = ['src/electron/service-bridge', 'src/shared/types/ws-channels.ts']

function hasFlag(name: string): boolean {
    return process.argv.includes(`--${name}`)
}

interface ContractExceptions {
    ignoredMethods: Record<string, string>
}

function readExceptions(): ContractExceptions {
    const parsed: JsonValue = JSON.parse(readFileSync(EXCEPTIONS_PATH, 'utf8'))
    if (!isJsonObject(parsed)) throw new Error(`${EXCEPTIONS_PATH} is not a JSON object`)
    return { ignoredMethods: stringRecord(parsed['ignoredMethods']) }
}

/**
 * A `notify(method, …)` call whose method could not be resolved to a literal.
 * `line` is reported on the console only. Source lines move for reasons that
 * have nothing to do with the JSON-RPC surface — adding an import shifts every
 * line below it — so a generated doc that embedded them would fail its own
 * freshness gate on main after any unrelated edit to the file.
 */
interface DynamicSite {
    label: string
    file: string
    line: number
}

interface BinarySurface {
    dir: string
    emits: Set<string>
    handles: Set<string>
    dynamic: DynamicSite[]
}

const METHOD_RE = /^(?:[a-z][a-zA-Z0-9]*(?:[:/][a-zA-Z0-9.-]+)+|ready|error)$/

function isMethodish(s: string): boolean {
    return METHOD_RE.test(s)
}

function listGoFiles(dir: string): string[] {
    const out: string[] = []
    const walk = (d: string): void => {
        for (const entry of readdirSync(d)) {
            const full = path.join(d, entry)
            const st = statSync(full)
            if (st.isDirectory()) {
                if (entry === 'testdata' || entry === '.git') continue
                walk(full)
            } else if (entry.endsWith('.go') && !entry.endsWith('_test.go')) {
                out.push(full)
            }
        }
    }
    walk(dir)
    return out
}

function buildConstMap(files: string[]): Map<string, string> {
    const map = new Map<string, string>()
    const re = /\b([A-Za-z_]\w*)\s*=\s*"([^"]+)"/g
    for (const file of files) {
        const text = readFileSync(file, 'utf8')
        for (const m of text.matchAll(re)) {
            const value = m[2]
            if (isMethodish(value)) map.set(m[1], value)
        }
    }
    return map
}

const QUOTED_RE = /"([^"]+)"/g
const IDENT_RE = /\b([A-Za-z_]\w*)\b/g
const NOTIFY_LITERAL_RE = /(?:notify|emit|publish|broadcast)\(\s*"([^"]+)"/gi
const NOTIFY_PREFIX_CONCAT_RE = /(?:notify|emit|publish|broadcast)\(\s*"([^"]+)"\s*\+/i
const NOTIFY_IDENT_RE = /(?:notify|emit|publish|broadcast)\(\s*([A-Za-z_]\w*)\s*,/gi

/** A `| `nvpair-ui-broker` | 0.37.0 | … |` row — forbidden in hand-maintained docs. */
const DOC_COMPONENT_ROW_RE = /^\|\s*`([a-z0-9-]+)`\s*\|\s*(\d+\.\d+\.\d+)\s*\|/gm
/** "…at product version 1.4.2" / "- Backend product: `1.4.2`" — forbidden. */
const DOC_PRODUCT_VERSION_RE = /(?:backend product|product version)[^\d\n]{0,4}(\d+\.\d+\.\d+)/gi

function extractSurface(dir: string): BinarySurface {
    const surface: BinarySurface = { dir, emits: new Set(), handles: new Set(), dynamic: [] }
    const files = listGoFiles(dir)
    const constMap = buildConstMap(files)

    for (const file of files) {
        const rel = path.basename(file)
        const lines = readFileSync(file, 'utf8').split('\n')
        lines.forEach((line, idx) => {
            const lineNo = idx + 1
            if (!line.includes('signal.Notify')) {
                for (const m of line.matchAll(NOTIFY_LITERAL_RE)) {
                    if (isMethodish(m[1])) surface.emits.add(m[1])
                }
                const concat = line.match(NOTIFY_PREFIX_CONCAT_RE)
                if (concat)
                    surface.dynamic.push({ label: `${concat[1]}*`, file: rel, line: lineNo })
                for (const m of line.matchAll(NOTIFY_IDENT_RE)) {
                    const resolved = constMap.get(m[1])
                    if (resolved) surface.emits.add(resolved)
                    else if (!concat && !line.includes('func(')) {
                        surface.dynamic.push({ label: `${m[1]} (var)`, file: rel, line: lineNo })
                    }
                }
            }
            if (/^\s*case\s+/.test(line)) {
                for (const m of line.matchAll(QUOTED_RE)) {
                    if (isMethodish(m[1])) surface.handles.add(m[1])
                }
                for (const m of line.matchAll(IDENT_RE)) {
                    const resolved = constMap.get(m[1])
                    if (resolved) surface.handles.add(resolved)
                }
            }
        })
    }
    return surface
}

function listTsFiles(target: string): string[] {
    const full = path.resolve(ROOT, target)
    if (!existsSync(full)) return []
    if (statSync(full).isFile()) return [full]
    const out: string[] = []
    const walk = (d: string): void => {
        for (const entry of readdirSync(d)) {
            const f = path.join(d, entry)
            const st = statSync(f)
            if (st.isDirectory()) walk(f)
            else if (entry.endsWith('.ts')) out.push(f)
        }
    }
    walk(full)
    return out
}

function loadUiText(): string {
    const parts: string[] = []
    for (const target of UI_SCAN_TARGETS) {
        for (const file of listTsFiles(target)) parts.push(readFileSync(file, 'utf8'))
    }
    return parts.join('\n')
}

interface MethodRow {
    method: string
    direction: 'notification' | 'request'
    referenced: boolean
    ignored: boolean
}

interface BinaryReport {
    dir: string
    integrated: boolean
    rows: MethodRow[]
    dynamic: DynamicSite[]
}

interface Drift {
    missingNotifications: string[]
    unusedRequests: string[]
    notIntegratedBinaries: string[]
}

interface FirewallDrift {
    missing: string[]
    unexpected: string[]
}

function buildFirewallDrift(): FirewallDrift {
    const source = readFileSync(PRIVILEGED_HELPER_PATH, 'utf8')
    const declaration = /static\s+let\s+networkedBinaries\s*=\s*\[([\s\S]*?)\]/.exec(source)
    if (!declaration) {
        throw new Error(
            `${PRIVILEGED_HELPER_PATH} does not declare static let networkedBinaries = [...]`
        )
    }
    const expected = new Set(modularFirewallBinaryBaseNames())
    const actual = new Set([...declaration[1].matchAll(/"([^"]+)"/g)].map(match => match[1]))
    return {
        missing: [...expected].filter(name => !actual.has(name)).sort(),
        unexpected: [...actual].filter(name => !expected.has(name)).sort()
    }
}

function buildReports(
    repo: string,
    exceptions: ContractExceptions,
    versions: Record<string, string>
): { reports: BinaryReport[]; drift: Drift } {
    const uiText = loadUiText()
    const ours = new Set(modularShippedBinaryBaseNames())
    const ignored = exceptions.ignoredMethods
    const backendDirs = Object.keys(versions).sort()
    const reports: BinaryReport[] = []
    const drift: Drift = { missingNotifications: [], unusedRequests: [], notIntegratedBinaries: [] }

    for (const dir of backendDirs) {
        const abs = path.join(repo, dir)
        const integrated = ours.has(dir)
        if (!integrated) drift.notIntegratedBinaries.push(dir)
        if (!existsSync(abs)) {
            reports.push({ dir, integrated, rows: [], dynamic: [] })
            continue
        }
        const surface = extractSurface(abs)
        const rows: MethodRow[] = []
        for (const method of [...surface.emits].sort()) {
            const referenced = uiText.includes(method)
            const isIgnored = method in ignored
            rows.push({ method, direction: 'notification', referenced, ignored: isIgnored })
            if (!referenced && !isIgnored) drift.missingNotifications.push(`${dir} → ${method}`)
        }
        for (const method of [...surface.handles].sort()) {
            if (surface.emits.has(method)) continue
            const referenced = uiText.includes(method)
            const isIgnored = method in ignored
            rows.push({ method, direction: 'request', referenced, ignored: isIgnored })
            if (!referenced && !isIgnored) drift.unusedRequests.push(`${dir} → ${method}`)
        }
        reports.push({ dir, integrated, rows, dynamic: surface.dynamic })
    }

    return { reports, drift }
}

function mark(row: MethodRow): string {
    if (row.referenced) return '✅ yes'
    if (row.ignored) return '➖ ignored'
    return row.direction === 'notification' ? '❌ **MISSING**' : '⚠️ not called'
}

interface DynamicSiteGroup {
    label: string
    file: string
    /** First occurrence, used for ordering only — never rendered into the doc. */
    line: number
    count: number
}

/**
 * Collapse sites to one entry per file and label, ordered by file then first
 * occurrence so the rendered list does not depend on directory read order. The
 * result changes only when the set of unresolved notify sites itself changes.
 */
function groupDynamicSites(sites: DynamicSite[]): DynamicSiteGroup[] {
    const groups = new Map<string, DynamicSiteGroup>()
    for (const site of sites) {
        const key = `${site.file}\u0000${site.label}`
        const existing = groups.get(key)
        if (existing) existing.count += 1
        else groups.set(key, { label: site.label, file: site.file, line: site.line, count: 1 })
    }
    return [...groups.values()].sort((a, b) => {
        if (a.file !== b.file) return a.file < b.file ? -1 : 1
        return a.line - b.line
    })
}

function renderApiDoc(reports: BinaryReport[], drift: Drift): string {
    const L: string[] = []
    L.push('# services JSON-RPC surface (GENERATED)')
    L.push('')
    L.push('> **Do not edit by hand.** Regenerate with `npm run service-contracts:write`')
    L.push('> (or `tsx scripts/verify-service-contracts.ts --write`). Extracts the')
    L.push('> current sibling `services/` tree and cross-checks it against the')
    L.push('> Electron bridge. Integration notes live in `docs/services-backend.md`;')
    L.push('> capability status lives in `docs/services-parity.md`.')
    L.push('')
    L.push(`- **Source tree**: \`services/\` in this monorepo`)
    L.push(`- **Versions**: see \`services/versions.json\` (product, installer, and per-component)`)
    L.push(
        '- **Legend**: ✅ referenced by the bridge · ❌ MISSING (no consumer/caller) · ➖ ignored (see `docs/service-contract-exceptions.json`)'
    )
    L.push('')

    L.push('## Drift summary')
    L.push('')
    L.push('### Notifications emitted but NOT consumed by the bridge')
    if (drift.missingNotifications.length === 0) L.push('- none ✅')
    else for (const m of drift.missingNotifications) L.push(`- ❌ ${m}`)
    L.push('')
    L.push('### Requests the backend handles but the bridge never calls (unused capability)')
    if (drift.unusedRequests.length === 0) L.push('- none ✅')
    else for (const m of drift.unusedRequests) L.push(`- ⚠️ ${m}`)
    L.push('')
    L.push('### Backend binaries not listed in `modular-binaries.ts`')
    if (drift.notIntegratedBinaries.length === 0) L.push('- none ✅')
    else
        for (const b of drift.notIntegratedBinaries)
            L.push(`- ❌ ${b} (add to MODULAR_RUNTIME_BINARIES)`)
    L.push('')

    for (const report of reports) {
        const tag = report.integrated ? '' : ' — ❌ NOT in modular-binaries.ts'
        L.push(`## ${report.dir}${tag}`)
        L.push('')
        if (report.rows.length === 0) {
            L.push('_No JSON-RPC methods detected (HTTP-only binary, or source not present)._')
            L.push('')
        } else {
            L.push('| Method | Direction | In bridge? |')
            L.push('|---|---|---|')
            for (const row of report.rows) {
                const dir =
                    row.direction === 'notification'
                        ? 'notification (we consume)'
                        : 'request (we call)'
                L.push(`| \`${row.method}\` | ${dir} | ${mark(row)} |`)
            }
            L.push('')
        }
        if (report.dynamic.length > 0) {
            L.push(
                '**Dynamic / unresolved notify sites (verify by hand — `npm run service-contracts` prints the line numbers):**'
            )
            for (const group of groupDynamicSites(report.dynamic)) {
                const sites = group.count > 1 ? `, ${group.count} sites` : ''
                L.push(`- \`${group.label}  (${group.file}${sites})\``)
            }
            L.push('')
        }
    }

    return L.join('\n') + '\n'
}

function printDrift(drift: Drift): void {
    console.log('\n── Service contract (Go JSON-RPC surface vs bridge) ───────────')
    if (drift.notIntegratedBinaries.length > 0) {
        console.log('❌ backend binaries not in modular-binaries.ts:')
        for (const b of drift.notIntegratedBinaries) console.log(`  - ${b}`)
    }
    if (drift.missingNotifications.length === 0) {
        console.log('✅ every emitted notification is referenced by the bridge (or ignored).')
    } else {
        console.log('❌ notifications emitted but NOT consumed:')
        for (const m of drift.missingNotifications) console.log(`  - ${m}`)
    }
    if (drift.unusedRequests.length > 0) {
        console.log(
            '\n⚠️  requests the backend handles but the bridge never calls (opportunities):'
        )
        for (const m of drift.unusedRequests) console.log(`  - ${m}`)
    }
}

/**
 * The exact source locations, kept out of the generated doc on purpose (see
 * `DynamicSite`). This report is where a human verifying an unresolved notify
 * site gets a pointer to jump to.
 */
function printDynamicSites(reports: BinaryReport[]): void {
    const withSites = reports.filter(report => report.dynamic.length > 0)
    if (withSites.length === 0) return
    console.log('\n── Dynamic / unresolved notify sites (verify by hand) ──────────')
    for (const report of withSites) {
        for (const site of report.dynamic) {
            console.log(`  - ${report.dir} → ${site.label}  (${site.file}:${site.line})`)
        }
    }
}

/**
 * Docs must not restate product or component versions — those live only in
 * `services/versions.json`. Hardcoded numbers go stale on every bump, and a
 * generated copy is no better: the release bot rewrites versions.json after
 * merge without regenerating anything, so a doc that embedded them would fail
 * its own freshness gate on main until someone regenerated by hand.
 */
function buildHardcodedVersionProblems(): string[] {
    const problems: string[] = []
    for (const docPath of [BACKEND_DOC_PATH, PARITY_DOC_PATH]) {
        const doc = path.basename(docPath)
        if (!existsSync(docPath)) {
            problems.push(`${doc} not found`)
            continue
        }
        const text = readFileSync(docPath, 'utf8')

        for (const match of text.matchAll(DOC_PRODUCT_VERSION_RE)) {
            problems.push(
                `${doc} hardcodes product version ${match[1]} — use services/versions.json`
            )
        }

        for (const match of text.matchAll(DOC_COMPONENT_ROW_RE)) {
            const [, name, value] = match
            problems.push(`${doc} hardcodes \`${name}\` ${value} — use services/versions.json`)
        }
    }
    return problems
}

function printHardcodedVersionProblems(problems: string[]): void {
    console.log('\n── Hand-maintained docs must not hardcode backend versions ────')
    if (problems.length === 0) {
        console.log(
            '✅ no hardcoded product/component versions in services-backend.md or services-parity.md.'
        )
        return
    }
    console.log('❌ hardcoded backend versions found:')
    for (const problem of problems) console.log(`  - ${problem}`)
}

function printFirewallDrift(drift: FirewallDrift): void {
    console.log('\n── macOS firewall helper parity ────────────────────────────────')
    if (drift.missing.length === 0 && drift.unexpected.length === 0) {
        console.log('✅ Swift networkedBinaries matches canonical firewall metadata.')
        return
    }
    if (drift.missing.length > 0) {
        console.log('❌ firewall-required binaries missing from Swift networkedBinaries:')
        for (const name of drift.missing) console.log(`  - ${name}`)
    }
    if (drift.unexpected.length > 0) {
        console.log('❌ stale binaries present in Swift networkedBinaries:')
        for (const name of drift.unexpected) console.log(`  - ${name}`)
    }
}

function main(): void {
    const write = hasFlag('write')
    const check = hasFlag('check')

    if (!existsSync(SERVICES_ROOT)) {
        console.error(`services/ tree not found at: ${SERVICES_ROOT}`)
        process.exit(2)
    }

    const versionsPath = path.join(SERVICES_ROOT, 'versions.json')
    if (!existsSync(versionsPath)) {
        console.error(`versions.json not found at ${versionsPath}`)
        process.exit(2)
    }

    const versionsRaw: JsonValue = JSON.parse(readFileSync(versionsPath, 'utf8'))
    const versionsObj: JsonObject = isJsonObject(versionsRaw) ? versionsRaw : {}
    // Only the component key set is read: it is the canonical list of backend
    // binaries to report on. The version numbers are deliberately not rendered —
    // they live in versions.json alone, so a release bump can never make the
    // generated doc stale (and turn the freshness gate red on main).
    const versions = stringRecord(versionsObj['components'])

    const exceptions = readExceptions()
    console.log(`services: ${SERVICES_ROOT}`)

    const { reports, drift } = buildReports(SERVICES_ROOT, exceptions, versions)
    printDrift(drift)
    printDynamicSites(reports)
    const hardcodedVersions = buildHardcodedVersionProblems()
    printHardcodedVersionProblems(hardcodedVersions)
    const firewallDrift = buildFirewallDrift()
    printFirewallDrift(firewallDrift)

    const generated = renderApiDoc(reports, drift)

    if (write) {
        writeFileSync(API_DOC_PATH, generated)
        console.log('\n📝 wrote docs/services-api.md')
    }

    if (check) {
        const existing = existsSync(API_DOC_PATH)
            ? readFileSync(API_DOC_PATH, 'utf8').replace(/\r\n/g, '\n')
            : ''
        const stale = existing !== generated
        const hasDrift =
            drift.missingNotifications.length > 0 ||
            drift.notIntegratedBinaries.length > 0 ||
            firewallDrift.missing.length > 0 ||
            firewallDrift.unexpected.length > 0
        if (stale) {
            console.error(
                '\n❌ docs/services-api.md is stale — run `npm run service-contracts:write`.'
            )
        }
        if (hasDrift) {
            console.error(
                '\n❌ unignored API or firewall drift — resolve the mismatch or update service-contract-exceptions.json.'
            )
        }
        if (hardcodedVersions.length > 0) {
            console.error(
                '\n❌ remove hardcoded backend versions from docs/services-backend.md and docs/services-parity.md — versions live in services/versions.json.'
            )
        }
        if (stale || hasDrift || hardcodedVersions.length > 0) process.exit(1)
        console.log('\n✅ check passed: doc fresh, no hardcoded versions, no unignored drift.')
    } else if (!write) {
        console.log('\n(run `npm run service-contracts:write` to refresh docs/services-api.md)')
    }
}

main()
