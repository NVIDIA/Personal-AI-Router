// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Mirrors the backend's `nvpair-shared/errors` action enum (currently
// placeholder values `retry|none|dismiss`). `retry` is the only one the UI
// renders an affordance for; the failed op is re-run from the engineType /
// operation / modelName fields below.
export type ServiceErrorAction = 'retry' | 'none' | 'dismiss'

export type ServiceErrorSeverity = 'error' | 'warning' | 'info'

export interface ServiceError {
    id: string
    message: string
    timestamp: number
    severity?: ServiceErrorSeverity
    action?: ServiceErrorAction
    nodeId?: string
    engineType?: string
    operation?: string
    modelName?: string
}
