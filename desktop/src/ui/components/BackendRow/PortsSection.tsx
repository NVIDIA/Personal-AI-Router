// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Text } from '@nvidia/foundations-react-core'
import { BackendPorts } from './BackendPorts'
import { EditState } from '@/ui/types/engine-edit-state'
import type { EngineCaps } from '@/ui/types/engine-manifest'

export function PortsSection({
    edit,
    portsChanged,
    anyLoading,
    isLocalNode,
    caps,
    onApplyPorts,
    onServerChange,
    onProxyChange
}: {
    edit: EditState
    portsChanged: boolean
    anyLoading: boolean
    isLocalNode: boolean
    caps: EngineCaps
    onApplyPorts: () => void
    onServerChange: (v: string) => void
    onProxyChange: (v: string) => void
}) {
    return (
        <details className="pair-accordion translucent-bg-accordion">
            <summary className="pair-accordion-summary">
                <Text kind="body/semibold/sm">Ports</Text>
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
            </div>
        </details>
    )
}
