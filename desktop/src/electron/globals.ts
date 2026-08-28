// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { PathProvider } from '@/electron/path'

const GLOBAL_KEY = '__pair_platform_paths__'

export function initPlatform(paths: PathProvider): void {
    globalThis[GLOBAL_KEY] = paths
}

export function getPaths(): PathProvider {
    const paths = globalThis[GLOBAL_KEY] as PathProvider | undefined
    if (!paths) throw new Error('Platform not initialized -- call initPlatform() first')
    return paths
}
