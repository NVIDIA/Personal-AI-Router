// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo, useMemo } from 'react'
import { Divider, Stack } from '@nvidia/foundations-react-core'
import { useDropdownClickable } from '@/ui/hooks/useDropdownClickable'
import { ModelRowHeader } from './ModelRowHeader'
import { ModelRowSecondaryRow } from './ModelRowSecondaryRow'
import type { ModelRowProps } from '@/ui/types/model-row'
import type { EngineCommandType } from '@/shared/types/engine-api'

/** "Working" label shown on the secondary row while an optimistic action is in flight. */
function pendingLabel(action: EngineCommandType): string | null {
    switch (action) {
        case 'loadModel':
            return 'Loading...'
        case 'unloadModel':
            return 'Ejecting...'
        case 'deleteModel':
            return 'Deleting...'
        case 'pullModel':
            return 'Starting download...'
        default:
            return null
    }
}

function ModelRowInner({
    model,
    isRunning,
    capabilities,
    progress,
    pendingAction,
    displayName,
    onAction,
    onExpiryChange
}: ModelRowProps) {
    const dropdown = useDropdownClickable()
    const expiryDd = dropdown.get(`expiry-${model.name}`)
    const actionDd = dropdown.get(`action-${model.name}`)

    const isLoaded = model.status === 'loaded'

    const actionItems = useMemo(() => {
        const items: { id: string; children: string; disabled?: boolean; danger?: boolean }[] = []
        // A command is already in flight for this model -- lock the whole menu so
        // the user cannot fire a second conflicting op before it resolves.
        const busy = pendingAction !== undefined
        items.push({
            id: 'load',
            children: 'Load',
            disabled: busy || isLoaded || !model.downloaded || !isRunning
        })
        if (capabilities.hasEject) {
            items.push({
                id: 'eject',
                children: 'Eject',
                // The backend reports per-model loaded state and stamps
                // `status: 'loaded'`, so Eject is offered only for a model that
                // is actually resident in memory — ejecting an idle model would
                // be a no-op.
                disabled: busy || !isRunning || !isLoaded
            })
        }
        if (capabilities.hasDeleteModel) {
            items.push({ id: 'delete', children: 'Delete', danger: true, disabled: busy })
        }
        return items
    }, [
        capabilities.hasEject,
        capabilities.hasDeleteModel,
        isLoaded,
        isRunning,
        model.downloaded,
        pendingAction
    ])

    const formattedName = displayName(model.name)
    // Pull already renders its own progress spinner via `progress`; only fall
    // back to the generic busy label for the actions the backend gives no
    // progress signal for (load/eject/delete) so we never double up.
    const busyLabel = pendingAction ? pendingLabel(pendingAction) : null

    return (
        <Stack gap="3" className="min-w-0">
            <Stack gap="0" className="grow min-w-0 mt-1">
                <ModelRowHeader
                    modelName={model.name}
                    formattedName={formattedName}
                    isLoaded={isLoaded}
                    actionItems={actionItems}
                    actionDd={actionDd}
                    onAction={onAction}
                />
                <ModelRowSecondaryRow
                    model={model}
                    progress={progress}
                    busyLabel={busyLabel}
                    hasExpiry={capabilities.hasExpiry}
                    expiryDd={expiryDd}
                    onExpiryChange={onExpiryChange}
                />
            </Stack>
            <Divider style={{ borderColor: '#ffffff22' }} />
        </Stack>
    )
}

const ModelRow = memo(ModelRowInner)
export default ModelRow
