// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineType } from '@/shared/types/engines'

/** Base URL shown for a local inference endpoint. */
export function gatewayEndpointDisplayUrl(
    proxyPort: number,
    inferenceType: EngineType
): string | null {
    void inferenceType
    if (proxyPort <= 0) return null
    return `http://127.0.0.1:${proxyPort}`
}
