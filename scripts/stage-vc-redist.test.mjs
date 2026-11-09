// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict'
import test from 'node:test'

import {
    compareVersions,
    createProvenance,
    validateAuthenticodeMetadata,
    VC_REDIST_MINIMUM_VERSION,
    windowsPowerShellEnv
} from './stage-vc-redist.mjs'

const MICROSOFT_SIGNATURE = {
    status: 'Valid',
    signerSubject:
        'CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US',
    signerThumbprint: '0123456789abcdef0123456789abcdef01234567',
    fileVersion: VC_REDIST_MINIMUM_VERSION,
    productVersion: VC_REDIST_MINIMUM_VERSION
}

test('compareVersions orders numeric runtime versions', () => {
    assert.equal(compareVersions('14.50.35719.0', '14.50.35719.0'), 0)
    assert.equal(compareVersions('14.51.1.0', '14.50.99999.0'), 1)
    assert.equal(compareVersions('14.9.99999.0', '14.50.1.0'), -1)
})

test('validateAuthenticodeMetadata accepts Microsoft at the version floor', () => {
    assert.deepEqual(validateAuthenticodeMetadata(MICROSOFT_SIGNATURE), {
        version: VC_REDIST_MINIMUM_VERSION,
        signerSubject: MICROSOFT_SIGNATURE.signerSubject,
        signerThumbprint: MICROSOFT_SIGNATURE.signerThumbprint.toUpperCase()
    })
})

test('validateAuthenticodeMetadata rejects an invalid signature', () => {
    assert.throws(
        () => validateAuthenticodeMetadata({ ...MICROSOFT_SIGNATURE, status: 'HashMismatch' }),
        /Authenticode status is "HashMismatch"/
    )
})

test('validateAuthenticodeMetadata rejects a non-Microsoft signer', () => {
    assert.throws(
        () =>
            validateAuthenticodeMetadata({
                ...MICROSOFT_SIGNATURE,
                signerSubject: 'CN=Example Corporation, O=Example Corporation, C=US'
            }),
        /signer is not Microsoft Corporation/
    )
})

test('validateAuthenticodeMetadata rejects a signed rollback', () => {
    assert.throws(
        () =>
            validateAuthenticodeMetadata({
                ...MICROSOFT_SIGNATURE,
                fileVersion: '14.49.99999.0',
                productVersion: '14.49.99999.0'
            }),
        /is older than the required/
    )
})

test('windowsPowerShellEnv drops a PSModulePath inherited from PowerShell 7', () => {
    const parentEnv = {
        Path: 'C:\\Windows\\system32',
        PSModulePath: 'C:\\Program Files\\PowerShell\\7\\Modules'
    }

    assert.deepEqual(windowsPowerShellEnv(parentEnv, 'C:\\stage\\VC_redist.x64.exe'), {
        Path: 'C:\\Windows\\system32',
        NVPAIR_VC_REDIST_PATH: 'C:\\stage\\VC_redist.x64.exe'
    })
    assert.equal(parentEnv.PSModulePath, 'C:\\Program Files\\PowerShell\\7\\Modules')
})

test('windowsPowerShellEnv drops PSModulePath whatever its case', () => {
    const parentEnv = { PSMODULEPATH: 'C:\\Program Files\\PowerShell\\7\\Modules' }

    assert.deepEqual(windowsPowerShellEnv(parentEnv, 'C:\\stage\\VC_redist.x64.exe'), {
        NVPAIR_VC_REDIST_PATH: 'C:\\stage\\VC_redist.x64.exe'
    })
})

test('createProvenance records the verified package identity', () => {
    const resolvedUrl =
        'https://download.visualstudio.microsoft.com/download/pr/package/VC_redist.x64.exe'
    const sha256 = 'a'.repeat(64)
    const signature = validateAuthenticodeMetadata(MICROSOFT_SIGNATURE)

    assert.deepEqual(createProvenance(resolvedUrl, signature, sha256), {
        schemaVersion: 1,
        sourceUrl: 'https://aka.ms/vc14/vc_redist.x64.exe',
        resolvedUrl,
        minimumVersion: VC_REDIST_MINIMUM_VERSION,
        version: VC_REDIST_MINIMUM_VERSION,
        sha256,
        signerSubject: MICROSOFT_SIGNATURE.signerSubject,
        signerThumbprint: MICROSOFT_SIGNATURE.signerThumbprint.toUpperCase()
    })
})
