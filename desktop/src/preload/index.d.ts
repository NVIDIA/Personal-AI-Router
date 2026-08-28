// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { IWindowApi } from './api/index'
import type { BootstrapPayload } from '@/shared/types/ipc-channels'

declare global {
    interface Window {
        windowApi: IWindowApi
        __PAIR_BOOTSTRAP__: BootstrapPayload
    }
}
