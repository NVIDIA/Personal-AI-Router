#!/usr/bin/env node
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Download and verify the latest Microsoft Visual C++ v14 Redistributable used
 * by the Windows installers.
 *
 * The source URL intentionally follows Microsoft's serviced "latest" package.
 * Authenticode establishes the publisher and integrity, while the minimum
 * version rejects a validly signed rollback. The observed version and SHA-256
 * are recorded for release provenance rather than pinned as build inputs.
 *
 * On Windows the operating system reads the signature. Everywhere else it is
 * read out of the PE directly, because `build:electron:win:*` cross-builds the
 * Windows installers from Linux and macOS, where there is no PowerShell to ask.
 * Both paths report the same fields and meet the same policy below; see
 * scripts/windows-pe-signature.mjs for what the direct read does and does not
 * establish.
 */

import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { readPeSignatureMetadata } from './windows-pe-signature.mjs'

export const VC_REDIST_SOURCE_URL = 'https://aka.ms/vc14/vc_redist.x64.exe'
export const VC_REDIST_MINIMUM_VERSION = '14.51.36247.0'

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
export const VC_REDIST_OUTPUT_DIR = join(REPO_ROOT, '.build', 'vc-redist')
export const VC_REDIST_OUTPUT_PATH = join(VC_REDIST_OUTPUT_DIR, 'VC_redist.x64.exe')
export const VC_REDIST_PROVENANCE_PATH = join(VC_REDIST_OUTPUT_DIR, 'manifest.json')

const MAX_DOWNLOAD_BYTES = 100 * 1024 * 1024
const DOWNLOAD_TIMEOUT_MS = 10 * 60 * 1000

function versionParts(version) {
    if (typeof version !== 'string' || !/^\d+(?:\.\d+){2,3}$/.test(version)) {
        throw new Error(`Invalid Visual C++ Redistributable version "${String(version)}".`)
    }
    const parts = version.split('.').map(part => Number.parseInt(part, 10))
    while (parts.length < 4) parts.push(0)
    return parts
}

export function compareVersions(left, right) {
    const leftParts = versionParts(left)
    const rightParts = versionParts(right)
    for (let index = 0; index < leftParts.length; index += 1) {
        if (leftParts[index] < rightParts[index]) return -1
        if (leftParts[index] > rightParts[index]) return 1
    }
    return 0
}

function normalizedVersion(metadata) {
    const candidates = [metadata.productVersion, metadata.fileVersion]
    for (const candidate of candidates) {
        if (typeof candidate !== 'string') continue
        const match = candidate.match(/\d+(?:\.\d+){2,3}/)
        if (match) return match[0]
    }
    throw new Error('The Microsoft-signed package did not report a numeric product version.')
}

function isMicrosoftSigner(subject) {
    if (typeof subject !== 'string') return false
    const distinguishedNames = subject.split(',').map(part => part.trim())
    return (
        distinguishedNames.includes('CN=Microsoft Corporation') &&
        distinguishedNames.includes('O=Microsoft Corporation')
    )
}

export function validateAuthenticodeMetadata(metadata) {
    if (typeof metadata !== 'object' || metadata === null || Array.isArray(metadata)) {
        throw new Error('The Authenticode probe returned invalid metadata.')
    }
    if (metadata.status !== 'Valid') {
        throw new Error(
            `Visual C++ Redistributable Authenticode status is "${String(metadata.status)}", not "Valid".`
        )
    }
    if (!isMicrosoftSigner(metadata.signerSubject)) {
        throw new Error(
            `Visual C++ Redistributable signer is not Microsoft Corporation: "${String(metadata.signerSubject)}".`
        )
    }
    if (
        typeof metadata.signerThumbprint !== 'string' ||
        !/^[0-9a-f]{40}$/i.test(metadata.signerThumbprint)
    ) {
        throw new Error('Visual C++ Redistributable signer thumbprint is missing or invalid.')
    }

    const version = normalizedVersion(metadata)
    if (compareVersions(version, VC_REDIST_MINIMUM_VERSION) < 0) {
        throw new Error(
            `Visual C++ Redistributable ${version} is older than the required ` +
                `${VC_REDIST_MINIMUM_VERSION}.`
        )
    }

    return {
        version,
        signerSubject: metadata.signerSubject,
        signerThumbprint: metadata.signerThumbprint.toUpperCase()
    }
}

/**
 * The environment for the Windows PowerShell child. A `PSModulePath` inherited
 * from PowerShell 7 points Windows PowerShell at 7's incompatible
 * Microsoft.PowerShell.Security, so `Get-AuthenticodeSignature` cannot load
 * (PowerShell/PowerShell#18530). With the variable unset, Windows PowerShell
 * builds its own default. Windows matches variable names case-insensitively.
 */
export function windowsPowerShellEnv(parentEnv, filePath) {
    const env = Object.fromEntries(
        Object.entries(parentEnv).filter(([name]) => name.toLowerCase() !== 'psmodulepath')
    )
    env.NVPAIR_VC_REDIST_PATH = filePath
    return env
}

function windowsAuthenticodeMetadata(filePath) {
    const command = [
        "$ErrorActionPreference = 'Stop'",
        '$signature = Get-AuthenticodeSignature -LiteralPath $env:NVPAIR_VC_REDIST_PATH',
        '$version = (Get-Item -LiteralPath $env:NVPAIR_VC_REDIST_PATH).VersionInfo',
        '[ordered]@{',
        'status = [string]$signature.Status',
        "signerSubject = $(if ($null -eq $signature.SignerCertificate) { '' } else { [string]$signature.SignerCertificate.Subject })",
        "signerThumbprint = $(if ($null -eq $signature.SignerCertificate) { '' } else { [string]$signature.SignerCertificate.Thumbprint })",
        'fileVersion = [string]$version.FileVersion',
        'productVersion = [string]$version.ProductVersion',
        '} | ConvertTo-Json -Compress'
    ].join('\n')

    const result = spawnSync(
        'powershell.exe',
        ['-NoProfile', '-NonInteractive', '-Command', command],
        {
            encoding: 'utf8',
            env: windowsPowerShellEnv(process.env, filePath),
            windowsHide: true
        }
    )
    if (result.error) {
        throw new Error(
            `Unable to run PowerShell Authenticode verification: ${result.error.message}`
        )
    }
    if (result.status !== 0) {
        throw new Error(
            `PowerShell Authenticode verification failed: ${result.stderr.trim() || `exit ${String(result.status)}`}`
        )
    }

    try {
        return JSON.parse(result.stdout.trim())
    } catch (error) {
        const message = error instanceof Error ? error.message : String(error)
        throw new Error(`Unable to parse Authenticode metadata: ${message}`)
    }
}

function inspectAuthenticode(filePath) {
    return validateAuthenticodeMetadata(
        process.platform === 'win32'
            ? windowsAuthenticodeMetadata(filePath)
            : readPeSignatureMetadata(filePath)
    )
}

function sha256(filePath) {
    return createHash('sha256').update(readFileSync(filePath)).digest('hex')
}

function validateResolvedUrl(rawUrl) {
    const url = new URL(rawUrl)
    if (url.protocol !== 'https:' || url.hostname !== 'download.visualstudio.microsoft.com') {
        throw new Error(`Microsoft Redistributable resolved to an unexpected URL: ${rawUrl}`)
    }
}

export function createProvenance(resolvedUrl, signature, digest) {
    validateResolvedUrl(resolvedUrl)
    if (typeof digest !== 'string' || !/^[0-9a-f]{64}$/.test(digest)) {
        throw new Error('Visual C++ Redistributable SHA-256 is missing or invalid.')
    }
    return {
        schemaVersion: 1,
        sourceUrl: VC_REDIST_SOURCE_URL,
        resolvedUrl,
        minimumVersion: VC_REDIST_MINIMUM_VERSION,
        version: signature.version,
        sha256: digest,
        signerSubject: signature.signerSubject,
        signerThumbprint: signature.signerThumbprint
    }
}

async function downloadToFile(destination) {
    const response = await fetch(VC_REDIST_SOURCE_URL, {
        redirect: 'follow',
        signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS)
    })
    if (!response.ok) {
        throw new Error(
            `Download ${VC_REDIST_SOURCE_URL} failed with HTTP ${String(response.status)}.`
        )
    }
    validateResolvedUrl(response.url)

    const declaredLength = Number.parseInt(response.headers.get('content-length') ?? '0', 10)
    if (declaredLength > MAX_DOWNLOAD_BYTES) {
        throw new Error(
            `Visual C++ Redistributable declares ${String(declaredLength)} bytes, exceeding the ` +
                `${String(MAX_DOWNLOAD_BYTES)}-byte limit.`
        )
    }

    const bytes = new Uint8Array(await response.arrayBuffer())
    if (bytes.byteLength === 0 || bytes.byteLength > MAX_DOWNLOAD_BYTES) {
        throw new Error(
            `Visual C++ Redistributable download size ${String(bytes.byteLength)} is invalid.`
        )
    }
    writeFileSync(destination, bytes, { flag: 'wx' })
    return response.url
}

function writeProvenance(provenance) {
    const temporaryPath = `${VC_REDIST_PROVENANCE_PATH}.${String(process.pid)}.tmp`
    try {
        writeFileSync(temporaryPath, `${JSON.stringify(provenance, null, 2)}\n`, {
            encoding: 'utf8',
            flag: 'wx'
        })
        rmSync(VC_REDIST_PROVENANCE_PATH, { force: true })
        renameSync(temporaryPath, VC_REDIST_PROVENANCE_PATH)
    } finally {
        rmSync(temporaryPath, { force: true })
    }
}

export async function stageVcRedist() {
    mkdirSync(VC_REDIST_OUTPUT_DIR, { recursive: true })
    const temporaryPath = join(
        VC_REDIST_OUTPUT_DIR,
        `VC_redist.x64.${String(process.pid)}.download`
    )
    rmSync(temporaryPath, { force: true })

    try {
        console.log(`[vc-redist] downloading ${VC_REDIST_SOURCE_URL}`)
        const resolvedUrl = await downloadToFile(temporaryPath)
        const signature = inspectAuthenticode(temporaryPath)
        const digest = sha256(temporaryPath)
        const provenance = createProvenance(resolvedUrl, signature, digest)

        rmSync(VC_REDIST_OUTPUT_PATH, { force: true })
        renameSync(temporaryPath, VC_REDIST_OUTPUT_PATH)
        writeProvenance(provenance)
        console.log(
            `[vc-redist] staged ${signature.version} (${digest}) at ${VC_REDIST_OUTPUT_PATH}`
        )
        return provenance
    } finally {
        rmSync(temporaryPath, { force: true })
    }
}

const invokedPath = process.argv[1]
if (invokedPath && import.meta.url === pathToFileURL(resolve(invokedPath)).href) {
    stageVcRedist().catch(error => {
        console.error(error instanceof Error ? error.message : String(error))
        process.exitCode = 1
    })
}
