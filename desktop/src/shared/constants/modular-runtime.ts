// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The proxy port is never assumed: the broker owns the engine proxies and
// reports each bound listener via that engine's `<engine>-proxy:ready` /
// `<engine>-proxy:get-status` pair. Until it does, the proxy port is unknown
// (surfaced as null) — we do not fabricate a default, since a wrong port
// misleads the user and can route requests to the wrong place.

// nvpair-cluster-manager pairing (EAP-NOOB) HTTP port. `cluster:invite-node` and
// the PIN handshake are driven against this port. Hardcoded pending backend
// exposure on the discovery channel; see the current client-side
// responsibilities in docs/services-parity.md.
export const MODULAR_CLUSTER_MANAGER_PORT = 14321

export const MODULAR_OPENAI_API_BASE_PATH = '/v1'

// Log levels accepted by every backend binary's shared `applog` package
// (`--log-level` flag / `log/set-level` JSON-RPC). Order is least→most severe.
export const MODULAR_LOG_LEVELS = ['debug', 'info', 'warn', 'error'] as const
export type ModularLogLevel = (typeof MODULAR_LOG_LEVELS)[number]

export function isModularLogLevel(value: string): value is ModularLogLevel {
    return (MODULAR_LOG_LEVELS as readonly string[]).includes(value)
}

export const MODULAR_DEFAULT_LOG_LEVEL: ModularLogLevel = 'warn'

// Maximum wait for the broker's app:ready notification. A live process that
// never reports ready is treated as a stalled startup so the UI can direct the
// user to Service settings instead of loading indefinitely.
export const MODULAR_STARTUP_READY_TIMEOUT_MS = 90_000

// Awaited engine lifecycle calls can include the bundled Ollama manifest's
// 10-minute readiness probe and the broker's combined settings operation. The
// shared enginesettings/timeouts.go defines the ladder above that probe:
// 10.5 minutes for configure, 11 minutes for the owner-side
// operation, 12 for a client call, 13 for a call relayed on a peer's behalf.
// This envelope is the outermost rung and has to outlast all of them, or a
// desktop timeout would report failure for an operation still running.
export const MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS = 14 * 60_000

// Upper bound for a local model load/unload/delete action. A delete on an
// engine whose manifest declares `restart_after` (LM Studio) does not reply
// until the engine has stopped and passed its readiness probe again — LM
// Studio's `ready.timeout_s` alone is 60s — so this has to cover a full engine
// bounce, not just the action. Every wait that observes such a delete is
// derived from this so the budgets cannot drift apart; see
// docs/services-parity.md.
export const MODULAR_MODEL_ACTION_TIMEOUT_MS = 120_000

// Upper bound for `engine:catalog`. The engine manager allows its own LM Studio
// fetch 20s, and only replies once that resolves, so the RPC envelope has to sit
// outside that budget or a slow catalogue rejects here while the backend is
// still working — and the modal renders its empty state, which reads as "this
// engine has no models" rather than "the catalogue could not be read". Hugging
// Face has been observed holding the request open far longer than its nominal
// timeout, so the margin is generous.
export const MODULAR_CATALOG_CALL_TIMEOUT_MS = 30_000

// Poll interval for `cluster:invite-status` while a pairing handshake is open.
// Also drives the pending-invite reconciliation sweep, the backstop for a missed
// receiver-side `cluster:invite-canceled` / `cluster:invite-expired` push.
export const MODULAR_INVITE_STATUS_POLL_INTERVAL_MS = 1_500
