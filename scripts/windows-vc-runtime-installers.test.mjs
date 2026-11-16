// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')

function repositoryFile(relativePath) {
    return readFileSync(resolve(REPO_ROOT, relativePath), 'utf8')
}

function assertRuntimeExitHandling(source, exitCodeVariable) {
    assert.match(source, /\/install \/quiet \/norestart/)
    for (const successCode of [0, 1638, 1641, 3010]) {
        assert.ok(
            source.includes(`${exitCodeVariable} == ${String(successCode)}`),
            `missing accepted runtime exit code ${String(successCode)}`
        )
    }
    assert.match(source, /SetRebootFlag true/)
    assert.match(source, /SetErrorLevel 3/)
}

test('both desktop Windows architectures stage the shared runtime package', () => {
    const packageJson = JSON.parse(repositoryFile('desktop/package.json'))

    assert.match(packageJson.scripts['build:electron:win:x64'], /^npm run stage:vc-redist &&/)
    assert.match(packageJson.scripts['build:electron:win:arm64'], /^npm run stage:vc-redist &&/)
})

test('desktop packaging verifies and installs the staged runtime', () => {
    const builderConfig = repositoryFile('desktop/electron-builder.config.ts')
    const installer = repositoryFile('desktop/scripts/build/installer.nsh')

    assert.match(builderConfig, /function assertVcRedistPackagingInput\(\): void/)
    assert.match(builderConfig, /createHash\('sha256'\)/)
    assert.match(builderConfig, /from: vcRedistStagedPath/)
    assert.match(builderConfig, /to: 'installer-tools\/VC_redist\.x64\.exe'/)
    assert.match(builderConfig, /signExts: \['!VC_redist\.x64\.exe'\]/)
    assert.match(
        installer,
        /!macro customInstall\s+!insertmacro pairAssertPayloadInstalled\s+!insertmacro pairInstallVcRuntime\s+!insertmacro pairAddFirewallRules\s+!macroend/
    )
    assert.match(installer, /Delete "\$INSTDIR\\resources\\installer-tools\\VC_redist\.x64\.exe"/)
    assertRuntimeExitHandling(installer, '$8')
})

test('standalone services installer stages and installs the shared runtime', () => {
    const buildScript = repositoryFile('services/installer_build.bat')
    const installer = repositoryFile('services/installer/nvpair-setup.nsi')

    const stageIndex = buildScript.indexOf('node "%ROOT%..\\scripts\\stage-vc-redist.mjs"')
    const makensisIndex = buildScript.indexOf('"%MAKENSIS%" /V3')
    assert.ok(stageIndex >= 0, 'services installer build does not stage the runtime')
    assert.ok(stageIndex < makensisIndex, 'services installer stages the runtime after makensis')
    assert.match(
        installer,
        /File \/oname=VC_redist\.x64\.exe "\.\.\\\.\.\\\.build\\vc-redist\\VC_redist\.x64\.exe"/
    )
    assert.match(installer, /Delete "\$PLUGINSDIR\\VC_redist\.x64\.exe"/)
    assertRuntimeExitHandling(installer, '$R4')
})
