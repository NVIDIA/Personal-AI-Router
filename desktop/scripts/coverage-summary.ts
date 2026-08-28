// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Print per-file line/function/branch coverage from the most recent
 * `coverage/coverage-summary.json` run, sorted ascending by line %.
 *
 * Usage:
 *   tsx scripts/coverage-summary.ts                  # all files + true total
 *   tsx scripts/coverage-summary.ts <substring> ...  # filter by path substr
 *
 * Examples:
 *   tsx scripts/coverage-summary.ts inference/runner
 *   tsx scripts/coverage-summary.ts inference/engines/ollama inference/engines/lm-studio
 *
 * Why this exists:
 *   Vitest's V8 coverage provider emits the same source path twice in
 *   `coverage-summary.json` when a project uses multiple test
 *   environments (`unit` and `e2e` here) — once with real coverage and
 *   once with a 0% sibling. We dedupe on the normalized path and keep
 *   the higher line %, which matches the per-file numbers in the HTML
 *   report. The vitest "All files" row shown in the V8 reporter is
 *   computed *before* dedup and therefore lies; this script prints a
 *   real total at the bottom.
 *
 *   This script runs automatically at the end of `npm run test:unit`,
 *   so the test command stays a single invocation. It exits 0 even
 *   when no coverage file exists (e.g. test run was skipped) — that
 *   branch is informational, not a hard failure.
 */
import fs from 'fs'
import path from 'path'

interface FileCoverageBucket {
    pct: number
    covered: number
    total: number
}

interface FileCoverage {
    lines: FileCoverageBucket
    functions: FileCoverageBucket
    branches: FileCoverageBucket
    statements: FileCoverageBucket
}

const SUMMARY = path.resolve(__dirname, '..', 'coverage', 'coverage-summary.json')

function pct(covered: number, total: number): number {
    if (total === 0) return 100
    return (covered / total) * 100
}

function pad5(n: number): string {
    return n.toFixed(1).padStart(5)
}

function main(): void {
    const filters = process.argv.slice(2)

    if (!fs.existsSync(SUMMARY)) {
        console.log(
            `\n[coverage] No summary at ${SUMMARY}. Run tests with coverage to populate it.`
        )
        return
    }

    const data: Record<string, FileCoverage> = JSON.parse(fs.readFileSync(SUMMARY, 'utf8'))
    const dedup = new Map<string, FileCoverage>()

    for (const [key, value] of Object.entries(data)) {
        if (key === 'total') continue
        const norm = key.replace(/\\/g, '/')
        if (filters.length > 0 && !filters.some(f => norm.includes(f))) continue
        const prev = dedup.get(norm)
        if (!prev || value.lines.pct > prev.lines.pct) dedup.set(norm, value)
    }

    if (dedup.size === 0) {
        if (filters.length > 0) {
            console.error(`\n[coverage] No entries match: ${filters.join(', ')}`)
            process.exit(1)
        }
        console.log('\n[coverage] No entries to report.')
        return
    }

    const rows = [...dedup.entries()].sort((a, b) => a[1].lines.pct - b[1].lines.pct)

    const header = filters.length > 0 ? `coverage (${filters.join(', ')})` : 'coverage'
    console.log(`\n${header}`)
    console.log('-'.repeat(96))

    for (const [key, value] of rows) {
        const rel = key.replace(/.*src\//, 'src/')
        console.log(
            rel.padEnd(72),
            'L',
            pad5(value.lines.pct),
            '%  F',
            pad5(value.functions.pct),
            '%  B',
            pad5(value.branches.pct),
            '%'
        )
    }

    let lc = 0
    let lt = 0
    let fc = 0
    let ft = 0
    let bc = 0
    let bt = 0
    for (const v of dedup.values()) {
        lc += v.lines.covered
        lt += v.lines.total
        fc += v.functions.covered
        ft += v.functions.total
        bc += v.branches.covered
        bt += v.branches.total
    }

    console.log('-'.repeat(96))
    console.log(
        `total (${dedup.size} files)`.padEnd(72),
        'L',
        pad5(pct(lc, lt)),
        '%  F',
        pad5(pct(fc, ft)),
        '%  B',
        pad5(pct(bc, bt)),
        '%'
    )
    console.log()
}

main()
