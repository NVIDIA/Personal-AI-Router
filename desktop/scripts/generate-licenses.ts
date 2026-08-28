// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Generates the repository's single third-party notice, `THIRD_PARTY_NOTICES.md`
 * at the repository root, covering:
 *   - every runtime (production) npm dependency + all their transitives
 *   - the Electron runtime itself (declared as a devDependency, but shipped
 *     with the packaged app, so legally it has to appear in the report)
 *   - the Go modules linked into the thirteen service binaries, which the
 *     installer ships from `../services` as extraResources
 *
 * Dev-only tooling (vite, tsx, typescript, prettier, knip, etc.) is deliberately
 * excluded — it is not redistributed and does not need attribution.
 *
 * There is deliberately one notice file. The packaged app ships this same file,
 * so a reader cannot end up with a copy that disagrees with the repository.
 *
 * Run:
 *   npm run licenses            writes the notice (invoked on every build)
 */
import { init, type InitOpts, type ModuleInfo, type ModuleInfos } from 'license-checker-rseidelsohn'
import { readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import pkg from '../package.json' with { type: 'json' }
import { collectGoModuleEntries, type LicenseEntry } from './collect-go-licenses'

const __dirname = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(__dirname, '..')
const servicesDir = resolve(repoRoot, '..', 'services')

const NOTICE_PATH = resolve(repoRoot, '..', 'THIRD_PARTY_NOTICES.md')
const ELECTRON_DIR = join(repoRoot, 'node_modules/electron')
const SERVICE_VERSIONS_PATH = join(servicesDir, 'versions.json')

type Entry = LicenseEntry

function runChecker(opts: InitOpts): Promise<ModuleInfos> {
    return new Promise((resolvePromise, rejectPromise) => {
        init(opts, (err, modules) => {
            if (err) {
                rejectPromise(err)
                return
            }
            resolvePromise(modules)
        })
    })
}

function parseKey(key: string): { name: string; version: string } {
    const at = key.lastIndexOf('@')
    if (at <= 0) return { name: key, version: '' }
    return { name: key.slice(0, at), version: key.slice(at + 1) }
}

function normalizeLicense(licenses: string | string[] | undefined): string {
    if (licenses == null) return 'UNKNOWN'
    if (Array.isArray(licenses)) return licenses.join(' OR ')
    return licenses
}

function toEntry(key: string, info: ModuleInfo): Entry {
    const { name, version } = parseKey(key)
    const licenseText = (info.licenseText ?? '').trim()
    return {
        name,
        version,
        license: normalizeLicense(info.licenses),
        author: info.publisher ?? null,
        email: info.email ?? null,
        repository: info.repository ?? null,
        homepage: info.url ?? null,
        licenseText: licenseText.length > 0 ? licenseText : '(no license file found in package)'
    }
}

async function buildElectronEntry(): Promise<Entry> {
    const rawPkg = await readFile(join(ELECTRON_DIR, 'package.json'), 'utf8')
    const electronPkg: {
        name: string
        version: string
        license?: string
        author?: string | { name?: string; email?: string; url?: string }
        homepage?: string
        repository?: string | { url?: string }
    } = JSON.parse(rawPkg)

    const licenseText = await readFile(join(ELECTRON_DIR, 'LICENSE'), 'utf8')

    const author =
        typeof electronPkg.author === 'string'
            ? electronPkg.author
            : (electronPkg.author?.name ?? null)
    const email =
        typeof electronPkg.author === 'string' ? null : (electronPkg.author?.email ?? null)
    const repository =
        typeof electronPkg.repository === 'string'
            ? electronPkg.repository
            : (electronPkg.repository?.url ?? null)

    return {
        name: electronPkg.name,
        version: electronPkg.version,
        license: electronPkg.license ?? 'UNKNOWN',
        author,
        email,
        repository,
        homepage: electronPkg.homepage ?? null,
        licenseText: licenseText.trim()
    }
}

function mergeEntries(scan: ModuleInfos, extras: Entry[]): Entry[] {
    const byKey = new Map<string, Entry>()
    for (const [key, info] of Object.entries(scan)) {
        const { name } = parseKey(key)
        if (name === pkg.name) continue
        byKey.set(key, toEntry(key, info))
    }
    for (const entry of extras) {
        byKey.set(`${entry.name}@${entry.version}`, entry)
    }
    return [...byKey.values()].sort((a, b) =>
        a.name === b.name ? a.version.localeCompare(b.version) : a.name.localeCompare(b.name)
    )
}

function renderMarkdown(entries: Entry[]): string {
    const parts: string[] = [
        '<!--',
        'SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.',
        'SPDX-License-Identifier: Apache-2.0',
        '-->',
        '',
        '# Third-Party Software Notices',
        '',
        '> **Generated file.** Run `cd desktop && npm run licenses` after changing Go',
        '> modules, npm dependencies, Electron, or license-generation logic.',
        '',
        'NVIDIA Personal AI Router is distributed under the Apache License 2.0 (see',
        '`LICENSE`). It incorporates the third-party open-source software listed below.',
        'Each component is the property of its respective copyright holders and is',
        'distributed under its own license; the full license text for each component is',
        'reproduced in this file.',
        '',
        'Scope: npm dependencies and the Electron runtime shipped in the desktop',
        'application, plus the Go modules linked into the thirteen shipped service',
        'binaries across the Windows, Linux, and macOS targets. First-party modules',
        '(`nvpair-shared`, `eapnoob`) are excluded.',
        '',
        '## Components',
        '',
        '| Component | Version | License |',
        '| --- | --- | --- |'
    ]

    for (const e of entries) {
        parts.push(`| \`${e.name}\` | ${e.version} | ${e.license} |`)
    }

    parts.push('', '## License texts', '')

    for (const e of entries) {
        parts.push(`### ${e.name} ${e.version}`, '', `License: ${e.license}`, '')
        if (e.repository !== null) parts.push(`Repository: ${e.repository}`, '')
        parts.push('```text', e.licenseText, '```', '')
    }

    return `${parts.join('\n').trimEnd()}\n`
}

async function serviceComponentNames(): Promise<string[]> {
    const raw = await readFile(SERVICE_VERSIONS_PATH, 'utf8')
    const parsed: { components?: Record<string, string> } = JSON.parse(raw)
    const components = parsed.components
    if (components === undefined || Object.keys(components).length === 0) {
        throw new Error(`No components listed in ${SERVICE_VERSIONS_PATH}`)
    }
    return Object.keys(components)
}

async function main(): Promise<void> {
    const prod = await runChecker({
        start: repoRoot,
        production: true,
        excludePrivatePackages: true,
        customFormat: { licenseText: '' }
    })

    const electronEntry = await buildElectronEntry()
    const goEntries = await collectGoModuleEntries(servicesDir, await serviceComponentNames())
    const entries = mergeEntries(prod, [electronEntry, ...goEntries])

    await writeFile(NOTICE_PATH, renderMarkdown(entries))
    console.log(
        `Wrote ${entries.length} dependencies (${goEntries.length} Go modules) to ${NOTICE_PATH}`
    )
}

main().catch((err: unknown) => {
    console.error(err)
    process.exit(1)
})
