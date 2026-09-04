// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Text } from '@nvidia/foundations-react-core'
import { BackendPorts } from './BackendPorts'
import { ServedModelRow } from './ServedModelRow'
import { EditState } from '@/ui/types/engine-edit-state'
import type { EngineCaps } from '@/ui/types/engine-manifest'

export function PortsSection({
    edit,
    portsChanged,
    servedModelChanged,
    anyLoading,
    isLocalNode,
    caps,
    onApplyPorts,
    onApplyServedModel,
    onServerChange,
    onProxyChange,
    onServedModelChange
}: {
    edit: EditState
    portsChanged: boolean
    servedModelChanged: boolean
    anyLoading: boolean
    isLocalNode: boolean
    caps: EngineCaps
    onApplyPorts: () => void
    onApplyServedModel: () => void
    onServerChange: (v: string) => void
    onProxyChange: (v: string) => void
    onServedModelChange: (v: string) => void
}) {
    return (
        <details className="pair-accordion translucent-bg-accordion">
            <summary className="pair-accordion-summary">
                <Text kind="body/semibold/sm">
                    {caps.hasServedModel ? 'Ports and model' : 'Ports'}
                </Text>
            </summary>
            <div className="p-3">
                <BackendPorts
                    proxyPort={edit.proxyPort}
                    serverPort={edit.serverPort}
                    changed={portsChanged}
                    disabled={anyLoading}
                    isLocalNode={isLocalNode}
                    showServerPort={caps.hasEnginePort}
                    onServerChange={onServerChange}
                    onProxyChange={onProxyChange}
                    onApply={onApplyPorts}
                />
                {/* An engine that serves one model per process is told which model
                    before it starts, so the choice belongs with the other
                    start-time settings rather than in the model list. */}
                {caps.hasServedModel && (
                    <ServedModelRow
                        model={edit.servedModel}
                        changed={servedModelChanged}
                        disabled={anyLoading}
                        isLocalNode={isLocalNode}
                        onChange={onServedModelChange}
                        onApply={onApplyServedModel}
                    />
                )}
            </div>
        </details>
    )
}
