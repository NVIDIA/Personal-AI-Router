// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import * as fs from 'fs'
import * as path from 'path'
import { describe, expect, it } from 'vitest'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { EngineType } from '@/shared/types/engines'

/**
 * Deleting a model restarts the engine only where the engine cannot
 * see the deletion any other way — LM Studio, which answers `/v1/models` from an
 * index built at startup and exposes no rescan. Two independent decisions have
 * to agree on that, and they live in different languages:
 *
 * - `restart_after` in the engine-manager manifest decides whether the *backend*
 *   bounces the engine.
 * - `EngineCaps.restartsOnModelDelete` decides whether the *UI* warns first.
 *
 * Nothing in the type system ties them together, so a manifest gaining
 * `restart_after` without the matching capability would silently bounce an
 * engine mid-inference with no confirmation. This test is that tie.
 */

const MANIFEST_DIR = path.resolve(process.cwd(), '../services/nvpair-engine-manager/manifests')

/** Manifest engine ids differ from our `EngineType` for LM Studio only. */
const ENGINE_TYPE_BY_MANIFEST_ID: Record<string, EngineType> = {
    ollama: 'ollama',
    lmstudio: 'lm-studio',
    vllm: 'vllm'
}

interface ManifestAction {
    restart_after?: boolean
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

describe('delete-model restart is scoped to LM Studio', () => {
    it('only LM Studio declares restart_after, and only on delete_model', () => {
        const declaring = readManifests().flatMap(m =>
            Object.entries(m.actions ?? {})
                .filter(([, action]) => action.restart_after === true)
                .map(([name]) => `${m.engine}.${name}`)
        )
        expect(declaring).toEqual(['lmstudio.delete_model'])
    })

    it('Ollama deletes without a restart and therefore without a confirmation', () => {
        const ollama = readManifests().find(m => m.engine === 'ollama')
        expect(ollama?.actions?.delete_model?.restart_after).toBeUndefined()
        // `?? false` in ModelManager.tsx turns "absent" into "no confirmation",
        // so absent is the assertion that matters, not `false`.
        expect(EngineCapabilities.ollama.restartsOnModelDelete).toBeFalsy()
        expect(EngineCapabilities.ollama.hasDeleteModel).toBe(true)
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

    it('LM Studio still bounces — the whole point of the ticket', () => {
        const lmstudio = readManifests().find(m => m.engine === 'lmstudio')
        expect(lmstudio?.actions?.delete_model?.restart_after).toBe(true)
        expect(EngineCapabilities['lm-studio'].restartsOnModelDelete).toBe(true)
    })
})
