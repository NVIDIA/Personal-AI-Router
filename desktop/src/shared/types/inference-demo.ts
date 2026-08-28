// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { DispatcherBackend } from '@/shared/types/inference-dispatcher'

/**
 * Inference Demo — a fixed, node-local burst of synthetic inference traffic sent
 * through PAIR's proxies so a user can watch real work move through the router.
 *
 * Where each request lands is the backend's decision. Requests are addressed to
 * a local proxy exactly as a third-party client would address it; whether that
 * produces work on one machine or several depends on the cluster the proxies see.
 *
 * This is deliberately NOT a benchmark or a diagnostic: no prompts, responses,
 * scores, findings, or result summaries are ever surfaced. The only user-visible
 * output is ordinary NV PAIR job activity on the Overview tab plus a small
 * status toast.
 *
 * Scheduling lives entirely in the Electron main process
 * (`@/electron/inference-demo`). The Go `inference-dispatcher` binary is
 * unchanged and is invoked once per scheduled request group.
 */

/** Wall-clock offsets, in seconds, at which a cohort of simulated agents starts. */
export const DEMO_COHORT_OFFSETS_SECONDS = [0, 10, 20, 30, 40] as const

/** Simulated agent workloads started per cohort, before target-count scaling. */
export const DEMO_BASE_AGENTS_PER_COHORT = 2

/**
 * Hard ceiling on the submission window. Nothing is submitted at or after this
 * point; work already in flight is left alone. The packaged `steady` profile's
 * 50s cohort is intentionally omitted so the last submission lands at 58s.
 */
export const DEMO_MAX_SUBMIT_SECONDS = 60

/** Per-request timeout handed to the dispatcher binary. */
export const DEMO_REQUEST_TIMEOUT_SECONDS = 120

/**
 * Engines the demo can drive, paired with the proxy each one sits behind.
 *
 * Ports are deliberately absent. The broker owns `ollama-proxy` and
 * `lmstudio-proxy` and reports their bound listeners; PAIR never fabricates a
 * port (see the note at the top of `@/shared/constants/modular-runtime`). The
 * demo resolves each port at start via `getProxyPort()` and skips any engine
 * whose proxy has not reported.
 *
 * Targeting a proxy rather than the engine itself is what makes the demo a
 * demo: requests enter PAIR's router and are placed by it. Hitting an engine's
 * own backend port would bypass routing entirely, which
 * `proxy-inference-routing.mdc` prohibits.
 */
export const DEMO_ENGINE_PROBES: readonly {
    backend: DispatcherBackend
    /** Engine key used by the broker's proxy port registry. */
    proxyEngine: 'ollama' | 'lm-studio'
}[] = [
    { backend: 'ollama', proxyEngine: 'ollama' },
    { backend: 'lmstudio', proxyEngine: 'lm-studio' }
]

/**
 * One stage of a simulated agent workload. Offsets are relative to the agent's
 * own start time, so the final submission of the 40s cohort lands at 58s.
 */
interface DemoStage {
    /** Internal identifier; never shown to the user. */
    id: string
    /** Seconds after the owning agent's start time. */
    offsetSeconds: number
    maxTokens: number
    temperature: number
    /** Hidden internal prompt. Never displayed or retained in the UI. */
    prompt: string
}

/**
 * The six request shapes of a simulated agent, mirroring `STAGES` in the
 * reference `agent_load_scenarios.py`. Prompts are internal and intentionally
 * generic so they exercise the engine without depending on any real workload.
 */
export const DEMO_STAGES: readonly DemoStage[] = [
    {
        id: 'intake',
        offsetSeconds: 0,
        maxTokens: 48,
        temperature: 0.0,
        prompt: 'Classify the following support request into one category: billing, technical, or account. Request: "My export finished but the download link returns a 404." Answer with the category only.'
    },
    {
        id: 'planning',
        offsetSeconds: 3,
        maxTokens: 192,
        temperature: 0.1,
        prompt: 'Draft a short numbered plan for diagnosing an intermittent HTTP 404 on a file download endpoint that only affects large exports. Keep it under six steps.'
    },
    {
        id: 'tool-selection',
        offsetSeconds: 7,
        maxTokens: 96,
        temperature: 0.0,
        prompt: 'Given the tools list_objects, read_log, restart_worker, and notify_user, choose the single best next tool for confirming whether an exported file was ever written to object storage. Answer with the tool name and one sentence of justification.'
    },
    {
        id: 'code-transformation',
        offsetSeconds: 10,
        maxTokens: 160,
        temperature: 0.0,
        prompt: 'Rewrite this function so it returns an explicit error instead of nil when the key is missing:\n\nfunc get(m map[string]string, k string) string { return m[k] }'
    },
    {
        id: 'observation-summary',
        offsetSeconds: 14,
        maxTokens: 128,
        temperature: 0.0,
        prompt: 'Summarize in two sentences: the worker log shows 14 successful uploads, 2 uploads that timed out after 30s, and no retry attempts recorded for the timeouts.'
    },
    {
        id: 'final-synthesis',
        offsetSeconds: 18,
        maxTokens: 256,
        temperature: 0.1,
        prompt: 'Write a brief incident note covering root cause, user impact, and the single highest-value follow-up action, for an issue where large export uploads timed out and were never retried.'
    }
]

/** A single engine/model pair discovered at demo start. */
export interface DemoTarget {
    backend: DispatcherBackend
    port: number
    model: string
}

/**
 * There is deliberately no "draining" state.
 *
 * Stopping — or reaching the end of the schedule — ends the demo immediately as
 * far as the user is concerned: the toast closes and the button comes back. The
 * spec forbids cancelling or waiting on requests that were already submitted, so
 * those simply finish on their own and show up as ordinary NV PAIR job activity.
 * Tracking them in the demo's own state would mean making the user wait on work
 * we were told not to wait on.
 */
export type DemoStatus =
    /** No demo running on this node. */
    | 'idle'
    /** Probing loopback ports and enumerating models; nothing submitted yet. */
    | 'preparing'
    /** Inside the submission window. */
    | 'running'

/**
 * Node-local demo state, broadcast to every renderer on change. Deliberately
 * carries no prompts, responses, or per-request results — only enough to render
 * a concise status toast.
 *
 * Failures are not represented here: start rejects over IPC and the caller shows
 * an ordinary error banner, because a toast must never carry an error.
 */
export interface DemoState {
    status: DemoStatus
    /** ISO timestamp of the moment the submission schedule began. */
    startedAt: string | null
    /** Distinct engine/model pairs snapshotted at start. */
    targetCount: number
    /** Distinct engines contributing targets. */
    engineCount: number
    /** Requests handed to the dispatcher so far. */
    submitted: number
    /** Total requests the schedule intends to submit. */
    planned: number
}

export const IDLE_DEMO_STATE: DemoState = {
    status: 'idle',
    startedAt: null,
    targetCount: 0,
    engineCount: 0,
    submitted: 0,
    planned: 0
}
