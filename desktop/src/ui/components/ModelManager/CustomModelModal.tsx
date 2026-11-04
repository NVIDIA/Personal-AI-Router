// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import {
    Button,
    Flex,
    FormField,
    ModalContent,
    ModalDialog,
    ModalRoot,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import { DialogHeader } from '@/ui/components/DialogHeader'
import { OpenInNew } from '@/ui/components/icons'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { ModelEntry } from '@/ui/types/model-hub'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { searchEngineHub } from '@/ui/utils/model-hub-search'
import { formatBytes } from '@/ui/utils/formatters'
import getErrorString from '@/shared/utils/get-error-string'

interface CustomModelModalProps {
    open: boolean
    onOpenChange: (open: boolean) => void
    backend: BackendInfo
    nodeId: string
}

type ProviderMode = 'file' | 'huggingface' | 'tag'

function sanitizeModelName(fileName: string): string {
    return fileName
        .replace(/\.(gguf|bin|safetensors)$/i, '')
        .toLowerCase()
        .replace(/[^a-z0-9._:-]/g, '-')
}

function resolveHfPullTarget(repo: string, engineType: string): string {
    const trimmed = repo.trim()
    if (!trimmed) return ''
    if (engineType === 'ollama') {
        return trimmed.startsWith('hf.co/') ? trimmed : `hf.co/${trimmed}`
    }
    if (engineType === 'llama-cpp') {
        return trimmed.includes(':') ? trimmed : `${trimmed}:Q4_K_M`
    }
    return trimmed
}

export function CustomModelModal({ open, onOpenChange, backend, nodeId }: CustomModelModalProps) {
    const selfId = useConnectionStore(state => state.selfId)
    const isLocal = !selfId || nodeId === selfId

    const [mode, setMode] = useState<ProviderMode>('file')

    // Local file state
    const [filePath, setFilePath] = useState('')
    const [modelName, setModelName] = useState('')

    // Hugging Face state
    const [hfSearchQuery, setHfSearchQuery] = useState('')
    const [hfSearching, setHfSearching] = useState(false)
    const [hfResults, setHfResults] = useState<ModelEntry[]>([])
    const [selectedHfRepo, setSelectedHfRepo] = useState('')

    // Custom tag state
    const [customTag, setCustomTag] = useState('')

    const [submitting, setSubmitting] = useState(false)
    const [errorMsg, setErrorMsg] = useState<string | null>(null)

    const handleClose = useCallback(() => {
        setFilePath('')
        setModelName('')
        setHfSearchQuery('')
        setHfResults([])
        setSelectedHfRepo('')
        setCustomTag('')
        setErrorMsg(null)
        setSubmitting(false)
        onOpenChange(false)
    }, [onOpenChange])

    const handleBrowse = useCallback(async () => {
        try {
            setErrorMsg(null)
            const result = await window.windowApi.window.selectModelFile()
            if (!result.canceled && result.filePath) {
                setFilePath(result.filePath)
                if (!modelName && result.fileName) {
                    setModelName(sanitizeModelName(result.fileName))
                }
            }
        } catch (err) {
            setErrorMsg(err instanceof Error ? err.message : String(err))
        }
    }, [modelName])

    const handleHfSearch = useCallback(async () => {
        const query = hfSearchQuery.trim()
        if (!query) return
        setHfSearching(true)
        setErrorMsg(null)
        try {
            const results = await searchEngineHub('llama-cpp', query)
            setHfResults(results)
            if (results.length > 0 && !selectedHfRepo) {
                setSelectedHfRepo(results[0].name)
            }
        } catch (err) {
            setErrorMsg(getErrorString(err))
        } finally {
            setHfSearching(false)
        }
    }, [hfSearchQuery, selectedHfRepo])

    const handleSubmit = useCallback(() => {
        setErrorMsg(null)

        if (mode === 'file') {
            const trimmedPath = filePath.trim()
            const trimmedName = modelName.trim()

            if (!trimmedPath) {
                setErrorMsg('Please select or specify a model file path.')
                return
            }
            if (!trimmedName) {
                setErrorMsg('Please enter a name for the model.')
                return
            }
            if (backend.type === 'ollama' && backend.processStatus !== 'running') {
                setErrorMsg('Ollama must be running to import a local model.')
                return
            }

            setSubmitting(true)
            window.pairApi.engines.importLocalModel(backend.type, nodeId, trimmedName, trimmedPath)
            handleClose()
        } else if (mode === 'huggingface') {
            const repo = selectedHfRepo.trim()
            if (!repo) {
                setErrorMsg('Please select or enter a Hugging Face repository.')
                return
            }

            const pullTarget = resolveHfPullTarget(repo, backend.type)
            setSubmitting(true)
            window.pairApi.engines.pullModel(backend.type, nodeId, pullTarget)
            handleClose()
        } else {
            const trimmedTag = customTag.trim()
            if (!trimmedTag) {
                setErrorMsg('Please enter a model name or tag.')
                return
            }

            setSubmitting(true)
            window.pairApi.engines.pullModel(backend.type, nodeId, trimmedTag)
            handleClose()
        }
    }, [
        backend.processStatus,
        backend.type,
        customTag,
        filePath,
        handleClose,
        mode,
        modelName,
        nodeId,
        selectedHfRepo
    ])

    const isRunning = backend.processStatus === 'running'
    const canSubmit =
        !submitting &&
        (mode === 'file'
            ? isLocal && filePath.trim().length > 0 && modelName.trim().length > 0
            : mode === 'huggingface'
              ? selectedHfRepo.trim().length > 0
              : customTag.trim().length > 0)

    const pullTargetPreview =
        mode === 'huggingface' && selectedHfRepo.trim().length > 0
            ? resolveHfPullTarget(selectedHfRepo, backend.type)
            : null

    return (
        <ModalRoot open={open} onOpenChange={onOpenChange} hideCloseButton>
            <ModalDialog>
                <ModalContent
                    className="no-drag-elements max-content-modal"
                    style={{
                        width: 'min(640px, calc(100vw - 64px))',
                        maxWidth: 'calc(100vw - 32px)',
                        maxHeight: 'calc(100vh - 64px)'
                    }}
                >
                    <DialogHeader onClose={handleClose} divider={false}>
                        Add model
                    </DialogHeader>

                    <Stack gap="4" className="pt-2 overflow-y-auto">
                        <Flex
                            gap="1"
                            className="p-1 rounded bg-[var(--color-background-surface-subtle)] w-fit"
                        >
                            <Button
                                size="small"
                                kind={mode === 'file' ? 'primary' : 'tertiary'}
                                color={mode === 'file' ? 'brand' : 'neutral'}
                                onClick={() => {
                                    setMode('file')
                                    setErrorMsg(null)
                                }}
                                disabled={!isLocal}
                            >
                                Local file (.gguf)
                            </Button>
                            <Button
                                size="small"
                                kind={mode === 'huggingface' ? 'primary' : 'tertiary'}
                                color={mode === 'huggingface' ? 'brand' : 'neutral'}
                                onClick={() => {
                                    setMode('huggingface')
                                    setErrorMsg(null)
                                }}
                            >
                                🤗 Hugging Face
                            </Button>
                            <Button
                                size="small"
                                kind={mode === 'tag' ? 'primary' : 'tertiary'}
                                color={mode === 'tag' ? 'brand' : 'neutral'}
                                onClick={() => {
                                    setMode('tag')
                                    setErrorMsg(null)
                                }}
                            >
                                Model tag / CLI
                            </Button>
                        </Flex>

                        {mode === 'file' && (
                            <Stack gap="3">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Import an existing GGUF model file from this machine into{' '}
                                    {backend.displayName}.
                                </Text>

                                <Flex align="end" gap="2">
                                    <FormField slotLabel="Model file path" className="flex-1">
                                        <TextInput
                                            value={filePath}
                                            onValueChange={setFilePath}
                                            placeholder="/path/to/model.gguf"
                                            disabled={submitting}
                                        />
                                    </FormField>
                                    <Button
                                        kind="secondary"
                                        size="medium"
                                        onClick={() => void handleBrowse()}
                                        disabled={submitting}
                                    >
                                        Browse...
                                    </Button>
                                </Flex>

                                <FormField slotLabel="Model name">
                                    <TextInput
                                        value={modelName}
                                        onValueChange={setModelName}
                                        placeholder="custom-model-name"
                                        disabled={submitting}
                                    />
                                </FormField>

                                {!isRunning && backend.type === 'ollama' && (
                                    <Text
                                        kind="body/regular/xs"
                                        className="text-[var(--color-feedback-warning)]"
                                    >
                                        Note: Start Ollama before importing to allow Ollama to
                                        register the model weights.
                                    </Text>
                                )}
                            </Stack>
                        )}

                        {mode === 'huggingface' && (
                            <Stack gap="3">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Search Hugging Face models or paste any repository ID to
                                    download into {backend.displayName}.
                                </Text>

                                <Flex align="end" gap="2">
                                    <FormField slotLabel="Search Hugging Face" className="flex-1">
                                        <TextInput
                                            value={hfSearchQuery}
                                            onValueChange={setHfSearchQuery}
                                            placeholder="e.g. deepseek, llama 3.2, qwen, mistral"
                                            disabled={hfSearching}
                                            onKeyDown={e => {
                                                if (e.key === 'Enter') void handleHfSearch()
                                            }}
                                        />
                                    </FormField>
                                    <Button
                                        kind="secondary"
                                        size="medium"
                                        onClick={() => void handleHfSearch()}
                                        disabled={hfSearching || !hfSearchQuery.trim()}
                                    >
                                        {hfSearching ? 'Searching...' : 'Search'}
                                    </Button>
                                </Flex>

                                {hfResults.length > 0 && (
                                    <Stack
                                        gap="1"
                                        className="max-h-48 overflow-y-auto border border-[var(--color-border-subtle)] rounded p-2 bg-[var(--color-background-surface-subtle)]"
                                    >
                                        <Text
                                            kind="body/semibold/xs"
                                            className="text-subtle-color pb-1"
                                        >
                                            Search results (click to select):
                                        </Text>
                                        {hfResults.map(entry => {
                                            const isSelected = selectedHfRepo === entry.name
                                            return (
                                                <Flex
                                                    key={entry.id}
                                                    align="center"
                                                    justify="between"
                                                    gap="2"
                                                    className={`px-2 py-1.5 rounded cursor-pointer transition-colors ${
                                                        isSelected
                                                            ? 'bg-[var(--color-background-brand-subtle)] text-[var(--color-text-brand)]'
                                                            : 'hover:bg-white/5'
                                                    }`}
                                                    onClick={() => setSelectedHfRepo(entry.name)}
                                                >
                                                    <Stack gap="0" className="min-w-0">
                                                        <Text
                                                            kind="body/bold/xs"
                                                            className="truncate"
                                                        >
                                                            {entry.name}
                                                        </Text>
                                                        <Flex gap="2" align="center">
                                                            <Text
                                                                kind="body/regular/xs"
                                                                className="text-subtle-color"
                                                            >
                                                                by {entry.author}
                                                            </Text>
                                                            {!!entry.parameterSize && (
                                                                <Text
                                                                    kind="body/regular/xs"
                                                                    className="text-subtle-color"
                                                                >
                                                                    {entry.parameterSize}
                                                                </Text>
                                                            )}
                                                            {!!entry.size && (
                                                                <Text
                                                                    kind="body/regular/xs"
                                                                    className="text-subtle-color"
                                                                >
                                                                    {formatBytes(entry.size)}
                                                                </Text>
                                                            )}
                                                        </Flex>
                                                    </Stack>
                                                    {!!entry.url && (
                                                        <Button
                                                            kind="tertiary"
                                                            size="small"
                                                            className="p-1"
                                                            onClick={e => {
                                                                e.stopPropagation()
                                                                window.open(entry.url, '_blank')
                                                            }}
                                                            aria-label="Open on Hugging Face"
                                                        >
                                                            <OpenInNew style={{ fontSize: 13 }} />
                                                        </Button>
                                                    )}
                                                </Flex>
                                            )
                                        })}
                                    </Stack>
                                )}

                                <FormField slotLabel="Selected Hugging Face repository">
                                    <TextInput
                                        value={selectedHfRepo}
                                        onValueChange={setSelectedHfRepo}
                                        placeholder="e.g. bartowski/Llama-3.2-1B-Instruct-GGUF"
                                        disabled={submitting}
                                    />
                                </FormField>

                                {pullTargetPreview && (
                                    <Text kind="body/regular/xs" className="text-subtle-color">
                                        Will pull:{' '}
                                        <span className="font-mono">{pullTargetPreview}</span>
                                    </Text>
                                )}
                            </Stack>
                        )}

                        {mode === 'tag' && (
                            <Stack gap="3">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Enter any model identifier or tag available to{' '}
                                    {backend.displayName}.
                                </Text>

                                <FormField slotLabel="Model identifier or tag">
                                    <TextInput
                                        value={customTag}
                                        onValueChange={setCustomTag}
                                        placeholder={
                                            backend.type === 'llama-cpp'
                                                ? 'bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M'
                                                : 'mistral:latest, deepseek-r1:1.5b, llama3.2:1b'
                                        }
                                        disabled={submitting}
                                        onKeyDown={e => {
                                            if (e.key === 'Enter' && canSubmit) handleSubmit()
                                        }}
                                    />
                                </FormField>
                            </Stack>
                        )}

                        {errorMsg && (
                            <Text
                                kind="body/regular/sm"
                                className="text-[var(--color-feedback-danger)]"
                            >
                                {errorMsg}
                            </Text>
                        )}

                        <Flex justify="end" gap="2" className="pt-2">
                            <Button
                                kind="secondary"
                                size="small"
                                onClick={handleClose}
                                disabled={submitting}
                            >
                                Cancel
                            </Button>
                            <Button
                                kind="primary"
                                color="brand"
                                size="small"
                                onClick={handleSubmit}
                                disabled={!canSubmit}
                            >
                                Add model
                            </Button>
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
