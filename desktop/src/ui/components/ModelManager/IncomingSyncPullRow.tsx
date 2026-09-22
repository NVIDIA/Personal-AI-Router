// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, ProgressBar, Stack, Text } from '@nvidia/foundations-react-core'
import { formatPullProgressLabel } from '@/ui/utils/formatters'
import type { IncomingSyncRow } from '@/ui/types/model-manager'

export function IncomingSyncPullRow({
    row,
    onCancel
}: {
    row: IncomingSyncRow
    onCancel: () => void
}) {
    const canceling = row.status === 'canceling'
    return (
        <Stack gap="2">
            <Flex
                align="center"
                justify="between"
                gap="2"
                className="mt-2 min-w-0"
                title={row.rawModel}
            >
                <Flex align="center" gap="2" className="min-w-0">
                    <span
                        className="spinner-element"
                        role="status"
                        aria-label=""
                        style={{ margin: 0 }}
                    />
                    <Flex align="center" wrap="wrap" gap="1" className="min-w-0">
                        <Text kind="body/semibold/sm" className="truncate">
                            {row.label}
                        </Text>
                        <Text
                            kind="body/regular/sm"
                            className="text-subtle-color whitespace-nowrap italic capitalize"
                        >
                            {formatPullProgressLabel(row)}
                        </Text>
                    </Flex>
                </Flex>
                <Button
                    size="small"
                    kind="secondary"
                    disabled={canceling || ['idle', 'error'].includes(row.status)}
                    onClick={onCancel}
                >
                    {canceling ? 'Canceling…' : 'Cancel download'}
                </Button>
            </Flex>
            <ProgressBar
                aria-label={`Downloading ${row.label}`}
                size="small"
                {...(row.percent === undefined || row.percent < 0
                    ? { kind: 'indeterminate' as const }
                    : { kind: 'determinate' as const, value: row.percent })}
            />
        </Stack>
    )
}
