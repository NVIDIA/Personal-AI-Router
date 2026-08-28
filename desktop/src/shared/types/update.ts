// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

type UpdatePhase =
    | 'idle'
    | 'checking'
    | 'available'
    | 'not-available'
    | 'downloading'
    | 'downloaded'
    | 'error'

export interface UpdateStatus {
    phase: UpdatePhase
    currentVersion: string
    latestVersion: string | null
    downloadPercent: number | null
    error: string | null
}
