// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { startTransition, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Button, Stack, StatusMessage, Text } from '@nvidia/foundations-react-core'
import { ModelHubSearchBar } from './ModelHubSearchBar'
import { ModelHubActions } from './ModelHubActions'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { ModelEntry, SortState } from '@/ui/types/model-hub'
import { ModelHubList } from './ModelHubList'
import {
    addModelHubError,
    mergeModelHubResults,
    searchEngineHub,
    type ModelHubError
} from '@/ui/utils/model-hub-search'
import { resolveStoredSort, writeStoredSort } from '@/ui/utils/model-hub-content-storage'
import { isHubEntryDownloaded } from '@/ui/utils/match-downloaded-model'
import { EngineType } from '@/shared/types/engines'
import type { EngineHubSearchResponse } from '@/shared/types/engine-api'
import type { ModelItem } from '@/ui/types/engine-info'
import getErrorString from '@/shared/utils/get-error-string'

type ModelHubContentProps = {
    engine: EngineType | null
    /** The node the chosen model is downloaded to, whose catalog is shown. */
    nodeId: string
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

const CATALOG_RETRY_DELAYS_MS = [5_000, 10_000]

export const ModelHubContent = ({
    engine,
    nodeId,
    multiple,
    onSubmit,
    downloadedModels
}: ModelHubContentProps) => {
    const [disabledListClick, setDisabledListClick] = useState(false)
    const [loading, setLoading] = useState(true)
    const [catalogError, setCatalogError] = useState<string | null>(null)
    const [catalogAttempt, setCatalogAttempt] = useState(0)
    const [catalogRetrySeconds, setCatalogRetrySeconds] = useState<number | null>(null)
    const [catalog, setCatalog] = useState<EngineHubSearchResponse<ModelEntry>>({
        models: [],
        searchable: null
    })
    const allModels = catalog.models
    const [remoteModels, setRemoteModels] = useState<ModelEntry[]>([])
    const [query, setQuery] = useState('')
    const [composingQuery, setComposingQuery] = useState(false)
    const [searching, setSearching] = useState(false)
    const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set())
    const [sort, setSort] = useState<SortState>(() =>
        engine ? resolveStoredSort(engine, SORT_DEFAULT_STATE) : { ...SORT_DEFAULT_STATE }
    )
    const [errors, setErrors] = useState<ModelHubError[]>([])
    const reportError = useCallback((message: string) => {
        setErrors(prev => addModelHubError(prev, message, performance.now().toString()))
    }, [])

    // Per-destination browse cache keeps local filtering immediate while upstream
    // searches wait for a pause in typing.
    const cacheRef = useRef<Map<string, EngineHubSearchResponse<ModelEntry>>>(new Map())
    // Monotonic generation guard so a stale in-flight fetch never overwrites
    // results for the engine the user has since switched to.
    const fetchGenRef = useRef(0)
    const searchGenRef = useRef(0)
    const submittedQueryRef = useRef('')
    const searchInFlightRef = useRef<ReturnType<typeof searchEngineHub> | null>(null)

    const loadModels = useCallback(async (backend: EngineType, node: string) => {
        const gen = ++fetchGenRef.current
        const cacheKey = `${backend}:${node}`
        const cached = cacheRef.current.get(cacheKey)
        setCatalogError(null)
        setCatalogRetrySeconds(null)
        if (cached) {
            setCatalog(cached)
            setLoading(false)
            return
        }
        setCatalogAttempt(attempt => attempt + 1)
        setLoading(true)
        setCatalog({ models: [], searchable: null })
        try {
            const result = await searchEngineHub(backend, node)
            if (fetchGenRef.current !== gen) return
            cacheRef.current.set(cacheKey, result)
            setCatalog(result)
            setLoading(false)
        } catch (error) {
            if (fetchGenRef.current !== gen) return
            setCatalogError(getErrorString(error))
            setCatalog({ models: [], searchable: null })
            setLoading(false)
        }
    }, [])

    useEffect(() => {
        setRemoteModels([])
        setSearching(false)
        setComposingQuery(false)
        setCatalogError(null)
        setCatalogAttempt(0)
        setErrors([])
        submittedQueryRef.current = ''
        if (!engine) {
            setCatalog({ models: [], searchable: null })
            setQuery('')
            setSort({ ...SORT_DEFAULT_STATE })
            return
        }
        setQuery('')
        setSelectedModels(new Set())
        setSort(resolveStoredSort(engine, SORT_DEFAULT_STATE))
        void loadModels(engine, nodeId)
        return () => {
            // Invalidate in-flight population or search for the previous engine.
            fetchGenRef.current += 1
            searchGenRef.current += 1
        }
    }, [engine, nodeId, loadModels])

    useEffect(() => {
        const delay = CATALOG_RETRY_DELAYS_MS[catalogAttempt - 1]
        if (!engine || !catalogError || loading || query.trim() || delay === undefined) {
            setCatalogRetrySeconds(null)
            return
        }
        const generation = fetchGenRef.current
        const retryAt = Date.now() + delay
        setCatalogRetrySeconds(Math.ceil(delay / 1_000))
        const countdown = setInterval(() => {
            if (fetchGenRef.current === generation) {
                setCatalogRetrySeconds(Math.max(0, Math.ceil((retryAt - Date.now()) / 1_000)))
            }
        }, 1_000)
        const timer = setTimeout(() => {
            if (fetchGenRef.current === generation) void loadModels(engine, nodeId)
        }, delay)
        return () => {
            clearTimeout(timer)
            clearInterval(countdown)
        }
    }, [engine, nodeId, catalogError, catalogAttempt, loading, query, loadModels])

    const retryCatalog = useCallback(() => {
        if (!engine) return
        setCatalogAttempt(0)
        void loadModels(engine, nodeId)
    }, [engine, nodeId, loadModels])

    const queryFiltered = useMemo(() => {
        const q = query.toLowerCase().trim()
        if (!q) return allModels
        const populatedMatches = allModels.filter(m => m.name.toLowerCase().includes(q))
        return mergeModelHubResults(populatedMatches, remoteModels)
    }, [allModels, query, remoteModels])

    const visibleModels = useMemo(() => {
        if (!engine || !downloadedModels || downloadedModels.length === 0) return queryFiltered
        return queryFiltered.filter(m => !isHubEntryDownloaded(engine, m, downloadedModels))
    }, [engine, queryFiltered, downloadedModels])

    const hasQuery = query.trim().length > 0
    const listError = hasQuery ? errors.at(-1)?.message : catalogError

    // A searchable source browses only a sample of its upstream, so a search
    // can reach models this list does not show, even when every listed model
    // is already installed.
    const browsingSample =
        catalog.searchable === true && !loading && query.trim() === '' && allModels.length > 0

    const handleSortPersist = useCallback(
        (next: SortState) => {
            setSort(next)
            if (engine) {
                writeStoredSort(engine, next)
            }
        },
        [engine]
    )

    const modelsRef = useRef(queryFiltered)
    modelsRef.current = queryFiltered

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

    const handleQueryChange = useCallback((nextQuery: string) => {
        setQuery(nextQuery)
        if (nextQuery.trim() === submittedQueryRef.current) return
        searchGenRef.current += 1
        submittedQueryRef.current = ''
        setErrors([])
        setRemoteModels([])
        setSearching(false)
    }, [])

    const handleCompositionChange = useCallback((composing: boolean) => {
        setComposingQuery(composing)
        if (composing) {
            searchGenRef.current += 1
            submittedQueryRef.current = ''
            setErrors([])
            setSearching(false)
        }
    }, [])

    const handleSearch = useCallback(
        async (nextQuery: string) => {
            const normalized = nextQuery.trim()
            setQuery(nextQuery)
            // An unread catalog leaves support unknown, so an explicit query
            // can still recover from a failed initial browse.
            if (!engine || catalog.searchable === false || normalized.length === 0) return

            const generation = ++searchGenRef.current
            submittedQueryRef.current = normalized
            setErrors([])
            setRemoteModels([])
            setSearching(true)
            // Wait for the active request; only the latest query may start next.
            if (searchInFlightRef.current) {
                await searchInFlightRef.current.catch(() => undefined)
            }
            if (searchGenRef.current !== generation) return
            const request = searchEngineHub(engine, nodeId, normalized)
            searchInFlightRef.current = request
            try {
                const result = await request
                if (searchGenRef.current !== generation) return
                setRemoteModels(result.searchable ? result.models : [])
            } catch (error) {
                if (searchGenRef.current !== generation) return
                reportError(getErrorString(error))
            } finally {
                if (searchInFlightRef.current === request) searchInFlightRef.current = null
                if (searchGenRef.current === generation) setSearching(false)
            }
        },
        [engine, nodeId, catalog.searchable, reportError]
    )

    useEffect(() => {
        if (!engine || catalog.searchable === false || composingQuery || query.trim().length < 2)
            return
        const timer = setTimeout(() => {
            // Enter or the search button may already have submitted this text.
            if (query.trim() !== submittedQueryRef.current) void handleSearch(query)
        }, 300)
        return () => clearTimeout(timer)
    }, [engine, catalog.searchable, query, composingQuery, handleSearch])

    const retryModels = () => {
        if (hasQuery) {
            void handleSearch(query)
        } else {
            retryCatalog()
        }
    }

    return (
        <Stack gap="0" className="overflow-hidden min-h-0 min-w-0 grow w-full">
            <Stack gap="4" className="overflow-hidden min-h-0 min-w-0 grow w-full">
                <Stack gap="1">
                    {hasQuery && !searching && visibleModels.length > 0 && errors.length > 0 && (
                        <Stack gap="1" className="mb-4">
                            {errors.map(error => (
                                <InlineErrorBanner
                                    key={error.id}
                                    message={error.message}
                                    onClose={() =>
                                        setErrors(prev => prev.filter(e => e.id !== error.id))
                                    }
                                >
                                    <Button kind="secondary" size="small" onClick={retryModels}>
                                        Retry
                                    </Button>
                                </InlineErrorBanner>
                            ))}
                        </Stack>
                    )}
                    <ModelHubSearchBar
                        onMenuOpenChange={setDisabledListClick}
                        query={query}
                        searching={searching}
                        onQueryChange={handleQueryChange}
                        onCompositionChange={handleCompositionChange}
                        onSearch={handleSearch}
                        sort={sort}
                        onSort={handleSortPersist}
                    />
                    {browsingSample && visibleModels.length > 0 && (
                        <Text kind="body/regular/sm" className="text-subtle-color mt-2 ml-2">
                            Showing popular models. Search to find more.
                        </Text>
                    )}
                </Stack>
                {listError && !loading && !searching && visibleModels.length === 0 ? (
                    <Stack align="center" justify="center" className="grow" role="alert">
                        <StatusMessage
                            size="small"
                            slotHeading={
                                hasQuery ? "Couldn't search models" : "Couldn't load models"
                            }
                            slotSubheading={listError}
                            slotFooter={
                                <Stack gap="2" align="center">
                                    <Button kind="primary" color="brand" onClick={retryModels}>
                                        Retry
                                    </Button>
                                    {!hasQuery && catalogRetrySeconds !== null && (
                                        <Text
                                            kind="body/regular/sm"
                                            className="text-subtle-color"
                                            role="timer"
                                            aria-live="off"
                                        >
                                            {catalogRetrySeconds > 0
                                                ? `Retrying in ${catalogRetrySeconds}s…`
                                                : 'Retrying…'}
                                        </Text>
                                    )}
                                </Stack>
                            }
                        />
                    </Stack>
                ) : (
                    <ModelHubList
                        className="grow min-h-0"
                        disabled={disabledListClick}
                        multiple={multiple}
                        loading={loading || (searching && visibleModels.length === 0)}
                        models={visibleModels}
                        emptyMessage={
                            browsingSample
                                ? 'Popular models are already installed. Search to find more.'
                                : undefined
                        }
                        selectedModels={selectedModels}
                        onToggleSelectModel={handleListToggle}
                        sort={sort}
                    />
                )}

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
