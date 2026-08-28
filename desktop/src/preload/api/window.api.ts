// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type { OverviewCommand } from '@/shared/types/overview'

export interface IWindowActionApi {
    close(): Promise<void>
    /** Current native maximized state for the invoking BrowserWindow. */
    isMaximized(): Promise<boolean>
    maximize(): Promise<void>
    unmaximize(): Promise<void>
    minimize(): Promise<void>
    /** Subscribe to maximize/restore from OS (e.g. title-bar double-click). Returns unsubscribe. */
    onMaximizedChanged(callback: (maximized: boolean) => void): () => void
    openOverview(): Promise<void>
    /** Open/focus Overview and expand the given node's inline engine settings (used by the tray). */
    focusNode(nodeId: string): Promise<void>
    /** Tell main the Overview renderer has subscribed and is ready for `overview:command`. */
    overviewReady(): Promise<void>
    /** Subscribe to Overview commands pushed from main (focus-node / message). */
    onOverviewCommand(callback: (command: OverviewCommand) => void): () => void
    openExternal(url: string): Promise<void>
    quit(): Promise<void>
    resizeTray(height: number): Promise<number>
    showTrayMenu(): Promise<void>
    copyToClipboard(text: string): Promise<void>
    /** PNG/JPEG/WebP/etc. bytes as standard base64; main uses `clipboard.writeImage`. */
    copyImageToClipboard(base64: string): Promise<void>
    /** Opens a native save dialog and writes a diagnostic log bundle. */
    saveDebugLogs(): Promise<string | null>
}

export const windowActionApi: IWindowActionApi = {
    close: () => invokeAndUnwrap<void>(ipcRenderer.invoke('window:close')),
    isMaximized: () => invokeAndUnwrap<boolean>(ipcRenderer.invoke('window:is-maximized')),
    maximize: () => invokeAndUnwrap<void>(ipcRenderer.invoke('window:maximize')),
    unmaximize: () => invokeAndUnwrap<void>(ipcRenderer.invoke('window:unmaximize')),
    minimize: () => invokeAndUnwrap<void>(ipcRenderer.invoke('window:minimize')),
    onMaximizedChanged: callback => {
        const handler = (_event: Electron.IpcRendererEvent, maximized: boolean) => {
            callback(maximized)
        }
        ipcRenderer.on('window:maximized-state', handler)
        return () => ipcRenderer.removeListener('window:maximized-state', handler)
    },
    openOverview: () => invokeAndUnwrap<void>(ipcRenderer.invoke('window:open-overview')),
    focusNode: nodeId => invokeAndUnwrap<void>(ipcRenderer.invoke('window:focus-node', { nodeId })),
    overviewReady: () => invokeAndUnwrap<void>(ipcRenderer.invoke('overview:ready')),
    onOverviewCommand: callback => {
        const handler = (_event: Electron.IpcRendererEvent, command: OverviewCommand) => {
            callback(command)
        }
        ipcRenderer.on('overview:command', handler)
        return () => ipcRenderer.removeListener('overview:command', handler)
    },
    openExternal: url => invokeAndUnwrap<void>(ipcRenderer.invoke('window:open-external', url)),
    quit: () => invokeAndUnwrap<void>(ipcRenderer.invoke('window:quit')),
    resizeTray: height => invokeAndUnwrap<number>(ipcRenderer.invoke('tray:resize', height)),
    showTrayMenu: () => invokeAndUnwrap<void>(ipcRenderer.invoke('tray:show-menu')),
    copyToClipboard: text =>
        invokeAndUnwrap<void>(ipcRenderer.invoke('window:copy-to-clipboard', text)),
    copyImageToClipboard: base64 =>
        invokeAndUnwrap<void>(ipcRenderer.invoke('window:copy-image-to-clipboard', { base64 })),
    saveDebugLogs: () =>
        invokeAndUnwrap<string | null>(ipcRenderer.invoke('window:save-debug-logs'))
}
