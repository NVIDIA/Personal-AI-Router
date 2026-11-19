// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** A single text run in a node card's header label. */
interface LabelSegment {
    id: string
    text: string
}

/**
 * Header-label segments for a node card: name, address, then one segment per
 * GPU.
 *
 * Each GPU gets its OWN segment rather than being folded into one joined
 * string, because `NodeLabel` applies wrapping, `·` separators and truncation
 * per segment. A joined string is a single segment, so on a narrow card it
 * truncates after the first card name and the rest of a multi-GPU host becomes
 * invisible — which is the failure this shape exists to prevent.
 *
 * Empty GPU names are dropped so a backend that reports a blank name cannot
 * render a stray separator with nothing after it.
 */
export function buildNodeLabelSegments(
    name: string,
    ipAddress: string,
    gpuLabels: string[]
): LabelSegment[] {
    const primaryText = (name || ipAddress).toUpperCase()
    const segments: LabelSegment[] = [{ id: 'primary', text: primaryText }]

    if (name && ipAddress) {
        segments.push({ id: 'ip', text: ipAddress })
    }

    gpuLabels.forEach((gpuLabel, index) => {
        if (gpuLabel) {
            segments.push({ id: `gpu-${index}`, text: gpuLabel })
        }
    })

    return segments
}
