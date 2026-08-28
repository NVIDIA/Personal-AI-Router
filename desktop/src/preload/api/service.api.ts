// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type { ServiceStatus, ServiceVersions } from '@/shared/types/ipc-channels'
import type { ModularLogLevel } from '@/shared/constants/modular-runtime'

export interface IServiceApi {
    getStatus(): Promise<ServiceStatus>
    getVersions(): Promise<ServiceVersions>
    stop(): Promise<void>
    start(): Promise<void>
    restart(): Promise<void>
    getLogLevel(): Promise<ModularLogLevel>
    setLogLevel(level: ModularLogLevel): Promise<void>
    openLogFile(): Promise<void>
    openLogDir(): Promise<void>
    openLicense(): Promise<void>
    openThirdPartyLicenses(): Promise<void>
    onStatusChanged(callback: (status: ServiceStatus) => void): () => void
}

export const serviceApi: IServiceApi = {
    getStatus: () => invokeAndUnwrap<ServiceStatus>(ipcRenderer.invoke('service:get-status')),
    getVersions: () => invokeAndUnwrap<ServiceVersions>(ipcRenderer.invoke('service:get-versions')),
    stop: () => invokeAndUnwrap<void>(ipcRenderer.invoke('service:stop')),
    start: () => invokeAndUnwrap<void>(ipcRenderer.invoke('service:start')),
    restart: () => invokeAndUnwrap<void>(ipcRenderer.invoke('service:restart')),
    getLogLevel: () =>
        invokeAndUnwrap<ModularLogLevel>(ipcRenderer.invoke('service:get-log-level')),
    setLogLevel: level =>
        invokeAndUnwrap<void>(ipcRenderer.invoke('service:set-log-level', { level })),
    openLogFile: () => invokeAndUnwrap<void>(ipcRenderer.invoke('service:open-log-file')),
    openLogDir: () => invokeAndUnwrap<void>(ipcRenderer.invoke('service:open-log-dir')),
    openLicense: () => invokeAndUnwrap<void>(ipcRenderer.invoke('service:open-license')),
    openThirdPartyLicenses: () =>
        invokeAndUnwrap<void>(ipcRenderer.invoke('service:open-third-party-licenses')),
    onStatusChanged: callback => {
        const handler = (_event: Electron.IpcRendererEvent, status: ServiceStatus) => {
            callback(status)
        }
        ipcRenderer.on('service:status', handler)
        return () => ipcRenderer.removeListener('service:status', handler)
    }
}
