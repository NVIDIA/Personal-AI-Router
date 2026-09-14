// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { startTransition, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Stack } from '@nvidia/foundations-react-core'
import { ModelHubSearchBar } from './ModelHubSearchBar'
import { ModelHubActions } from './ModelHubActions'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { ModelEntry, SortState } from '@/ui/types/model-hub'
import { ModelHubList } from './ModelHubList'
import { searchEngineHub } from '@/ui/utils/model-hub-search'
import { resolveStoredSort, writeStoredSort } from '@/ui/utils/model-hub-content-storage'
import { isHubEntryDownloaded } from '@/ui/utils/match-downloaded-model'
import { EngineType } from '@/shared/types/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { ModelItem } from '@/ui/types/engine-info'
import getErrorString from '@/shared/utils/get-error-string'

type ModelHubContentProps = {
    engine: EngineType | null
    multiple: boolean
    onSubmit: (models: ModelEntry[]) => void
    /** Already-installed models for this engine; matching hub results are hidden. */
    downloadedModels?: readonly ModelItem[]
}

const SORT_DEFAULT_STATE: SortState = {
    sort: 'lastModified',
    sortDescending: false,
    allDirections: {
        lastModified: false,
        name: false,
        size: false
    }
}

export const ModelHubContent = ({
    engine,
    multiple,
    onSubmit,
    downloadedModels
}: ModelHubContentProps) => {
    const [disabledListClick, setDisabledListClick] = useState(false)
    const [loading, setLoading] = useState(true)
    const [allModels, setAllModels] = useState<ModelEntry[]>([])
    const [query, setQuery] = useState('')
    const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set())
    const [sort, setSort] = useState<SortState>(() =>
        engine ? resolveStoredSort(engine, SORT_DEFAULT_STATE) : { ...SORT_DEFAULT_STATE }
    )
    const [errors, setErrors] = useState<{ id: string; message: string }[]>([])

    // Per-engine raw-response cache so reopening an engine is instant and
    // keystrokes filter locally instead of re-invoking the search IPC.
    const cacheRef = useRef<Map<EngineType, ModelEntry[]>>(new Map())
    // Monotonic generation guard so a stale in-flight fetch never overwrites
    // results for the engine the user has since switched to.
    const fetchGenRef = useRef(0)

    const loadModels = useCallback(async (backend: EngineType) => {
        const cached = cacheRef.current.get(backend)
        if (cached) {
            setAllModels(cached)
            setLoading(false)
            return
        }
        const gen = ++fetchGenRef.current
        setLoading(true)
        try {
            const result = await searchEngineHub(backend)
            if (fetchGenRef.current !== gen) return
            cacheRef.current.set(backend, result)
            setAllModels(result)
            setLoading(false)
        } catch (error) {
            if (fetchGenRef.current !== gen) return
            console.warn('Model hub load error', error)
            setErrors(prev => [
                ...prev,
                { id: performance.now().toString(), message: getErrorString(error) }
            ])
            setAllModels([])
            setLoading(false)
        }
    }, [])

    useEffect(() => {
        if (!engine) {
            setAllModels([])
            setQuery('')
            setSort({ ...SORT_DEFAULT_STATE })
            return
        }
        setQuery('')
        setSelectedModels(new Set())
        setSort(resolveStoredSort(engine, SORT_DEFAULT_STATE))
        void loadModels(engine)
        return () => {
            // Invalidate any in-flight fetch for the previous engine.
            fetchGenRef.current += 1
        }
    }, [engine, loadModels])

    const queryFiltered = useMemo(() => {
        const q = query.toLowerCase().trim()
        if (!q) return allModels
        return allModels.filter(m => m.name.toLowerCase().includes(q))
    }, [allModels, query])

    const catalogueModels = useMemo(() => {
        if (!engine || !downloadedModels || downloadedModels.length === 0) return queryFiltered
        return queryFiltered.filter(m => !isHubEntryDownloaded(engine, m, downloadedModels))
    }, [engine, queryFiltered, downloadedModels])

    /**
     * The identifier the user typed, offered as a row when the catalogue cannot
     * offer it.
     *
     * MLX has no catalogue at all -- `getEngineHubModels` returns `[]` for it --
     * so without this the modal is permanently "No models found" and there is no
     * way to add anything. A locally built model (a quantization) has no repo id
     * either, and can only ever be named by its path.
     *
     * Deliberately not validated here beyond containing a `/`, which separates a
     * repo id or a path from a stray word. Whether the thing exists is the
     * engine's question, and it answers it properly: `pull_model` checks a path
     * really is a servable model directory before recording it, and reports a
     * specific error if not. Guessing in the renderer would only duplicate that
     * check and disagree with it.
     */
    const typedEntry = useMemo(() => {
        if (!engine || !EngineCapabilities[engine]?.acceptsTypedModelId) return null
        const typed = query.trim()
        if (!typed || !typed.includes('/')) return null
        if (allModels.some(m => m.name === typed)) return null
        const entry: ModelEntry = {
            id: typed,
            name: typed,
            author: '',
            url: '',
            updatedAt: new Date(0)
        }
        if (downloadedModels && isHubEntryDownloaded(engine, entry, downloadedModels)) return null
        return entry
    }, [engine, query, allModels, downloadedModels])

    const visibleModels = useMemo(
        () => (typedEntry ? [typedEntry, ...catalogueModels] : catalogueModels),
        [typedEntry, catalogueModels]
    )

    const handleSortPersist = useCallback(
        (next: SortState) => {
            setSort(next)
            if (engine) {
                writeStoredSort(engine, next)
            }
        },
        [engine]
    )

    const modelsRef = useRef(allModels)
    modelsRef.current = allModels

    const handleSubmit = useCallback(
        (ids: string[]) => {
            const modelSelected: ModelEntry[] = []
            for (const id of ids) {
                const found = modelsRef.current.find(model => model.id === id)
                if (found !== undefined) {
                    modelSelected.push(found)
                }
            }

            if (modelSelected.length > 0) {
                onSubmit(modelSelected)
            }
        },
        [onSubmit]
    )

    const onToggleSelectModel = useCallback((modelId: string) => {
        startTransition(() => {
            setSelectedModels(prev => {
                const newSet = new Set(prev)
                newSet.has(modelId) ? newSet.delete(modelId) : newSet.add(modelId)
                return newSet
            })
        })
    }, [])

    const handleListToggle = useCallback(
        (modelId: string) => {
            if (multiple) {
                onToggleSelectModel(modelId)
            } else {
                handleSubmit([modelId])
            }
        },
        [multiple, onToggleSelectModel, handleSubmit]
    )

    const handleSearchSubmit = useCallback((q: string) => setQuery(q), [])

    return (
        <Stack gap="0" className="overflow-hidden min-h-0 min-w-0 grow w-full">
            <Stack gap="4" className="overflow-hidden min-h-0 min-w-0 grow w-full">
                <Stack gap="1">
                    {errors.length > 0 && (
                        <Stack gap="1" className="mb-4">
                            {errors.map(error => (
                                <InlineErrorBanner
                                    key={error.id}
                                    message={error.message}
                                    onClose={() =>
                                        setErrors(prev => prev.filter(e => e.id !== error.id))
                                    }
                                />
                            ))}
                        </Stack>
                    )}
                    <ModelHubSearchBar
                        onMenuOpenChange={setDisabledListClick}
                        onSubmit={handleSearchSubmit}
                        sort={sort}
                        onSort={handleSortPersist}
                    />
                </Stack>
                <ModelHubList
                    className="grow min-h-0"
                    disabled={disabledListClick}
                    multiple={multiple}
                    loading={loading}
                    models={visibleModels}
                    selectedModels={selectedModels}
                    onToggleSelectModel={handleListToggle}
                    sort={sort}
                />

                {!!multiple && (
                    <ModelHubActions
                        onSubmit={() => handleSubmit(Array.from(selectedModels))}
                        disabled={selectedModels.size === 0 || loading}
                        count={selectedModels.size}
                    />
                )}
            </Stack>
        </Stack>
    )
}
