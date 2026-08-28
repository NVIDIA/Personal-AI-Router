// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { Dropdown, Flex, Text, type DropdownEntry } from '@nvidia/foundations-react-core'
import { MoreHoriz } from '@/ui/components/icons'
import { DOT_DOWNLOADED, DOT_LOADED } from '@/ui/constants/model-row'
import type { ModelRowDropdownControl } from '@/ui/types/model-row'

export function ModelRowHeader({
    modelName,
    formattedName,
    isLoaded,
    actionItems,
    actionDd,
    onAction
}: {
    modelName: string
    formattedName: string
    isLoaded: boolean
    actionItems: { id: string; children: string; disabled?: boolean; danger?: boolean }[]
    actionDd: ModelRowDropdownControl
    onAction: (modelName: string, action: string) => void
}) {
    const dropdownItems: DropdownEntry[] = useMemo(
        () =>
            actionItems.map(item => ({
                children: item.children,
                disabled: item.disabled,
                danger: item.danger,
                onSelect: () => onAction(modelName, item.id)
            })),
        [actionItems, modelName, onAction]
    )

    return (
        <Flex align="center" justify="between" gap="2" className="min-w-0 overflow-hidden">
            <Flex align="center" gap="2" className="min-w-0 overflow-hidden">
                <div
                    style={{
                        width: 8,
                        height: 8,
                        borderRadius: '50%',
                        flexShrink: 0,
                        background: isLoaded ? DOT_LOADED : DOT_DOWNLOADED
                    }}
                />
                <Text kind="body/bold/sm" className="truncate min-w-0" title={modelName}>
                    {formattedName}
                </Text>
            </Flex>

            {actionItems.length > 0 && (
                <Dropdown
                    items={dropdownItems}
                    showChevron={false}
                    size="small"
                    className="compact-dropdown shrink-0"
                    aria-label={`Open ${formattedName} actions`}
                    onOpenChange={actionDd.onOpenChange}
                    attributes={{
                        DropdownContent: {
                            className: actionDd.dropDownContentClassName
                        }
                    }}
                >
                    <MoreHoriz style={{ fontSize: 16 }} />
                </Dropdown>
            )}
        </Flex>
    )
}
