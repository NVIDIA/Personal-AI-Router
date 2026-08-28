// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * SVG path geometry for the radial chart's donut segments.
 * A segment sweeps from angle 0 up to value%, optionally filleted at its ends.
 */

/**
 * Longest sweep a single `A` command may cover. A one-command full turn ends
 * where it starts, and SVG drops an arc whose endpoints coincide (Chromium
 * substitutes a line), so a 100% ring would vanish. Splitting keeps every
 * command's endpoints distinct.
 */
const MAX_ARC_SWEEP = Math.PI / 2

/**
 * Ring geometry only spans one turn; a wrapped sweep would read as a smaller
 * value. `NaN` is rejected first because it survives both `Math.min` and
 * `Math.max` and would poison every coordinate in the path.
 */
export function clampPercent(value: number): number {
    if (Number.isNaN(value)) return 0
    return Math.min(100, Math.max(0, value))
}

/**
 * Corner radius actually usable at this value: capped so opposing fillets cannot
 * cross, and dropped entirely at a full turn where the ends meet and a fillet
 * would only carve a notch out of a closed ring.
 */
export function effectiveCornerRadius(
    innerR: number,
    outerR: number,
    valuePct: number,
    cornerR: number
): number {
    const pct = clampPercent(valuePct)
    if (pct >= 100) return 0
    const endAngle = (pct / 100) * 2 * Math.PI
    const maxR = (endAngle / 2) * Math.min(innerR, outerR)
    return Math.max(0, Math.min(cornerR, maxR))
}

/** `A` commands from `from` to `to`, split so no command exceeds `MAX_ARC_SWEEP`. */
function arcCommands(cx: number, cy: number, r: number, from: number, to: number): string[] {
    const sweep = to - from
    if (sweep === 0) return []
    const steps = Math.ceil(Math.abs(sweep) / MAX_ARC_SWEEP)
    const sweepFlag = sweep > 0 ? 1 : 0
    const commands: string[] = []
    for (let i = 1; i <= steps; i++) {
        const angle = from + (sweep * i) / steps
        const x = cx + r * Math.cos(angle)
        const y = cy + r * Math.sin(angle)
        commands.push(`A ${r} ${r} 0 0 ${sweepFlag} ${x} ${y}`)
    }
    return commands
}

/** SVG path for a donut segment from 0 to value%, with rounded corners of radius R. */
export function donutSegmentPath(
    cx: number,
    cy: number,
    innerR: number,
    outerR: number,
    valuePct: number,
    cornerR: number
): string {
    const pct = clampPercent(valuePct)
    if (pct <= 0) return ''
    const endAngle = (pct / 100) * 2 * Math.PI
    const R = effectiveCornerRadius(innerR, outerR, pct, cornerR)
    if (R <= 0) {
        const startX = cx + innerR
        const startY = cy
        const innerEndX = cx + innerR * Math.cos(endAngle)
        const innerEndY = cy + innerR * Math.sin(endAngle)
        return [
            `M ${startX} ${startY}`,
            `L ${cx + outerR} ${cy}`,
            ...arcCommands(cx, cy, outerR, 0, endAngle),
            `L ${innerEndX} ${innerEndY}`,
            ...arcCommands(cx, cy, innerR, endAngle, 0),
            'Z'
        ].join(' ')
    }
    const innerStartAngle = R / innerR
    const outerStartAngle = R / outerR
    const innerEndTrim = R / innerR
    const outerEndTrim = R / outerR

    const innerStartX = cx + innerR * Math.cos(innerStartAngle)
    const innerStartY = cy + innerR * Math.sin(innerStartAngle)
    const outerStartX = cx + outerR * Math.cos(outerStartAngle)
    const outerStartY = cy + outerR * Math.sin(outerStartAngle)

    const outerEndX = cx + outerR * Math.cos(endAngle)
    const outerEndY = cy + outerR * Math.sin(endAngle)
    const innerEndX = cx + innerR * Math.cos(endAngle)
    const innerEndY = cy + innerR * Math.sin(endAngle)

    const outerArcEndAngle = endAngle - outerEndTrim
    const innerArcEndAngle = endAngle - innerEndTrim
    const innerArcEndX = cx + innerR * Math.cos(innerArcEndAngle)
    const innerArcEndY = cy + innerR * Math.sin(innerArcEndAngle)

    const faceLen = Math.hypot(outerEndX - innerEndX, outerEndY - innerEndY) || 1
    const faceOuterX = outerEndX + (R / faceLen) * (innerEndX - outerEndX)
    const faceOuterY = outerEndY + (R / faceLen) * (innerEndY - outerEndY)
    const faceInnerX = innerEndX + (R / faceLen) * (outerEndX - innerEndX)
    const faceInnerY = innerEndY + (R / faceLen) * (outerEndY - innerEndY)

    return [
        `M ${innerStartX} ${innerStartY}`,
        `A ${R} ${R} 0 0 1 ${cx + innerR + R} ${cy}`,
        `L ${cx + outerR - R} ${cy}`,
        `A ${R} ${R} 0 0 1 ${outerStartX} ${outerStartY}`,
        ...arcCommands(cx, cy, outerR, outerStartAngle, outerArcEndAngle),
        `A ${R} ${R} 0 0 1 ${faceOuterX} ${faceOuterY}`,
        `L ${faceInnerX} ${faceInnerY}`,
        `A ${R} ${R} 0 0 1 ${innerArcEndX} ${innerArcEndY}`,
        ...arcCommands(cx, cy, innerR, innerArcEndAngle, innerStartAngle),
        'Z'
    ].join(' ')
}

/**
 * Path along the outer arc and the leading end edge of the donut segment (for stroking).
 * Includes valueCornerRadius: start flat + start fillet + outer arc + end fillet + end face.
 */
export function donutSegmentOuterEdgePath(
    cx: number,
    cy: number,
    innerR: number,
    outerR: number,
    valuePct: number,
    cornerR: number
): string {
    const pct = clampPercent(valuePct)
    if (pct <= 0) return ''
    const endAngle = (pct / 100) * 2 * Math.PI
    const R = effectiveCornerRadius(innerR, outerR, pct, cornerR)
    const outerEndX = cx + outerR * Math.cos(endAngle)
    const outerEndY = cy + outerR * Math.sin(endAngle)
    const innerEndX = cx + innerR * Math.cos(endAngle)
    const innerEndY = cy + innerR * Math.sin(endAngle)
    if (R <= 0) {
        return [
            `M ${cx + outerR} ${cy}`,
            ...arcCommands(cx, cy, outerR, 0, endAngle),
            // At a full turn the end face lands on the start, where a radial
            // tick would just scar an otherwise closed ring.
            ...(pct >= 100 ? [] : [`L ${innerEndX} ${innerEndY}`])
        ].join(' ')
    }
    const outerStartAngle = R / outerR
    const outerEndTrim = R / outerR
    const outerStartX = cx + outerR * Math.cos(outerStartAngle)
    const outerStartY = cy + outerR * Math.sin(outerStartAngle)
    const outerArcEndAngle = endAngle - outerEndTrim
    const faceLen = Math.hypot(outerEndX - innerEndX, outerEndY - innerEndY) || 1
    const faceOuterX = outerEndX + (R / faceLen) * (innerEndX - outerEndX)
    const faceOuterY = outerEndY + (R / faceLen) * (innerEndY - outerEndY)
    const faceInnerX = innerEndX + (R / faceLen) * (outerEndX - innerEndX)
    const faceInnerY = innerEndY + (R / faceLen) * (outerEndY - innerEndY)
    // Start at inner end of flat (where fillet meets flat); outer boundary is the fillet arc only, not a line to (cx+outerR,cy)
    return [
        `M ${cx + outerR - R} ${cy}`,
        `A ${R} ${R} 0 0 1 ${outerStartX} ${outerStartY}`,
        ...arcCommands(cx, cy, outerR, outerStartAngle, outerArcEndAngle),
        `A ${R} ${R} 0 0 1 ${faceOuterX} ${faceOuterY}`,
        `L ${faceInnerX} ${faceInnerY}`
    ].join(' ')
}
