// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type { DemoState } from '@/shared/types/inference-demo'

export interface IInferenceDemoApi {
    getState(): Promise<DemoState>
    /**
     * Start the node-local demo. Rejects if a demo is already running here or if
     * no local engine responded, so callers should surface the rejection as an
     * ordinary error message.
     */
    start(): Promise<DemoState>
    /** Cancel future submissions. In-flight work is left to finish. */
    stop(): Promise<DemoState>
    onStateChanged(callback: (state: DemoState) => void): () => void
}

export const inferenceDemoApi: IInferenceDemoApi = {
    getState: () => invokeAndUnwrap<DemoState>(ipcRenderer.invoke('demo:get-state')),
    start: () => invokeAndUnwrap<DemoState>(ipcRenderer.invoke('demo:start')),
    stop: () => invokeAndUnwrap<DemoState>(ipcRenderer.invoke('demo:stop')),
    onStateChanged: callback => {
        const handler = (_event: Electron.IpcRendererEvent, state: DemoState) => callback(state)
        ipcRenderer.on('demo:state', handler)
        return () => ipcRenderer.removeListener('demo:state', handler)
    }
}
