#!/usr/bin/env node
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Point the docs pages' repository links at GitHub, for the published site only.
 *
 *   node scripts/rewrite-docs-links.mjs           report what would change
 *   node scripts/rewrite-docs-links.mjs --write   rewrite in place
 *
 * `docs/*.mdx` is read in two places. On GitHub it is browsed inside the
 * repository, where `../SECURITY.md` resolves from `docs/` up to the root and
 * lands on the file in whatever branch or fork the reader is looking at. On the
 * documentation site those files are not pages at all, and the relative path
 * climbs above the site root, so every one of them is a dead link.
 *
 * Keeping the source relative is what preserves the GitHub behavior, so the
 * publishing step runs this with `--write` against a throwaway checkout just
 * before it builds the site. Nothing is committed.
 *
 * Never run `--write` against a working tree you intend to commit.
 *
 * Report mode is the default and changes nothing, which makes it a check as much
 * as a preview: a link to a path this tree does not carry fails the run instead
 * of publishing a URL that resolves nowhere. Trees differ, so run it wherever
 * the pages are published from.
 *
 * Dependency-free and run directly by node, so a fresh clone can use it with no
 * install step.
 */

import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const DOCS_DIR = join(REPO_ROOT, 'docs')

// The published site is read by people outside NVIDIA, so links resolve against
// the public mirror's default branch rather than the internal branch being built.
const GITHUB_BLOB_BASE = 'https://github.com/NVIDIA/Personal-AI-Router/blob/main'

// Matches the inline markdown link and image target, which is the only link form
// these pages use.
const LINK_PATTERN = /(!?\[[^\]]*\]\()(\.\.\/[^)\s]+)(\))/g

function mdxFiles(dir) {
    return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
        const full = join(dir, entry.name)
        if (entry.isDirectory()) return mdxFiles(full)
        return entry.name.endsWith('.mdx') ? [full] : []
    })
}

// A fenced block can contain example markdown, which must survive untouched.
function fencedLineFlags(lines) {
    let fenced = false
    return lines.map((line) => {
        if (/^\s*(```|~~~)/.test(line)) {
            fenced = !fenced
            return true
        }
        return fenced
    })
}

function rewriteFile(file) {
    const original = readFileSync(file, 'utf8')
    const lines = original.split('\n')
    const fenced = fencedLineFlags(lines)
    const changes = []
    const missing = []

    const rewritten = lines.map((line, index) => {
        if (fenced[index]) return line
        return line.replace(LINK_PATTERN, (match, open, target, close) => {
            const [path, anchor = ''] = splitAnchor(target)
            const repoPath = relative(REPO_ROOT, resolve(DOCS_DIR, path))
            if (!exists(join(REPO_ROOT, repoPath))) {
                missing.push({ line: index + 1, target, repoPath })
                return match
            }
            const url = `${GITHUB_BLOB_BASE}/${repoPath.split('\\').join('/')}${anchor}`
            changes.push({ line: index + 1, target, url })
            return `${open}${url}${close}`
        })
    })

    return { original, rewritten: rewritten.join('\n'), changes, missing }
}

function splitAnchor(target) {
    const hash = target.indexOf('#')
    return hash === -1 ? [target, ''] : [target.slice(0, hash), target.slice(hash)]
}

function exists(path) {
    try {
        statSync(path)
        return true
    } catch {
        return false
    }
}

const write = process.argv.includes('--write')
let rewrittenCount = 0
let missingCount = 0

for (const file of mdxFiles(DOCS_DIR).sort()) {
    const { original, rewritten, changes, missing } = rewriteFile(file)
    const shown = relative(REPO_ROOT, file)

    for (const entry of missing) {
        console.error(`${shown}:${entry.line}: ${entry.target} -> ${entry.repoPath} does not exist in the repository`)
    }
    missingCount += missing.length

    if (changes.length === 0) continue
    rewrittenCount += changes.length
    console.log(`${shown} (${changes.length})`)
    for (const entry of changes) console.log(`    ${entry.line}: ${entry.target} -> ${entry.url}`)

    if (write && rewritten !== original) writeFileSync(file, rewritten)
}

if (missingCount > 0) {
    console.error(`\n${missingCount} link(s) point outside the repository. Fix them before publishing.`)
    process.exit(1)
}

console.log(`\n${write ? 'Rewrote' : 'Would rewrite'} ${rewrittenCount} repository link(s) across docs/.`)

// A link the pattern did not reach would publish dead, so make that loud rather
// than leaving it to a reader to discover.
if (write) {
    const leftover = mdxFiles(DOCS_DIR)
        .flatMap((file) => {
            const lines = readFileSync(file, 'utf8').split('\n')
            const fenced = fencedLineFlags(lines)
            return lines
                .map((text, index) => ({ file: relative(REPO_ROOT, file), line: index + 1, text }))
                .filter((entry) => !fenced[entry.line - 1] && /\]\(\.\.\//.test(entry.text))
        })
        .map((entry) => `${entry.file}:${entry.line}: ${entry.text.trim()}`)

    if (leftover.length > 0) {
        console.error(`\nRelative repository links survived the rewrite:\n${leftover.join('\n')}`)
        process.exit(1)
    }
}
