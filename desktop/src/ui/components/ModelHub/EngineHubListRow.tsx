// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo, useCallback, useMemo } from 'react'
import { Button, Checkbox, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import { ModelEntry } from '@/ui/types/model-hub'
import { formatModelName } from '@/ui/utils/format-model-name'
import { formatBytes, formatRelativeTime } from '@/ui/utils/formatters'

/** List row when browsing an engine-native public catalog (e.g. Ollama library). */
const EngineHubListRow = memo(function EngineHubListRow({
    disabled,
    model,
    checked,
    onToggleSelectModel,
    multiple
}: {
    disabled: boolean
    model: ModelEntry
    checked: boolean
    onToggleSelectModel: (modelId: string) => void
    multiple: boolean
}) {
    const onCheckedChange = useCallback(() => {
        if (disabled) return
        onToggleSelectModel(model.id)
    }, [disabled, model.id, onToggleSelectModel])

    const displayName = useMemo(
        () => formatModelName(model.name, model.author),
        [model.name, model.author]
    )

    const openExternal = useCallback((evt: React.MouseEvent, url: string) => {
        evt.preventDefault()
        evt.stopPropagation()
        window.open(url, '_blank')
    }, [])

    return (
        <Stack
            gap="0"
            className="px-2 py-2 rounded-md cursor-pointer hover:bg-white/4"
            onClick={onCheckedChange}
        >
            <Flex align="start" gap="3">
                {!!multiple && (
                    <Checkbox
                        checked={checked}
                        onCheckedChange={onCheckedChange}
                        onClick={event => event.stopPropagation()}
                        className="mt-0.5"
                        aria-label={`Select ${displayName}`}
                    />
                )}
                <Stack>
                    <Text kind="body/bold/sm">{displayName}</Text>
                    <Flex align="center" wrap="wrap" gap="2">
                        {!!model.size && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {formatBytes(model.size)}
                            </Text>
                        )}

                        {!!model.parameterSize && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {model.parameterSize}
                            </Text>
                        )}

                        {!!model.family && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {model.family}
                            </Text>
                        )}

                        <Text
                            kind="body/regular/sm"
                            className="text-subtle-color whitespace-nowrap"
                        >
                            Updated {formatRelativeTime(new Date(model.updatedAt).getTime())}
                        </Text>

                        {!!model.url && (
                            <Button
                                kind="tertiary"
                                size="small"
                                className="px-1 py-0.5 -mt-1"
                                onClick={e => openExternal(e, model.url)}
                                aria-label={`Open ${displayName}`}
                            >
                                <OpenInNew style={{ fontSize: 12 }} className="text-subtle-color" />
                            </Button>
                        )}
                    </Flex>
                </Stack>
            </Flex>
        </Stack>
    )
})

export default EngineHubListRow
