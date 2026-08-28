// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Radial Bar Chart Component (SVG)
 * Generic radial gauge showing metric percentages as concentric rings.
 * Track: stroked circle. Value: filled path with specified corner radius (fillet).
 * Each metric: { value, color, shown } - chart does not interpret values.
 */

import React, { useEffect, useId, useMemo, useRef, useState } from 'react'
import {
    clampPercent,
    donutSegmentOuterEdgePath,
    donutSegmentPath,
    effectiveCornerRadius
} from '@/ui/utils/donut-segment-path'

export interface RadialMetric {
    value: number
    color: string
    shown: boolean
}

interface GpuRadialChartSvgProps {
    metrics: RadialMetric[]
    isActive?: string
    size?: number
    bgColor?: string
    /** Inner hole as fraction of radius (0–1). Same as Chart.js cutout. */
    cutout?: number
    /** Gap in pixels between concentric rings. */
    ringSpacing?: number
    /** Corner radius as fraction of track thickness (0–1), like CSS borderRadius. Result is capped at ½ track thickness. */
    valueCornerRadius?: number
}

export default function GpuRadialChartSvg({
    metrics,
    isActive,
    bgColor = '#00000000',
    size = 100,
    cutout = 0.25,
    ringSpacing = 5,
    valueCornerRadius = 0.25
}: GpuRadialChartSvgProps) {
    const baseId = `glow-${useId().replace(/:/g, '')}`

    const currentRef = useRef<number[]>(metrics.map(m => m.value))
    const [animatedValues, setAnimatedValues] = useState<number[]>(() => metrics.map(m => m.value))

    useEffect(() => {
        const targets = metrics.map(m => m.value)
        const curr = currentRef.current
        while (curr.length < targets.length) curr.push(targets[curr.length])
        curr.length = targets.length

        let cancelled = false
        let rafId: number | undefined
        const tick = () => {
            if (cancelled) return
            let settled = true
            for (let i = 0; i < curr.length; i++) {
                const diff = targets[i] - curr[i]
                if (Math.abs(diff) > 0.5) {
                    curr[i] += diff * 0.3
                    settled = false
                } else {
                    curr[i] = targets[i]
                }
            }
            setAnimatedValues([...curr])
            if (!settled) rafId = requestAnimationFrame(tick)
        }

        rafId = requestAnimationFrame(tick)
        return () => {
            cancelled = true
            if (rafId !== undefined) cancelAnimationFrame(rafId)
        }
    }, [metrics])

    const visible = useMemo(
        () =>
            metrics
                .map((m, i) => ({ ...m, value: animatedValues[i] ?? m.value }))
                .filter(m => m.shown),
        [metrics, animatedValues]
    )
    const hasVisibleMetrics = visible.length > 0

    const rings = useMemo(() => {
        if (visible.length === 0) return []
        const fullRadius = size / 2
        const innerRadius = fullRadius * cutout
        const ringThickness = fullRadius - innerRadius
        const totalGaps = (visible.length - 1) * ringSpacing
        const perRing = Math.max(0, (ringThickness - totalGaps) / visible.length)
        const maxCornerRadiusPx = Math.floor(perRing / 2.2)

        return visible.map((metric, i) => {
            const value = clampPercent(metric.value)
            const outerRadius = fullRadius - i * (perRing + ringSpacing)
            const strokeRadius = Math.max(0, outerRadius - perRing / 2)
            const innerR = Math.max(0, strokeRadius - perRing / 2)
            const outerR = Math.max(innerR, strokeRadius + perRing / 2)
            const cornerRadius = Math.min(valueCornerRadius * perRing, maxCornerRadiusPx)
            const circumference = 2 * Math.PI * strokeRadius
            const trackSegmentCount = Math.max(4, Math.floor(circumference / 14))
            const trackSegmentLength = circumference / trackSegmentCount
            const trackDashLength = trackSegmentLength * 0.48 * 1.5
            const trackGapLength = trackSegmentLength * 0.52 * 0.5
            const trackDasharray = `${trackDashLength} ${trackGapLength}`
            const trackColor = `${metric.color}18`
            const valueColor = `${metric.color}33`
            const borderColor = metric.color
            const gradColor = `${metric.color}AA`
            const cx = size / 2
            const cy = size / 2
            const valuePath = donutSegmentPath(cx, cy, innerR, outerR, value, cornerRadius)
            const outerEdgePath = donutSegmentOuterEdgePath(
                cx,
                cy,
                innerR,
                outerR,
                value,
                cornerRadius
            )
            // Outer end corner (leading edge) for gradient direction — glow brightest here
            const endAngle = value > 0 ? (value / 100) * 2 * Math.PI : 0
            const R = effectiveCornerRadius(innerR, outerR, value, cornerRadius)
            const outerEndX = cx + outerR * Math.cos(endAngle)
            const outerEndY = cy + outerR * Math.sin(endAngle)
            const innerEndX = cx + innerR * Math.cos(endAngle)
            const innerEndY = cy + innerR * Math.sin(endAngle)
            const faceLen = Math.hypot(outerEndX - innerEndX, outerEndY - innerEndY) || 1
            const gradientEndX =
                R > 0 ? outerEndX + (R / faceLen) * (innerEndX - outerEndX) : outerEndX
            const gradientEndY =
                R > 0 ? outerEndY + (R / faceLen) * (innerEndY - outerEndY) : outerEndY
            const ringThicknessPx = outerR - innerR
            const peakRadius = ringThicknessPx * 2.5
            return {
                cx,
                cy,
                strokeRadius,
                strokeWidth: perRing,
                circumference,
                trackDasharray,
                trackColor,
                valueColor,
                borderColor,
                gradColor,
                valuePath,
                outerEdgePath,
                gradientEndX,
                gradientEndY,
                peakRadius
            }
        })
    }, [visible, size, cutout, ringSpacing, valueCornerRadius])

    const containerStyle = useMemo(
        () => ({
            width: size,
            height: size,
            borderRadius: '50%'
        }),
        [size]
    )

    const innerStyle = useMemo(
        () => ({
            backgroundColor: bgColor,
            borderRadius: '50%',
            boxShadow: isActive ?? 'none',
            transition: 'box-shadow 0.3s ease-in-out'
        }),
        [bgColor, isActive]
    )

    if (!hasVisibleMetrics) {
        return <div className="shrink-0 flex items-center justify-center" style={containerStyle} />
    }

    const bgStyle = {
        // backgroundImage: 'radial-gradient(circle, #000000 2%, #00000000 33%)'
    }

    return (
        <div
            className="shrink-0 flex items-center justify-center"
            style={{ ...containerStyle, ...bgStyle }}
        >
            {/* <style
                dangerouslySetInnerHTML={{
                    __html: '@keyframes chart-track-spin { to { transform: rotate(360deg); } }',
                }}
            /> */}
            <div
                className="shrink-0 w-full h-full flex items-center justify-center rounded-full overflow-hidden"
                style={{ ...innerStyle, ...containerStyle }}
            >
                <svg
                    width={size}
                    height={size}
                    viewBox={`0 0 ${size} ${size}`}
                    className="block"
                    style={{ overflow: 'visible' }}
                >
                    <defs>
                        <filter
                            id={`${baseId}-strokeGlow`}
                            x="-30%"
                            y="-30%"
                            width="160%"
                            height="160%"
                        >
                            <feGaussianBlur in="SourceGraphic" stdDeviation="3" result="blur" />
                        </filter>
                        {rings.map((ring, i) => (
                            <React.Fragment key={i}>
                                {ring.valuePath ? (
                                    <mask id={`${baseId}-glowMask-${i}`}>
                                        <path d={ring.valuePath} fill="white" />
                                    </mask>
                                ) : null}
                                {/* Fill gradient: darker at start → bright at outer end corner (futuristic) */}
                                <linearGradient
                                    id={`${baseId}-fillGrad-${i}`}
                                    gradientUnits="userSpaceOnUse"
                                    x1={ring.cx}
                                    y1={ring.cy}
                                    x2={ring.gradientEndX}
                                    y2={ring.gradientEndY}
                                >
                                    <stop
                                        offset="0"
                                        stopColor={ring.gradColor}
                                        stopOpacity={0.45}
                                    />
                                    <stop
                                        offset="0.6"
                                        stopColor={ring.gradColor}
                                        stopOpacity={0.55}
                                    />
                                    <stop
                                        offset="1"
                                        stopColor={ring.gradColor}
                                        stopOpacity={0.65}
                                    />
                                </linearGradient>
                                {/* Stroke/glow gradient: dim at start → full intensity at outer end corner */}
                                <linearGradient
                                    id={`${baseId}-strokeGrad-${i}`}
                                    gradientUnits="userSpaceOnUse"
                                    x1={ring.cx}
                                    y1={ring.cy}
                                    x2={ring.gradientEndX}
                                    y2={ring.gradientEndY}
                                >
                                    <stop
                                        offset="0"
                                        stopColor={ring.borderColor}
                                        stopOpacity={0.25}
                                    />
                                    <stop
                                        offset="0.7"
                                        stopColor={ring.borderColor}
                                        stopOpacity={0.5}
                                    />
                                    <stop
                                        offset="1"
                                        stopColor={ring.borderColor}
                                        stopOpacity={0.7}
                                    />
                                </linearGradient>
                                {/* Radial gradient for peak stroke mask: visible at outer end corner, fades toward start and inner corner */}
                                <radialGradient
                                    id={`${baseId}-peakGrad-${i}`}
                                    gradientUnits="userSpaceOnUse"
                                    cx={ring.gradientEndX}
                                    cy={ring.gradientEndY}
                                    r={ring.peakRadius}
                                    fx={ring.gradientEndX}
                                    fy={ring.gradientEndY}
                                >
                                    <stop offset="0" stopColor="white" stopOpacity="1" />
                                    <stop offset="0.4" stopColor="white" stopOpacity="0.4" />
                                    <stop offset="1" stopColor="white" stopOpacity="0" />
                                </radialGradient>
                                <mask id={`${baseId}-peakStrokeMask-${i}`}>
                                    <rect
                                        x={0}
                                        y={0}
                                        width={size}
                                        height={size}
                                        fill={`url(#${baseId}-peakGrad-${i})`}
                                    />
                                </mask>
                            </React.Fragment>
                        ))}
                    </defs>
                    <g transform={`rotate(90 ${size / 2} ${size / 2})`}>
                        {rings.map((ring, i) => (
                            <g key={i}>
                                <circle
                                    cx={ring.cx}
                                    cy={ring.cy}
                                    r={Math.max(0, ring.strokeRadius)}
                                    fill="none"
                                    stroke={ring.trackColor}
                                    strokeWidth={Math.max(0, ring.strokeWidth)}
                                    strokeDasharray={`${ring.circumference} ${ring.circumference}`}
                                    strokeDashoffset={0}
                                />
                                {/* Value: filled path with gradient (dark → bright at outer end corner) */}
                                {ring.valuePath && (
                                    <path
                                        d={ring.valuePath}
                                        fill={`url(#${baseId}-fillGrad-${i})`}
                                        style={{ opacity: 0.5 }}
                                    />
                                )}
                                {ring.valuePath && (
                                    <path
                                        d={ring.valuePath}
                                        fill={ring.valueColor}
                                        style={{ opacity: 0.65 }}
                                    />
                                )}
                                {/* Inner glow: blurred stroke with gradient, masked so only inside shows; brightest at end corner */}
                                {ring.outerEdgePath && ring.valuePath && (
                                    <path
                                        d={ring.outerEdgePath}
                                        fill="none"
                                        stroke={`url(#${baseId}-strokeGrad-${i})`}
                                        strokeWidth={2}
                                        filter={`url(#${baseId}-strokeGlow)`}
                                        mask={`url(#${baseId}-glowMask-${i})`}
                                        style={{ opacity: 0.5 }}
                                    />
                                )}
                                {/* Sharp stroke on outer + end edges with same gradient (full intensity at end corner) */}
                                {ring.outerEdgePath && (
                                    <path
                                        d={ring.outerEdgePath}
                                        fill="none"
                                        stroke={`url(#${baseId}-strokeGrad-${i})`}
                                        strokeWidth={1}
                                        style={{ opacity: 0.5 }}
                                    />
                                )}
                                {/* Peak layer: borderColor ~3px at outer/end corner, tapers off toward start and inner corner */}
                                {ring.outerEdgePath && (
                                    <path
                                        d={ring.outerEdgePath}
                                        fill="none"
                                        stroke={ring.borderColor}
                                        strokeWidth={1}
                                        mask={`url(#${baseId}-peakStrokeMask-${i})`}
                                        style={{ opacity: 0.5 }}
                                    />
                                )}
                            </g>
                        ))}
                    </g>
                </svg>
            </div>
        </div>
    )
}
