// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { SettingsWindowTab } from '@/ui/types/settings-window'
import type { OverviewTab } from '@/shared/types/overview'
import { useErrorsStore } from '@/ui/stores/errors.store'

interface OverviewUiStore {
    /** Which top-level Overview tab is showing the content area. */
    activeTab: OverviewTab
    /** Active sub-tab when the Settings content is shown. */
    settingsSubTab: SettingsWindowTab
    /** One-shot request to expand a node card's inline engine settings. */
    focusNodeId: string | null
    /** Turn-on/install interaction waiting to offer vLLM model selection. */
    modelSelectionNodeId: string | null
    /** Error identities at the start of setup, retained across tab navigation. */
    modelSelectionErrorBaseline: ReadonlyMap<string, number>
    setActiveTab: (tab: OverviewTab) => void
    /** Switch to the Settings tab, optionally selecting a sub-tab. */
    openSettings: (subTab?: SettingsWindowTab) => void
    setSettingsSubTab: (subTab: SettingsWindowTab) => void
    /** Switch to the Overview tab and request a node card expand its engine settings. */
    focusNodeEngineSettings: (nodeId: string) => void
    /** Clear a pending focus request once a card has consumed it. */
    consumeFocusNode: () => void
    requestModelSelection: (nodeId: string) => void
    dismissModelSelection: () => void
}

export const useOverviewUiStore = create<OverviewUiStore>(set => ({
    activeTab: 'overview',
    settingsSubTab: 'cluster',
    focusNodeId: null,
    modelSelectionNodeId: null,
    modelSelectionErrorBaseline: new Map(),

    setActiveTab: tab => set({ activeTab: tab }),

    openSettings: subTab =>
        set(state => ({
            activeTab: 'settings',
            settingsSubTab: subTab ?? state.settingsSubTab
        })),

    setSettingsSubTab: subTab => set({ settingsSubTab: subTab }),

    focusNodeEngineSettings: nodeId => set({ activeTab: 'overview', focusNodeId: nodeId }),

    consumeFocusNode: () => set({ focusNodeId: null }),

    requestModelSelection: nodeId =>
        set({
            modelSelectionNodeId: nodeId,
            modelSelectionErrorBaseline: new Map(
                useErrorsStore.getState().errors.map(error => [error.id, error.timestamp])
            )
        }),

    dismissModelSelection: () => set({ modelSelectionNodeId: null })
}))
