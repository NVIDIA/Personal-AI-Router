// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Button, Flex, Stack } from '@nvidia/foundations-react-core'
import { BarChartOutlined, SettingsOutlined } from '@/ui/components/icons'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import type { NodeItem } from '@/shared/types/nodes'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import { useNodeActiveJobCount } from '@/ui/stores/workloads.store'
import { getGpuColor, getVramColor } from '@/ui/utils/colors'
import GpuRadialChartSvg from '@/ui/components/GpuRadialChartSvg'
import NodeLabel from './NodeLabel'
import NodeActive from '@/ui/components/NodeActive/NodeActive'
import { hasFirstMetrics } from '@/ui/utils/has-first-metrics'
import type { CpuFallbackInfo, GpuInfo } from '@/ui/types/node-hardware'
import { formatBytes } from '@/ui/utils/formatters'
import { CHART_COLORS } from '@/ui/constants/colors'
import NodePerformance from './NodePerformance'
import NodeChartLegend from './NodeChartLegend'
import { buildRadialChartMetrics } from '@/ui/utils/build-radial-chart-metrics'
import { hasInferenceReadyGpu, isGpuInferenceReady } from '@/ui/utils/gpu-inference'
import NodeEnginesInline from './NodeEnginesInline'
import NodeEngineSettings from './NodeEngineSettings'

const MAX_CHART_SIZE = 60

interface NodeCardDetailsProps {
    node: NodeItem
}

function NodeCardDetails({ node }: NodeCardDetailsProps) {
    const nodeMetrics = useMetricsStore(state => state.nodeMetrics.get(node.id))
    const isLocal = useConnectionStore(state => state.selfId === node.id)
    const [isExpanded, setIsExpanded] = useState(false)
    const [isEngineSettingsExpanded, setIsEngineSettingsExpanded] = useState(false)
    // Mount the heavy engine editor only after the first expand so N node cards
    // don't each render a full BackendRow stack while collapsed.
    const [hasOpenedEngineSettings, setHasOpenedEngineSettings] = useState(false)
    // The engine cards cast a 15px drop shadow that the accordion's
    // overflow-hidden wrapper (needed to animate height) would clip. Flip the
    // wrapper to overflow-visible only once the open transition settles.
    const [engineSettingsOpenSettled, setEngineSettingsOpenSettled] = useState(false)
    const focusNodeId = useOverviewUiStore(state => state.focusNodeId)
    const consumeFocusNode = useOverviewUiStore(state => state.consumeFocusNode)
    // "Has a GPU" means "has an inference-ready GPU" per the backend readiness
    // list (or any GPU when the backend sends no list); see gpu-inference.ts.
    // A node with none renders the CPU + RAM fallback instead of a GPU ring.
    const hasGpu = hasInferenceReadyGpu(node.topology)
    const baseColor = hasGpu ? getGpuColor(0) : CHART_COLORS.CPU
    const containerRef = useRef<HTMLDivElement>(null)
    const [chartSize, setChartSize] = useState(MAX_CHART_SIZE)
    // Below this card width the expand buttons drop from a right-hand column to
    // a third row under the engine toggles so the node info isn't squeezed.
    const [isNarrow, setIsNarrow] = useState(false)
    const resizeRafRef = useRef(0)

    const chartMetrics = useMemo(
        () => buildRadialChartMetrics(node.topology, nodeMetrics, hasGpu),
        [hasGpu, node.topology, nodeMetrics]
    )

    const metricsReady = hasFirstMetrics(node, nodeMetrics)

    const getLatestValue = (data: { timestamp: number; value: number }[]) => {
        if (data.length === 0) return 0
        return data[data.length - 1].value ?? 0
    }

    const gpuInfo: GpuInfo[] = useMemo(() => {
        if (!hasGpu) return []

        // Keep the original index (metrics/colors are positional to
        // topology.gpus) but drop any GPU the backend did not mark
        // inference-ready so the legend mirrors the radial rings exactly.
        return node.topology.gpus.flatMap((gpu, index) => {
            if (!isGpuInferenceReady(gpu, node.topology.inferenceHardwareIds)) return []
            const gpuUtilData = nodeMetrics?.gpuUtilization[index]
            const gpuVramData = nodeMetrics?.gpuVramUsage[index]

            return [
                {
                    id: gpu.id,
                    name: gpu.name,
                    vramTotal: gpu.vramTotal,
                    vramTotalFormatted: formatBytes(gpu.vramTotal, 1),
                    vramUsageFormatted: formatBytes(
                        (gpu.vramTotal * (gpuVramData ? getLatestValue(gpuVramData.data) : 0)) /
                            100,
                        1
                    ),
                    vramUsage: gpuVramData ? getLatestValue(gpuVramData.data) : 0,
                    usage: gpuUtilData ? getLatestValue(gpuUtilData.data) : 0,
                    usageColor: getGpuColor(index),
                    vramColor: getVramColor(index)
                }
            ]
        })
    }, [hasGpu, node, nodeMetrics])

    // Memoized so the label's segment list keeps a stable identity across
    // renders; NodeLabel observes its segments and would otherwise rebuild the
    // ResizeObserver every render.
    const gpuLabels = useMemo(() => gpuInfo.map(gpu => gpu.name), [gpuInfo])

    const cpuFallbackInfo: CpuFallbackInfo | null = useMemo(() => {
        if (hasGpu) return null
        const ramTotal = node.topology.ram
        const cpuUsage = nodeMetrics ? getLatestValue(nodeMetrics.cpuUtilization) : 0
        const memUsage = nodeMetrics ? getLatestValue(nodeMetrics.memoryUsage) : 0
        return {
            model: node.topology.cpu.model,
            usage: Math.round(cpuUsage),
            usageColor: CHART_COLORS.CPU,
            memoryUsage: memUsage,
            memoryUsageFormatted: formatBytes((ramTotal * memUsage) / 100, 1),
            memoryTotalFormatted: formatBytes(ramTotal, 1),
            memoryColor: CHART_COLORS.MEMORY
        }
    }, [hasGpu, node, nodeMetrics])

    const jobCount = useNodeActiveJobCount(node.id)

    // Performance chart and engine settings are mutually exclusive — opening one
    // collapses the other so only a single section is ever expanded per card.
    const togglePerformance = useCallback(() => {
        setIsExpanded(prev => {
            const next = !prev
            if (next) setIsEngineSettingsExpanded(false)
            return next
        })
    }, [])

    const toggleEngineSettings = useCallback(() => {
        setIsEngineSettingsExpanded(prev => {
            const next = !prev
            if (next) {
                setHasOpenedEngineSettings(true)
                setIsExpanded(false)
            }
            return next
        })
    }, [])

    // Respond to a cross-component request (cluster tab "Edit node", welcome
    // modal, tray) to reveal this node's engine settings and scroll to it.
    useEffect(() => {
        if (focusNodeId !== node.id) return
        setIsEngineSettingsExpanded(true)
        setHasOpenedEngineSettings(true)
        setIsExpanded(false)
        consumeFocusNode()
        requestAnimationFrame(() => {
            containerRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
        })
    }, [focusNodeId, node.id, consumeFocusNode])

    // Re-clip while the accordion animates open or closed; the onTransitionEnd
    // below re-enables overflow-visible once a fully-open transition finishes.
    useEffect(() => {
        setEngineSettingsOpenSettled(false)
    }, [isEngineSettingsExpanded])

    const measure = useCallback(() => {
        if (!containerRef.current) return
        const { width } = containerRef.current.getBoundingClientRect()
        if (width <= 0) return

        setIsNarrow(width < 550)

        if (width < 300) {
            setChartSize(30)
        } else {
            setChartSize(MAX_CHART_SIZE)
        }
    }, [])

    const scheduleResize = useCallback(() => {
        if (resizeRafRef.current) return
        resizeRafRef.current = requestAnimationFrame(() => {
            resizeRafRef.current = 0
            measure()
        })
    }, [measure])

    useEffect(() => {
        measure()
        const el = containerRef.current
        if (!el) return
        const ro = new ResizeObserver(scheduleResize)
        ro.observe(el)
        return () => {
            ro.disconnect()
        }
    }, [measure, scheduleResize])

    const expandButtons = (placement: 'left' | 'top') => (
        <>
            <DismissibleTooltip slotContent="Show performance" placement={placement}>
                <Button
                    kind={isExpanded ? 'primary' : 'tertiary'}
                    color="neutral"
                    size="tiny"
                    className="px-2"
                    onClick={togglePerformance}
                    aria-label={
                        isExpanded ? 'Collapse performance metrics' : 'Expand performance metrics'
                    }
                    aria-expanded={isExpanded}
                >
                    <BarChartOutlined style={{ fontSize: 14 }} />
                </Button>
            </DismissibleTooltip>
            <DismissibleTooltip slotContent="Show engine settings" placement={placement}>
                <Button
                    kind={isEngineSettingsExpanded ? 'primary' : 'tertiary'}
                    color="neutral"
                    size="tiny"
                    className="px-2"
                    onClick={toggleEngineSettings}
                    aria-label="Engine settings"
                    aria-expanded={isEngineSettingsExpanded}
                >
                    <SettingsOutlined style={{ fontSize: 14 }} />
                </Button>
            </DismissibleTooltip>
        </>
    )

    return (
        <div className="node-card pair-paper" data-node-id={node.id} ref={containerRef}>
            <div className="min-w-0 w-full">
                <Flex align="center" gap="3" className="w-full min-w-0 relative">
                    {chartSize <= 30 ? null : (
                        <DismissibleTooltip
                            slotContent={
                                <Stack gap="3">
                                    <NodeChartLegend
                                        gpuInfo={gpuInfo}
                                        cpuFallbackInfo={cpuFallbackInfo}
                                    />
                                </Stack>
                            }
                        >
                            <div className="flex items-center gap-1 shrink-0">
                                <div
                                    className="relative rounded-full"
                                    data-node-gpu-chart-anchor=""
                                >
                                    {metricsReady ? (
                                        <>
                                            <NodeActive show={jobCount > 0} color={baseColor} />
                                            <GpuRadialChartSvg
                                                metrics={chartMetrics}
                                                size={chartSize}
                                                isActive={`0 0 2px 1px ${baseColor}88`}
                                                cutout={0.2}
                                                ringSpacing={1}
                                                valueCornerRadius={0}
                                                bgColor="#00000055"
                                            />
                                        </>
                                    ) : (
                                        <div
                                            aria-hidden
                                            style={{ width: chartSize, height: chartSize }}
                                        />
                                    )}
                                </div>
                            </div>
                        </DismissibleTooltip>
                    )}

                    <Stack className="min-w-0 grow" gap="3">
                        <div className="ml-1">
                            <NodeLabel
                                name={node.name}
                                ipAddress={node.ipAddress}
                                gpuLabels={gpuLabels}
                                isLocal={isLocal}
                            />
                        </div>
                        <Flex
                            align="center"
                            wrap="wrap"
                            gap="4"
                            className="shrink min-w-0 ml-1 -mt-1"
                        >
                            <NodeEnginesInline nodeId={node.id} />
                        </Flex>
                        {isNarrow && (
                            <Flex align="center" gap="2" className="ml-1 -mt-0.5">
                                {expandButtons('top')}
                            </Flex>
                        )}
                    </Stack>

                    {!isNarrow && (
                        <Stack className="shrink-0 mt-1" gap="2">
                            {expandButtons('left')}
                        </Stack>
                    )}
                </Flex>
                <div
                    style={{
                        display: 'grid',
                        gridTemplateRows: isExpanded && metricsReady ? '1fr' : '0fr',
                        transition: 'grid-template-rows 0.25s ease'
                    }}
                >
                    <div className="overflow-hidden min-h-0">
                        <NodePerformance node={node} className="mt-5" />
                    </div>
                </div>
                <div
                    style={{
                        display: 'grid',
                        gridTemplateRows: isEngineSettingsExpanded ? '1fr' : '0fr',
                        transition: 'grid-template-rows 0.25s ease'
                    }}
                    onTransitionEnd={event => {
                        if (
                            event.propertyName === 'grid-template-rows' &&
                            isEngineSettingsExpanded
                        ) {
                            setEngineSettingsOpenSettled(true)
                        }
                    }}
                >
                    <div
                        className={`min-h-0 ${
                            isEngineSettingsExpanded && engineSettingsOpenSettled
                                ? 'overflow-visible'
                                : 'overflow-hidden'
                        }`}
                    >
                        {hasOpenedEngineSettings && (
                            <div className="mt-4">
                                <NodeEngineSettings nodeId={node.id} />
                            </div>
                        )}
                    </div>
                </div>
            </div>
        </div>
    )
}

export default memo(NodeCardDetails)
