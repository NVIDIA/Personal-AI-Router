// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { IpcResult } from '@/shared/types/ipc-channels'

function unwrapIpc<T>(result: IpcResult<T>): T {
    if (!result.success) throw new Error(result.error)
    return result.data
}

/**
 * Type-safe invoke + unwrap helper.
 * Callers pass `ipcRenderer.invoke(channel, ...)` which Electron types as `Promise<any>`.
 * We accept `Promise<IpcResult<T>>` so the Electron→typed boundary is at each call site
 * rather than hidden inside this function.
 */
export function invokeAndUnwrap<T>(promise: Promise<IpcResult<T>>): Promise<T> {
    return promise.then(unwrapIpc)
}
