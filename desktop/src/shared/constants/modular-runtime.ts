// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The proxy port is never assumed: the broker owns ollama-proxy and reports its
// bound listener via `proxy:ready` / `proxy:get-status`. Until it does, the
// proxy port is unknown (surfaced as null) — we do not fabricate a default,
// since a wrong port misleads the user and can route requests to the wrong
// place.

// nvpair-cluster-manager pairing (EAP-NOOB) HTTP port. `cluster:invite-node` and
// the PIN handshake are driven against this port. Hardcoded pending backend
// exposure on the discovery channel; see the current client-side
// responsibilities in docs/services-parity.md.
export const MODULAR_CLUSTER_MANAGER_PORT = 14321

export const MODULAR_NODE_INFO_PATH = '/v1/node-info'

// Log levels accepted by every backend binary's shared `applog` package
// (`--log-level` flag / `log/set-level` JSON-RPC). Order is least→most severe.
export const MODULAR_LOG_LEVELS = ['debug', 'info', 'warn', 'error'] as const
export type ModularLogLevel = (typeof MODULAR_LOG_LEVELS)[number]

export function isModularLogLevel(value: string): value is ModularLogLevel {
    return (MODULAR_LOG_LEVELS as readonly string[]).includes(value)
}

export const MODULAR_DEFAULT_LOG_LEVEL: ModularLogLevel = 'warn'

// Local `/v1/node-info` HTTP poll. This is the single sanctioned HTTP exception
// for the modular bridge: the broker does not yet expose rich per-node
// telemetry, so Electron polls each discovered node's `/v1/node-info` endpoint
// for CPU/GPU/VRAM/memory metrics. Everything else flows over JSON-RPC. Healthy
// nodes use this cadence; repeated failures back off to the cap below.
export const MODULAR_NODE_INFO_POLL_INTERVAL_MS = 2_000
export const MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS = 30_000
// One attempt against one address. In the steady state that is the whole cost of
// a node's poll: the poller remembers the address that answered and asks only
// that one, so the address count multiplies nothing.
export const MODULAR_NODE_INFO_POLL_TIMEOUT_MS = 1_500
// How long a node whose every published address failed is checked at its
// top-ranked address alone before the full list is walked again. A multi-homed
// node that is simply switched off would otherwise pay a full walk on every due
// retry to learn what the last one already established, and its recovery will
// appear at the address it ranks first regardless.
export const MODULAR_NODE_INFO_WALK_COOLDOWN_MS = 10_000
// Where this machine's own node-info is asked. `nvpair-node-info` binds `:port`,
// so loopback and the advertised LAN address reach the same listener — but only
// the LAN address depends on the host's own inbound path, which puts the node's
// own metrics behind a local firewall rule and spends an inbound LAN connection
// every tick to learn what loopback already answers.
//
// `loopbackHost` in services/nvpair-node-scanner/daemon.go is the same constant
// for the same reason, reached first from a Windows firewall block on inbound to
// a node's own LAN address. macOS field logs since show the desktop poller
// reporting a host's own CPU and GPU as stale while loopback answered in a
// millisecond, so the desktop was the remaining side still asking over the LAN.
export const MODULAR_NODE_INFO_SELF_HOST = '127.0.0.1'

// Maximum wait for the broker's app:ready notification. A live process that
// never reports ready is treated as a stalled startup so the UI can direct the
// user to Service settings instead of loading indefinitely.
export const MODULAR_STARTUP_READY_TIMEOUT_MS = 15_000

// Awaited engine lifecycle calls can include the bundled Ollama manifest's
// 10-minute readiness probe (for example a running engine:set-port rebind).
// Keep the desktop JSON-RPC envelope bounded but outside that backend budget.
export const MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS = 11 * 60_000

// Upper bound for a local model load/unload/delete action. A delete on an
// engine whose manifest declares `restart_after` (LM Studio) does not reply
// until the engine has stopped and passed its readiness probe again — LM
// Studio's `ready.timeout_s` alone is 60s — so this has to cover a full engine
// bounce, not just the action. Every wait that observes such a delete is
// derived from this so the budgets cannot drift apart; see
// docs/services-parity.md.
export const MODULAR_MODEL_ACTION_TIMEOUT_MS = 120_000

// Poll interval for `cluster:invite-status` while a pairing handshake is open.
// Also drives the pending-invite reconciliation sweep, the backstop for a missed
// receiver-side `cluster:invite-canceled` / `cluster:invite-expired` push.
export const MODULAR_INVITE_STATUS_POLL_INTERVAL_MS = 1_500

// Minimum `lastSeen` gap before one node record is treated as having superseded
// another that shares its address and hostname — a machine whose appdata was
// wiped rejoins under a fresh hostUuid and would otherwise show twice.
//
// Matches `supersedeMinAge` in `services/nvpair-node-scanner/directory.go`
// (300 seconds), but note the two are no longer the same rule. There, address,
// hostname and age only NOMINATE a record: they come from an unauthenticated
// mDNS advertisement, and a healthy peer's `lastSeen` freezes exactly as a
// ghost's does, so the scanner deletes nothing until node-info names a
// different host. The bridge collapses on the nomination alone, which is why
// this stays a display-side concern — it hides a row, it does not evict from
// the directory the TUI, the CLI, and every other consumer read. Hardcoded
// pending backend exposure — see
// docs/services-parity.md#current-client-side-responsibilities
export const MODULAR_SUPERSEDE_MIN_AGE_MS = 300_000
