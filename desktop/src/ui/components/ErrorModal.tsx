// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState, type ReactNode } from 'react'
import { Button, ModalContent, ModalDialog, ModalRoot, Stack } from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { errorDismissalKey } from '@/ui/utils/error-modal-dismissal'
import { InlineErrorBanner } from './InlineErrorBanner'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { isEngineType } from '@/shared/utils/engines'
import type { ServiceError } from '@/shared/types/errors'
import type { NodeItem } from '@/shared/types/nodes'

interface ErrorModalFilter {
    nodeId?: string
}

/**
 * Shared error modal with context-based filtering.
 *
 * - No filter: shows all errors (main window catch-all).
 * - `{ nodeId }`: shows only errors relevant to that node (Edit Node window).
 *
 * Dismissal clears matched errors both locally and on the service.
 *
 * `open` is derived from a latch over the matched error set rather than being
 * pinned true: the service can re-deliver the same errors while the dialog is
 * closing, and an `open` that does not follow the dialog's own close sequence
 * makes the primitive re-open it, replaying the backdrop animation. A new error
 * — including the same id reported again with a fresh timestamp — changes the
 * key and legitimately re-opens the dialog. See `errorDismissalKey`.
 */
export function ErrorModal({ filter }: { filter?: ErrorModalFilter }) {
    const errors = useErrorsStore(state => state.errors)
    const clearError = useErrorsStore(state => state.clearError)
    const [dismissedKey, setDismissedKey] = useState<string | null>(null)

    const matched = useMemo(() => {
        if (!filter) return errors
        return errors.filter(e => {
            if (filter.nodeId && e.nodeId !== filter.nodeId) return false
            return true
        })
    }, [errors, filter])

    const matchedKey = useMemo(() => errorDismissalKey(matched), [matched])

    const handleDismiss = useCallback(() => {
        if (matchedKey === dismissedKey) return
        setDismissedKey(matchedKey)
        matched.forEach(err => clearError(err.id))
    }, [clearError, dismissedKey, matched, matchedKey])

    if (matched.length === 0) return null

    return (
        <ModalRoot
            open={matchedKey !== dismissedKey}
            onOpenChange={open => !open && handleDismiss()}
            hideCloseButton
        >
            <ModalDialog>
                <ModalContent className="no-drag-elements">
                    <DialogHeader className="no-drag-elements" onClose={handleDismiss}>
                        {modalTitle(matched)}
                    </DialogHeader>
                    <Stack gap="2">
                        {matched.map(err => (
                            <ErrorBanner
                                key={`${err.nodeId ?? ''}/${err.id}`}
                                error={err}
                                onClear={clearError}
                                hideNodePrefix={filter?.nodeId === err.nodeId}
                            />
                        ))}
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}

function modalTitle(errors: ServiceError[]): string {
    if (errors.every(e => e.severity === 'info')) return 'Info'
    if (errors.every(e => e.severity === 'info' || e.severity === 'warning')) return 'Warning'
    return 'Error'
}

function bannerStatus(severity: ServiceError['severity']): 'info' | 'warning' | 'error' {
    if (severity === 'info') return 'info'
    if (severity === 'warning') return 'warning'
    return 'error'
}

/**
 * Build the retry handler for a `retry` action error by re-running the failed
 * engine operation from the fields the backend stamped on the error. Returns
 * `null` (no button) when the action isn't `retry` or the error lacks the
 * engine/node/model context needed to re-dispatch.
 */
function buildRetry(error: ServiceError): (() => void) | null {
    if (error.action !== 'retry') return null
    const { engineType, nodeId, operation, modelName } = error
    if (!engineType || !isEngineType(engineType) || !nodeId) return null
    const engines = window.pairApi.engines
    switch (operation) {
        case 'install':
            return () => engines.install(engineType, nodeId)
        case 'uninstall':
            return () => engines.uninstall(engineType, nodeId)
        case 'start':
            return () => engines.toggle(engineType, nodeId)
        case 'pull':
            return modelName ? () => engines.pullModel(engineType, nodeId, modelName) : null
        default:
            return null
    }
}

function resolveNodeLabel(nodes: Map<string, NodeItem>, nodeId: string): string | null {
    const node = nodes.get(nodeId)
    if (node?.name) return node.name
    if (node?.ipAddress) return node.ipAddress
    return null
}

function nodeErrorPrefix(severity: ServiceError['severity'], nodeLabel: string | null): string {
    const target = nodeLabel ? `node ${nodeLabel}` : 'a node'
    if (severity === 'warning') return `A warning has occurred on ${target}; `
    if (severity === 'info') return `Info on ${target}; `
    return `An error has occurred on ${target}; `
}

function formatErrorMessage(
    error: ServiceError,
    nodeLabel: string | null,
    showNodePrefix: boolean
): ReactNode {
    if (!showNodePrefix) return error.message
    return (
        <>
            <span style={{ color: 'var(--color-white)', fontWeight: 700 }}>
                {nodeErrorPrefix(error.severity, nodeLabel)}
            </span>
            {error.message}
        </>
    )
}

function ErrorBanner({
    error,
    onClear,
    hideNodePrefix
}: {
    error: ServiceError
    onClear: (id: string) => void
    hideNodePrefix: boolean
}) {
    const nodeLabel = useNodesStore(state =>
        error.nodeId ? resolveNodeLabel(state.nodes, error.nodeId) : null
    )
    const showNodePrefix = Boolean(error.nodeId) && !hideNodePrefix
    const displayMessage = formatErrorMessage(error, nodeLabel, showNodePrefix)
    const severity = bannerStatus(error.severity)
    const retry = buildRetry(error)

    return (
        <InlineErrorBanner severity={severity} message={displayMessage}>
            {retry && (
                <Button
                    kind="primary"
                    size="small"
                    onClick={() => {
                        onClear(error.id)
                        retry()
                    }}
                >
                    Retry
                </Button>
            )}
        </InlineErrorBanner>
    )
}
