// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Validation and CLI-arg formatting for the "Proxy response timeout" setting
 * (persisted as minutes, passed to the broker as `--proxy-response-timeout`).
 *
 * The value is entered and stored as minutes rather than a raw Go duration
 * string (e.g. "5m30s") so the UI never has to parse or round-trip Go's
 * duration grammar — a plain non-negative number is unambiguous, and 0 maps
 * directly onto the broker/proxy's own "0 disables the timeout" convention
 * (see `services/nvpair-ui-broker/main.go`, `services/ollama-proxy/main.go`).
 */

/** Non-negative, finite minutes — `0` means "wait indefinitely". */
export function isValidProxyResponseTimeoutMinutes(value: number): boolean {
    return Number.isFinite(value) && value >= 0
}

/**
 * Render minutes as the Go duration string the broker's `--proxy-response-
 * timeout` flag (a `time.Duration`) expects, e.g. `5` -> `"5m"`, `0` -> `"0m"`.
 * Go's `time.ParseDuration` accepts fractional values with a unit (`"2.5m"`),
 * so this is safe for non-integer minutes too.
 */
export function proxyResponseTimeoutArg(minutes: number): string {
    return `${minutes}m`
}
