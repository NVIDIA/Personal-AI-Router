// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    clampPercent,
    donutSegmentOuterEdgePath,
    donutSegmentPath
} from '@/ui/utils/donut-segment-path'

/**
 * The radial chart's rings are hand-built SVG paths. A single `A` command
 * covering a full turn ends on the point it started from, and SVG omits an arc
 * whose endpoints are identical (Chromium substitutes a straight line), so a
 * metric at exactly 100 collapsed to a zero-area sliver and read as 0%.
 *
 * Path coordinates are parsed into float32 by the renderer, so "identical" is
 * decided at float32 precision, not double — hence the `Math.fround` comparison
 * below rather than a strict double equality.
 */

// Matches the geometry the two production call sites request: cutout 0.2 with
// no fillet (`valueCornerRadius={0}`), which is the branch that degenerated.
const CX = 30
const CY = 30
const INNER_R = 18
const OUTER_R = 30

function pathPoints(path: string): { x: number; y: number }[] {
    const points: { x: number; y: number }[] = []
    const tokens = path.split(/\s+/)
    for (let i = 0; i < tokens.length; i++) {
        const token = tokens[i]
        // A command's target is its last two parameters; M and L take exactly
        // two. Everything else in these paths is a radius or a flag.
        if (token === 'M' || token === 'L') {
            points.push({ x: Number(tokens[i + 1]), y: Number(tokens[i + 2]) })
            i += 2
        } else if (token === 'A') {
            points.push({ x: Number(tokens[i + 6]), y: Number(tokens[i + 7]) })
            i += 7
        }
    }
    return points
}

function hasCoincidentConsecutivePoints(path: string): boolean {
    const points = pathPoints(path)
    for (let i = 1; i < points.length; i++) {
        const prev = points[i - 1]
        const curr = points[i]
        if (
            Math.fround(prev.x) === Math.fround(curr.x) &&
            Math.fround(prev.y) === Math.fround(curr.y)
        )
            return true
    }
    return false
}

function countArcs(path: string, radius: number): number {
    return path.split(' ').filter((token, i, tokens) => {
        return token === 'A' && Number(tokens[i + 1]) === radius
    }).length
}

describe('donutSegmentPath', () => {
    for (const cornerR of [0, 4]) {
        for (const value of [1, 25, 50, 99, 99.9, 100]) {
            it(`emits no zero-length command at ${value}% with corner radius ${cornerR}`, () => {
                const fill = donutSegmentPath(CX, CY, INNER_R, OUTER_R, value, cornerR)
                const edge = donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, value, cornerR)

                expect(fill).not.toBe('')
                expect(edge).not.toBe('')
                expect(hasCoincidentConsecutivePoints(fill)).toBe(false)
                expect(hasCoincidentConsecutivePoints(edge)).toBe(false)
            })
        }
    }

    it('splits a full turn across several arcs per radius', () => {
        const fill = donutSegmentPath(CX, CY, INNER_R, OUTER_R, 100, 0)

        // A regression to one `A` per radius is exactly the degenerate full-turn
        // arc that made a maxed-out ring disappear.
        expect(countArcs(fill, OUTER_R)).toBeGreaterThan(1)
        expect(countArcs(fill, INNER_R)).toBeGreaterThan(1)
    })

    it('closes a full ring without a seam tick on the outer edge', () => {
        const edge = donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, 100, 0)

        // The leading end face lands on the start at a full turn, so drawing it
        // would scar an otherwise closed ring.
        expect(edge).not.toContain('L')
    })

    it('renders nothing at or below zero', () => {
        expect(donutSegmentPath(CX, CY, INNER_R, OUTER_R, 0, 0)).toBe('')
        expect(donutSegmentPath(CX, CY, INNER_R, OUTER_R, -5, 0)).toBe('')
        expect(donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, 0, 0)).toBe('')
        expect(donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, -5, 0)).toBe('')
    })

    // Nothing clamps backend metrics on the way in: the metrics store keeps the
    // reported sample verbatim. An over-range value must saturate, because a
    // sweep past one turn would wrap and read as a *smaller* value (120 as 20)
    // and 200 would land back on the degenerate full turn.
    it('saturates above 100 instead of wrapping', () => {
        const full = donutSegmentPath(CX, CY, INNER_R, OUTER_R, 100, 0)
        const fullEdge = donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, 100, 0)

        for (const value of [100.5, 120, 200, 1e6, Number.POSITIVE_INFINITY]) {
            expect(donutSegmentPath(CX, CY, INNER_R, OUTER_R, value, 0)).toBe(full)
            expect(donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, value, 0)).toBe(fullEdge)
        }
    })

    it('renders nothing for a non-finite value', () => {
        // NaN survives both Math.min and Math.max, so an unguarded clamp would
        // poison every coordinate in the path string.
        expect(donutSegmentPath(CX, CY, INNER_R, OUTER_R, Number.NaN, 0)).toBe('')
        expect(donutSegmentOuterEdgePath(CX, CY, INNER_R, OUTER_R, Number.NaN, 0)).toBe('')
        expect(donutSegmentPath(CX, CY, INNER_R, OUTER_R, Number.NEGATIVE_INFINITY, 0)).toBe('')
    })
})

describe('clampPercent', () => {
    it('caps to the 0-100 range and rejects non-finite input', () => {
        expect(clampPercent(50)).toBe(50)
        expect(clampPercent(0)).toBe(0)
        expect(clampPercent(100)).toBe(100)
        expect(clampPercent(137)).toBe(100)
        expect(clampPercent(Number.POSITIVE_INFINITY)).toBe(100)
        expect(clampPercent(-1)).toBe(0)
        expect(clampPercent(Number.NaN)).toBe(0)
    })
})
