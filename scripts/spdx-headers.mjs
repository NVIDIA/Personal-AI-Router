#!/usr/bin/env node
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Check, and optionally insert, the SPDX copyright and license header that every
 * source file in this monorepo carries.
 *
 *   node scripts/spdx-headers.mjs                  report files missing a header
 *   node scripts/spdx-headers.mjs --fix            insert the header where missing
 *   node scripts/spdx-headers.mjs --staged         restrict the run to staged files
 *   node scripts/spdx-headers.mjs --show-skipped   list what is not checked, and why
 *   node scripts/spdx-headers.mjs desktop/src      restrict the run to a path prefix
 *
 * Every candidate file is classified exactly once: it either has a registered
 * comment style and is checked, or it matches an explicit skip rule. A file that
 * matches neither is reported as unclassified and fails the run, so a new file
 * type cannot quietly slip past the gate.
 *
 * Dependency-free and run directly by node, so a fresh clone can use it before
 * `npm ci`, a Go-only contributor never installs the desktop toolchain, and the
 * public GitHub tree keeps a working gate after the internal `ci/` overlay is cut.
 */

import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')

const COPYRIGHT_HOLDER = 'NVIDIA CORPORATION & AFFILIATES. All rights reserved.'
const LICENSE_IDENTIFIER = 'Apache-2.0'

// A header may sit below a shebang, an XML prologue, or YAML frontmatter, so the
// tags are not always on the first line.
const HEADER_SCAN_LINES = 24

function headerLines(year) {
    return [
        `SPDX-FileCopyrightText: Copyright (c) ${year} ${COPYRIGHT_HOLDER}`,
        `SPDX-License-Identifier: ${LICENSE_IDENTIFIER}`,
    ]
}

function lineStyle(prefix) {
    return { render: lines => lines.map(line => `${prefix} ${line}`) }
}

function blockStyle(open, close, continuation, options = {}) {
    return {
        render: lines => [open, ...lines.map(line => `${continuation}${line}`), close],
        ...options,
    }
}

const SLASH = lineStyle('//')
const HASH = lineStyle('#')
// services/build.bat and services/installer_build.bat set the precedent: `@REM`
// rather than `REM`, so the header does not echo before `@echo off` runs.
const BATCH = lineStyle('@REM')
const MARKDOWN = blockStyle('<!--', '-->', '', { frontmatter: true })
// MDX parses tags as JSX and has no HTML comments, so `<!-- -->` reaches the
// rendered page as text. An expression comment compiles away.
const MDX = blockStyle('{/*', '*/}', '', { frontmatter: true })
const MARKUP = blockStyle('<!--', '-->', '', { xmlPrologue: true })
const CSS = blockStyle('/*', ' */', ' * ')

const STYLE_BY_EXTENSION = new Map([
    ['.go', SLASH],
    ['.ts', SLASH],
    ['.tsx', SLASH],
    ['.js', SLASH],
    ['.jsx', SLASH],
    ['.mjs', SLASH],
    ['.cjs', SLASH],
    ['.swift', SLASH],
    ['.css', CSS],
    ['.sh', HASH],
    ['.bash', HASH],
    ['.zsh', HASH],
    ['.ps1', HASH],
    ['.psm1', HASH],
    ['.py', HASH],
    ['.yml', HASH],
    ['.yaml', HASH],
    ['.toml', HASH],
    // Vault agent HCL configs and the consul-template files they render. The CI
    // env parser skips `#` lines, so a header in a .tmpl does not reach the
    // rendered secrets file as data.
    ['.config', HASH],
    ['.tmpl', HASH],
    // NSIS accepts `#` line comments in both installer scripts and includes.
    ['.nsi', HASH],
    ['.nsh', HASH],
    ['.bat', BATCH],
    ['.cmd', BATCH],
    ['.md', MARKDOWN],
    ['.mdx', MDX],
    ['.mdc', MARKDOWN],
    ['.html', MARKUP],
    ['.xml', MARKUP],
    ['.plist', MARKUP],
])

const STYLE_BY_FILENAME = new Map([
    ['Makefile', HASH],
    ['Dockerfile', HASH],
    ['.gitignore', HASH],
    ['.gitattributes', HASH],
    ['.dockerignore', HASH],
    ['.cursorignore', HASH],
    ['.prettierignore', HASH],
    ['.editorconfig', HASH],
    ['.npmrc', HASH],
])

const BINARY_EXTENSIONS = new Set([
    '.png',
    '.jpg',
    '.jpeg',
    '.gif',
    '.ico',
    '.icns',
    '.webp',
    '.svg',
    '.mp4',
    '.webm',
    '.woff',
    '.woff2',
    '.ttf',
    '.otf',
    '.zip',
    '.gz',
    '.pdf',
])

const SKIPPED_EXTENSIONS = new Map([
    ['.json', 'JSON has no comment syntax'],
    ['.webmanifest', 'JSON has no comment syntax'],
    ['.mod', 'maintained by the Go toolchain'],
    ['.sum', 'maintained by the Go toolchain'],
    ['.txt', 'license text, IETF RFCs, and byte-compared test fixtures'],
])

// Matched on the file name alone, in any directory.
const SKIPPED_FILENAMES = new Map([
    ['LICENSE', 'the license text itself'],
    ['THIRD_PARTY_NOTICES.md', 'reproduces third-party license texts, so it carries no NVIDIA header'],
])

// Matched as an exact path or a directory prefix, against the repo-relative path.
const SKIPPED_PATHS = [
    ['CHANGELOG.md', 'release notes, owned by the release process rather than edited by hand'],
    ['desktop/docs/services-api.md', 'generated by npm run service-contracts:write'],
    ['desktop/src/ui/lib/kaizen-ui-foundations/', 'vendored Kaizen UI Foundations CSS'],
    ['.github/PULL_REQUEST_TEMPLATE.md', 'copied verbatim into pull request descriptions'],
    ['.gitlab/merge_request_templates/', 'copied verbatim into merge request descriptions'],
    ['scripts/collectlogs/testdata/', 'log fixtures the sanitizer tests compare byte for byte'],
]

const LICENSE_TAG = /SPDX-License-Identifier:[ \t]*(\S+)/
const COPYRIGHT_TAG = /SPDX-FileCopyrightText:[ \t]*(.+)/
const COPYRIGHT_TEXT = /^Copyright \(c\) \d{4}(?:-\d{4})? (.+)$/
const XML_PROLOGUE = /^\s*<(\?xml|!doctype)/i

function classify(path) {
    for (const [prefix, reason] of SKIPPED_PATHS) {
        if (path === prefix || path.startsWith(prefix)) return { skip: reason }
    }

    const name = path.slice(path.lastIndexOf('/') + 1)
    const dot = name.lastIndexOf('.')
    const extension = dot > 0 ? name.slice(dot).toLowerCase() : ''

    const skippedName = SKIPPED_FILENAMES.get(name)
    if (skippedName !== undefined) return { skip: skippedName }

    if (BINARY_EXTENSIONS.has(extension)) return { skip: 'binary asset' }

    const skipped = SKIPPED_EXTENSIONS.get(extension)
    if (skipped !== undefined) return { skip: skipped }

    const style = STYLE_BY_FILENAME.get(name) ?? STYLE_BY_EXTENSION.get(extension)
    if (style === undefined) return { unclassified: extension === '' ? name : extension }
    return { style }
}

function inspect(text) {
    const head = text.split('\n').slice(0, HEADER_SCAN_LINES).join('\n')
    const license = head.match(LICENSE_TAG)
    const copyright = head.match(COPYRIGHT_TAG)

    if (license === null && copyright === null) return { state: 'missing' }
    if (license === null) return { state: 'review', detail: 'no SPDX-License-Identifier tag' }
    if (copyright === null) return { state: 'review', detail: 'no SPDX-FileCopyrightText tag' }
    if (license[1] !== LICENSE_IDENTIFIER) {
        return { state: 'review', detail: `declares SPDX-License-Identifier: ${license[1]}` }
    }

    // Strip a block-comment terminator the tag regex swept up on a one-line header.
    const notice = copyright[1].replace(/\s*(-->|\*\/)\s*$/, '').trim()
    const parsed = notice.match(COPYRIGHT_TEXT)
    if (parsed === null || parsed[1] !== COPYRIGHT_HOLDER) {
        return { state: 'review', detail: `nonstandard copyright line: ${notice}` }
    }
    return { state: 'ok' }
}

function prologueLength(lines, style) {
    let index = 0
    if (lines[0]?.startsWith('#!')) index += 1
    if (style.xmlPrologue) {
        while (XML_PROLOGUE.test(lines[index] ?? '')) index += 1
    }
    // Frontmatter is only frontmatter on the very first line.
    if (style.frontmatter && lines[0]?.trim() === '---') {
        const close = lines.findIndex((line, at) => at > 0 && line.trim() === '---')
        if (close > 0) index = close + 1
    }
    return index
}

function withHeader(text, style, year) {
    const newline = text.includes('\r\n') ? '\r\n' : '\n'
    const bom = text.startsWith('\uFEFF') ? '\uFEFF' : ''
    const lines = (bom === '' ? text : text.slice(1)).split(/\r?\n/)

    const prologue = prologueLength(lines, style)
    const body = lines.slice(prologue)
    while (body.length > 0 && body[0].trim() === '') body.shift()

    const out = [...lines.slice(0, prologue), ...style.render(headerLines(year))]
    if (body.length > 0) out.push('', ...body)

    const result = bom + out.join(newline)
    return result.endsWith(newline) ? result : result + newline
}

function gitPaths(args) {
    const stdout = execFileSync('git', args, {
        cwd: REPO_ROOT,
        encoding: 'utf8',
        maxBuffer: 64 * 1024 * 1024,
    })
    return stdout.split('\0').filter(entry => entry !== '')
}

function candidates(staged, prefixes) {
    // Tracked plus untracked-but-not-ignored, so a file created and not yet added
    // is checked before the dev commits it.
    const paths = staged
        ? gitPaths(['diff', '--cached', '--name-only', '--diff-filter=ACMR', '-z'])
        : gitPaths(['ls-files', '--cached', '--others', '--exclude-standard', '-z'])
    const unique = [...new Set(paths)].sort()
    if (prefixes.length === 0) return unique
    return unique.filter(path => prefixes.some(prefix => path === prefix || path.startsWith(prefix)))
}

const USAGE = `Usage: node scripts/spdx-headers.mjs [options] [path...]

Verify that every file carries the NVIDIA SPDX copyright and license header.

Options:
  --fix            Insert the header into files that have none.
  --staged         Only look at files staged for commit.
  --show-skipped   Also list the files no rule requires a header on.
  -h, --help       Show this message.

Exits 0 when every checked file has the canonical header.`

function parseArgs(argv) {
    const options = { fix: false, staged: false, showSkipped: false, prefixes: [] }
    for (const arg of argv) {
        if (arg === '--fix') options.fix = true
        else if (arg === '--staged') options.staged = true
        else if (arg === '--show-skipped') options.showSkipped = true
        else if (arg === '-h' || arg === '--help') return null
        else if (arg.startsWith('-')) throw new Error(`Unknown option: ${arg}`)
        else options.prefixes.push(arg.replace(/^\.\//, '').replace(/\/+$/, '/'))
    }
    return options
}

function report(label, entries) {
    if (entries.length === 0) return
    process.stdout.write(`\n${label} (${entries.length}):\n`)
    for (const entry of entries) process.stdout.write(`  ${entry}\n`)
}

function main() {
    const options = parseArgs(process.argv.slice(2))
    if (options === null) {
        process.stdout.write(`${USAGE}\n`)
        return 0
    }

    const year = String(new Date().getFullYear())
    const skipped = []
    const unclassified = []
    const missing = []
    const review = []
    let checked = 0

    for (const path of candidates(options.staged, options.prefixes)) {
        const verdict = classify(path)
        if (verdict.skip !== undefined) {
            skipped.push(`${path} — ${verdict.skip}`)
            continue
        }
        if (verdict.unclassified !== undefined) {
            unclassified.push(`${path} — no rule covers '${verdict.unclassified}'`)
            continue
        }

        let text
        try {
            text = readFileSync(resolve(REPO_ROOT, path), 'utf8')
        } catch {
            // Staged-but-deleted, or removed between listing and read.
            continue
        }
        if (text.includes('\0')) {
            skipped.push(`${path} — binary content`)
            continue
        }

        checked += 1
        const result = inspect(text)
        if (result.state === 'ok') continue
        if (result.state === 'missing') missing.push({ path, style: verdict.style })
        else review.push(`${path} — ${result.detail}`)
    }

    const fixed = []
    const unwritable = []
    if (options.fix) {
        // One unwritable file must not abandon the rest of the run, or a partial
        // pass leaves the tree in a state neither `--fix` nor the gate explains.
        for (const { path, style } of missing.splice(0)) {
            const absolute = resolve(REPO_ROOT, path)
            try {
                writeFileSync(absolute, withHeader(readFileSync(absolute, 'utf8'), style, year), 'utf8')
                fixed.push(path)
            } catch (error) {
                unwritable.push(`${path} — ${error instanceof Error ? error.message : String(error)}`)
            }
        }
    }

    process.stdout.write(
        `SPDX headers: ${checked} checked, ${missing.length} missing, ` +
            `${review.length} to review, ${unclassified.length} unclassified, ` +
            `${skipped.length} skipped\n`
    )

    report('Inserted', fixed)
    report('Missing the header', missing.map(entry => entry.path))
    report('Could not be written', unwritable)
    report('Needs a human — present but not the canonical NVIDIA Apache-2.0 header', review)
    report('Unclassified — add to STYLE_BY_EXTENSION or SKIPPED_EXTENSIONS', unclassified)
    if (options.showSkipped) report('Skipped', skipped)

    if (missing.length > 0) {
        process.stdout.write('\nRun `node scripts/spdx-headers.mjs --fix` to insert them.\n')
    }
    if (fixed.length > 0 && options.staged) {
        process.stdout.write('\nRe-stage the files above so the header is part of your commit.\n')
    }

    const failures = missing.length + unwritable.length + review.length + unclassified.length
    return failures === 0 ? 0 : 1
}

try {
    process.exitCode = main()
} catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 2
}
