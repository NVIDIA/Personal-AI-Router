// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Verify (or normalize) installer/packaging scripts under scripts/build/.
 *
 *   npm run verify:build-scripts   — fail on CRLF, missing shebang, or no final newline
 *   npm run fix:build-scripts      — rewrite to LF + trailing newline, then re-verify
 *
 * CI runs verify before platform builds. .gitattributes keeps *.sh / *.nsh at LF in Git.
 */

import { readdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'

const REPO_ROOT = path.resolve(__dirname, '..')
const BUILD_SCRIPTS_ROOT = path.join(REPO_ROOT, 'scripts', 'build')

interface ScriptIssue {
    file: string
    message: string
}

function relativePath(absolutePath: string): string {
    return path.relative(REPO_ROOT, absolutePath).split(path.sep).join('/')
}

async function collectBuildScripts(dir: string): Promise<string[]> {
    const entries = await readdir(dir, { withFileTypes: true })
    const files: string[] = []

    for (const entry of entries) {
        const fullPath = path.join(dir, entry.name)
        if (entry.isDirectory()) {
            files.push(...(await collectBuildScripts(fullPath)))
            continue
        }
        if (entry.isFile() && (entry.name.endsWith('.sh') || entry.name.endsWith('.nsh'))) {
            files.push(fullPath)
        }
    }

    return files.sort()
}

function normalizeScriptText(text: string): string {
    const lfOnly = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n')
    return lfOnly.endsWith('\n') ? lfOnly : `${lfOnly}\n`
}

function verifyScriptText(relativeFile: string, text: string): ScriptIssue[] {
    const issues: ScriptIssue[] = []

    if (text.includes('\r\n') || text.includes('\r')) {
        issues.push({ file: relativeFile, message: 'contains CR line endings (require LF)' })
    }

    if (!text.endsWith('\n')) {
        issues.push({ file: relativeFile, message: 'must end with a single newline' })
    }

    if (relativeFile.endsWith('.sh') && !text.startsWith('#!')) {
        issues.push({
            file: relativeFile,
            message: 'shell script must start with a shebang (#!...)'
        })
    }

    return issues
}

async function main(): Promise<void> {
    const fix = process.argv.includes('--fix')

    const files = await collectBuildScripts(BUILD_SCRIPTS_ROOT)
    if (files.length === 0) {
        console.error(`No .sh or .nsh files found under ${relativePath(BUILD_SCRIPTS_ROOT)}/`)
        process.exit(1)
    }

    const issues: ScriptIssue[] = []
    const fixed: string[] = []

    for (const filePath of files) {
        const relativeFile = relativePath(filePath)
        const original = await readFile(filePath, 'utf8')

        if (fix) {
            const normalized = normalizeScriptText(original)
            if (normalized !== original) {
                await writeFile(filePath, normalized, 'utf8')
                fixed.push(relativeFile)
            }
            issues.push(...verifyScriptText(relativeFile, normalized))
        } else {
            issues.push(...verifyScriptText(relativeFile, original))
        }
    }

    if (fix && fixed.length > 0) {
        console.log(`Normalized line endings for ${fixed.length} file(s):`)
        for (const file of fixed) {
            console.log(`  ${file}`)
        }
    }

    if (issues.length > 0) {
        console.error(`${issues.length} installer script issue(s):`)
        for (const issue of issues) {
            console.error(`  ${issue.file}: ${issue.message}`)
        }
        if (!fix) {
            console.error('Run `npm run fix:build-scripts` to normalize LF line endings.')
        }
        process.exit(1)
    }

    console.log(`OK — ${files.length} installer script(s) under scripts/build/`)
}

void main()
