// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Text } from '@nvidia/foundations-react-core'
import type { ModelItem } from '@/ui/types/engine-info'
import { formatPullProgressLabel } from '@/ui/utils/formatters'
import type { EngineProgress } from '@/shared/types/engines'

export function TransientModelStatusRow({
    transientModel,
    displayName,
    pullProgress
}: {
    transientModel: ModelItem
    displayName: (name: string) => string
    pullProgress: EngineProgress | undefined
}) {
    return (
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
        </Flex>
    )
}
