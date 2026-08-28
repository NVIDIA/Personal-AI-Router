// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Divider, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { ModelEntry, SortState } from '@/ui/types/model-hub'
import { memo, useCallback, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useVirtualList } from '@/ui/hooks/useVirtualList'
import { formatModelName } from '@/ui/utils/format-model-name'
import EngineHubListRow from './EngineHubListRow'

type ModelRowProps = {
    index: number
    model: ModelEntry
    selectedModels: Set<string>
    disabled: boolean
    multiple: boolean
    isLast: boolean
    onToggleSelectModel: (modelId: string) => void
}

/**
 * Custom equality: compare the derived `checked` boolean from the Set
 * rather than the Set reference itself, so only rows whose selection
 * actually changed re-render.
 */
const ModelRow = memo(
    function ModelRow({
        index,
        model,
        selectedModels,
        disabled,
        multiple,
        isLast,
        onToggleSelectModel
    }: ModelRowProps) {
        const checked = selectedModels.has(model.id)
        return (
            <Stack gap="0" data-vlist-index={index}>
                <EngineHubListRow
                    disabled={disabled}
                    multiple={multiple}
                    model={model}
                    checked={checked}
                    onToggleSelectModel={onToggleSelectModel}
                />
                {!isLast && <Divider />}
            </Stack>
        )
    },
    (prev, next) =>
        prev.index === next.index &&
        prev.model === next.model &&
        prev.multiple === next.multiple &&
        prev.isLast === next.isLast &&
        prev.onToggleSelectModel === next.onToggleSelectModel &&
        prev.selectedModels.has(prev.model.id) === next.selectedModels.has(next.model.id)
)

type ModelHubListProps = {
    loading: boolean
    models: ModelEntry[]
    selectedModels: Set<string>
    onToggleSelectModel: (modelId: string) => void
    sort: SortState
    className?: string
    multiple: boolean
    disabled: boolean
}

const ESTIMATE_ROW_HEIGHT = 80

export const ModelHubList = ({
    disabled,
    loading,
    models,
    selectedModels,
    onToggleSelectModel,
    sort,
    className,
    multiple
}: ModelHubListProps) => {
    const sortedModels = useMemo(() => {
        const copy = [...models]

        switch (sort.sort) {
            case 'lastModified': {
                const lastModifiedSort = copy.sort(
                    (a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime()
                )
                return sort.sortDescending ? lastModifiedSort.reverse() : lastModifiedSort
            }
            case 'name': {
                const nameSort = copy.sort((a, b) =>
                    formatModelName(b.name, b.author).localeCompare(
                        formatModelName(a.name, a.author)
                    )
                )
                return sort.sortDescending ? nameSort : nameSort.reverse()
            }
            case 'size': {
                const sizeSort = copy.sort((a, b) => (b.size ?? 0) - (a.size ?? 0))
                return sort.sortDescending ? sizeSort : sizeSort.reverse()
            }
            default:
                return copy
        }
    }, [models, sort])

    const [viewportH, setViewportH] = useState(400)

    // `sortedModels` is a fresh array on every render because the upstream
    // `downloadedModels`/`backend` chain is rebuilt on each state push. Key the
    // virtual-list reset on the ordered id signature so scroll only resets when
    // the visible set/order actually changes, not on every push tick.
    const resetKey = useMemo(() => sortedModels.map(m => m.id).join('\n'), [sortedModels])

    const vlist = useVirtualList({
        count: sortedModels.length,
        estimateRowHeight: ESTIMATE_ROW_HEIGHT,
        viewportHeight: viewportH,
        threshold: 30,
        overscan: 4,
        resetKey
    })

    const observedElRef = useRef<HTMLElement | null>(null)
    const roRef = useRef<ResizeObserver | null>(null)

    useLayoutEffect(() => {
        const el = vlist.scrollRef.current
        if (el === observedElRef.current) return
        observedElRef.current = el
        roRef.current?.disconnect()
        roRef.current = null
        if (!el) return
        const h = el.clientHeight
        if (h > 0) setViewportH(h)
        const ro = new ResizeObserver(entries => {
            const rh = entries[0]?.contentRect.height ?? 0
            if (rh > 0) setViewportH(prev => (prev !== rh ? rh : prev))
        })
        ro.observe(el)
        roRef.current = ro
        return () => ro.disconnect()
    }, [vlist.scrollRef])

    const disabledRef = useRef(disabled)
    disabledRef.current = disabled

    const handleToggleSelectModel = useCallback(
        (modelId: string) => {
            if (disabledRef.current) return
            onToggleSelectModel(modelId)
        },
        [onToggleSelectModel]
    )

    const showList = !loading && sortedModels.length > 0

    return (
        <Stack
            gap="0"
            className={`relative overflow-hidden${disabled ? ' pointer-events-none' : ''} ${className ?? ''}`}
            style={{ width: '100%', minHeight: '50px' }}
        >
            {loading && (
                <Flex align="center" justify="center" gap="1" className="m-4 grow">
                    <span className="spinner-element" role="status" aria-label="Loading..." />
                    <Text kind="body/regular/sm">Searching models...</Text>
                </Flex>
            )}

            {showList && (
                <div
                    ref={vlist.scrollRef}
                    className="model-hub-scroll overflow-y-auto min-h-0 min-w-0 grow"
                    onScroll={vlist.onScroll}
                >
                    {vlist.virtualize ? (
                        <>
                            <div style={{ height: vlist.topSpacerHeight }} />
                            {sortedModels
                                .slice(vlist.startIndex, vlist.endIndex)
                                .map((model, i) => {
                                    const realIndex = vlist.startIndex + i
                                    return (
                                        <ModelRow
                                            key={model.id}
                                            index={realIndex}
                                            model={model}
                                            selectedModels={selectedModels}
                                            disabled={disabled}
                                            multiple={multiple}
                                            isLast={realIndex === sortedModels.length - 1}
                                            onToggleSelectModel={handleToggleSelectModel}
                                        />
                                    )
                                })}
                            <div style={{ height: vlist.bottomSpacerHeight }} />
                        </>
                    ) : (
                        sortedModels.map((model, i) => (
                            <ModelRow
                                key={model.id}
                                index={i}
                                model={model}
                                selectedModels={selectedModels}
                                disabled={disabled}
                                multiple={multiple}
                                isLast={i === sortedModels.length - 1}
                                onToggleSelectModel={handleToggleSelectModel}
                            />
                        ))
                    )}
                </div>
            )}

            {!loading && sortedModels.length === 0 && (
                <Stack align="center" justify="center" gap="2" className="grow min-h-0 p-4">
                    <Text kind="body/regular/sm" className="text-center">
                        No models found
                    </Text>
                </Stack>
            )}
        </Stack>
    )
}
