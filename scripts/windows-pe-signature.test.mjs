// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict'
import test from 'node:test'

import {
    decodeObjectIdentifier,
    derChildren,
    distinguishedName,
    MICROSOFT_ISSUER_FINGERPRINTS,
    readTypeLengthValue,
    verifyChainToMicrosoftIssuer,
    versionFromParts
} from './windows-pe-signature.mjs'

const DER_SEQUENCE = 0x30
const DER_OBJECT_IDENTIFIER = 0x06

const objectIdentifier = bytes => Buffer.from([DER_OBJECT_IDENTIFIER, bytes.length, ...bytes])

/**
 * A certificate stood up from a name and the name of its issuer, exposing only
 * what the chain walk uses. `checkIssued(candidate)` answers whether this
 * certificate was issued by the candidate, the same direction Node's
 * X509Certificate uses.
 */
function certificate({ name, fingerprint256, issuedBy = null, signatureValid = true }) {
    return {
        subject: `CN=${name}`,
        fingerprint256,
        publicKey: { name },
        checkIssued: candidate => issuedBy !== null && candidate.publicKey.name === issuedBy,
        verify: publicKey => signatureValid && publicKey.name === issuedBy
    }
}

const PINNED_ISSUER_FINGERPRINT = [...MICROSOFT_ISSUER_FINGERPRINTS][0]

test('every pinned issuer is recorded as a SHA-256 fingerprint', () => {
    // The chain walk compares against `fingerprint256`, so a SHA-1 thumbprint
    // pasted in here would reject every file instead of failing visibly.
    assert.ok(MICROSOFT_ISSUER_FINGERPRINTS.size > 0)
    for (const fingerprint of MICROSOFT_ISSUER_FINGERPRINTS) {
        assert.match(fingerprint, /^([0-9A-F]{2}:){31}[0-9A-F]{2}$/)
    }
})

test('readTypeLengthValue reads a short-form element', () => {
    const buffer = Buffer.from([DER_SEQUENCE, 0x03, 0x01, 0x02, 0x03])
    assert.deepEqual(readTypeLengthValue(buffer, 0), {
        tag: DER_SEQUENCE,
        offset: 0,
        start: 2,
        end: 5
    })
})

test('readTypeLengthValue keeps the element offset apart from its content', () => {
    // A certificate has to be handed to X509Certificate with its own header, so
    // `offset` addressing the element and `start` addressing the content is the
    // distinction the signature reader depends on.
    const buffer = Buffer.concat([
        Buffer.from([DER_SEQUENCE, 0x81, 0xc8]),
        Buffer.alloc(0xc8, 0x41)
    ])
    const node = readTypeLengthValue(buffer, 0)
    assert.equal(node.offset, 0)
    assert.equal(node.start, 3)
    assert.equal(node.end, buffer.length)
})

test('readTypeLengthValue reads a two-byte length', () => {
    const buffer = Buffer.concat([
        Buffer.from([DER_SEQUENCE, 0x82, 0x01, 0x00]),
        Buffer.alloc(256, 0x41)
    ])
    assert.equal(readTypeLengthValue(buffer, 0).end, 260)
})

test('readTypeLengthValue rejects a value that runs past the end', () => {
    assert.throws(
        () => readTypeLengthValue(Buffer.from([DER_SEQUENCE, 0x10, 0x00]), 0),
        /truncated/
    )
})

test('readTypeLengthValue rejects an unsupported length width', () => {
    assert.throws(
        () => readTypeLengthValue(Buffer.from([DER_SEQUENCE, 0x85, 0, 0, 0, 0, 0]), 0),
        /unsupported DER length of 5 bytes/
    )
})

test('derChildren walks the elements of a sequence', () => {
    const buffer = Buffer.from([DER_SEQUENCE, 0x06, 0x02, 0x01, 0x07, 0x02, 0x02, 0x08, 0x09])
    const children = derChildren(buffer, readTypeLengthValue(buffer, 0))
    assert.deepEqual(
        children.map(child => [child.tag, child.end - child.start]),
        [
            [0x02, 1],
            [0x02, 2]
        ]
    )
})

test('decodeObjectIdentifier decodes the PKCS#7 and Authenticode identifiers', () => {
    const signedData = objectIdentifier([0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02])
    const spcIndirectData = objectIdentifier([
        0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x01, 0x04
    ])
    assert.equal(
        decodeObjectIdentifier(signedData, readTypeLengthValue(signedData, 0)),
        '1.2.840.113549.1.7.2'
    )
    // 311 needs two continuation bytes, so this covers the multi-byte arc.
    assert.equal(
        decodeObjectIdentifier(spcIndirectData, readTypeLengthValue(spcIndirectData, 0)),
        '1.3.6.1.4.1.311.2.1.4'
    )
})

test('versionFromParts unpacks a VS_FIXEDFILEINFO version', () => {
    // 14.51.36247.0, as the staged redistributable reports itself.
    assert.equal(versionFromParts(0x000e0033, 0x8d970000), '14.51.36247.0')
    assert.equal(versionFromParts(0, 0), '0.0.0.0')
    assert.equal(versionFromParts(0xffffffff, 0xffffffff), '65535.65535.65535.65535')
})

test('distinguishedName reorders a subject the way PowerShell prints it', () => {
    assert.equal(
        distinguishedName('C=US\nST=Washington\nL=Redmond\nO=Microsoft Corporation\n'),
        'O=Microsoft Corporation, L=Redmond, S=Washington, C=US'
    )
})

test('verifyChainToMicrosoftIssuer returns the signing certificate', () => {
    const issuer = certificate({
        name: 'Microsoft Code Signing PCA 2024',
        fingerprint256: PINNED_ISSUER_FINGERPRINT
    })
    const leaf = certificate({
        name: 'Microsoft Corporation',
        fingerprint256: 'AA:BB',
        issuedBy: 'Microsoft Code Signing PCA 2024'
    })
    // Certificate order in the signature is not specified, so the walk has to
    // find the signing certificate rather than take the first one.
    assert.equal(verifyChainToMicrosoftIssuer([issuer, leaf]), leaf)
})

test('verifyChainToMicrosoftIssuer rejects a chain to an unpinned authority', () => {
    const issuer = certificate({ name: 'Example Root CA', fingerprint256: 'CC:DD' })
    const leaf = certificate({
        name: 'Microsoft Corporation',
        fingerprint256: 'AA:BB',
        issuedBy: 'Example Root CA'
    })
    assert.throws(
        () => verifyChainToMicrosoftIssuer([leaf, issuer]),
        /ends at "CN=Example Root CA" \(CC:DD\), which is not a pinned Microsoft/
    )
})

test('verifyChainToMicrosoftIssuer rejects a certificate its issuer did not sign', () => {
    const issuer = certificate({
        name: 'Microsoft Code Signing PCA 2024',
        fingerprint256: PINNED_ISSUER_FINGERPRINT
    })
    const leaf = certificate({
        name: 'Microsoft Corporation',
        fingerprint256: 'AA:BB',
        issuedBy: 'Microsoft Code Signing PCA 2024',
        signatureValid: false
    })
    assert.throws(
        () => verifyChainToMicrosoftIssuer([leaf, issuer]),
        /chain is broken: "CN=Microsoft Corporation" is not signed by its issuer/
    )
})

test('verifyChainToMicrosoftIssuer rejects a chain that never terminates', () => {
    const first = certificate({ name: 'First CA', fingerprint256: 'CC:DD', issuedBy: 'Second CA' })
    const second = certificate({ name: 'Second CA', fingerprint256: 'EE:FF', issuedBy: 'First CA' })
    const leaf = certificate({
        name: 'Microsoft Corporation',
        fingerprint256: 'AA:BB',
        issuedBy: 'First CA'
    })
    assert.throws(() => verifyChainToMicrosoftIssuer([leaf, first, second]), /loops back on itself/)
})

test('verifyChainToMicrosoftIssuer rejects a signature with no signing certificate', () => {
    const first = certificate({ name: 'First CA', fingerprint256: 'CC:DD', issuedBy: 'Second CA' })
    const second = certificate({ name: 'Second CA', fingerprint256: 'EE:FF', issuedBy: 'First CA' })
    assert.throws(() => verifyChainToMicrosoftIssuer([first, second]), /no signing certificate/)
})
