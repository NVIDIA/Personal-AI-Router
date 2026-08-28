// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { execFile, spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { app, BrowserWindow } from 'electron'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import {
    INFERENCE_DISPATCHER_RESOURCE_DIR,
    inferenceDispatcherFileName
} from '@/shared/constants/inference-dispatcher'
import { currentPlatform } from '@/shared/utils/platform'
import type { DispatcherBackend, DispatcherModel } from '@/shared/types/inference-dispatcher'
import type { IpcPushChannelKey, IpcPushChannelMap } from '@/shared/types/ipc-channels'
import {
    DEMO_ENGINE_PROBES,
    DEMO_MAX_SUBMIT_SECONDS,
    DEMO_REQUEST_TIMEOUT_SECONDS,
    DEMO_STAGES,
    IDLE_DEMO_STATE,
    type DemoState,
    type DemoTarget
} from '@/shared/types/inference-demo'
import { buildDemoSchedule, type ScheduledRequest } from '@/electron/inference-demo-schedule'

/**
 * Inference Demo scheduler (node-local).
 *
 * Owns the open-loop submission schedule described in the mini spec. The Go
 * `inference-dispatcher` binary is unchanged: it still runs one backend/model
 * per invocation, so this module spawns one short-lived dispatcher process per
 * scheduled request and lets NV PAIR do all queueing and routing.
 *
 * Three invariants matter more than anything else here:
 *
 *  1. Nothing is submitted at or after `DEMO_MAX_SUBMIT_SECONDS`. This is checked
 *     against the wall clock at fire time, not just when the timer was armed, so
 *     a suspended machine or a stalled event loop cannot flush a backlog of
 *     expired timers past the ceiling.
 *  2. Stopping cancels *future* submissions only. Already-spawned dispatcher
 *     processes are never signalled, so queued and in-progress work finishes
 *     naturally — the opposite of `destroyInferenceDemoSync`, which is reserved
 *     for app teardown.
 *  3. Ending is immediate. Stop, or the end of the schedule, returns the state to
 *     `idle` at once. We deliberately do not linger in a "draining" state waiting
 *     on processes we were told not to wait on; `live` exists only so teardown
 *     can reap them.
 */

const PROBE_TIMEOUT_SECONDS = 5
const PROBE_MAX_BYTES = 2 * 1024 * 1024

let state: DemoState = { ...IDLE_DEMO_STATE }
/** Pending submission timers for the active run. Excludes the ceiling timer. */
let timers = new Set<ReturnType<typeof setTimeout>>()
/** Backstop that shuts the window at the ceiling even if a timer misbehaves. */
let ceilingTimer: ReturnType<typeof setTimeout> | null = null
/** Spawned dispatcher processes, tracked purely so app teardown can reap them. */
let live = new Set<ReturnType<typeof spawn>>()
/** Wall-clock deadline for the active run; the real 60s guarantee. */
let deadlineMs = 0
/** Guards against a stale run's callbacks mutating state after a newer start. */
let generation = 0

/**
 * Packaged builds place `extraResources` next to `process.resourcesPath`; in dev
 * they sit at the desktop project root (`app.getAppPath()`). Mirrors
 * `getCliBinDir()` in the modular supervisor, but resolves the dispatcher's own
 * `tools/` directory: the dispatcher is not a services binary and deliberately
 * stays out of the cli-bin inventory.
 */
function binaryPath(): string {
    const base = app.isPackaged ? process.resourcesPath : app.getAppPath()
    return path.join(
        base,
        INFERENCE_DISPATCHER_RESOURCE_DIR,
        inferenceDispatcherFileName(currentPlatform())
    )
}

function assertBinaryExists(): string {
    const executable = binaryPath()
    if (!fs.existsSync(executable)) {
        throw new Error(
            `Inference dispatcher binary not found at ${executable}. Run npm run build:tools.`
        )
    }
    return executable
}

function snapshot(): DemoState {
    return { ...state }
}

function broadcastPush<C extends IpcPushChannelKey>(channel: C, value: IpcPushChannelMap[C]): void {
    for (const win of BrowserWindow.getAllWindows()) {
        if (!win.isDestroyed()) win.webContents.send(channel, value)
    }
}

function broadcastState(): void {
    broadcastPush('demo:state', snapshot())
}

function patch(changes: Partial<DemoState>): void {
    state = { ...state, ...changes }
    broadcastState()
}

/** Narrows one entry of the binary's parsed `--list-models` JSON. */
function isDispatcherModel(value: unknown): value is DispatcherModel {
    return (
        typeof value === 'object' &&
        value !== null &&
        'name' in value &&
        typeof value.name === 'string'
    )
}

/**
 * Mirrors `supportsGeneration` in the Go client. An explicit type wins;
 * otherwise we look at capabilities; a model advertising neither is given the
 * benefit of the doubt, matching the binary's own behaviour.
 */
function isTextGenerationModel(model: DispatcherModel): boolean {
    if (model.type) return model.type.toLowerCase() === 'llm'
    if (!model.capabilities || model.capabilities.length === 0) return true
    return model.capabilities.some(capability =>
        ['completion', 'chat', 'generate'].includes(capability.toLowerCase())
    )
}

/**
 * Ask the binary for one engine's live inventory. A refused connection or any
 * other failure means the engine simply isn't a target — the demo never treats
 * an absent engine as an error.
 */
function probeEngine(
    executable: string,
    backend: DispatcherBackend,
    port: number
): Promise<DemoTarget[]> {
    const args = [
        '--backend',
        backend,
        '--port',
        String(port),
        '--timeout',
        String(PROBE_TIMEOUT_SECONDS),
        '--list-models'
    ]
    return new Promise(resolve => {
        const probe = execFile(
            executable,
            args,
            {
                encoding: 'utf8',
                windowsHide: true,
                timeout: PROBE_TIMEOUT_SECONDS * 1000 + 2_000,
                maxBuffer: PROBE_MAX_BYTES,
                // Discovery is a demo child like any other: the same
                // INFERENCE_DISPATCHER_* variables that could redirect a submit
                // could redirect a probe.
                env: dispatcherChildEnv()
            },
            (error, stdout) => {
                if (error) {
                    resolve([])
                    return
                }
                try {
                    const parsed: unknown = JSON.parse(stdout)
                    if (!Array.isArray(parsed)) {
                        resolve([])
                        return
                    }
                    const targets = parsed
                        .filter(isDispatcherModel)
                        .filter(isTextGenerationModel)
                        .map(model => ({ backend, port, model: model.name }))
                    resolve(targets)
                } catch {
                    resolve([])
                }
            }
        )
        // Probes are reapable like any other demo child; otherwise a stop during
        // discovery leaves them running and app exit orphans them.
        //
        // Untracking happens on 'close' rather than inside the execFile callback
        // so it never touches `probe` before the assignment completes.
        live.add(probe)
        probe.once('close', () => live.delete(probe))
        probe.once('error', () => live.delete(probe))
    })
}

/**
 * Snapshot every engine/model pair the local proxies expose.
 *
 * Ports come from the broker's live proxy registry rather than constants, so we
 * always target the proxy facade — never an engine's own backend port. An
 * engine whose proxy has not reported a port yet is simply not a target.
 *
 * The same model exposed by two engines is two distinct targets, per the spec.
 */
async function discoverTargets(executable: string): Promise<DemoTarget[]> {
    const bridge = getModularBridgeState()
    const reachable = DEMO_ENGINE_PROBES.map(probe => ({
        backend: probe.backend,
        port: bridge.getProxyPort(probe.proxyEngine)
    })).filter(
        (probe): probe is { backend: DispatcherBackend; port: number } => probe.port !== null
    )

    const results = await Promise.all(
        reachable.map(probe => probeEngine(executable, probe.backend, probe.port))
    )
    return results.flat()
}

const DISPATCHER_ENV_PREFIX = 'inference_dispatcher_'

/**
 * Environment for a demo child.
 *
 * The whole `INFERENCE_DISPATCHER_*` namespace is stripped rather than any
 * single variable. `INFERENCE_DISPATCHER_CONFIG` would load an arbitrary JSON
 * config, and `INFERENCE_DISPATCHER_RESULT_LOG` / `_DEBUG_ERROR_LOG` would make
 * the child write inference metadata to disk. `_LOOP` would blow the 60s ceiling.
 * The schedule is the only thing that decides what a demo child does.
 *
 * The prefix test is case-insensitive because Windows environment variables are
 * matched case-insensitively: a lowercase `inference_dispatcher_loop` in the
 * parent's environment would still reach the child as `INFERENCE_DISPATCHER_LOOP`.
 */
function dispatcherChildEnv(): NodeJS.ProcessEnv {
    return Object.fromEntries(
        Object.entries(process.env).filter(
            ([key]) => !key.toLowerCase().startsWith(DISPATCHER_ENV_PREFIX)
        )
    )
}

/** Spawn one dispatcher process for a single request. Fire and forget. */
function submit(executable: string, request: ScheduledRequest, currentGeneration: number): void {
    const stage = DEMO_STAGES[request.stageIndex]
    if (!stage) return
    const { target } = request

    const args = [
        '--backend',
        target.backend,
        '--port',
        String(target.port),
        '--model',
        target.model,
        '--prompt',
        stage.prompt,
        '--count',
        '1',
        '--mode',
        'series',
        '--concurrency',
        '1',
        '--timeout',
        String(DEMO_REQUEST_TIMEOUT_SECONDS),
        '--max-tokens',
        String(stage.maxTokens),
        '--temperature',
        String(stage.temperature)
    ]
    // Ollama's separate reasoning channel would inflate latency and token counts
    // without changing what the demo is showing, so switch it off where supported.
    if (target.backend === 'ollama') args.push('--ollama-think', 'false')

    const child = spawn(executable, args, {
        windowsHide: true,
        stdio: 'ignore',
        env: dispatcherChildEnv()
    })

    // Tracked only so teardown can reap it. Nothing about demo state or the
    // toast depends on when this process finishes.
    live.add(child)
    const forget = () => live.delete(child)
    child.once('close', forget)
    // A spawn failure for one request is not a demo failure — the schedule
    // continues — but it must not be counted, which is why `submitted` is
    // incremented on 'spawn' below rather than here. `spawn` reports a missing
    // or unexecutable binary asynchronously via 'error', not by throwing, so a
    // try/catch around the call above would catch nothing.
    child.once('error', forget)
    child.once('spawn', () => {
        if (currentGeneration !== generation) return
        if (state.status !== 'running') return
        patch({ submitted: state.submitted + 1 })
    })
}

/**
 * End the run immediately: cancel every pending submission and return to idle so
 * the toast closes and the button comes back.
 *
 * Processes already spawned are deliberately left running and untracked by the
 * state — the spec forbids cancelling, awaiting, or reporting on them, and their
 * output is meant to land as ordinary NV PAIR job activity.
 */
function finishRun(): void {
    for (const timer of timers) clearTimeout(timer)
    timers = new Set()
    if (ceilingTimer) {
        clearTimeout(ceilingTimer)
        ceilingTimer = null
    }
    deadlineMs = 0
    if (state.status === 'idle') return
    state = { ...IDLE_DEMO_STATE }
    broadcastState()
}

export function getInferenceDemoState(): DemoState {
    return snapshot()
}

/**
 * Begin a demo on this node.
 *
 * Rejects if a demo is already active locally. Demos on other nodes are
 * unaffected and unknown to us — demo state is deliberately not synchronized.
 */
export async function startInferenceDemo(): Promise<DemoState> {
    if (state.status !== 'idle') {
        throw new Error('An inference demo is already running on this node.')
    }

    const executable = assertBinaryExists()
    const currentGeneration = ++generation

    state = { ...IDLE_DEMO_STATE, status: 'preparing' }
    broadcastState()

    let targets: DemoTarget[]
    try {
        targets = await discoverTargets(executable)
    } catch (error) {
        if (currentGeneration === generation) {
            state = { ...IDLE_DEMO_STATE }
            broadcastState()
        }
        throw error instanceof Error ? error : new Error(String(error))
    }

    // Discovery is async, so the user may have hit Stop (or the app may have
    // torn down) while we were probing. Either drops us out of `preparing`, in
    // which case no schedule should be armed at all.
    if (currentGeneration !== generation || state.status !== 'preparing') {
        return snapshot()
    }

    if (targets.length === 0) {
        state = { ...IDLE_DEMO_STATE }
        broadcastState()
        // Surfaced by the caller as an ordinary error banner, not in the toast.
        // Deliberately names no port: the proxies own their listeners, and
        // quoting a number here would be the same mistake as hardcoding one.
        throw new Error(
            'No local inference engine exposed a text-generation model. Start Ollama or LM Studio, wait for it to appear in Settings, and try again.'
        )
    }

    const schedule = buildDemoSchedule(targets)
    const engines = new Set(targets.map(target => `${target.backend}:${target.port}`))

    deadlineMs = Date.now() + DEMO_MAX_SUBMIT_SECONDS * 1000
    state = {
        status: 'running',
        startedAt: new Date().toISOString(),
        targetCount: targets.length,
        engineCount: engines.size,
        submitted: 0,
        planned: schedule.length
    }
    broadcastState()

    for (const request of schedule) {
        const timer = setTimeout(() => {
            timers.delete(timer)
            if (currentGeneration !== generation) return
            if (state.status !== 'running') return
            // The wall clock, not the timer, is what actually enforces the
            // ceiling. If the machine slept or the loop stalled, several expired
            // timers can fire back-to-back well past the limit; every one of them
            // must be dropped rather than flushed as a burst.
            if (Date.now() >= deadlineMs) {
                finishRun()
                return
            }
            submit(executable, request, currentGeneration)
            // Last submission has gone out (58s). Nothing is left to wait for, so
            // end now rather than idling until the ceiling at 60s.
            if (timers.size === 0) finishRun()
        }, request.atMs)
        timers.add(timer)
    }

    // Backstop in case a request timer never fires at all.
    ceilingTimer = setTimeout(() => {
        ceilingTimer = null
        if (currentGeneration !== generation) return
        finishRun()
    }, DEMO_MAX_SUBMIT_SECONDS * 1000)

    return snapshot()
}

/**
 * Stop immediately.
 *
 * Future submissions are cancelled and the demo returns to idle at once, so the
 * toast closes and the button comes back with no "finishing up" tail. Requests
 * already submitted are left alone to finish on their own — they are neither
 * cancelled, awaited, nor reported on.
 */
export function stopInferenceDemo(): DemoState {
    finishRun()
    return snapshot()
}

/**
 * App teardown only: drop timers and hard-kill anything still running.
 *
 * This is the one place demo requests are killed. `stopInferenceDemo` must never
 * do this — the spec requires in-flight work to finish naturally — but we cannot
 * leave orphaned child processes behind when the app exits.
 */
export function destroyInferenceDemoSync(): void {
    generation++
    for (const timer of timers) clearTimeout(timer)
    timers = new Set()
    if (ceilingTimer) {
        clearTimeout(ceilingTimer)
        ceilingTimer = null
    }
    deadlineMs = 0
    for (const child of live) {
        if (child.exitCode === null) child.kill('SIGKILL')
    }
    live = new Set()
    state = { ...IDLE_DEMO_STATE }
}
