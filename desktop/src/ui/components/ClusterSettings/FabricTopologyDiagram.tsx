// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { FabricTopologyCandidate } from '@/ui/utils/fabric-topology'

interface Props {
    candidate: FabricTopologyCandidate
    nodeName(nodeId: string): string
}

const positions = {
    single: [{ x: 260, y: 88 }],
    direct: [
        { x: 130, y: 88 },
        { x: 390, y: 88 }
    ],
    ring: [
        { x: 260, y: 40 },
        { x: 125, y: 140 },
        { x: 395, y: 140 }
    ]
} as const

function shortName(value: string): string {
    return value.length > 18 ? `${value.slice(0, 17)}…` : value
}

function edgeColor(state: FabricTopologyCandidate['edges'][number]['state']): string {
    if (state === 'matched' || state === 'active') return '#76b900'
    if (state === 'pending') return '#8a8a8a'
    return '#d94646'
}

export default function FabricTopologyDiagram({ candidate, nodeName }: Props) {
    const nodes = candidate.nodeIds.map((nodeId, index) => ({
        nodeId,
        ...(positions[candidate.layout][index] ?? positions.single[0])
    }))
    const point = (nodeId: string) => nodes.find(node => node.nodeId === nodeId)!

    return (
        <svg
            viewBox="0 0 520 190"
            role="img"
            aria-label={`${candidate.layout} physical-port wiring diagram`}
            className="w-full max-w-[620px]"
        >
            {candidate.edges.map(edge => {
                const left = point(edge.left.nodeId)
                const right = point(edge.right.nodeId)
                const x = (left.x + right.x) / 2
                const y = (left.y + right.y) / 2
                const color = edgeColor(edge.state)
                return (
                    <g
                        key={`${edge.left.nodeId}-${edge.left.portName}-${edge.right.nodeId}-${edge.right.portName}`}
                    >
                        <line
                            x1={left.x}
                            y1={left.y}
                            x2={right.x}
                            y2={right.y}
                            stroke={color}
                            strokeWidth="4"
                            strokeLinecap="round"
                            strokeDasharray={edge.state === 'pending' ? '8 7' : undefined}
                        >
                            {edge.state === 'pending' && (
                                <animate
                                    attributeName="stroke-dashoffset"
                                    from="0"
                                    to="-15"
                                    dur="0.8s"
                                    repeatCount="indefinite"
                                />
                            )}
                        </line>
                        {edge.state === 'active' &&
                            [
                                `M ${left.x},${left.y} L ${right.x},${right.y}`,
                                `M ${right.x},${right.y} L ${left.x},${left.y}`
                            ].map(path => (
                                <circle key={path} r="3.5" fill="#c6f27a">
                                    <animateMotion
                                        dur="1.6s"
                                        repeatCount="indefinite"
                                        path={path}
                                    />
                                </circle>
                            ))}
                        <rect
                            x={x - 37}
                            y={y - 10}
                            width="74"
                            height="20"
                            rx="10"
                            fill="var(--surface-color-base, #151515)"
                            stroke={color}
                        />
                        <text x={x} y={y + 4} textAnchor="middle" fill="currentColor" fontSize="10">
                            {edge.left.portName} ↔ {edge.right.portName}
                        </text>
                    </g>
                )
            })}
            {nodes.map(node => (
                <g key={node.nodeId}>
                    <rect
                        x={node.x - 64}
                        y={node.y - 25}
                        width="128"
                        height="50"
                        rx="10"
                        fill="var(--surface-color-base, #151515)"
                        stroke="#76b900"
                        strokeWidth="2"
                    />
                    <circle cx={node.x - 47} cy={node.y} r="5" fill="#76b900" />
                    <text
                        x={node.x + 7}
                        y={node.y + 4}
                        textAnchor="middle"
                        fill="currentColor"
                        fontSize="12"
                        fontWeight="600"
                    >
                        {shortName(nodeName(node.nodeId))}
                    </text>
                </g>
            ))}
        </svg>
    )
}
