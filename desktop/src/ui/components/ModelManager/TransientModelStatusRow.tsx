// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, ProgressBar, Stack, Text } from '@nvidia/foundations-react-core'
import type { ModelItem } from '@/ui/types/engine-info'
import { formatPullProgressLabel } from '@/ui/utils/formatters'
import type { EngineProgress } from '@/shared/types/engines'

export function TransientModelStatusRow({
    transientModel,
    displayName,
    pullProgress,
    onCancel
}: {
    transientModel: ModelItem
    displayName: (name: string) => string
    pullProgress: EngineProgress | undefined
    onCancel: () => void
}) {
    const pulling = transientModel.status === 'pulling'
    const canceling = pullProgress?.status === 'canceling'
    const percent = pullProgress?.percent
    return (
        <Stack gap="2">
            <Flex align="center" justify="between" gap="2" className="mt-2 min-w-0">
                <Flex align="center" gap="1" className="min-w-0">
                    <span
                        className="spinner-element"
                        role="status"
                        aria-label=""
                        style={{ margin: 0 }}
                    />
                    <Text kind="body/semibold/sm" className="truncate">
                        {transientModel.status === 'loading' &&
                            `Loading ${displayName(transientModel.name)}...`}
                        {transientModel.status === 'ejecting' &&
                            `Ejecting ${displayName(transientModel.name)}...`}
                        {transientModel.status === 'pulling' &&
                            `Pulling ${displayName(transientModel.name)}${
                                pullProgress && pullProgress.status !== 'idle'
                                    ? ` · ${formatPullProgressLabel(pullProgress)}`
                                    : '...'
                            }`}
                    </Text>
                </Flex>
                {pulling && (
                    <Button
                        size="small"
                        kind="secondary"
                        disabled={
                            canceling ||
                            !pullProgress ||
                            ['idle', 'error'].includes(pullProgress.status)
                        }
                        onClick={onCancel}
                    >
                        {canceling ? 'Canceling…' : 'Cancel download'}
                    </Button>
                )}
            </Flex>
            {pulling && (
                <ProgressBar
                    aria-label={`Downloading ${displayName(transientModel.name)}`}
                    size="small"
                    {...(percent === undefined || percent < 0
                        ? { kind: 'indeterminate' as const }
                        : { kind: 'determinate' as const, value: percent })}
                />
            )}
        </Stack>
    )
}
