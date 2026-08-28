// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type { UpdateStatus } from '@/shared/types/update'

export interface IUpdateApi {
    getStatus(): Promise<UpdateStatus>
    check(): Promise<void>
    download(): Promise<void>
    install(): Promise<void>
    onStatusChanged(callback: (status: UpdateStatus) => void): () => void
}

export const updateApi: IUpdateApi = {
    getStatus: () => invokeAndUnwrap<UpdateStatus>(ipcRenderer.invoke('update:get-status')),
    check: () => invokeAndUnwrap<void>(ipcRenderer.invoke('update:check')),
    download: () => invokeAndUnwrap<void>(ipcRenderer.invoke('update:download')),
    install: () => invokeAndUnwrap<void>(ipcRenderer.invoke('update:install')),
    onStatusChanged: callback => {
        const handler = (_event: Electron.IpcRendererEvent, status: UpdateStatus) => {
            callback(status)
        }
        ipcRenderer.on('update:status', handler)
        return () => ipcRenderer.removeListener('update:status', handler)
    }
}
