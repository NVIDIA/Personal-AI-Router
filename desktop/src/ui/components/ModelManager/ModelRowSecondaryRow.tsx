// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { Dropdown, Flex, Text, type DropdownEntry } from '@nvidia/foundations-react-core'
import type { ModelItem } from '@/ui/types/engine-info'
import type { ModelExpiry } from '@/shared/types/engines'
import { isModelExpiry } from '@/shared/utils/engines'
import { formatBytes, formatPullProgressLabel } from '@/ui/utils/formatters'
import type { EngineProgress } from '@/shared/types/engines'
import { isEnginePullInProgress } from '@/shared/utils/engine-progress'
import { EXPIRY_OPTIONS, EXPIRY_SHORT_LABELS } from '@/ui/constants/model-row'
import type { ModelRowDropdownControl } from '@/ui/types/model-row'

export function ModelRowSecondaryRow({
    model,
    progress,
    busyLabel,
    hasExpiry,
    expiryDd,
    onExpiryChange
}: {
    model: ModelItem
    progress?: EngineProgress
    /** Optimistic "working" label (load/eject/delete in flight) shown with a spinner. */
    busyLabel?: string | null
    hasExpiry: boolean
    expiryDd: ModelRowDropdownControl
    onExpiryChange: (modelName: string, value: ModelExpiry) => void
}) {
    const expiryItems: DropdownEntry[] = useMemo(
        () =>
            EXPIRY_OPTIONS.map(item => ({
                children: item.children,
                onSelect: () => {
                    if (isModelExpiry(item.id)) onExpiryChange(model.name, item.id)
                }
            })),
        [model.name, onExpiryChange]
    )

    return (
        <Flex align="center" justify="between" gap="2" className="min-w-0 overflow-hidden ml-4">
            {progress && isEnginePullInProgress(progress) ? (
                <Flex align="center" className="min-w-0 shrink">
                    <Flex align="center" gap="2" className="shrink-0 whitespace-nowrap">
                        <span
                            className="spinner-element"
                            role="status"
                            aria-label=""
                            style={{ margin: '0' }}
                        />
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {formatPullProgressLabel(progress)}
                        </Text>
                    </Flex>
                </Flex>
            ) : busyLabel ? (
                <Flex align="center" className="min-w-0 shrink">
                    <Flex align="center" gap="2" className="shrink-0 whitespace-nowrap">
                        <span
                            className="spinner-element"
                            role="status"
                            aria-label=""
                            style={{ margin: '0' }}
                        />
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {busyLabel}
                        </Text>
                    </Flex>
                </Flex>
            ) : model.size > 0 ? (
                <Text
                    kind="body/regular/sm"
                    className="shrink-0 text-subtle-color whitespace-nowrap"
                >
                    {formatBytes(model.size)}
                </Text>
            ) : (
                <span />
            )}
            <Flex align="center" gap="1" className="shrink-0">
                {hasExpiry && (
                    <Dropdown
                        items={expiryItems}
                        size="small"
                        className="compact-dropdown"
                        onOpenChange={expiryDd.onOpenChange}
                        attributes={{
                            DropdownContent: {
                                className: expiryDd.dropDownContentClassName
                            }
                        }}
                    >
                        <Text
                            kind="body/regular/sm"
                            className="text-subtle-color truncate max-w-[7rem]"
                        >
                            {`Expires ${EXPIRY_SHORT_LABELS[model.expiry ?? '10m']}`}
                        </Text>
                    </Dropdown>
                )}
            </Flex>
        </Flex>
    )
}
