// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import * as fs from 'fs'
import * as path from 'path'
import { describe, expect, it } from 'vitest'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { EngineType } from '@/shared/types/engines'

/**
 * Deleting a model changes the engine only where the engine cannot see the
 * deletion any other way: LM Studio, which answers `/v1/models` from an index
 * built at startup and exposes no rescan, and MTPLX, whose `mtplx remove` has no
 * in-use guard. Three independent decisions have to agree on what that change
 * is, and they live in different languages:
 *
 * - `restart_after` in the engine-manager manifest bounces the engine back up.
 * - `stop_before` in the manifest quits it and waits for its port to go quiet
 *   first, and leaves it down.
 * - `EngineCaps.restartsOnModelDelete` / `stopsEngineBeforeDelete` decide whether
 *   the desktop warns first, and which wording it uses.
 *
 * Nothing in the type system ties them together, so a manifest gaining
 * `restart_after` without the matching capability would silently bounce an
 * engine mid-inference with no confirmation. This test is that tie. Reading the
 * real manifests also holds the pair that `Manifest.Validate` (`registry.go`)
 * refuses apart: no engine both quits and restarts, which is what lets one
 * engine declare exactly one wording.
 */

const MANIFEST_DIR = path.resolve(process.cwd(), '../services/nvpair-engine-manager/manifests')

/** Map engine-manager manifest ids to the desktop's closed engine ids. */
const ENGINE_TYPE_BY_MANIFEST_ID: Record<string, EngineType> = {
    ollama: 'ollama',
    lmstudio: 'lm-studio',
    llamacpp: 'llama-cpp',
    mtplx: 'mtplx'
}

interface ManifestAction {
    restart_after?: boolean
    stop_before?: boolean
}

interface Manifest {
    engine: string
    actions?: Record<string, ManifestAction>
}

function readManifests(): Manifest[] {
    return fs
        .readdirSync(MANIFEST_DIR)
        .filter(name => name.endsWith('.json'))
        .map(name => JSON.parse(fs.readFileSync(path.join(MANIFEST_DIR, name), 'utf8')) as Manifest)
}

describe('delete-model engine changes are declared, not inferred', () => {
    it('only LM Studio declares restart_after, and only on delete_model', () => {
        const declaring = readManifests().flatMap(m =>
            Object.entries(m.actions ?? {})
                .filter(([, action]) => action.restart_after === true)
                .map(([name]) => `${m.engine}.${name}`)
        )
        expect(declaring).toEqual(['lmstudio.delete_model'])
    })

    it('only MTPLX declares stop_before, and only on delete_model', () => {
        const declaring = readManifests().flatMap(m =>
            Object.entries(m.actions ?? {})
                .filter(([, action]) => action.stop_before === true)
                .map(([name]) => `${m.engine}.${name}`)
        )
        expect(declaring).toEqual(['mtplx.delete_model'])
    })

    it('no action declares both stop_before and restart_after', () => {
        // One wording per engine is only sound while no engine both quits and
        // restarts: `deleteConfirmCopy` reads exactly these two flags to choose it,
        // and `Manifest.Validate` in `services/nvpair-engine-manager/registry.go`
        // rejects the pair. Pinned here because the desktop cannot see that check.
        for (const manifest of readManifests()) {
            for (const [name, action] of Object.entries(manifest.actions ?? {})) {
                expect(
                    action.restart_after === true && action.stop_before === true,
                    `${manifest.engine}.${name} declares both stop_before and restart_after`
                ).toBe(false)
            }
        }
    })

    it('Ollama deletes with neither flag and therefore without a confirmation', () => {
        const ollama = readManifests().find(m => m.engine === 'ollama')
        expect(ollama?.actions?.delete_model?.restart_after).toBeUndefined()
        expect(ollama?.actions?.delete_model?.stop_before).toBeUndefined()
        // `modelDeleteMode` only asks first when one of the two is set, so absent
        // is the assertion that matters, not `false`.
        expect(EngineCapabilities.ollama.restartsOnModelDelete).toBeFalsy()
        expect(EngineCapabilities.ollama.stopsEngineBeforeDelete).toBeFalsy()
        expect(EngineCapabilities.ollama.hasDeleteModel).toBe(true)
    })

    it('llama.cpp deletes without restarting the router', () => {
        const llamacpp = readManifests().find(m => m.engine === 'llamacpp')
        expect(llamacpp?.actions?.delete_model).toBeDefined()
        expect(llamacpp?.actions?.delete_model?.restart_after).toBeUndefined()
        expect(llamacpp?.actions?.delete_model?.stop_before).toBeUndefined()
        expect(EngineCapabilities['llama-cpp'].restartsOnModelDelete).toBeFalsy()
        expect(EngineCapabilities['llama-cpp'].stopsEngineBeforeDelete).toBeFalsy()
        expect(EngineCapabilities['llama-cpp'].hasDeleteModel).toBe(true)
    })

    it('every engine that bounces on delete also warns the user first', () => {
        for (const manifest of readManifests()) {
            const engineType = ENGINE_TYPE_BY_MANIFEST_ID[manifest.engine]
            if (!engineType) continue
            const bounces = manifest.actions?.delete_model?.restart_after === true
            expect(
                EngineCapabilities[engineType].restartsOnModelDelete ?? false,
                `${manifest.engine}: manifest restart_after=${bounces} but EngineCapabilities['${engineType}'].restartsOnModelDelete disagrees`
            ).toBe(bounces)
        }
    })

    it('every engine that quits on delete also warns the user first', () => {
        for (const manifest of readManifests()) {
            const engineType = ENGINE_TYPE_BY_MANIFEST_ID[manifest.engine]
            if (!engineType) continue
            const quits = manifest.actions?.delete_model?.stop_before === true
            expect(
                EngineCapabilities[engineType].stopsEngineBeforeDelete ?? false,
                `${manifest.engine}: manifest stop_before=${quits} but EngineCapabilities['${engineType}'].stopsEngineBeforeDelete disagrees`
            ).toBe(quits)
        }
    })

    it('keeps LM Studio bouncing and MTPLX quitting, and only MTPLX quitting', () => {
        const lmstudio = readManifests().find(m => m.engine === 'lmstudio')
        expect(lmstudio?.actions?.delete_model?.restart_after).toBe(true)
        expect(EngineCapabilities['lm-studio'].restartsOnModelDelete).toBe(true)

        const mtplx = readManifests().find(m => m.engine === 'mtplx')
        expect(mtplx?.actions?.delete_model?.stop_before).toBe(true)
        expect(mtplx?.actions?.delete_model?.restart_after).toBeUndefined()
        expect(EngineCapabilities.mtplx.stopsEngineBeforeDelete).toBe(true)
        // MTPLX does not close the door the way vLLM closes it, and
        // `modelDeleteMode` tests the block first: a stale
        // `deleteBlockedWhileLoaded` here would disable Delete on exactly the pack
        // whose copy has to say the engine is quitting.
        expect(EngineCapabilities.mtplx.deleteBlockedWhileLoaded).toBeUndefined()
    })
})
