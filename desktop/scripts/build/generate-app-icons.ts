// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Generates every shipped app-icon derivative from the source master set in
 * `resources/app-icon/` (`pair-<size>.png` + `pair.ico`). Re-run after dropping
 * a new icon set into `resources/app-icon/`:
 *
 *     npm run generate:icons
 *
 * Outputs (filenames kept stable so the electron-builder configs, tray.ts, and
 * window.ts need no changes):
 *   resources/icons/logo.png        <- pair-1024.png (tray/window downscale)
 *   resources/icons/logo.ico        <- pair.ico (Windows builder)
 *   resources/icons/logo.icns       <- assembled from the sized PNGs (macOS builder)
 *   resources/icons/linux/<N>x<N>.png <- freedesktop hicolor set (Linux builder)
 *   src/ui/public/favicon.png       <- pair-256.png (renderer favicon)
 *
 * The Linux builder needs a *directory* of size-named PNGs, not a single file:
 * electron-builder 26 no longer synthesizes a multi-size set from one PNG, so a
 * lone logo.png installs only hicolor/1024x1024 (which no desktop environment
 * indexes) and the launcher falls back to a generic icon.
 *
 * The `.icns` is the only non-trivial artifact: there is no cross-platform
 * `iconutil` on Windows/Linux, so the container is assembled directly from the
 * pre-sized PNGs with @fiahfy/icns (every OSType maps to a size we already have,
 * so no resizing is needed).
 */
import { Icns, IcnsImage } from '@fiahfy/icns'
import { copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'fs'
import { resolve } from 'path'

const repoRoot = resolve(__dirname, '..', '..')
const srcDir = resolve(repoRoot, 'resources', 'app-icon')
const iconsDir = resolve(repoRoot, 'resources', 'icons')
const linuxIconsDir = resolve(iconsDir, 'linux')
const faviconDir = resolve(repoRoot, 'src', 'ui', 'public')

/** Source PNG sizes required to assemble the full icns + all copies. */
const requiredSizes = [16, 32, 64, 128, 256, 512, 1024] as const

/**
 * Standard freedesktop hicolor apps sizes emitted as `resources/icons/linux/
 * <N>x<N>.png`. electron-builder installs each one into
 * `/usr/share/icons/hicolor/<N>x<N>/apps/<executableName>.png`, so every entry
 * here must map to a directory the hicolor index.theme actually declares
 * (1024 is intentionally omitted — it is not an indexed hicolor size).
 */
const linuxIconSizes = [16, 24, 32, 48, 64, 128, 256, 512] as const

function srcPngPath(size: number): string {
    return resolve(srcDir, `pair-${size}.png`)
}

const PNG_SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])

/** Pixel dimensions from a PNG's IHDR, or null when the buffer is not a PNG. */
function readPngSize(buffer: Buffer): { width: number; height: number } | null {
    if (buffer.length < 24 || !buffer.subarray(0, 8).equals(PNG_SIGNATURE)) return null
    return { width: buffer.readUInt32BE(16), height: buffer.readUInt32BE(20) }
}

/**
 * Validate the source set before assembly so a bad drop-in fails with an exact
 * reason (the icns library otherwise throws a generic "Image must be PNG
 * format"). Catches the common "JPEG renamed to .png" and wrong-size mistakes.
 */
function assertSources(): void {
    const problems: string[] = []
    const sizesToCheck = [...new Set([...requiredSizes, ...linuxIconSizes])].sort((a, b) => a - b)
    for (const size of sizesToCheck) {
        const path = srcPngPath(size)
        if (!existsSync(path)) {
            problems.push(`pair-${size}.png is missing`)
            continue
        }
        const dims = readPngSize(readFileSync(path))
        if (!dims) {
            problems.push(
                `pair-${size}.png is not a real PNG (wrong signature -- likely a renamed JPEG)`
            )
        } else if (dims.width !== size || dims.height !== size) {
            problems.push(
                `pair-${size}.png must be ${size}x${size}, got ${dims.width}x${dims.height}`
            )
        }
    }
    if (!existsSync(resolve(srcDir, 'pair.ico'))) problems.push('pair.ico is missing')
    if (problems.length > 0) {
        throw new Error(
            'Invalid source icons in resources/app-icon/:\n  - ' +
                problems.join('\n  - ') +
                '\nProvide the full transparent pair-<size>.png set ' +
                '(16/24/32/48/64/128/256/512/1024) plus pair.ico.'
        )
    }
}

/** Assemble a macOS .icns from the pre-sized PNGs. */
function buildIcns(): void {
    const icns = new Icns()
    // Append order follows fiahfy/icns#6: emit the retina PNG types before the
    // small ic04/ic05 ARGB types so the 16px slot renders correctly.
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(1024)), 'ic10'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(512)), 'ic09'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(512)), 'ic14'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(256)), 'ic08'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(256)), 'ic13'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(128)), 'ic07'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(64)), 'ic12'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(32)), 'ic05'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(16)), 'ic04'))
    icns.append(IcnsImage.fromPNG(readFileSync(srcPngPath(32)), 'ic11'))

    writeFileSync(resolve(iconsDir, 'logo.icns'), icns.data)
}

function copyStaticIcons(): void {
    const master = srcPngPath(1024)
    const ico = resolve(srcDir, 'pair.ico')

    copyFileSync(master, resolve(iconsDir, 'logo.png'))
    copyFileSync(ico, resolve(iconsDir, 'logo.ico'))

    copyFileSync(srcPngPath(256), resolve(faviconDir, 'favicon.png'))
}

/**
 * Emit the freedesktop hicolor set electron-builder installs for the Linux deb.
 * Named `<N>x<N>.png` so electron-builder's directory collector picks up every
 * size and maps each to `/usr/share/icons/hicolor/<N>x<N>/apps/`.
 */
function buildLinuxIconSet(): void {
    mkdirSync(linuxIconsDir, { recursive: true })
    for (const size of linuxIconSizes) {
        copyFileSync(srcPngPath(size), resolve(linuxIconsDir, `${size}x${size}.png`))
    }
}

function main(): void {
    assertSources()
    // resources/icons/ is generated output (gitignored), so it may not exist on a
    // clean checkout — create it (and the linux subdir) before anything writes.
    mkdirSync(linuxIconsDir, { recursive: true })
    buildIcns()
    buildLinuxIconSet()
    copyStaticIcons()
    console.log(
        'Generated app icons:\n' +
            '  resources/icons/logo.{png,ico,icns}\n' +
            `  resources/icons/linux/<N>x<N>.png (${linuxIconSizes.join(', ')})\n` +
            '  src/ui/public/favicon.png'
    )
}

main()
