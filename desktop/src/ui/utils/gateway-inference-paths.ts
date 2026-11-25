// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { MODULAR_OPENAI_API_BASE_PATH } from '@/shared/constants/modular-runtime'

/**
 * OpenAI-compatible base URL shown and copied for a local inference endpoint.
 *
 * Every facade — a single engine's, and the cluster routing endpoint's own —
 * serves `/v1` at the root of its port, so one rule covers them all and the
 * caller only has to say which port. A port that is not bound is not shown at
 * all rather than shown with a guess.
 */
export function gatewayEndpointDisplayUrl(proxyPort: number): string | null {
    if (proxyPort <= 0) return null
    return `http://127.0.0.1:${proxyPort}${MODULAR_OPENAI_API_BASE_PATH}`
}
