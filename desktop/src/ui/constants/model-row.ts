// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ModelExpiry } from '@/shared/types/engines'
import { ModelExpiries, ModelExpiryLabels } from '@/shared/constants/engines'

export const DOT_LOADED = 'var(--color-green-400)'
export const DOT_DOWNLOADED = 'var(--color-gray-400)'

export const EXPIRY_OPTIONS = ModelExpiries.map(id => ({
    id,
    children: ModelExpiryLabels[id]
}))

export const EXPIRY_SHORT_LABELS: Record<ModelExpiry, string> = {
    '0': 'immediately',
    '1s': '1s',
    '10s': '10s',
    '1m': '1m',
    '10m': '10m',
    '-1': 'never'
}
