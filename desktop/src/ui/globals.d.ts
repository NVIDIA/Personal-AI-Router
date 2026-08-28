// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/// <reference types="vite/client" />

import { IWindowApi } from '@/preload/api'
import { IPairApi } from '@/ui/api/pair-api'
import type { PreloadServiceTransport } from '@/shared/types/service-bridge'

declare global {
    interface Window {
        windowApi: IWindowApi
        pairApi: IPairApi
        __PAIR_TRANSPORT__?: PreloadServiceTransport
    }
}

export {}
