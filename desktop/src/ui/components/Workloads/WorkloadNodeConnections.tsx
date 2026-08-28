// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState, memo } from 'react'
import { useAssignedWorkloads } from '@/ui/stores/workloads.store'
import type { Workload } from '@/shared/types/workloads'
import { workloadExecutionNodeId, workloadKey } from '@/shared/utils/workloads'
import { getWorkloadStateColor } from '@/ui/utils/colors'
import { WORKLOAD_COLOR_MAP } from '@/ui/constants/colors'
import { CONNECTIONS_WIDTH } from '@/ui/constants/app'

// Above this many concurrently *running* connections we drop the costly per-line
// Gaussian-blur glow (lineGlow + beadGlow) and collapse the three-bead trail to a
// single plain moving dot. The blur filters are the dominant cost when a burst of
// jobs lands; a lone un-filtered animated circle is cheap, so the "running" signal
// still shows under load instead of disappearing.
const HEAVY_LINE_LIMIT = 12

// The job-card list sits below a fixed filter row + divider that occupy ~52px at
// the top of the column. Lines on the job-card side must never anchor above this,
// or they'd cross into the header/divider and rise over the top of the first
// card. (The connector SVG shares its top edge with the job-list column, so this
// is measured from svg y=0.)
const JOB_LIST_TOP_Y = 54

interface Connection {
    workloadId: string
    nodeId: string
    color: string
    path: string
    isRunning: boolean
    animateForward: boolean
}

function connectionsEqual(a: readonly Connection[], b: readonly Connection[]): boolean {
    if (a.length !== b.length) return false
    for (let i = 0; i < a.length; i++) {
        const x = a[i]
        const y = b[i]
        if (
            x.workloadId !== y.workloadId ||
            x.nodeId !== y.nodeId ||
            x.color !== y.color ||
            x.path !== y.path ||
            x.isRunning !== y.isRunning ||
            x.animateForward !== y.animateForward
        )
            return false
    }
    return true
}

// Build a key that uniquely identifies a workload's DOM anchor. Workload ids are
// a per-node proxy counter and collide across nodes, so we key on the
// (origin, id) pair the same way `workloadKey` does. The null byte can't appear
// in either value, so it is a safe separator.
function anchorKey(origin: string, id: string): string {
    return `${origin}\u0000${id}`
}

function computeConnections(svg: SVGSVGElement, workloads: readonly Workload[]): Connection[] {
    const svgRect = svg.getBoundingClientRect()
    const result: Connection[] = []

    // One pass over the DOM instead of two querySelector calls per workload per
    // frame. With N running jobs the old path issued 2 * N selector queries every
    // animation frame; this issues two querySelectorAll calls total.
    const workloadElByKey = new Map<string, Element>()
    for (const el of document.querySelectorAll('[data-workload-id]')) {
        const id = el.getAttribute('data-workload-id')
        if (id === null) continue
        workloadElByKey.set(anchorKey(el.getAttribute('data-workload-origin') ?? '', id), el)
    }
    const nodeElById = new Map<string, Element>()
    for (const el of document.querySelectorAll('[data-node-id]')) {
        const id = el.getAttribute('data-node-id')
        if (id !== null) nodeElById.set(id, el)
    }

    for (const workload of workloads) {
        const executionNodeId = workloadExecutionNodeId(workload)
        if (!executionNodeId) continue

        const workloadEl = workloadElByKey.get(
            anchorKey(workload.originatedFrom ?? '', workload.id)
        )
        const nodeEl = nodeElById.get(executionNodeId)
        if (!workloadEl || !nodeEl) continue

        const workloadRect = workloadEl.getBoundingClientRect()
        const nodeRect = nodeEl.getBoundingClientRect()

        const chartEl = nodeEl.querySelector('[data-node-gpu-chart-anchor]')
        const chartRect = chartEl?.getBoundingClientRect()
        const nodeAnchorY =
            chartRect && chartRect.width > 0 && chartRect.height > 0
                ? chartRect.top + chartRect.height / 2 - svgRect.top
                : nodeRect.top + nodeRect.height / 2 - svgRect.top

        const nodeIsLeft = nodeRect.left < workloadRect.left
        // Clamp the job anchor between the top of the job-card list (below the
        // filter row + divider) and the connector's bottom edge. A running job
        // scrolled out of view keeps its line pinned to the edge it scrolled past
        // instead of vanishing (which reads as "nothing running") or shooting up
        // over the list header/divider.
        const workloadAnchorY = Math.max(
            JOB_LIST_TOP_Y,
            Math.min(svgRect.height, workloadRect.top + 10 - svgRect.top)
        )

        let startX: number, startY: number, endX: number, endY: number
        if (nodeIsLeft) {
            startX = nodeRect.right - svgRect.left
            startY = nodeAnchorY
            endX = workloadRect.left - svgRect.left
            endY = workloadAnchorY
        } else {
            startX = workloadRect.right - svgRect.left
            startY = workloadAnchorY
            endX = nodeRect.left - svgRect.left
            endY = nodeAnchorY
        }

        const cpOff = Math.abs(endX - startX) * 0.5
        const path = `M ${startX},${startY} C ${startX + cpOff},${startY} ${endX - cpOff},${endY} ${endX},${endY}`

        const color = getWorkloadStateColor(workload.state)
        result.push({
            workloadId: workloadKey(workload.originatedFrom, workload.id),
            nodeId: executionNodeId,
            color: `${WORKLOAD_COLOR_MAP[color]}${workload.state === 'running' ? 'FF' : '88'}`,
            path,
            isRunning: workload.state === 'running',
            animateForward: !nodeIsLeft
        })
    }

    return result
}

function WorkloadNodeConnections({
    className,
    style
}: {
    className?: string
    style?: React.CSSProperties
}) {
    const [connections, setConnections] = useState<Connection[]>([])
    const wrapperRef = useRef<HTMLDivElement>(null)
    const svgRef = useRef<SVGSVGElement>(null)
    const rafId = useRef(0)
    const loopRunning = useRef(false)

    const assignedWorkloads = useAssignedWorkloads()
    const workloadsRef = useRef(assignedWorkloads)
    workloadsRef.current = assignedWorkloads

    const connectionsRef = useRef(connections)
    connectionsRef.current = connections

    // The connector only needs to re-measure while something is actually moving:
    // the job set changing, a scroll in either list, or a layout resize. A
    // permanent 60fps loop thrashed layout with getBoundingClientRect every frame
    // for every job even when nothing moved. Instead we run the rAF loop only
    // inside a short "awake" window that interactions extend, and let it sleep
    // once geometry settles. The SMIL beads keep animating while the loop sleeps.
    //
    // The loop reads live data from refs, so it only needs to start/stop with
    // `assignedWorkloads`; defining `tick`/`wake` inside the effect keeps them off
    // the dependency list. The cleanup cancels the frame and listeners on both
    // restart and unmount.
    useEffect(() => {
        if (assignedWorkloads.length === 0) {
            setConnections([])
            return
        }

        const QUIET_MS = 350
        let wakeUntil = performance.now() + QUIET_MS

        function ensureRunning() {
            if (loopRunning.current) return
            loopRunning.current = true
            rafId.current = requestAnimationFrame(tick)
        }

        function wake() {
            wakeUntil = performance.now() + QUIET_MS
            ensureRunning()
        }

        function tick() {
            const workloads = workloadsRef.current

            // No work to do: stop cleanly. This effect restarts the loop when jobs return.
            if (workloads.length === 0) {
                loopRunning.current = false
                rafId.current = 0
                if (connectionsRef.current.length > 0) setConnections([])
                return
            }

            let changed = false
            try {
                const svg = svgRef.current
                if (svg) {
                    const next = computeConnections(svg, workloads)
                    // Avoid a re-render every frame: only commit when something moved.
                    if (!connectionsEqual(connectionsRef.current, next)) {
                        changed = true
                        setConnections(next)
                    }
                }
            } catch {
                // A transient compute/DOM error must never kill the loop (otherwise
                // `loopRunning` would latch and lines would vanish for the session).
            } finally {
                const now = performance.now()
                // Keep the loop awake while geometry is still settling.
                if (changed) wakeUntil = now + QUIET_MS
                if (now < wakeUntil && workloadsRef.current.length > 0) {
                    rafId.current = requestAnimationFrame(tick)
                } else {
                    loopRunning.current = false
                    rafId.current = 0
                }
            }
        }

        // Initial measure when the job set changes.
        loopRunning.current = true
        rafId.current = requestAnimationFrame(tick)

        const onInteraction = () => wake()
        // Capture phase catches scrolls from the nested OverlayScrollbars viewports
        // without needing a handle to them.
        document.addEventListener('scroll', onInteraction, { capture: true, passive: true })
        window.addEventListener('resize', onInteraction, { passive: true })

        // A node/workload card expand-collapse (grid-template-rows over 250ms),
        // nested engine-row expand, or list add/remove/reorder moves a radial
        // chart without scrolling, resizing the window, resizing this 60px strip,
        // or changing the job set — so none of the triggers above fire and the
        // lines would stay pinned to stale positions. Watch the two growing list
        // content containers instead of this wrapper: the ResizeObserver fires per
        // frame while a card's height animates and on add/remove, and the
        // MutationObserver (childList/subtree only) catches reorders that don't
        // change the container's own height. Both just wake() the existing loop,
        // which then follows the motion to completion and sleeps. Attribute
        // mutations are intentionally not observed so the radial metric animation,
        // NodeActive opacity fades, and box-shadow transitions (constant inline-
        // style churn that never moves layout) can't wake it.
        const ro = new ResizeObserver(onInteraction)
        const wrapper = wrapperRef.current
        if (wrapper) ro.observe(wrapper)

        const mutationObservers: MutationObserver[] = []
        for (const selector of ['[data-node-list-content]', '[data-workload-list-content]']) {
            const contentEl = document.querySelector(selector)
            if (!contentEl) continue
            ro.observe(contentEl)
            const mo = new MutationObserver(onInteraction)
            mo.observe(contentEl, { childList: true, subtree: true })
            mutationObservers.push(mo)
        }

        return () => {
            cancelAnimationFrame(rafId.current)
            rafId.current = 0
            loopRunning.current = false
            document.removeEventListener('scroll', onInteraction, { capture: true })
            window.removeEventListener('resize', onInteraction)
            ro.disconnect()
            for (const mo of mutationObservers) mo.disconnect()
        }
    }, [assignedWorkloads])

    const runningCount = connections.reduce((n, c) => (c.isRunning ? n + 1 : n), 0)
    const heavy = runningCount > HEAVY_LINE_LIMIT

    return (
        <div
            ref={wrapperRef}
            className={`min-w-0 shrink-0 relative ${className}`}
            style={{ width: `${CONNECTIONS_WIDTH}px`, ...style }}
        >
            <svg
                ref={svgRef}
                className="absolute top-0 left-0 w-full h-full pointer-events-none overflow-visible"
            >
                <defs>
                    <filter id="lineGlow" x="-50%" y="-50%" width="200%" height="200%">
                        <feGaussianBlur in="SourceGraphic" stdDeviation="2" result="blur" />
                        <feColorMatrix
                            in="blur"
                            type="matrix"
                            values="1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 0.15 0"
                            result="glow"
                        />
                        <feMerge>
                            <feMergeNode in="glow" />
                            <feMergeNode in="SourceGraphic" />
                        </feMerge>
                    </filter>

                    <filter id="beadGlow" x="-100%" y="-100%" width="300%" height="300%">
                        <feGaussianBlur in="SourceGraphic" stdDeviation="8" result="blur1" />
                        <feColorMatrix
                            in="blur1"
                            type="matrix"
                            values="1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 0.3 0"
                            result="glow1"
                        />
                        <feGaussianBlur in="SourceGraphic" stdDeviation="4" result="blur2" />
                        <feColorMatrix
                            in="blur2"
                            type="matrix"
                            values="1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 0.5 0"
                            result="glow2"
                        />
                        <feGaussianBlur in="SourceGraphic" stdDeviation="2" result="blur3" />
                        <feColorMatrix
                            in="blur3"
                            type="matrix"
                            values="1.2 0 0 0 0  0 1.2 0 0 0  0 0 1.2 0 0  0 0 0 0.8 0"
                            result="glow3"
                        />
                        <feMerge>
                            <feMergeNode in="glow1" />
                            <feMergeNode in="glow2" />
                            <feMergeNode in="glow3" />
                            <feMergeNode in="SourceGraphic" />
                        </feMerge>
                    </filter>

                    {/* Vertical-only clip: CSS cannot express overflow-x visible +
                        overflow-y hidden, but this can. Lines may still bridge
                        left/right across the panels, but can never render above the
                        content row (over ContentBar/title) or below it. The rect
                        spans the SVG height (= content row height) via height="100%"
                        and a very wide horizontal range. */}
                    <clipPath id="connClip" clipPathUnits="userSpaceOnUse">
                        <rect x="-9999" y="0" width="19998" height="100%" />
                    </clipPath>
                </defs>

                <g clipPath="url(#connClip)">
                    {connections.map(connection => (
                        <g key={`${connection.workloadId}-${connection.nodeId}`}>
                            <path
                                d={connection.path}
                                stroke={connection.color}
                                strokeWidth="2"
                                fill="none"
                                opacity={connection.isRunning ? '0.8' : '0.6'}
                                strokeLinecap="round"
                                filter={
                                    !heavy && connection.isRunning ? 'url(#lineGlow)' : undefined
                                }
                            />

                            {connection.isRunning &&
                                (heavy ? (
                                    // Under heavy load we drop the costly Gaussian-blur
                                    // glow but keep one plain moving dot so "jobs are
                                    // running" still reads at a glance.
                                    <circle r="2.5" fill={connection.color} opacity="0.7">
                                        <animateMotion
                                            dur="1s"
                                            repeatCount="indefinite"
                                            path={connection.path}
                                            keyPoints={connection.animateForward ? '0;1' : '1;0'}
                                            keyTimes="0;1"
                                        />
                                    </circle>
                                ) : (
                                    <g filter="url(#beadGlow)">
                                        <circle r="5" fill={connection.color} opacity="0.15">
                                            <animateMotion
                                                dur="1s"
                                                repeatCount="indefinite"
                                                path={connection.path}
                                                keyPoints={
                                                    connection.animateForward ? '0;1' : '1;0'
                                                }
                                                keyTimes="0;1"
                                            />
                                        </circle>
                                        <circle r="2.5" fill={connection.color} opacity="0.4">
                                            <animateMotion
                                                dur="1s"
                                                repeatCount="indefinite"
                                                path={connection.path}
                                                keyPoints={
                                                    connection.animateForward ? '0;1' : '1;0'
                                                }
                                                keyTimes="0;1"
                                            />
                                        </circle>
                                        <circle r="1" fill="#FFFFFF" opacity="0.5">
                                            <animateMotion
                                                dur="1s"
                                                repeatCount="indefinite"
                                                path={connection.path}
                                                keyPoints={
                                                    connection.animateForward ? '0;1' : '1;0'
                                                }
                                                keyTimes="0;1"
                                            />
                                        </circle>
                                    </g>
                                ))}
                        </g>
                    ))}
                </g>
            </svg>
        </div>
    )
}

export default memo(WorkloadNodeConnections)
