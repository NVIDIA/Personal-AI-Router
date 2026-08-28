// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { PlatformMap } from '@/shared/constants/platform'

export type SupportedPlatform = keyof typeof PlatformMap
export type PlatformDisplayName = (typeof PlatformMap)[SupportedPlatform]
