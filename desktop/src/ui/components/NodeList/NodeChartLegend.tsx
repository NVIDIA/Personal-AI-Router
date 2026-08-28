// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo } from 'react'
import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { CpuFallbackInfo, GpuInfo } from '@/ui/types/node-hardware'

const dotSize = '10px'

function ColorDot({ color }: { color: string }) {
    return (
        <div
            style={{
                width: dotSize,
                height: dotSize,
                borderRadius: '50%',
                backgroundColor: `${color}AA`,
                border: `2px solid ${color}`
            }}
        />
    )
}

function NodeChartLegend({
    gpuInfo,
    cpuFallbackInfo
}: {
    gpuInfo: GpuInfo[]
    cpuFallbackInfo?: CpuFallbackInfo | null
}) {
    if (cpuFallbackInfo) {
        return (
            <Stack gap="1">
                <Text kind="body/semibold/sm">{cpuFallbackInfo.model}</Text>
                <Flex align="center" gap="2">
                    <ColorDot color={cpuFallbackInfo.usageColor} />
                    <Flex align="center" gap="2">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            CPU
                        </Text>
                        <Text kind="body/semibold/sm">{cpuFallbackInfo.usage}%</Text>
                    </Flex>
                </Flex>
                <Flex align="center" gap="2">
                    <ColorDot color={cpuFallbackInfo.memoryColor} />
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        RAM
                    </Text>
                    <Text kind="body/semibold/sm">
                        {cpuFallbackInfo.memoryUsageFormatted} /{' '}
                        {cpuFallbackInfo.memoryTotalFormatted}
                    </Text>
                </Flex>
            </Stack>
        )
    }

    return (
        <Stack gap="3">
            {gpuInfo.map(gpu => (
                <Stack key={gpu.id} gap="1">
                    <Text kind="body/semibold/sm">{gpu.name}</Text>
                    <Flex align="center" gap="2">
                        <ColorDot color={gpu.usageColor} />
                        <Flex align="center" gap="2">
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Usage
                            </Text>
                            <Text kind="body/semibold/sm">{gpu.usage}%</Text>
                        </Flex>
                    </Flex>
                    <Flex align="center" gap="2">
                        <ColorDot color={gpu.vramColor} />
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            VRAM
                        </Text>
                        <Text kind="body/semibold/sm">
                            {gpu.vramUsageFormatted} / {gpu.vramTotalFormatted}
                        </Text>
                    </Flex>
                </Stack>
            ))}
        </Stack>
    )
}

export default memo(NodeChartLegend)
