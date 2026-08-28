// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const PULL_INFRASTRUCTURE_LOST_MESSAGE =
    'The connection to the engine service was lost while downloading a model.'

/** Broker/engine-manager subprocess loss, RPC timeout, or process down mid-pull. */
export function isPullInfrastructureError(message: string): boolean {
    const lower = message.toLowerCase()
    return (
        lower.includes('jsonrpc peer closed') ||
        lower.includes(' timed out') ||
        lower.endsWith(' is not running')
    )
}

/** Engine-manager already emitted errors:report with this copy. */
export function isBackendPullFailureMessage(message: string): boolean {
    return message.includes('experienced an error while downloading a model')
}

/**
 * Decide whether the supervisor should synthesize a pull catch error, and with
 * what message. Returns null when the backend/peer errors pipeline already owns
 * the failure.
 */
export function resolvePullCatchError(message: string): string | null {
    if (!message) return null
    if (isBackendPullFailureMessage(message)) return null
    if (isPullInfrastructureError(message)) return PULL_INFRASTRUCTURE_LOST_MESSAGE
    return message
}

/** Preserve indeterminate pull percent when the wire omits or sends 0. */
export function mergePullProgressPercent(
    framePercent: number,
    existingPercent: number | undefined
): number | undefined {
    return framePercent > 0 ? framePercent : existingPercent
}
