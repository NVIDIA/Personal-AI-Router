// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// electron-builder configuration. `npm run build` does not use this — it only
// produces the bundles the application runs from. This config is what turns those
// bundles into an application directory and, on the `build:win*` / `build:linux*`
// scripts, an installer.
//
// Its installer targets are reference material rather than a distribution path:
// signing and notarization live outside this repository, so anything built here
// is unsigned. Released builds come from NVIDIA's own signed pipeline.
import { readFileSync, readdirSync } from 'node:fs'
import type { Configuration } from 'electron-builder'
import electronPkg from 'electron/package.json'
import pkg from './package.json'
// electron-builder loads this config through jiti, which does not resolve the
// `@/*` TypeScript path alias. Import shared modules by relative path here (only
// their type-only `@/` imports remain, and jiti erases those during the TS
// transform).
import {
    APP_DISPLAY_NAME,
    APP_EXECUTABLE_NAME,
    APP_EXIT_ARGUMENT,
    APP_ID
} from './src/shared/constants/app'
import {
    modularBinaryFileName,
    modularShippedBinaryBaseNames
} from './src/shared/constants/modular-binaries'
import {
    INFERENCE_DISPATCHER_RESOURCE_DIR,
    inferenceDispatcherFileName
} from './src/shared/constants/inference-dispatcher'
import type { JsonValue } from './src/shared/types/json'
import type { SupportedPlatform } from './src/shared/types/platform'
import { macAfterAllArtifactBuild, macAfterPack } from './scripts/build/macos/hooks'

// electron-builder's `directories` is a root-only field — there is no
// per-platform override. To keep OS-scoped release folders
// (release/<version>/windows, release/<version>/linux, ...) we look at the
// CLI flags electron-builder was invoked with and pick a subfolder.
//
// `PAIR_RELEASE_PLATFORM` is an explicit override for cases where argv does
// not contain the expected flag (Docker build, postinstall hooks, workers).
const argv = process.argv.join(' ')
const envSegment = process.env.PAIR_RELEASE_PLATFORM
const osSegment =
    envSegment === 'windows' || envSegment === 'linux' || envSegment === 'mac'
        ? envSegment
        : argv.includes('--win')
          ? 'windows'
          : argv.includes('--linux')
            ? 'linux'
            : argv.includes('--mac')
              ? 'mac'
              : null

const output = osSegment ? `release/${pkg.version}/${osSegment}` : `release/${pkg.version}`
// Only Windows uses the display name as the packaging product name (drives NSIS
// branding; the install dir and executable are pinned to APP_EXECUTABLE_NAME via
// win.executableName). macOS and Linux keep the technical name so the app bundle
// (`PAIR.app`) and Linux install dir (`/opt/PAIR`) stay stable across the rename
// — existing generated CLI launchers embed those absolute paths.
const packagingProductName = osSegment === 'windows' ? APP_DISPLAY_NAME : APP_EXECUTABLE_NAME

function packagingPlatform(): SupportedPlatform {
    const platform =
        osSegment === 'windows'
            ? 'win32'
            : osSegment === 'mac'
              ? 'darwin'
              : osSegment === 'linux'
                ? 'linux'
                : null
    if (!platform) {
        throw new Error(
            'Unable to determine the packaging platform. Pass --win, --mac, or --linux.'
        )
    }
    return platform
}

function assertCliBinPackagingInputs(): void {
    const platform = packagingPlatform()

    const expected = new Set([
        ...modularShippedBinaryBaseNames().map(baseName =>
            modularBinaryFileName(baseName, platform)
        ),
        'manifest.json'
    ])
    const entries = readdirSync('cli-bin', { withFileTypes: true })
    const unexpected = entries
        .filter(entry => !entry.isFile() || !expected.has(entry.name))
        .map(entry => entry.name)
        .sort()
    const missing = [...expected].filter(
        fileName => !entries.some(entry => entry.name === fileName)
    )

    if (unexpected.length > 0 || missing.length > 0) {
        throw new Error(
            [
                'Refusing to package an invalid cli-bin directory.',
                unexpected.length > 0 ? `Unexpected: ${unexpected.join(', ')}` : '',
                missing.length > 0 ? `Missing: ${missing.join(', ')}` : '',
                'Run npm run build:modular-binaries for the target platform.'
            ]
                .filter(Boolean)
                .join('\n')
        )
    }

    // Filenames alone cannot tell a linux build from a macOS one (same names,
    // no extension) or an x64 build from arm64. The manifest records the real
    // target, so reject a cli-bin that was built for the wrong platform or a
    // different architecture than the one being packaged.
    const manifest: JsonValue = JSON.parse(readFileSync('cli-bin/manifest.json', 'utf8'))
    if (typeof manifest !== 'object' || manifest === null || Array.isArray(manifest)) {
        throw new Error('cli-bin/manifest.json is not a JSON object.')
    }
    const manifestPlatform = manifest['platform']
    const manifestArch = manifest['arch']
    if (manifestPlatform !== platform) {
        throw new Error(
            `cli-bin was built for platform "${String(manifestPlatform)}" but packaging ` +
                `targets "${platform}". Run npm run build:modular-binaries for the target platform.`
        )
    }
    if (manifestArch !== 'x64' && manifestArch !== 'arm64') {
        throw new Error(`cli-bin/manifest.json has an invalid arch "${String(manifestArch)}".`)
    }
    // cli-bin holds exactly one architecture's binaries (built per-arch by
    // build:modular-binaries:*). Packaging must therefore target that single
    // arch — a flag-less invocation that selects both x64 and arm64 would emit
    // two installers from the same single-arch cli-bin, so reject anything but
    // an exact one-arch match.
    if (selectedArchs.length !== 1 || selectedArchs[0] !== manifestArch) {
        throw new Error(
            `cli-bin was built for arch "${manifestArch}" but packaging targets ` +
                `${selectedArchs.join(', ')}. Package exactly one architecture whose modular ` +
                `binaries are staged in cli-bin (pass --x64 or --arm64).`
        )
    }
}

/**
 * The same guarantee `assertCliBinPackagingInputs` gives cli-bin, for the
 * `inference-dispatcher` client in `tools/`. Its own manifest records the real
 * target, because the file name alone cannot distinguish a linux build from a
 * macOS one or x64 from arm64.
 */
function assertToolsPackagingInputs(): void {
    const platform = packagingPlatform()
    const expected = new Set([inferenceDispatcherFileName(platform), 'manifest.json'])

    const entries = readdirSync(INFERENCE_DISPATCHER_RESOURCE_DIR, { withFileTypes: true })
    const unexpected = entries
        .filter(entry => !entry.isFile() || !expected.has(entry.name))
        .map(entry => entry.name)
        .sort()
    const missing = [...expected].filter(
        fileName => !entries.some(entry => entry.name === fileName)
    )

    if (unexpected.length > 0 || missing.length > 0) {
        throw new Error(
            [
                `Refusing to package an invalid ${INFERENCE_DISPATCHER_RESOURCE_DIR} directory.`,
                unexpected.length > 0 ? `Unexpected: ${unexpected.join(', ')}` : '',
                missing.length > 0 ? `Missing: ${missing.join(', ')}` : '',
                'Run npm run build:tools for the target platform.'
            ]
                .filter(Boolean)
                .join('\n')
        )
    }

    const manifest: JsonValue = JSON.parse(
        readFileSync(`${INFERENCE_DISPATCHER_RESOURCE_DIR}/manifest.json`, 'utf8')
    )
    if (typeof manifest !== 'object' || manifest === null || Array.isArray(manifest)) {
        throw new Error(`${INFERENCE_DISPATCHER_RESOURCE_DIR}/manifest.json is not a JSON object.`)
    }
    const manifestPlatform = manifest['platform']
    const manifestArch = manifest['arch']
    if (manifestPlatform !== platform) {
        throw new Error(
            `${INFERENCE_DISPATCHER_RESOURCE_DIR} was built for platform ` +
                `"${String(manifestPlatform)}" but packaging targets "${platform}". ` +
                'Run npm run build:tools for the target platform.'
        )
    }
    if (selectedArchs.length !== 1 || selectedArchs[0] !== manifestArch) {
        throw new Error(
            `${INFERENCE_DISPATCHER_RESOURCE_DIR} was built for arch "${String(manifestArch)}" ` +
                `but packaging targets ${selectedArchs.join(', ')}. Package exactly one ` +
                'architecture (pass --x64 or --arm64).'
        )
    }
}

// Narrow the packaged architectures based on CLI flags / env. Without this,
// declaring `arch: ['x64', 'arm64']` on a target builds both installers even
// when the user passes only `--arm64`.
const argvHasX64 = argv.includes('--x64')
const argvHasArm64 = argv.includes('--arm64')
type PkgArch = 'x64' | 'arm64'
const selectedArchs: PkgArch[] =
    argvHasArm64 && !argvHasX64
        ? ['arm64']
        : argvHasX64 && !argvHasArm64
          ? ['x64']
          : ['x64', 'arm64']

assertCliBinPackagingInputs()
assertToolsPackagingInputs()

// Pin the NSIS payload's 7z branch filter to BCJ.
//
// electron-builder packs app-<arch>.7z with its bundled 7za 24.09, which picks a
// CPU-specific branch converter for executable content: BCJ2 for x64, and the
// ARM64 converter (new in 7-Zip 21.00) for arm64. The installer unpacks that
// archive with the Nsis7z plugin from nsis-resources-3.4.1, whose decoder is
// 7-Zip 19.00 and so predates the ARM64 converter. It writes the plain LZMA2
// blocks and silently skips the ones it cannot decode, and extractAppPackage.nsh
// never notices, because its `Pop $R0` retrieves the `Push $OUTDIR` from three
// lines earlier rather than an extraction result. An arm64 install therefore
// reports success with the whole app tree present except every .exe and .dll.
//
// BCJ is the best-ratio filter that decoder understands. It gains nothing on
// arm64 code, but it costs about a percent of archive size and keeps one filter
// across both architectures. It belongs here rather than in the build scripts
// because a forgotten environment variable ships a silently broken installer:
// internal-build/electron-builder.config.ts imports this module, so the signed
// pipeline and every local build inherit it.
if (osSegment === 'windows') {
    process.env.ELECTRON_BUILDER_7Z_FILTER = 'BCJ'
}

const config: Configuration = {
    electronVersion: electronPkg.version,
    appId: APP_ID,
    productName: packagingProductName,
    artifactName: 'NVPAIR-Setup-${version}-${arch}.${ext}',
    asar: true,
    directories: {
        output
    },
    // The public build never publishes. NVIDIA's update-feed publishing is
    // layered on by internal-build/electron-builder.config.ts.
    publish: null,
    /**
     * Positive whitelist. The main/preload bundles inline every npm dep they
     * touch (electron.vite.config.ts does not externalize anything), so the
     * packaged app needs **zero** entries from `node_modules/`. The modular Go
     * binaries are shipped via `extraResources`, not asar, so `cli-bin/**` is
     * not needed here. Everything else on disk (source, configs, caches, release
     * artefacts, node_modules, tsconfigs, build scripts, docs, docker) is
     * auto-excluded because it is not listed.
     *
     * Top-level `resources/icons/**` is included because `tray.ts` resolves
     * icons via `__dirname/../../resources/icons` (walks from out/main back
     * to the repo root inside the asar). `out/resources/**` is also shipped
     * for `window.ts`, which uses the sibling `out/resources` copy produced
     * by electron.vite.config.ts's copy-icons plugin.
     */
    files: [
        'out/main/**/*',
        'out/preload/**/*',
        'out/ui/**/*',
        'out/resources/**/*',
        'resources/icons/**/*',
        'package.json'
    ],
    /**
     * Ship the modular Go subprocesses outside the asar so the Electron main
     * process can spawn them from `process.resourcesPath/cli-bin`.
     */
    extraResources: [
        {
            from: 'cli-bin',
            to: 'cli-bin'
        },
        {
            // The `inference-dispatcher` HTTP client the Inference Demo spawns
            // (built by scripts/build-inference-dispatcher.ts). It ships beside
            // cli-bin rather than inside it because it is not a services binary:
            // no JSON-RPC, absent from services/versions.json, never supervised
            // by the broker.
            from: INFERENCE_DISPATCHER_RESOURCE_DIR,
            to: INFERENCE_DISPATCHER_RESOURCE_DIR
        },
        {
            // Repo-root wipe scripts (append-only inventory). Packaged builds call
            // these from Electron after shutdown — same entrypoints developers run
            // from the monorepo without Node.
            from: '../scripts/wipe-app-data.sh',
            to: 'scripts/wipe-app-data.sh'
        },
        {
            from: '../scripts/wipe-app-data.cmd',
            to: 'scripts/wipe-app-data.cmd'
        },
        {
            from: '../scripts/wipe-app-data.ps1',
            to: 'scripts/wipe-app-data.ps1'
        },
        {
            // Log sanitizer, so an installed user can produce a shareable bundle
            // without cloning the repository or installing Go. The wrappers use
            // the compiled collector when it sits beside them, which is why the
            // binary and the scripts land in the same directory.
            from: '../scripts/collect-logs.sh',
            to: 'scripts/collect-logs.sh'
        },
        {
            from: '../scripts/collect-logs.ps1',
            to: 'scripts/collect-logs.ps1'
        },
        {
            // Built by scripts/build-collect-logs.ts for this target.
            from: 'out/collect-logs',
            to: 'scripts'
        },
        {
            // The repository keeps one copy of each of these at its root; the
            // packaged app ships those same files rather than a desktop-local
            // duplicate that could disagree with them.
            from: '../LICENSE',
            to: 'LICENSE'
        },
        {
            from: '../THIRD_PARTY_NOTICES.md',
            to: 'THIRD_PARTY_NOTICES.md'
        }
    ],
    win: {
        executableName: APP_EXECUTABLE_NAME,
        // The public build is unsigned. NVIDIA Authenticode signing is layered
        // on by internal-build/electron-builder.config.ts.
        icon: './resources/icons/logo.ico',
        target: [
            {
                target: 'nsis',
                arch: selectedArchs
            }
        ]
    },
    nsis: {
        guid: 'eafd1530-9af3-5c5d-bb19-0b27768efdac',
        oneClick: true,
        perMachine: true,
        createDesktopShortcut: 'always',
        shortcutName: APP_DISPLAY_NAME,
        uninstallDisplayName: APP_DISPLAY_NAME,
        include: 'scripts/build/installer.nsh',
        installerHeaderIcon: './resources/icons/logo.ico',
        installerIcon: './resources/icons/logo.ico'
    },
    linux: {
        // Directory of freedesktop hicolor `<N>x<N>.png` files (generated by
        // scripts/build/generate-app-icons.ts). electron-builder 26 no longer
        // synthesizes a size set from a single PNG, so a lone logo.png would
        // install only hicolor/1024x1024 (unindexed) and the launcher would
        // fall back to a generic icon.
        icon: './resources/icons/linux',
        executableName: pkg.name,
        target: [
            {
                target: 'deb',
                arch: selectedArchs
            }
        ],
        category: 'Utility',
        desktop: {
            entry: {
                // The install dir is /opt/PAIR (technical productName), but the
                // visible launcher label is the display name.
                Name: APP_DISPLAY_NAME,
                Actions: 'Exit;'
            },
            desktopActions: {
                Exit: {
                    Name: `Exit ${APP_DISPLAY_NAME}`,
                    Exec: `"/opt/${APP_EXECUTABLE_NAME}/${pkg.name}" ${APP_EXIT_ARGUMENT}`
                }
            }
        }
    },
    deb: {
        afterInstall: 'scripts/build/linux/after-install.sh',
        afterRemove: 'scripts/build/linux/after-remove.sh'
    },
    mac: {
        executableName: APP_EXECUTABLE_NAME,
        extendInfo: {
            CFBundleDisplayName: APP_DISPLAY_NAME
        },
        icon: './resources/icons/logo.icns',
        // SMAppService (the privileged firewall helper) requires macOS 13+.
        minimumSystemVersion: '13.0',
        target: [
            {
                target: 'dmg',
                arch: selectedArchs
            },
            {
                target: 'zip',
                arch: selectedArchs
            }
        ],
        // The public build is unsigned (`identity: null`); electron-builder
        // performs no code signing. NVIDIA 3S signing + notarization (hardened
        // runtime + entitlements) is layered on by internal-build.
        identity: null,
        extraResources: [
            {
                from: 'scripts/build/macos/uninstall.sh',
                to: 'installer-tools/uninstall-macos.sh'
            }
        ],
        // SMAppService privileged helper (built by scripts/build/macos/build-helper.ts).
        // extraFiles `to` is relative to Contents/. Both Mach-O binaries must sit
        // in Contents/MacOS so nvpair-helper-ctl's Bundle.main resolves to the .app
        // (required by SMAppService.daemon(plistName:)). NVIDIA's per-binary 3S
        // sign loop (internal-build/signing/macos/sign.ts) also signs them.
        extraFiles: [
            {
                from: 'native/build/com.nvidia.nvpair.helper',
                to: 'MacOS/com.nvidia.nvpair.helper'
            },
            {
                from: 'native/build/nvpair-helper-ctl',
                to: 'MacOS/nvpair-helper-ctl'
            },
            {
                from: 'scripts/build/macos/com.nvidia.nvpair.helper.plist',
                to: 'Library/LaunchDaemons/com.nvidia.nvpair.helper.plist'
            }
        ]
    },
    dmg: {
        title: `${APP_DISPLAY_NAME} \${version}`
    },
    afterPack: macAfterPack,
    afterAllArtifactBuild: macAfterAllArtifactBuild
}

export default config
