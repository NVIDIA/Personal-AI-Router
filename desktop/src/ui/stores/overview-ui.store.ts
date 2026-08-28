// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { SettingsWindowTab } from '@/ui/types/settings-window'
import type { OverviewTab } from '@/shared/types/overview'

interface OverviewUiStore {
    /** Which top-level Overview tab is showing the content area. */
    activeTab: OverviewTab
    /** Active sub-tab when the Settings content is shown. */
    settingsSubTab: SettingsWindowTab
    /** One-shot request to expand a node card's inline engine settings. */
    focusNodeId: string | null
    setActiveTab: (tab: OverviewTab) => void
    /** Switch to the Settings tab, optionally selecting a sub-tab. */
    openSettings: (subTab?: SettingsWindowTab) => void
    setSettingsSubTab: (subTab: SettingsWindowTab) => void
    /** Switch to the Overview tab and request a node card expand its engine settings. */
    focusNodeEngineSettings: (nodeId: string) => void
    /** Clear a pending focus request once a card has consumed it. */
    consumeFocusNode: () => void
}

export const useOverviewUiStore = create<OverviewUiStore>(set => ({
    activeTab: 'overview',
    settingsSubTab: 'cluster',
    focusNodeId: null,

    setActiveTab: tab => set({ activeTab: tab }),

    openSettings: subTab =>
        set(state => ({
            activeTab: 'settings',
            settingsSubTab: subTab ?? state.settingsSubTab
        })),

    setSettingsSubTab: subTab => set({ settingsSubTab: subTab }),

    focusNodeEngineSettings: nodeId => set({ activeTab: 'overview', focusNodeId: nodeId }),

    consumeFocusNode: () => set({ focusNodeId: null })
}))
