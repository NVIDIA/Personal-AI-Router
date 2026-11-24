// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineCaps } from '@/ui/types/engine-manifest'

/**
 * What removing one model does to the engine, and therefore what its row offers.
 * The engine manager decides this; the desktop only says it, so every arm here
 * traces to a manifest declaration plus what the engine was able to answer.
 *
 * A manifest's `restart_after` bounces the engine back up after the removal
 * (LM Studio, which builds its model index at startup). Its `stop_before` quits
 * the engine and waits for its port to go quiet first (MTPLX), and it does that
 * for **two** of the three answers to "is this model the one being served?": the
 * engine says it is, and the engine cannot be asked — see `stopBeforeAction` in
 * `services/nvpair-engine-manager/actions.go`, which treats an unreadable answer
 * as the model being in use. `Manifest.Validate` refuses a manifest declaring
 * both `stop_before` and `restart_after` (`registry.go`), so no engine both
 * quits and restarts, and the two flags stay enough to pick one wording.
 *
 * That is the same three-way split the TUI's LOADED column keeps apart —
 * `"no"`, `"yes"`, `"?"`, pinned by `TestUnreportedLoadState_IsUnknown` in
 * `services/nvpair-tui/ui/nodedetail_test.go` — where an engine left out of
 * `loadedByEngine` means "cannot tell", not "nothing resident". Two of the three
 * answers are the same button here, because quitting the engine is always safe
 * and a one-click Delete is only safe when non-residency is known. That is why
 * `residencyKnown` is an input: reading a missing answer as "not loaded" is what
 * this file exists to prevent.
 */
type ModelDeleteMode = 'blocked' | 'plain' | 'restarts-engine' | 'quits-engine'

/**
 * Choose the affordance for one model row.
 *
 * @param capabilities the engine's declared capabilities.
 * @param residencyKnown whether the engine answered what it holds in memory.
 *   False covers both "no residency read exists" and "the read failed", and is
 *   deliberately not the same as {@link residentInEngine}: not knowing is not
 *   knowing-absent.
 * @param residentInEngine the engine reported this model resident.
 */
export function modelDeleteMode(
    capabilities: EngineCaps,
    residencyKnown: boolean,
    residentInEngine: boolean
): ModelDeleteMode {
    // vLLM refuses a delete that would pull the model out from under its own
    // server, so there is nothing to offer — not a warning, a closed door.
    if (residentInEngine && capabilities.deleteBlockedWhileLoaded) return 'blocked'
    // MTPLX's removal stops the engine whenever it cannot rule the model out, so
    // the row warns for a resident pack and an unreadable one alike.
    if (capabilities.stopsEngineBeforeDelete && (!residencyKnown || residentInEngine)) {
        return 'quits-engine'
    }
    // LM Studio rebuilds its index only at startup, so any delete bounces it.
    if (capabilities.restartsOnModelDelete) return 'restarts-engine'
    return 'plain'
}

/** The menu text. Names the engine only where deleting takes it down. */
export function deleteMenuItemLabel(mode: ModelDeleteMode, engineName: string): string {
    return mode === 'quits-engine' ? `Quit ${engineName} and delete` : 'Delete'
}

/**
 * Which handler the click goes to: act now, or ask first. Carried as the menu
 * item's id because that is the row's only channel to the list, and it keeps
 * the decision in one place — the row that shows the warning is the row that
 * asks for it.
 */
export function deleteActionName(mode: ModelDeleteMode): 'delete' | 'delete-confirm' {
    return mode === 'quits-engine' || mode === 'restarts-engine' ? 'delete-confirm' : 'delete'
}

/**
 * What the confirmation says, once the user has asked for a delete that takes
 * the engine with it. It describes what the engine manager is about to do to the
 * engine, which is the one thing that is certain at that moment, in words taken
 * from `manifests/mtplx.json` and `manifests/lmstudio.json`. It deliberately does
 * not say the model is loaded: the same dialog covers "the engine is serving it"
 * and "the engine could not be asked", and only the second one is a guess.
 */
export function deleteConfirmCopy(
    capabilities: EngineCaps,
    engineName: string,
    modelName: string
): { message: string; confirmLabel: string } {
    if (capabilities.stopsEngineBeforeDelete) {
        return {
            message: `Deleting ${modelName} stops ${engineName} first and leaves it down: it serves one model per process, so PAIR will not remove a pack it cannot rule out being the one served.`,
            confirmLabel: `Quit ${engineName} and delete`
        }
    }
    return {
        message: `Deleting ${modelName} requires ${engineName} to be restarted.`,
        confirmLabel: 'Delete and restart'
    }
}
