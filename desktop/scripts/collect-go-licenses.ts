// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Collects license information for the Go modules linked into the shipped
 * service binaries.
 *
 * The installer bundles all thirteen Go executables from `../services` as
 * `extraResources`, so their dependencies are redistributed and need attribution
 * in the notice file that ships inside the application.
 *
 * Module sets are build-tag dependent — `github.com/Microsoft/go-winio` only
 * appears for `GOOS=windows` — so every release target is enumerated and the
 * results are unioned. Each service is its own Go module, so two binaries can
 * legitimately link different versions of the same dependency; those are kept as
 * separate entries.
 *
 * License text is read from the module cache directory that `go list` reports,
 * so this needs the modules downloaded (any successful build satisfies that) but
 * makes no network requests of its own.
 */
import { execFile } from 'node:child_process'
import { readdir, readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { promisify } from 'node:util'
import { detectLicenseType } from '@/shared/utils/detect-license'

const execFileAsync = promisify(execFile)

export interface LicenseEntry {
    name: string
    version: string
    license: string
    author: string | null
    email: string | null
    repository: string | null
    homepage: string | null
    licenseText: string
}

/** Release targets the service build scripts support. */
const GO_TARGETS: ReadonlyArray<{ goos: string; goarch: string }> = [
    { goos: 'windows', goarch: 'amd64' },
    { goos: 'windows', goarch: 'arm64' },
    { goos: 'linux', goarch: 'amd64' },
    { goos: 'linux', goarch: 'arm64' },
    { goos: 'darwin', goarch: 'amd64' },
    { goos: 'darwin', goarch: 'arm64' }
]

/**
 * First-party modules in this repository. They are covered by the project's own
 * LICENSE and must not be listed as third-party components.
 */
const FIRST_PARTY_MODULES = new Set(['nvpair-shared', 'eapnoob'])

const LICENSE_FILE_PATTERN = /^(LICEN[SC]E|COPYING|NOTICE)([.-]\w+)?$/i

interface GoModule {
    path: string
    version: string
    dir: string
}

function isThirdParty(module: GoModule): boolean {
    if (FIRST_PARTY_MODULES.has(module.path)) return false
    // A main or replaced local module has no resolved upstream version.
    if (!module.version) return false
    if (module.version.startsWith('v0.0.0-00010101000000')) return false
    return module.dir.length > 0
}

async function listModulesForTarget(
    componentDir: string,
    goos: string,
    goarch: string
): Promise<GoModule[]> {
    const template = '{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}{{end}}'
    let stdout: string
    try {
        const result = await execFileAsync('go', ['list', '-deps', '-f', template, '.'], {
            cwd: componentDir,
            env: { ...process.env, GOOS: goos, GOARCH: goarch, CGO_ENABLED: '0' },
            maxBuffer: 32 * 1024 * 1024
        })
        stdout = result.stdout
    } catch (err) {
        // A component that cannot resolve for one target must not silently drop
        // its dependencies from the notice file.
        throw new Error(
            `go list failed for ${componentDir} (${goos}/${goarch}): ${String(err instanceof Error ? err.message : err)}`
        )
    }

    const modules: GoModule[] = []
    for (const line of stdout.split('\n')) {
        if (!line.trim()) continue
        const [path, version, dir] = line.split('\t')
        modules.push({ path, version: version ?? '', dir: dir ?? '' })
    }
    return modules
}

async function readLicenseText(moduleDir: string): Promise<string> {
    let names: string[]
    try {
        names = await readdir(moduleDir)
    } catch {
        return ''
    }

    // Prefer a plain LICENSE over NOTICE when a module ships both.
    const candidates = names
        .filter(name => LICENSE_FILE_PATTERN.test(name))
        .sort((a, b) => Number(/^notice/i.test(a)) - Number(/^notice/i.test(b)))

    for (const name of candidates) {
        try {
            const text = (await readFile(join(moduleDir, name), 'utf8')).trim()
            if (text) return text
        } catch {
            /* try the next candidate */
        }
    }
    return ''
}

function repositoryUrl(modulePath: string): string {
    return `https://${modulePath}`
}

export async function collectGoModuleEntries(
    servicesDir: string,
    componentNames: readonly string[]
): Promise<LicenseEntry[]> {
    const byKey = new Map<string, GoModule>()

    for (const component of componentNames) {
        const componentDir = join(servicesDir, component)
        for (const { goos, goarch } of GO_TARGETS) {
            for (const module of await listModulesForTarget(componentDir, goos, goarch)) {
                if (!isThirdParty(module)) continue
                byKey.set(`${module.path}@${module.version}`, module)
            }
        }
    }

    const entries: LicenseEntry[] = []
    for (const module of byKey.values()) {
        const licenseText = await readLicenseText(module.dir)
        entries.push({
            name: module.path,
            version: module.version,
            license: licenseText ? detectLicenseType(licenseText) : 'Unknown',
            author: null,
            email: null,
            repository: repositoryUrl(module.path),
            homepage: null,
            licenseText: licenseText || '(no license file found in module)'
        })
    }

    return entries.sort((a, b) =>
        a.name === b.name ? a.version.localeCompare(b.version) : a.name.localeCompare(b.name)
    )
}
