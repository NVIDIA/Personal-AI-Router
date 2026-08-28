#!/usr/bin/env tsx
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Dead-code analyzer for PAIR. Runs knip (cross-file unused exports/files/deps)
 * plus a stricter tsc --noEmit pass (in-file unused locals/params/unreachable).
 *
 * Knip config: knip.json (entry points for electron main, preload, and UI).
 * Omissions: dead-code-omissions.json — known false positives filtered from the report.
 *
 * Usage: npm run dead-code          (report only, always exits 0)
 *        npm run dead-code:check    (CI gate — exits 1 on any unfiltered finding)
 * Output: dead-code-report.md
 */

import { spawnSync } from 'node:child_process'
import { writeFileSync, readFileSync, unlinkSync, existsSync } from 'node:fs'
import { resolve } from 'node:path'

const ROOT = process.cwd()
const REPORT_PATH = resolve(ROOT, 'dead-code-report.md')
const OMISSIONS_PATH = resolve(ROOT, 'dead-code-omissions.json')
const TSCONFIG_NODE = resolve(ROOT, 'tsconfig.deadcode.node.json')
const TSCONFIG_WEB = resolve(ROOT, 'tsconfig.deadcode.web.json')

// ── Omissions ────────────────────────────────────────────────────────────────

interface Omissions {
    /** File paths (relative to repo root) to ignore entirely */
    files: string[]
    /** "file:symbol" or just "symbol" patterns to ignore in unused exports/types */
    exports: string[]
    /** Type names to ignore in unused types */
    types: string[]
    /** Package names to ignore in unused dependencies */
    dependencies: string[]
}

function loadOmissions(): Omissions {
    const empty: Omissions = { files: [], exports: [], types: [], dependencies: [] }
    if (!existsSync(OMISSIONS_PATH)) return empty
    try {
        const raw = JSON.parse(readFileSync(OMISSIONS_PATH, 'utf8'))
        return {
            files: Array.isArray(raw.files) ? raw.files : [],
            exports: Array.isArray(raw.exports) ? raw.exports : [],
            types: Array.isArray(raw.types) ? raw.types : [],
            dependencies: Array.isArray(raw.dependencies) ? raw.dependencies : []
        }
    } catch {
        console.warn('Warning: could not parse dead-code-omissions.json, using empty omissions')
        return empty
    }
}

function isOmittedFile(file: string, omissions: Omissions): boolean {
    return omissions.files.some(pattern => file === pattern || file.startsWith(pattern + '/'))
}

function isOmittedExport(file: string, symbolName: string, omissions: Omissions): boolean {
    return omissions.exports.some(pattern => {
        if (pattern.includes(':')) {
            return pattern === `${file}:${symbolName}`
        }
        return pattern === symbolName
    })
}

function isOmittedType(file: string, symbolName: string, omissions: Omissions): boolean {
    if (isOmittedExport(file, symbolName, omissions)) return true
    return omissions.types.some(t => t === symbolName)
}

function isOmittedDependency(name: string, omissions: Omissions): boolean {
    return omissions.dependencies.includes(name)
}

// ── Strict tsconfig generation ───────────────────────────────────────────────

function strictTsconfig(base: string) {
    return {
        extends: `./${base}`,
        compilerOptions: {
            composite: false,
            noUnusedLocals: true,
            noUnusedParameters: true,
            allowUnreachableCode: false,
            allowUnusedLabels: false
        }
    }
}

// ── Process runner ───────────────────────────────────────────────────────────

interface SpawnResult {
    stdout: string
    stderr: string
    status: number
}

function run(cmd: string, args: string[]): SpawnResult {
    const isWin = process.platform === 'win32'
    const result = spawnSync(isWin ? `${cmd}.cmd` : cmd, args, {
        cwd: ROOT,
        encoding: 'utf8',
        shell: isWin
    })
    return {
        stdout: result.stdout ?? '',
        stderr: result.stderr ?? '',
        status: result.status ?? 0
    }
}

// ── Knip ─────────────────────────────────────────────────────────────────────

interface KnipItem {
    name: string
    line?: number
    symbols?: string[]
}

interface KnipIssue {
    file: string
    files?: unknown[]
    exports?: KnipItem[]
    types?: KnipItem[]
    enumMembers?: KnipItem[]
    duplicates?: KnipItem[]
    unlisted?: KnipItem[]
    binaries?: KnipItem[]
    dependencies?: KnipItem[]
    devDependencies?: KnipItem[]
    optionalPeerDependencies?: KnipItem[]
}

interface KnipOutput {
    issues?: KnipIssue[]
}

function runKnip(): KnipOutput {
    console.log('Running knip ...')
    const { stdout, stderr } = run('npx', [
        'knip',
        '--reporter',
        'json',
        '--include',
        'files,exports,types,duplicates,enumMembers,unlisted,binaries,dependencies,devDependencies,optionalPeerDependencies',
        '--no-exit-code'
    ])
    if (!stdout.trim()) {
        console.error(stderr)
        throw new Error('knip produced no output')
    }
    return JSON.parse(stdout)
}

// ── Strict tsc ───────────────────────────────────────────────────────────────

// TS error codes for dead-code diagnostics:
//   TS6133 — declared but never read
//   TS6138 — property declared but never read
//   TS6192 — all imports unused
//   TS6196 — declared but never used (types)
//   TS6198 — all destructured elements unused
//   TS7027 — unreachable code
//   TS7028 — unused label
const DEAD_CODE_TS_ERRORS = /error TS(6133|6138|6192|6196|6198|7027|7028)\b/

function runTscStrict(tsconfigPath: string, label: string): string[] {
    console.log(`Running strict tsc (${label}) ...`)
    const { stdout } = run('npx', ['tsc', '--noEmit', '-p', tsconfigPath])
    return stdout
        .split(/\r?\n/)
        .map(line => line.trim())
        .filter(line => DEAD_CODE_TS_ERRORS.test(line))
}

function writeStrictTsconfigs(): void {
    writeFileSync(TSCONFIG_NODE, JSON.stringify(strictTsconfig('tsconfig.node.json'), null, 4))
    writeFileSync(TSCONFIG_WEB, JSON.stringify(strictTsconfig('tsconfig.web.json'), null, 4))
}

function cleanupStrictTsconfigs(): void {
    for (const p of [TSCONFIG_NODE, TSCONFIG_WEB]) {
        try {
            if (existsSync(p)) unlinkSync(p)
        } catch {
            /* ignore */
        }
    }
}

// ── Aggregation (with omissions filtering) ───────────────────────────────────

interface AggregatedIssues {
    unusedFiles: string[]
    unusedExportsByFile: Map<string, KnipItem[]>
    unusedTypesByFile: Map<string, KnipItem[]>
    unusedEnumMembersByFile: Map<string, KnipItem[]>
    duplicatesByFile: Map<string, KnipItem[]>
    unlistedByFile: Map<string, KnipItem[]>
    missingBinariesByFile: Map<string, KnipItem[]>
    unusedDependencies: KnipItem[]
    unusedDevDependencies: KnipItem[]
    unusedOptionalPeers: KnipItem[]
    omittedCount: number
}

function aggregate(issues: KnipIssue[], omissions: Omissions): AggregatedIssues {
    const unusedFiles: string[] = []
    const unusedExportsByFile = new Map<string, KnipItem[]>()
    const unusedTypesByFile = new Map<string, KnipItem[]>()
    const unusedEnumMembersByFile = new Map<string, KnipItem[]>()
    const duplicatesByFile = new Map<string, KnipItem[]>()
    const unlistedByFile = new Map<string, KnipItem[]>()
    const missingBinariesByFile = new Map<string, KnipItem[]>()
    const unusedDependencies: KnipItem[] = []
    const unusedDevDependencies: KnipItem[] = []
    const unusedOptionalPeers: KnipItem[] = []
    let omittedCount = 0

    function filterItems(
        file: string,
        items: KnipItem[],
        checker: (file: string, name: string, om: Omissions) => boolean
    ): KnipItem[] {
        const kept: KnipItem[] = []
        for (const item of items) {
            if (checker(file, item.name, omissions)) {
                omittedCount++
            } else {
                kept.push(item)
            }
        }
        return kept
    }

    for (const issue of issues) {
        if (isOmittedFile(issue.file, omissions)) {
            if (issue.files?.length) omittedCount++
            omittedCount += (issue.exports?.length ?? 0) + (issue.types?.length ?? 0)
            continue
        }

        if (issue.files?.length) unusedFiles.push(issue.file)

        if (issue.exports?.length) {
            const kept = filterItems(issue.file, issue.exports, isOmittedExport)
            if (kept.length) unusedExportsByFile.set(issue.file, kept)
        }
        if (issue.types?.length) {
            const kept = filterItems(issue.file, issue.types, isOmittedType)
            if (kept.length) unusedTypesByFile.set(issue.file, kept)
        }
        if (issue.enumMembers?.length) unusedEnumMembersByFile.set(issue.file, issue.enumMembers)
        if (issue.duplicates?.length) duplicatesByFile.set(issue.file, issue.duplicates)
        if (issue.unlisted?.length) unlistedByFile.set(issue.file, issue.unlisted)
        if (issue.binaries?.length) missingBinariesByFile.set(issue.file, issue.binaries)

        if (issue.dependencies?.length) {
            for (const d of issue.dependencies) {
                if (isOmittedDependency(d.name, omissions)) omittedCount++
                else unusedDependencies.push(d)
            }
        }
        if (issue.devDependencies?.length) {
            for (const d of issue.devDependencies) {
                if (isOmittedDependency(d.name, omissions)) omittedCount++
                else unusedDevDependencies.push(d)
            }
        }
        if (issue.optionalPeerDependencies?.length) {
            unusedOptionalPeers.push(...issue.optionalPeerDependencies)
        }
    }

    unusedFiles.sort()
    unusedDependencies.sort((a, b) => a.name.localeCompare(b.name))
    unusedDevDependencies.sort((a, b) => a.name.localeCompare(b.name))
    unusedOptionalPeers.sort((a, b) => a.name.localeCompare(b.name))

    return {
        unusedFiles,
        unusedExportsByFile,
        unusedTypesByFile,
        unusedEnumMembersByFile,
        duplicatesByFile,
        unlistedByFile,
        missingBinariesByFile,
        unusedDependencies,
        unusedDevDependencies,
        unusedOptionalPeers,
        omittedCount
    }
}

// ── Report rendering ─────────────────────────────────────────────────────────

function countExports(map: Map<string, KnipItem[]>): number {
    let total = 0
    for (const v of map.values()) total += v.length
    return total
}

function sortedEntries(map: Map<string, KnipItem[]>): [string, KnipItem[]][] {
    return [...map.entries()].sort(([a], [b]) => a.localeCompare(b))
}

function section(title: string): string {
    return `\n---\n\n## ${title}\n\n`
}

function renderFilesSection(unusedFiles: string[]): string {
    if (unusedFiles.length === 0) return 'No unused files detected.\n'
    const rows = unusedFiles.map((f, i) => `| ${i + 1} | [\`${f}\`](${f}) |`).join('\n')
    return `| # | Path |\n|---:|---|\n${rows}\n`
}

function renderExportsSection(byFile: Map<string, KnipItem[]>, heading: string): string {
    if (byFile.size === 0) return `No ${heading}.\n`
    const parts: string[] = []
    for (const [file, items] of sortedEntries(byFile)) {
        parts.push(`\n### \`${file}\`\n`)
        parts.push('| Line | Symbol |')
        parts.push('|---:|---|')
        for (const item of items) {
            parts.push(`| ${item.line} | \`${item.name}\` |`)
        }
        parts.push('')
    }
    return parts.join('\n')
}

function renderTypesSection(byFile: Map<string, KnipItem[]>): string {
    if (byFile.size === 0) return 'No unused exported types.\n'
    const parts: string[] = ['| Symbol | Location |', '|---|---|']
    for (const [file, items] of sortedEntries(byFile)) {
        for (const item of items) {
            parts.push(`| \`${item.name}\` | [\`${file}:${item.line}\`](${file}) |`)
        }
    }
    return parts.join('\n') + '\n'
}

function renderEnumMembersSection(byFile: Map<string, KnipItem[]>): string {
    if (byFile.size === 0) return 'No unused enum members.\n'
    const parts: string[] = ['| Enum member | Location |', '|---|---|']
    for (const [file, items] of sortedEntries(byFile)) {
        for (const item of items) {
            parts.push(`| \`${item.name}\` | [\`${file}:${item.line}\`](${file}) |`)
        }
    }
    return parts.join('\n') + '\n'
}

function renderDuplicatesSection(byFile: Map<string, KnipItem[]>): string {
    if (byFile.size === 0) return 'No duplicate exports.\n'
    const parts: string[] = ['| Location | Duplicates |', '|---|---|']
    for (const [file, items] of sortedEntries(byFile)) {
        for (const item of items) {
            const label = Array.isArray(item.symbols)
                ? item.symbols.join(', ')
                : (item.name ?? JSON.stringify(item))
            parts.push(`| [\`${file}:${item.line ?? ''}\`](${file}) | \`${label}\` |`)
        }
    }
    return parts.join('\n') + '\n'
}

function renderUnlistedSection(byFile: Map<string, KnipItem[]>): string {
    if (byFile.size === 0) return 'None.\n'
    const parts: string[] = ['| Package | Imported from |', '|---|---|']
    for (const [file, items] of sortedEntries(byFile)) {
        for (const item of items) {
            parts.push(`| \`${item.name}\` | [\`${file}:${item.line}\`](${file}) |`)
        }
    }
    return parts.join('\n') + '\n'
}

function renderMissingBinariesSection(byFile: Map<string, KnipItem[]>): string {
    if (byFile.size === 0) return 'None.\n'
    const parts: string[] = ['| Binary | Referenced from |', '|---|---|']
    for (const [file, items] of sortedEntries(byFile)) {
        for (const item of items) {
            parts.push(`| \`${item.name}\` | [\`${file}\`](${file}) |`)
        }
    }
    return parts.join('\n') + '\n'
}

function renderPackageListSection(items: KnipItem[], hint: string | null): string {
    if (items.length === 0) return 'None.\n'
    const parts: string[] = ['| # | Package |', '|---:|---|']
    items.forEach((item, i) => {
        parts.push(`| ${i + 1} | \`${item.name}\` |`)
    })
    if (hint) {
        parts.push('')
        parts.push(hint)
    }
    return parts.join('\n') + '\n'
}

function renderTscSection(nodeLines: string[], webLines: string[]): string {
    const all = [...nodeLines, ...webLines]
    if (all.length === 0) {
        return '`tsc --noEmit` with `noUnusedLocals`, `noUnusedParameters`, `allowUnreachableCode: false`, and `allowUnusedLabels: false` against both `tsconfig.node.json` and `tsconfig.web.json`:\n\n**Zero diagnostics.**\n'
    }
    const fence = (lines: string[]) =>
        lines.length ? '```\n' + lines.join('\n') + '\n```\n' : '_clean_\n'
    return [
        '`tsc --noEmit` with `noUnusedLocals`, `noUnusedParameters`, `allowUnreachableCode: false`, and `allowUnusedLabels: false`:',
        '',
        '### `tsconfig.node.json`',
        fence(nodeLines),
        '### `tsconfig.web.json`',
        fence(webLines)
    ].join('\n')
}

interface ReportData {
    knip: KnipOutput
    tscNode: string[]
    tscWeb: string[]
    omissions: Omissions
}

function renderReport({ knip, tscNode, tscWeb, omissions }: ReportData): string {
    const agg = aggregate(knip.issues ?? [], omissions)
    const exportCount = countExports(agg.unusedExportsByFile)
    const typeCount = countExports(agg.unusedTypesByFile)
    const enumCount = countExports(agg.unusedEnumMembersByFile)
    const duplicateCount = countExports(agg.duplicatesByFile)
    const unlistedCount = countExports(agg.unlistedByFile)
    const binariesCount = countExports(agg.missingBinariesByFile)
    const tscCount = tscNode.length + tscWeb.length
    const depCount = agg.unusedDependencies.length
    const devDepCount = agg.unusedDevDependencies.length
    const optPeerCount = agg.unusedOptionalPeers.length
    const now = new Date().toISOString().replace('T', ' ').slice(0, 19) + 'Z'

    const lines: string[] = []
    lines.push('# Dead Code Report')
    lines.push('')
    lines.push(`_Generated ${now} by \`npm run dead-code\`._`)
    lines.push('')
    lines.push(
        'Runs `knip` (config in [`knip.json`](knip.json)) plus a stricter `tsc --noEmit` pass for in-file dead code. Re-run any time.'
    )
    if (agg.omittedCount > 0) {
        lines.push('')
        lines.push(
            `_${agg.omittedCount} known false positive(s) filtered via [\`dead-code-omissions.json\`](dead-code-omissions.json)._`
        )
    }
    lines.push('')
    lines.push('## Summary')
    lines.push('')
    lines.push('| Category | Count |')
    lines.push('|---|---:|')
    lines.push(`| Unused files | ${agg.unusedFiles.length} |`)
    lines.push(`| Unused \`dependencies\` | ${depCount} |`)
    lines.push(`| Unused \`devDependencies\` | ${devDepCount} |`)
    lines.push(`| Unused \`optionalPeerDependencies\` | ${optPeerCount} |`)
    lines.push(`| Unlisted dependencies | ${unlistedCount} |`)
    lines.push(`| Unused exported values | ${exportCount} |`)
    lines.push(`| Unused exported types | ${typeCount} |`)
    lines.push(`| Unused enum members | ${enumCount} |`)
    lines.push(`| Duplicate exports | ${duplicateCount} |`)
    lines.push(`| Missing binaries | ${binariesCount} |`)
    lines.push(`| Unused locals / params / unreachable (tsc) | ${tscCount} |`)

    lines.push(section(`Section A \u2014 Unused files (${agg.unusedFiles.length})`))
    lines.push(
        'Files not reachable from any entry point (`src/electron/index.ts`, `src/preload/index.ts`, `src/ui/main.tsx`).'
    )
    lines.push('')
    lines.push(renderFilesSection(agg.unusedFiles))

    lines.push(section(`Section B \u2014 Unused exported values (${exportCount})`))
    lines.push('Declared exports that nothing else imports.')
    lines.push('')
    lines.push(renderExportsSection(agg.unusedExportsByFile, 'unused exported values'))

    lines.push(section(`Section C \u2014 Unused exported types (${typeCount})`))
    lines.push(renderTypesSection(agg.unusedTypesByFile))

    if (enumCount > 0) {
        lines.push(section(`Section C2 \u2014 Unused enum members (${enumCount})`))
        lines.push(renderEnumMembersSection(agg.unusedEnumMembersByFile))
    }

    lines.push(section(`Section D \u2014 In-file dead code (${tscCount})`))
    lines.push(renderTscSection(tscNode, tscWeb))

    lines.push(section(`Section E \u2014 Duplicate exports (${duplicateCount})`))
    lines.push(renderDuplicatesSection(agg.duplicatesByFile))

    lines.push(section(`Section F \u2014 Unused \`dependencies\` (${depCount})`))
    lines.push(
        'Packages listed in `dependencies` in [`package.json`](package.json) but not imported anywhere in the codebase. Safe to remove unless they are implicitly required at runtime (e.g. by a script or side-effect import in a JS config).'
    )
    lines.push('')
    lines.push(
        renderPackageListSection(
            agg.unusedDependencies,
            agg.unusedDependencies.length > 0
                ? '_Uninstall with:_ `npm uninstall ' +
                      agg.unusedDependencies.map(d => d.name).join(' ') +
                      '`'
                : null
        )
    )

    lines.push(section(`Section G \u2014 Unused \`devDependencies\` (${devDepCount})`))
    lines.push(
        'Packages listed in `devDependencies` but not imported from source. Still verify each one is not invoked by an npm script, build tool, editor plugin, or CI step before removing.'
    )
    lines.push('')
    lines.push(
        renderPackageListSection(
            agg.unusedDevDependencies,
            agg.unusedDevDependencies.length > 0
                ? '_Uninstall with:_ `npm uninstall ' +
                      agg.unusedDevDependencies.map(d => d.name).join(' ') +
                      '`'
                : null
        )
    )

    if (optPeerCount > 0) {
        lines.push(
            section(`Section G2 \u2014 Unused \`optionalPeerDependencies\` (${optPeerCount})`)
        )
        lines.push(renderPackageListSection(agg.unusedOptionalPeers, null))
    }

    lines.push(section(`Section H \u2014 Unlisted dependencies (${unlistedCount})`))
    lines.push(
        'Packages imported from source but not declared as direct dependencies in [`package.json`](package.json). They currently resolve via transitive deps \u2014 add them explicitly or the import will break if the transitive chain changes.'
    )
    lines.push('')
    lines.push(renderUnlistedSection(agg.unlistedByFile))

    if (binariesCount > 0) {
        lines.push(section(`Section I \u2014 Missing binaries (${binariesCount})`))
        lines.push(renderMissingBinariesSection(agg.missingBinariesByFile))
    }

    lines.push(section('Omissions'))
    lines.push(
        'Known false positives are listed in [`dead-code-omissions.json`](dead-code-omissions.json). ' +
            'After reviewing the report, add entries there to suppress items that are intentionally unused ' +
            '(e.g. exports consumed by external tools, runtime-only imports, plugin hooks).'
    )
    lines.push('')
    lines.push('Supported omission keys:')
    lines.push('')
    lines.push('| Key | Format | Example |')
    lines.push('|---|---|---|')
    lines.push('| `files` | Relative path or prefix | `"src/shared/types/legacy.ts"` |')
    lines.push(
        '| `exports` | `"symbol"` or `"file:symbol"` | `"myHelper"`, `"src/utils/foo.ts:bar"` |'
    )
    lines.push('| `types` | Type name | `"MyInternalType"` |')
    lines.push('| `dependencies` | Package name | `"some-runtime-dep"` |')

    lines.push(section('Notes'))
    lines.push(
        [
            '- Ambient `.d.ts` files and generated code (`src/**/generated/**`) are excluded from analysis via `knip.json`.',
            '- `?raw` imports and the `@/ -> src/` Vite alias are covered by `knip.json` paths config.',
            '- Before deleting, grep the repo for any dynamic string reference. The rules forbid dynamic `import()`, so unused-file findings are usually safe to remove.',
            '- Strict-tsc findings in Section D point at a specific line; fix the diagnostic or scope the noise-inducing symbol.'
        ].join('\n')
    )
    lines.push('')
    return lines.join('\n')
}

// ── Main ─────────────────────────────────────────────────────────────────────

function main(): void {
    const omissions = loadOmissions()
    if (
        omissions.files.length +
            omissions.exports.length +
            omissions.types.length +
            omissions.dependencies.length >
        0
    ) {
        const total =
            omissions.files.length +
            omissions.exports.length +
            omissions.types.length +
            omissions.dependencies.length
        console.log(`Loaded ${total} omission rule(s) from dead-code-omissions.json`)
    }

    let knipJson: KnipOutput
    let tscNode: string[] = []
    let tscWeb: string[] = []
    try {
        knipJson = runKnip()
        writeStrictTsconfigs()
        tscNode = runTscStrict(TSCONFIG_NODE, 'node')
        tscWeb = runTscStrict(TSCONFIG_WEB, 'web')
    } finally {
        cleanupStrictTsconfigs()
    }

    const report = renderReport({ knip: knipJson, tscNode, tscWeb, omissions })
    writeFileSync(REPORT_PATH, report)

    const agg = aggregate(knipJson.issues ?? [], omissions)
    const findings =
        agg.unusedFiles.length +
        countExports(agg.unusedExportsByFile) +
        countExports(agg.unusedTypesByFile) +
        agg.unusedDependencies.length +
        agg.unusedDevDependencies.length +
        tscNode.length +
        tscWeb.length
    console.log('\nDead-code report written to dead-code-report.md')
    console.log(
        `  ${agg.unusedFiles.length} unused files | ${countExports(agg.unusedExportsByFile)} unused exports | ${countExports(agg.unusedTypesByFile)} unused types | ${agg.unusedDependencies.length} unused deps | ${agg.unusedDevDependencies.length} unused devDeps | ${tscNode.length + tscWeb.length} strict-tsc findings`
    )
    if (agg.omittedCount > 0) {
        console.log(`  ${agg.omittedCount} item(s) filtered by omissions`)
    }

    if (!process.argv.includes('--check')) return
    if (findings > 0) {
        console.error(
            `\n❌ ${findings} dead-code finding(s) — delete the dead code, or add a justified entry to dead-code-omissions.json. See dead-code-report.md.`
        )
        process.exit(1)
    }
    console.log('\n✅ check passed: no dead code.')
}

main()
