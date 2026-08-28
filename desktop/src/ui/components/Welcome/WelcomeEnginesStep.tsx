// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Fragment } from 'react'
import { Divider, Stack, Text } from '@nvidia/foundations-react-core'
import type { EngineType } from '@/shared/types/engines'
import { WelcomeEngineRow } from './WelcomeEngineRow'

interface WelcomeEnginesStepProps {
    candidates: EngineType[]
    nodeId: string
    engineSelections: Partial<Record<EngineType, boolean>> | null
    installing: boolean
    onEngineToggle: (engineType: EngineType, checked: boolean) => void
    isEngineInstalled: (engineType: EngineType) => boolean
}

export function WelcomeEnginesStep({
    candidates,
    nodeId,
    engineSelections,
    installing,
    onEngineToggle,
    isEngineInstalled
}: WelcomeEnginesStepProps) {
    return (
        <>
            <Text kind="body/regular/sm" className="text-subtle-color">
                {installing
                    ? 'Installing. Closing this dialog will not cancel it; progress continues on the node.'
                    : 'Choose which engines to install before continuing.'}
            </Text>
            <Stack gap="3" className="mb-1">
                {candidates.map((t, i) => (
                    <Fragment key={t}>
                        {i > 0 ? <Divider /> : null}
                        <WelcomeEngineRow
                            engineType={t}
                            nodeId={nodeId}
                            checked={
                                isEngineInstalled(t) ? false : (engineSelections?.[t] ?? false)
                            }
                            installing={installing}
                            alreadyInstalled={isEngineInstalled(t)}
                            onCheckedChange={v => onEngineToggle(t, v)}
                        />
                    </Fragment>
                ))}
            </Stack>
        </>
    )
}
