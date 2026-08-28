// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Pending-action store -- optimistic, memory-only UI state.
 *
 * The modular backend gives little or no immediate feedback for engine/model
 * commands (and on remote nodes a real state push can lag far behind the
 * click). This store flips the clicked control into a disabled/"working" state
 * the instant a command is fired so the user gets feedback and cannot double
 * fire the same operation.
 *
 * It ALWAYS yields to backend truth: every entry is cleared the moment a
 * superseding push arrives (`engines:state-changed` / `engines:progress-changed`
 * / `errors:update`) and, for actions the backend emits no signal for today
 * (model load/eject), after a fixed safety-net timeout.
 *
 * When the modular service is stopped (`connectorStatus === 'disconnected'`) no
 * command -- local or remote -- can reach the broker, so `begin` records
 * nothing and any in-flight entries are dropped the instant the connector
 * reports disconnected. Without this a fired command would otherwise sit in the
 * optimistic "working" state until the safety-net timeout even though it can
 * never complete.
 *
 * This is the single sanctioned exception to the "no local loading state" rule:
 * it is centralized here, never scattered across component `useState`, and it is
 * refresh-safe because it is memory only -- a reload simply shows real backend
 * state.
 */
import { create } from 'zustand'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { EngineCommandPayload, EngineCommandType } from '@/shared/types/engine-api'
import type { EngineProcessStatus, EngineType } from '@/shared/types/engines'
import type { ServiceStatus } from '@/shared/types/ipc-channels'
import { isEngineType } from '@/shared/utils/engines'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'

/** Re-enable a control after this long if no superseding push ever arrives. */
const PENDING_TIMEOUT_MS = 60_000
/** Keep Ollama Load locked one safety-net interval beyond the longest (11m) backend path. */
const OLLAMA_LOAD_PENDING_TIMEOUT_MS = 12 * 60_000
/**
 * Safety net for a delete on an engine whose backend restarts to pick the
 * deletion up (`EngineCaps.restartsOnModelDelete` / the manifest's
 * `restart_after`). That delete does not reply until the engine has stopped and
 * passed its readiness probe again — for LM Studio, `ready.timeout_s` alone is
 * 60s — so the default net would expire mid-flight and drop the spinner while
 * the operation is still running. Keep this above
 * `MODULAR_MODEL_ACTION_TIMEOUT_MS` (120s) so the real result, success or
 * failure, always wins the race against the net.
 */
const RESTART_DELETE_TIMEOUT_MS = 180_000
/** How often the sweep drops expired entries. */
const SWEEP_INTERVAL_MS = 1_000

/** Commands that flip an engine's lifecycle -- only one at a time per engine. */
const LIFECYCLE_COMMANDS: ReadonlySet<EngineCommandType> = new Set<EngineCommandType>([
    'toggle',
    'install',
    'uninstall',
    'update'
])

/** Commands that act on a single named model. */
const MODEL_COMMANDS: ReadonlySet<EngineCommandType> = new Set<EngineCommandType>([
    'pullModel',
    'loadModel',
    'unloadModel',
    'deleteModel'
])

interface PendingAction {
    nodeId: string
    engineType: EngineType
    action: EngineCommandType
    model?: string
    /**
     * Engine process status captured when a lifecycle command fired. The entry
     * clears on the first `engines:state-changed` whose status differs from
     * this, i.e. the backend acknowledged the transition.
     */
    baselineStatus?: EngineProcessStatus
    expiresAt: number
}

/**
 * How long the safety net waits for a command before re-enabling its control.
 * Slow Ollama loads and restart-backed deletes outlast the default net; every
 * other command keeps 60 seconds.
 */
function timeoutFor(command: EngineCommandType, engineType: EngineType): number {
    if (command === 'loadModel' && engineType === 'ollama') {
        return OLLAMA_LOAD_PENDING_TIMEOUT_MS
    }
    if (command === 'deleteModel' && EngineCapabilities[engineType]?.restartsOnModelDelete) {
        return RESTART_DELETE_TIMEOUT_MS
    }
    return PENDING_TIMEOUT_MS
}

function lifecycleKey(nodeId: string, engineType: EngineType): string {
    return `${nodeId}:${engineType}:lifecycle`
}

function modelKey(nodeId: string, engineType: EngineType, model: string): string {
    return `${nodeId}:${engineType}:model:${model}`
}

interface PendingActionsState {
    pending: Map<string, PendingAction>
    /** Record an optimistic pending entry for a fired command. No-op for non-lifecycle/non-model commands. */
    begin(payload: EngineCommandPayload): void
    /** The pending lifecycle action for an engine, or undefined when none is in flight. */
    getLifecyclePending(nodeId: string, engineType: EngineType): EngineCommandType | undefined
    /** The pending action for a specific model, or undefined when none is in flight. */
    getModelPending(
        nodeId: string,
        engineType: EngineType,
        model: string
    ): EngineCommandType | undefined
    initialize(): void
    cleanup(): void
}

let unsubs: Array<() => void> = []
let sweepTimer: ReturnType<typeof setInterval> | null = null

/**
 * Last-known modular connector status. The store only initializes on connect,
 * so `'connected'` is the correct optimistic default until the first
 * `service:get-status` / `service:status` corrects it.
 */
let connectorStatus: ServiceStatus['connectorStatus'] = 'connected'

export const usePendingActionsStore = create<PendingActionsState>((set, get) => ({
    pending: new Map(),

    begin: payload => {
        // With the modular service stopped, no engine command (local or remote)
        // can reach the broker, so recording an optimistic entry would only spin
        // the control until the safety-net timeout. The main process still
        // reports the "not available" warning.
        if (connectorStatus === 'disconnected') return

        const { command, engineType, nodeId, model } = payload
        const expiresAt = Date.now() + timeoutFor(command, engineType)

        if (LIFECYCLE_COMMANDS.has(command)) {
            const baselineStatus = useEngineStatusStore
                .getState()
                .getStatus(nodeId, engineType).processStatus
            const next = new Map(get().pending)
            next.set(lifecycleKey(nodeId, engineType), {
                nodeId,
                engineType,
                action: command,
                baselineStatus,
                expiresAt
            })
            set({ pending: next })
            return
        }

        if (MODEL_COMMANDS.has(command) && model) {
            const next = new Map(get().pending)
            next.set(modelKey(nodeId, engineType, model), {
                nodeId,
                engineType,
                action: command,
                model,
                expiresAt
            })
            set({ pending: next })
        }
    },

    getLifecyclePending: (nodeId, engineType) =>
        get().pending.get(lifecycleKey(nodeId, engineType))?.action,

    getModelPending: (nodeId, engineType, model) =>
        get().pending.get(modelKey(nodeId, engineType, model))?.action,

    initialize: () => {
        // Guard against double-subscribe: initialize() runs on connect and again
        // after a leave-cluster refresh without an intervening cleanup().
        if (sweepTimer !== null) return

        const clearKeys = (keys: string[]): void => {
            if (keys.length === 0) return
            const current = get().pending
            let next: Map<string, PendingAction> | null = null
            for (const key of keys) {
                if (current.has(key)) {
                    if (!next) next = new Map(current)
                    next.delete(key)
                }
            }
            if (next) set({ pending: next })
        }

        if (window.pairApi) {
            unsubs.push(
                window.pairApi.engines.onStateChanged(patch => {
                    // Lifecycle: clear once the backend reports a status that
                    // differs from the one captured when the command fired.
                    if (patch.status) {
                        const key = lifecycleKey(patch.nodeId, patch.engineType)
                        const entry = get().pending.get(key)
                        if (entry && patch.status.processStatus !== entry.baselineStatus) {
                            clearKeys([key])
                        }
                    }
                    // Model patches resolve delete (model gone), pull (model now
                    // present), and — since the backend stamps `ModelItem.status`
                    // — load (now `'loaded'`) and eject (present but no longer
                    // `'loaded'`), so the optimistic entry clears on real backend
                    // truth instead of the safety-net timeout.
                    if (patch.models) {
                        const statuses = new Map(patch.models.models.map(m => [m.name, m.status]))
                        const prefix = `${patch.nodeId}:${patch.engineType}:model:`
                        const toClear: string[] = []
                        for (const [key, p] of get().pending) {
                            if (!key.startsWith(prefix) || !p.model) continue
                            const present = statuses.has(p.model)
                            const loaded = statuses.get(p.model) === 'loaded'
                            if (p.action === 'deleteModel' && !present) {
                                toClear.push(key)
                            } else if (p.action === 'pullModel' && present) {
                                toClear.push(key)
                            } else if (p.action === 'loadModel' && loaded) {
                                toClear.push(key)
                            } else if (p.action === 'unloadModel' && present && !loaded) {
                                toClear.push(key)
                            }
                        }
                        clearKeys(toClear)
                    }
                }),
                window.pairApi.engines.onProgress(progress => {
                    // Any real progress event for a model supersedes the optimistic
                    // entry -- the progress store now owns the visual.
                    if (!progress.model) return
                    clearKeys([modelKey(progress.nodeId, progress.engineType, progress.model)])
                }),
                window.pairApi.errors.onUpdate(errors => {
                    const toClear: string[] = []
                    for (const e of errors) {
                        if (!e.nodeId || !isEngineType(e.engineType)) continue
                        toClear.push(
                            e.modelName
                                ? modelKey(e.nodeId, e.engineType, e.modelName)
                                : lifecycleKey(e.nodeId, e.engineType)
                        )
                    }
                    clearKeys(toClear)
                })
            )
        }

        // Track the modular connector so `begin` can refuse optimism while the
        // service is stopped, and so any entry still in flight is dropped the
        // instant the broker goes down (e.g. an unexpected crash). Seed the
        // cache once, then follow live `service:status` pushes.
        if (window.windowApi?.service) {
            window.windowApi.service
                .getStatus()
                .then(status => {
                    connectorStatus = status.connectorStatus
                })
                .catch(() => {})
            unsubs.push(
                window.windowApi.service.onStatusChanged(status => {
                    connectorStatus = status.connectorStatus
                    if (status.connectorStatus === 'disconnected' && get().pending.size > 0) {
                        set({ pending: new Map() })
                    }
                })
            )
        }

        sweepTimer = setInterval(() => {
            const now = Date.now()
            const toClear: string[] = []
            for (const [key, p] of get().pending) {
                if (now >= p.expiresAt) toClear.push(key)
            }
            clearKeys(toClear)
        }, SWEEP_INTERVAL_MS)
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        if (sweepTimer !== null) {
            clearInterval(sweepTimer)
            sweepTimer = null
        }
        if (get().pending.size > 0) set({ pending: new Map() })
    }
}))
