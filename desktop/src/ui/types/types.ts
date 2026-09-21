// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface PerformanceMetric {
    timestamp: number
    value: number
}

export type JobsFilterType = 'active' | 'completed' | 'failed' | 'cancelled'

export type BadgeColor = 'yellow' | 'blue' | 'green' | 'gray' | 'red' | 'teal' | 'purple'
