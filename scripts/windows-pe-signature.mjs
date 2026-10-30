// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Read an Authenticode signature out of a Windows PE file on any platform.
 *
 * The Windows installers cross-build on Linux, where `Get-AuthenticodeSignature`
 * does not exist, so the staged Visual C++ Redistributable has to be inspected
 * without the operating system's help. This module reads the PE's certificate
 * table directly and reports the same fields the PowerShell probe does.
 *
 * What it establishes: the file carries a PKCS#7 signature; the digest that was
 * signed is the digest of these bytes; the signing certificate chains, by
 * issuance and by signature, to the Microsoft certificate authority pinned
 * below; and the version the PE reports about itself. What it does not
 * establish: certificate validity windows, revocation, or the RFC 3161
 * countersignature. A signing certificate that has expired since it signed is
 * normal and still accepted here, which is the main reason this is not a
 * general-purpose Authenticode verifier — on Windows the operating system
 * remains the one making that judgement.
 */

import { X509Certificate, createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'

// Authenticode stores a PKCS#7 SignedData whose encapsulated content is an
// SpcIndirectDataContent carrying the digest of the image.
const SIGNED_DATA_OID = '1.2.840.113549.1.7.2'
const SPC_INDIRECT_DATA_OID = '1.3.6.1.4.1.311.2.1.4'

const DIGEST_ALGORITHM_OIDS = new Map([
    ['1.3.14.3.2.26', 'sha1'],
    ['2.16.840.1.101.3.4.2.1', 'sha256'],
    ['2.16.840.1.101.3.4.2.2', 'sha384'],
    ['2.16.840.1.101.3.4.2.3', 'sha512']
])

// The certificate authority the embedded chain is required to terminate in.
//
// A PE embeds the signing certificate and its issuers but not the root, so this
// pins the issuing CA rather than a root. Pinning is what makes the signer
// subject meaningful: without an anchor, any self-signed certificate could name
// itself Microsoft Corporation. When Microsoft moves to a new CA the staging
// fails with the new fingerprint in the message, and that fingerprint is added
// here after being confirmed against a Microsoft-published certificate.
export const MICROSOFT_ISSUER_FINGERPRINTS = new Set([
    // CN=Microsoft Code Signing PCA 2024, O=Microsoft Corporation, C=US — the
    // last certificate in the chain embedded in VC_redist.x64.exe 14.51.36247.0.
    '3D:AD:FA:F8:12:DD:1B:BA:EF:45:83:4C:CB:D1:88:F3:CD:97:13:9E:2E:D1:AC:A6:9C:2D:D6:30:82:14:2F:8F'
])

const PE32_MAGIC = 0x10b
const PE32PLUS_MAGIC = 0x20b
// Offsets inside the optional header. The checksum sits at the same place in
// both variants; the data directory array does not.
const CHECKSUM_OFFSET_IN_OPTIONAL_HEADER = 64
const DATA_DIRECTORY_OFFSET_IN_OPTIONAL_HEADER = new Map([
    [PE32_MAGIC, 96],
    [PE32PLUS_MAGIC, 112]
])
const DATA_DIRECTORY_ENTRY_SIZE = 8
const RESOURCE_DIRECTORY_INDEX = 2
const CERTIFICATE_TABLE_INDEX = 4

const WIN_CERTIFICATE_HEADER_SIZE = 8
const WIN_CERTIFICATE_TYPE_PKCS_SIGNED_DATA = 0x0002

const RESOURCE_TYPE_VERSION = 16
const VS_FIXEDFILEINFO_SIGNATURE = 0xfeef04bd

export function readTypeLengthValue(buffer, offset) {
    if (offset + 2 > buffer.length) {
        throw new Error('The signature is truncated: a DER header runs past the end.')
    }
    const tag = buffer[offset]
    const firstLengthByte = buffer[offset + 1]
    let length = firstLengthByte
    let headerLength = 2

    if ((firstLengthByte & 0x80) !== 0) {
        const lengthByteCount = firstLengthByte & 0x7f
        // Indefinite length is not legal in DER, and four bytes already covers
        // any signature that fits the download size limit.
        if (lengthByteCount === 0 || lengthByteCount > 4) {
            throw new Error(
                `The signature uses an unsupported DER length of ${lengthByteCount} bytes.`
            )
        }
        length = 0
        for (let index = 0; index < lengthByteCount; index += 1) {
            length = length * 256 + buffer[offset + 2 + index]
        }
        headerLength = 2 + lengthByteCount
    }

    const start = offset + headerLength
    const end = start + length
    if (end > buffer.length) {
        throw new Error('The signature is truncated: a DER value runs past the end.')
    }
    // `offset` is the element itself, `start` only its content: a certificate
    // has to be handed on with its own header intact.
    return { tag, offset, start, end }
}

export function derChildren(buffer, node) {
    const nodes = []
    let offset = node.start
    while (offset < node.end) {
        const child = readTypeLengthValue(buffer, offset)
        nodes.push(child)
        offset = child.end
    }
    return nodes
}

function expectTag(node, tag, description) {
    if (node.tag !== tag) {
        throw new Error(
            `The signature is malformed: expected ${description} (tag 0x${tag.toString(16)}), ` +
                `found tag 0x${node.tag.toString(16)}.`
        )
    }
    return node
}

export function decodeObjectIdentifier(buffer, node) {
    const bytes = buffer.subarray(node.start, node.end)
    if (bytes.length === 0) {
        throw new Error('The signature is malformed: an empty object identifier.')
    }
    const parts = [Math.floor(bytes[0] / 40), bytes[0] % 40]
    let value = 0
    for (let index = 1; index < bytes.length; index += 1) {
        value = value * 128 + (bytes[index] & 0x7f)
        if ((bytes[index] & 0x80) === 0) {
            parts.push(value)
            value = 0
        }
    }
    return parts.join('.')
}

/**
 * The PE offsets this module needs: what to exclude from the image digest, and
 * where the certificate table and resources live.
 */
function readPortableExecutableLayout(buffer) {
    if (buffer.length < 64 || buffer[0] !== 0x4d || buffer[1] !== 0x5a) {
        throw new Error('The file is not a Windows executable (no MZ header).')
    }
    const peHeaderOffset = buffer.readUInt32LE(0x3c)
    if (peHeaderOffset + 24 > buffer.length || buffer.readUInt32LE(peHeaderOffset) !== 0x00004550) {
        throw new Error('The file is not a Windows executable (no PE signature).')
    }

    const sectionCount = buffer.readUInt16LE(peHeaderOffset + 6)
    const optionalHeaderSize = buffer.readUInt16LE(peHeaderOffset + 20)
    const optionalHeaderOffset = peHeaderOffset + 24
    const magic = buffer.readUInt16LE(optionalHeaderOffset)
    const dataDirectoryOffset = DATA_DIRECTORY_OFFSET_IN_OPTIONAL_HEADER.get(magic)
    if (dataDirectoryOffset === undefined) {
        throw new Error(
            `The executable has an unknown optional header magic 0x${magic.toString(16)}.`
        )
    }

    const directoryOffset = index =>
        optionalHeaderOffset + dataDirectoryOffset + index * DATA_DIRECTORY_ENTRY_SIZE
    const readDirectory = index => ({
        address: buffer.readUInt32LE(directoryOffset(index)),
        size: buffer.readUInt32LE(directoryOffset(index) + 4)
    })

    const sectionTableOffset = optionalHeaderOffset + optionalHeaderSize
    const sections = []
    for (let index = 0; index < sectionCount; index += 1) {
        const offset = sectionTableOffset + index * 40
        sections.push({
            virtualAddress: buffer.readUInt32LE(offset + 12),
            virtualSize: buffer.readUInt32LE(offset + 8),
            rawOffset: buffer.readUInt32LE(offset + 20),
            rawSize: buffer.readUInt32LE(offset + 16)
        })
    }

    return {
        checksumOffset: optionalHeaderOffset + CHECKSUM_OFFSET_IN_OPTIONAL_HEADER,
        certificateDirectoryOffset: directoryOffset(CERTIFICATE_TABLE_INDEX),
        certificateTable: readDirectory(CERTIFICATE_TABLE_INDEX),
        resourceDirectory: readDirectory(RESOURCE_DIRECTORY_INDEX),
        sections
    }
}

function fileOffsetForAddress(layout, address) {
    for (const section of layout.sections) {
        const end = section.virtualAddress + Math.max(section.virtualSize, section.rawSize)
        if (address >= section.virtualAddress && address < end) {
            return section.rawOffset + (address - section.virtualAddress)
        }
    }
    throw new Error(`The executable has no section containing address 0x${address.toString(16)}.`)
}

/**
 * The Authenticode image digest: the whole file except the three regions a
 * signature cannot cover — its own checksum, the certificate table's data
 * directory entry, and the certificate table itself.
 */
function imageDigest(buffer, layout, algorithm) {
    const certificateTableOffset = layout.certificateTable.address
    const hash = createHash(algorithm)
    hash.update(buffer.subarray(0, layout.checksumOffset))
    hash.update(buffer.subarray(layout.checksumOffset + 4, layout.certificateDirectoryOffset))
    hash.update(
        buffer.subarray(
            layout.certificateDirectoryOffset + DATA_DIRECTORY_ENTRY_SIZE,
            certificateTableOffset
        )
    )
    // Anything appended after the certificate table is covered, so a trailing
    // payload cannot be swapped out. For these packages there is none.
    hash.update(buffer.subarray(certificateTableOffset + layout.certificateTable.size))
    return hash.digest('hex')
}

function readCertificateTable(buffer, layout) {
    const { address, size } = layout.certificateTable
    if (address === 0 || size === 0) {
        throw new Error('The executable is not signed: it has no certificate table.')
    }
    if (address + size > buffer.length) {
        throw new Error('The executable declares a certificate table past the end of the file.')
    }
    const certificateType = buffer.readUInt16LE(address + 6)
    if (certificateType !== WIN_CERTIFICATE_TYPE_PKCS_SIGNED_DATA) {
        throw new Error(
            `The executable's certificate table holds type ${certificateType}, not PKCS#7 signed data.`
        )
    }
    const declaredLength = buffer.readUInt32LE(address)
    return buffer.subarray(address + WIN_CERTIFICATE_HEADER_SIZE, address + declaredLength)
}

/**
 * Pull the certificates and the signed image digest out of the PKCS#7 blob.
 */
function readSignedData(pkcs7) {
    const contentInfo = expectTag(readTypeLengthValue(pkcs7, 0), 0x30, 'a PKCS#7 ContentInfo')
    const [contentType, wrappedContent] = derChildren(pkcs7, contentInfo)
    if (
        decodeObjectIdentifier(pkcs7, expectTag(contentType, 0x06, 'a content type')) !==
        SIGNED_DATA_OID
    ) {
        throw new Error('The executable is not signed with PKCS#7 SignedData.')
    }

    const signedData = expectTag(
        derChildren(pkcs7, expectTag(wrappedContent, 0xa0, 'the SignedData wrapper'))[0],
        0x30,
        'a SignedData'
    )
    const members = derChildren(pkcs7, signedData)
    // version, digestAlgorithms, contentInfo, then the optional [0] certificates
    // and [1] CRLs, then signerInfos.
    const encapsulated = expectTag(members[2], 0x30, 'an encapsulated ContentInfo')
    const certificateSet = members.find(member => member.tag === 0xa0)
    if (certificateSet === undefined) {
        throw new Error('The signature carries no certificates.')
    }

    const [encapsulatedType, encapsulatedContent] = derChildren(pkcs7, encapsulated)
    if (
        decodeObjectIdentifier(pkcs7, expectTag(encapsulatedType, 0x06, 'a content type')) !==
        SPC_INDIRECT_DATA_OID
    ) {
        throw new Error('The signature does not carry an Authenticode image digest.')
    }

    const indirectData = expectTag(
        derChildren(pkcs7, expectTag(encapsulatedContent, 0xa0, 'the content wrapper'))[0],
        0x30,
        'an SpcIndirectDataContent'
    )
    const digestInfo = expectTag(derChildren(pkcs7, indirectData)[1], 0x30, 'the signed digest')
    const [algorithmIdentifier, digestValue] = derChildren(pkcs7, digestInfo)
    const algorithmOid = decodeObjectIdentifier(
        pkcs7,
        expectTag(
            derChildren(pkcs7, expectTag(algorithmIdentifier, 0x30, 'a digest algorithm'))[0],
            0x06,
            'a digest algorithm identifier'
        )
    )
    const algorithm = DIGEST_ALGORITHM_OIDS.get(algorithmOid)
    if (algorithm === undefined) {
        throw new Error(`The signature uses an unsupported digest algorithm ${algorithmOid}.`)
    }

    return {
        algorithm,
        digest: pkcs7
            .subarray(
                expectTag(digestValue, 0x04, 'the signed digest value').start,
                digestValue.end
            )
            .toString('hex'),
        certificates: derChildren(pkcs7, certificateSet).map(
            node => new X509Certificate(pkcs7.subarray(node.offset, node.end))
        )
    }
}

/**
 * Walk the embedded certificates from the signing certificate outwards, and
 * require the chain to terminate in a pinned Microsoft certificate authority.
 *
 * Only issuance and signature are checked, not the validity window: Microsoft's
 * signing certificates outlive their own signatures by design, and the
 * countersignature recording when the signing happened is not verified here.
 */
export function verifyChainToMicrosoftIssuer(certificates) {
    const issuesAnother = certificate =>
        certificates.some(other => other !== certificate && other.checkIssued(certificate))
    const leaf = certificates.find(certificate => !issuesAnother(certificate))
    if (leaf === undefined) {
        throw new Error(
            'The signature has no signing certificate: every certificate issues another.'
        )
    }

    let current = leaf
    const seen = new Set([current.fingerprint256])
    for (;;) {
        const issuer = certificates.find(
            candidate => candidate !== current && current.checkIssued(candidate)
        )
        if (issuer === undefined) break
        if (!current.verify(issuer.publicKey)) {
            throw new Error(
                `The certificate chain is broken: "${distinguishedName(current.subject)}" is not ` +
                    'signed by its issuer.'
            )
        }
        if (seen.has(issuer.fingerprint256)) {
            throw new Error('The certificate chain loops back on itself.')
        }
        seen.add(issuer.fingerprint256)
        current = issuer
    }

    if (!MICROSOFT_ISSUER_FINGERPRINTS.has(current.fingerprint256)) {
        throw new Error(
            `The certificate chain ends at "${distinguishedName(current.subject)}" ` +
                `(${current.fingerprint256}), which is not a pinned Microsoft ` +
                'certificate authority.'
        )
    }
    return leaf
}

/**
 * One level of the resource tree: named entries first, then the ones addressed
 * by numeric id. A directory entry's offset is relative to the tree's root.
 */
function resourceEntries(buffer, directoryOffset) {
    const namedCount = buffer.readUInt16LE(directoryOffset + 12)
    const idCount = buffer.readUInt16LE(directoryOffset + 14)
    const entries = []
    for (let index = 0; index < namedCount + idCount; index += 1) {
        const offset = directoryOffset + 16 + index * 8
        const name = buffer.readUInt32LE(offset)
        const data = buffer.readUInt32LE(offset + 4)
        entries.push({
            id: (name & 0x80000000) === 0 ? name : null,
            isDirectory: (data & 0x80000000) !== 0,
            offset: data & 0x7fffffff
        })
    }
    return entries
}

/**
 * VS_FIXEDFILEINFO sits behind a UTF-16 key and alignment padding, so it is
 * found by its signature rather than by counting bytes to it.
 */
function fixedFileInfoOffset(buffer, versionOffset) {
    for (let offset = versionOffset; offset < versionOffset + 64; offset += 4) {
        if (buffer.readUInt32LE(offset) === VS_FIXEDFILEINFO_SIGNATURE) return offset
    }
    throw new Error("The executable's version resource carries no VS_FIXEDFILEINFO.")
}

/**
 * The version the PE reports about itself, from its first RT_VERSION resource.
 */
function readVersionInfo(buffer, layout) {
    const resourceRoot = fileOffsetForAddress(layout, layout.resourceDirectory.address)
    const typeEntry = resourceEntries(buffer, resourceRoot).find(
        entry => entry.id === RESOURCE_TYPE_VERSION && entry.isDirectory
    )
    if (typeEntry === undefined) {
        throw new Error('The executable carries no version resource.')
    }
    const nameEntry = resourceEntries(buffer, resourceRoot + typeEntry.offset)[0]
    const languageEntry = resourceEntries(buffer, resourceRoot + nameEntry.offset)[0]
    // The leaf addresses an IMAGE_RESOURCE_DATA_ENTRY, and that entry's own
    // OffsetToData is an image address rather than a resource-relative one.
    const dataEntryOffset = resourceRoot + languageEntry.offset
    const versionOffset = fileOffsetForAddress(layout, buffer.readUInt32LE(dataEntryOffset))

    const fixedInfoOffset = fixedFileInfoOffset(buffer, versionOffset)
    const fileVersion = versionFromParts(
        buffer.readUInt32LE(fixedInfoOffset + 8),
        buffer.readUInt32LE(fixedInfoOffset + 12)
    )
    const productVersion = versionFromParts(
        buffer.readUInt32LE(fixedInfoOffset + 16),
        buffer.readUInt32LE(fixedInfoOffset + 20)
    )
    return { fileVersion, productVersion }
}

export function versionFromParts(most, least) {
    return [most >>> 16, most & 0xffff, least >>> 16, least & 0xffff].join('.')
}

/**
 * Node lists a subject one attribute per line, least significant first, and
 * spells stateOrProvinceName `ST`. PowerShell prints a single comma-separated
 * line, most significant first, and spells it `S`. Following PowerShell keeps
 * the recorded signer identical whichever platform staged the package.
 */
export function distinguishedName(subject) {
    return subject
        .split('\n')
        .filter(attribute => attribute.length > 0)
        .reverse()
        .map(attribute => attribute.replace(/^ST=/, 'S='))
        .join(', ')
}

export function readPeSignatureMetadata(filePath) {
    const buffer = readFileSync(filePath)
    const layout = readPortableExecutableLayout(buffer)
    const { algorithm, digest, certificates } = readSignedData(readCertificateTable(buffer, layout))

    const leaf = verifyChainToMicrosoftIssuer(certificates)
    const computed = imageDigest(buffer, layout, algorithm)
    const { fileVersion, productVersion } = readVersionInfo(buffer, layout)

    return {
        status: computed === digest ? 'Valid' : 'HashMismatch',
        signerSubject: distinguishedName(leaf.subject),
        signerThumbprint: leaf.fingerprint.replaceAll(':', ''),
        fileVersion,
        productVersion
    }
}
