// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { resolve } from 'path'

const ROOT = resolve(__dirname, '..')

export const srcAlias = {
    find: /^@\//,
    replacement: resolve(ROOT, 'src').replace(/\\/g, '/') + '/'
}
