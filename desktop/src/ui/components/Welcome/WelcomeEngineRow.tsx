// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, Stack, Switch, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import { type EngineType } from '@/shared/types/engines'
import { EngineDefaultLinks, EngineDisplayNames } from '@/shared/constants/engines'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { engineProgressKey } from '@/shared/utils/engine-progress'

interface WelcomeEngineRowProps {
    engineType: EngineType
    nodeId: string
    checked: boolean
    installing: boolean
    /** When true, switch is off and disabled with a tooltip (engine already on disk). */
    alreadyInstalled: boolean
    onCheckedChange: (checked: boolean) => void
}

export function WelcomeEngineRow({
    engineType,
    nodeId,
    checked,
    installing,
    alreadyInstalled,
    onCheckedChange
}: WelcomeEngineRowProps) {
    const progress = useEngineProgressStore(s =>
        s.progress.get(engineProgressKey({ nodeId, engineType, operation: 'install' }))
    )
    const docs = EngineDefaultLinks[engineType]?.docsUrl
    const label = EngineDisplayNames[engineType]
    return (
        <Flex align="center" justify="between" gap="3">
            <Stack gap="1" className="min-w-0 flex-1">
                <Flex align="center" gap="1">
                    <Text kind="body/semibold/sm">{label}</Text>
                    {docs ? (
                        <Button
                            kind="tertiary"
                            size="small"
                            className="self-start px-0"
                            onClick={() => void window.windowApi.window.openExternal(docs)}
                            aria-label={`Open ${label} documentation`}
                        >
                            <OpenInNew style={{ width: 14, height: 14 }} />
                        </Button>
                    ) : null}
                </Flex>
                {installing && checked ? (
                    <Flex align="center" gap="2" className="shrink-0 whitespace-nowrap">
                        <span
                            className="spinner-element"
                            role="status"
                            aria-label=""
                            style={{ margin: '0' }}
                        />
                        <Text kind="body/regular/sm" className="text-subtle-color capitalize">
                            {progress?.status ?? 'Installing'}
                            {progress?.percent !== undefined
                                ? ` ${Math.round(progress.percent)}%`
                                : '...'}
                        </Text>
                    </Flex>
                ) : null}
            </Stack>
            {alreadyInstalled ? (
                <DismissibleTooltip slotContent="This engine is already installed on this machine.">
                    <span className="inline-flex shrink-0">
                        <Switch
                            checked={checked}
                            disabled
                            size="small"
                            aria-label={`${label} already installed`}
                        />
                    </span>
                </DismissibleTooltip>
            ) : (
                <Switch
                    checked={checked}
                    onCheckedChange={onCheckedChange}
                    disabled={installing}
                    size="small"
                    aria-label={`Install ${label}`}
                />
            )}
        </Flex>
    )
}
