// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo, useCallback, useMemo } from 'react'
import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { LocalBadge } from '@/ui/components/LocalBadge'
import type { NodeItem } from '@/shared/types/nodes'
import type { Workload } from '@/shared/types/workloads'
import { workloadExecutionNodeId } from '@/shared/utils/workloads'
import { engineStatusesToListRows, getEnginesForNode } from '@/ui/utils/get-engines-for-node'
import { EnabledEngineTypes } from '@/shared/constants/engines'
import { getGpuColor } from '@/ui/utils/colors'
import { CHART_COLORS } from '@/ui/constants/colors'
import GpuRadialChartSvg from '@/ui/components/GpuRadialChartSvg'
import NodeActive from '@/ui/components/NodeActive/NodeActive'
import { hasFirstMetrics } from '@/ui/utils/has-first-metrics'
import { buildRadialChartMetrics } from '@/ui/utils/build-radial-chart-metrics'
import { hasInferenceReadyGpu } from '@/ui/utils/gpu-inference'

const CHART_SIZE = 48

const gridStyle = {
    gridTemplateColumns: `${CHART_SIZE}px 1fr`,
    borderBottom: '1px solid var(--color-translucent-white-100)'
}

function TrayNodeRow({ node, activeWorkloads }: { node: NodeItem; activeWorkloads: Workload[] }) {
    const nodeMetrics = useMetricsStore(state => state.nodeMetrics.get(node.id))
    const isLocal = useConnectionStore(state => state.selfId === node.id)
    const statusByNode = useEngineStatusStore(s => s.statusByNode)
    const engineRows = useMemo(
        () =>
            engineStatusesToListRows(getEnginesForNode(node.id, statusByNode, EnabledEngineTypes)),
        [node.id, statusByNode]
    )
    // Treat only inference-ready GPUs as "has GPU" per the backend readiness
    // list (or any GPU when the backend sends no list); see gpu-inference.ts.
    // A node with none gets the CPU + RAM ring.
    const hasGpu = hasInferenceReadyGpu(node.topology)
    const baseColor = hasGpu ? getGpuColor(0) : CHART_COLORS.CPU

    const chartMetrics = useMemo(
        () => buildRadialChartMetrics(node.topology, nodeMetrics, hasGpu),
        [hasGpu, node.topology, nodeMetrics]
    )

    const isSingleEngine = EnabledEngineTypes.length === 1
    const singleEngineRunning = useMemo(() => {
        if (!isSingleEngine) return false
        return engineRows[0]?.processStatus === 'running'
    }, [isSingleEngine, engineRows])
    const servicesLabel = useMemo(() => {
        if (isSingleEngine) return engineRows[0]?.displayName ?? ''
        const running = engineRows.filter(b => b.processStatus === 'running').length
        return `Engines (${running}/${engineRows.length})`
    }, [engineRows, isSingleEngine])

    const jobCount = useMemo(
        () => activeWorkloads.filter(w => workloadExecutionNodeId(w) === node.id).length,
        [activeWorkloads, node.id]
    )

    const openNodeSettings = useCallback(() => {
        window.windowApi.window.focusNode(node.id)
    }, [node.id])

    return (
        <div
            className={`px-4 py-3 items-center overflow-hidden cursor-pointer`}
            style={{
                ...gridStyle,
                display: 'grid',
                gap: 'calc(var(--spacing)*3)'
            }}
            onClick={openNodeSettings}
        >
            <div className="flex items-center justify-center">
                <div className="relative">
                    {hasFirstMetrics(node, nodeMetrics) ? (
                        <>
                            <NodeActive show={jobCount > 0} color={baseColor} />
                            <GpuRadialChartSvg
                                metrics={chartMetrics}
                                size={CHART_SIZE}
                                isActive={`0 0 2px 1px ${baseColor}88`}
                                cutout={0.2}
                                ringSpacing={2}
                                valueCornerRadius={0}
                            />
                        </>
                    ) : (
                        <div aria-hidden style={{ width: CHART_SIZE, height: CHART_SIZE }} />
                    )}
                </div>
            </div>

            <div className="min-w-0 pb-1">
                <Stack>
                    <Flex align="center" gap="2" className="min-w-0">
                        <Text kind="body/bold/sm" className="uppercase truncate">
                            {(node.name || node.ipAddress || '')?.trim().toUpperCase()}
                        </Text>
                        {isLocal && <LocalBadge />}
                    </Flex>
                    <Flex align="center" gap="1">
                        {isSingleEngine && (
                            <span
                                className="inline-block w-1.5 h-1.5 rounded-full"
                                style={{
                                    backgroundColor: singleEngineRunning ? '#76b900' : '#666'
                                }}
                            />
                        )}
                        <Text kind="body/regular/xs" className="text-subtle-color">
                            {servicesLabel}
                        </Text>
                    </Flex>
                    <Text kind="body/regular/xs" className="text-subtle-color">
                        Jobs ({jobCount})
                    </Text>
                </Stack>
            </div>
        </div>
    )
}

export default memo(TrayNodeRow)
