// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState, type ReactNode } from 'react'
import { Button, ModalContent, ModalDialog, ModalRoot, Stack } from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { errorDismissalKey } from '@/ui/utils/error-modal-dismissal'
import { InlineErrorBanner } from './InlineErrorBanner'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineUpdateAvailableStore } from '@/ui/stores/engine-update-available.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { isEngineType } from '@/shared/utils/engines'
import type { ServiceError } from '@/shared/types/errors'

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
    const selfId = useConnectionStore(state => state.selfId)
    const [dismissedKey, setDismissedKey] = useState<string | null>(null)

    const matched = useMemo(() => {
        if (!filter) return errors
        return errors.filter(e => {
            if (filter.nodeId && e.nodeId !== filter.nodeId) return false
            return true
        })
    }, [errors, filter])

    const matchedKey = useMemo(() => errorDismissalKey(matched), [matched])

    const clear = useCallback(
        (error: ServiceError) => {
            // The peer's engine manager owns its update errors. errors.clear(id)
            // addresses this node and could clear its own error with the same ID.
            if (error.operation === 'update' && error.nodeId && error.nodeId !== selfId) return
            clearError(error.id)
        },
        [clearError, selfId]
    )

    const handleDismiss = useCallback(() => {
        if (matchedKey === dismissedKey) return
        setDismissedKey(matchedKey)
        matched.forEach(clear)
    }, [clear, dismissedKey, matched, matchedKey])

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
                                onClear={clear}
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
function buildRetry(error: ServiceError, canUpdate: boolean): (() => void) | null {
    if (error.action !== 'retry') return null
    const { engineType, nodeId, operation, modelName } = error
    if (!engineType || !isEngineType(engineType) || !nodeId) return null
    const engines = window.pairApi.engines
    switch (operation) {
        case 'install':
            return () => engines.install(engineType, nodeId)
        case 'update':
            return canUpdate ? () => engines.update(engineType, nodeId) : null
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
    onClear: (error: ServiceError) => void
    hideNodePrefix: boolean
}) {
    const node = useNodesStore(state => (error.nodeId ? state.nodes.get(error.nodeId) : undefined))
    const nodeLabel = node?.name || node?.ipAddress || null
    const showNodePrefix = Boolean(error.nodeId) && !hideNodePrefix
    const displayMessage = formatErrorMessage(error, nodeLabel, showNodePrefix)
    const severity = bannerStatus(error.severity)
    const isLocalNode = useConnectionStore(state => state.selfId === error.nodeId)
    const hasManagedUpdate = useEngineUpdateAvailableStore(state =>
        error.operation === 'update' &&
        error.nodeId &&
        error.engineType &&
        isEngineType(error.engineType)
            ? state.getInfo(error.nodeId, error.engineType)?.installType === 'managed'
            : false
    )
    const retry = buildRetry(error, isLocalNode || (node?.status === 'active' && hasManagedUpdate))

    return (
        <InlineErrorBanner severity={severity} message={displayMessage}>
            {retry && (
                <Button
                    kind="primary"
                    size="small"
                    onClick={() => {
                        onClear(error)
                        retry()
                    }}
                >
                    Retry
                </Button>
            )}
        </InlineErrorBanner>
    )
}
