// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Text } from '@nvidia/foundations-react-core'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { LocalBadge } from '@/ui/components/LocalBadge'
import { buildNodeLabelSegments } from '@/ui/utils/node-label-segments'

export default function NodeLabel({
    name,
    ipAddress,
    gpuLabels,
    isLocal
}: {
    name: string
    ipAddress: string
    gpuLabels: string[]
    isLocal?: boolean
}) {
    const segmentRefs = useRef<(HTMLDivElement | null)[]>([])
    const containerRef = useRef<HTMLDivElement | null>(null)
    const [separatorAfter, setSeparatorAfter] = useState<boolean[]>([])
    const segments = useMemo(
        () => buildNodeLabelSegments(name, ipAddress, gpuLabels),
        [gpuLabels, ipAddress, name]
    )

    const updateSeparators = useCallback(() => {
        const next = segments.map((_segment, index) => {
            if (index >= segments.length - 1) return false
            const currentElement = segmentRefs.current[index]
            const nextElement = segmentRefs.current[index + 1]
            if (!currentElement || !nextElement) return false
            return (
                currentElement.offsetTop === nextElement.offsetTop &&
                nextElement.offsetLeft > currentElement.offsetLeft
            )
        })

        setSeparatorAfter(prev => {
            const unchanged =
                prev.length === next.length && prev.every((value, index) => value === next[index])
            return unchanged ? prev : next
        })
    }, [segments])

    useLayoutEffect(() => {
        updateSeparators()
    }, [updateSeparators])

    useEffect(() => {
        const observer = new ResizeObserver(updateSeparators)
        const container = containerRef.current
        if (container) {
            observer.observe(container)
        }
        for (const segmentElement of segmentRefs.current) {
            if (segmentElement) {
                observer.observe(segmentElement)
            }
        }

        return () => {
            observer.disconnect()
        }
    }, [segments, updateSeparators])

    return (
        <Flex
            ref={containerRef}
            align="center"
            wrap="wrap"
            className="w-full max-w-full overflow-hidden gap-x-4 gap-y-1"
        >
            {segments.map((segment, index) => {
                const isPrimary = segment.id === 'primary'
                return (
                    <Flex
                        key={segment.id}
                        ref={element => {
                            segmentRefs.current[index] = element
                        }}
                        align="center"
                        gap="2"
                        className="relative min-w-0"
                    >
                        <Text
                            kind={isPrimary ? 'body/bold/sm' : 'body/regular/sm'}
                            className={
                                isPrimary ? 'truncate' : 'text-subtle-color text-left truncate'
                            }
                        >
                            {segment.text}
                        </Text>
                        {isPrimary && isLocal && <LocalBadge />}
                        {separatorAfter[index] && (
                            <Text
                                kind="body/regular/lg"
                                className="text-subtle-color pointer-events-none absolute -right-3"
                                aria-hidden="true"
                            >
                                &middot;
                            </Text>
                        )}
                    </Flex>
                )
            })}
        </Flex>
    )
}
