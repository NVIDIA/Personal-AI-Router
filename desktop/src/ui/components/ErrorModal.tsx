// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState, type ReactNode } from 'react'
import { Button, ModalContent, ModalDialog, ModalRoot, Stack } from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { errorDismissalKey } from '@/ui/utils/error-modal-dismissal'
import { InlineErrorBanner } from './InlineErrorBanner'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { isEngineType } from '@/shared/utils/engines'
import { EngineDisplayNames } from '@/shared/constants/engines'
import type { EngineType } from '@/shared/types/engines'
import type { ServiceError } from '@/shared/types/errors'
import type { NodeItem } from '@/shared/types/nodes'
import { ConfirmModal } from './ConfirmModal'
import { EnginePathConsentModal } from './EnginePathConsentModal'

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
 * How a `retry` action error re-runs the failed engine operation from the
 * fields the backend stamped on it. Install and uninstall change PATH, so they
 * carry the target and are asked about before they run.
 */
type RetryAction =
    | { kind: 'install' | 'uninstall'; engineType: EngineType; nodeId: string }
    | { kind: 'run'; run: () => void }

/**
 * Returns `null` (no button) when the action isn't `retry` or the error lacks
 * the engine/node/model context needed to re-dispatch.
 */
function buildRetry(error: ServiceError): RetryAction | null {
    if (error.action !== 'retry') return null
    const { engineType, nodeId, operation, modelName } = error
    if (!engineType || !isEngineType(engineType) || !nodeId) return null
    const engines = window.pairApi.engines
    switch (operation) {
        case 'install':
        case 'uninstall':
            return { kind: operation, engineType, nodeId }
        case 'start':
            return { kind: 'run', run: () => engines.toggle(engineType, nodeId) }
        case 'pull':
            return modelName
                ? { kind: 'run', run: () => engines.pullModel(engineType, nodeId, modelName) }
                : null
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
    const selfId = useConnectionStore(state => state.selfId)
    const retryTarget = retry && retry.kind !== 'run' ? retry : null
    const pathManaged = useEngineStatusStore(state =>
        retryTarget
            ? state.statusByNode.get(retryTarget.nodeId)?.get(retryTarget.engineType)
                  ?.pathManaged === true
            : false
    )
    const [askInstallPath, setAskInstallPath] = useState(false)
    const [askUninstallPath, setAskUninstallPath] = useState(false)
    const [removePath, setRemovePath] = useState(false)

    // Clearing the error unmounts this banner, so it waits for any PATH answer.
    const rerun = (run: () => void) => {
        onClear(error.id)
        run()
    }

    const handleRetry = () => {
        if (!retry) return
        if (retry.kind === 'run') {
            rerun(retry.run)
            return
        }
        const { engineType, nodeId } = retry
        const isLocal = nodeId === selfId
        if (retry.kind === 'install') {
            if (isLocal) setAskInstallPath(true)
            else rerun(() => window.pairApi.engines.install(engineType, nodeId, false))
            return
        }
        if (pathManaged) {
            // Unchecked: the failed attempt may have been one that kept the entry.
            setRemovePath(false)
            setAskUninstallPath(true)
        } else {
            rerun(() => window.pairApi.engines.uninstall(engineType, nodeId))
        }
    }

    return (
        <InlineErrorBanner severity={severity} message={displayMessage}>
            {retry && (
                <Button kind="primary" size="small" onClick={handleRetry}>
                    Retry
                </Button>
            )}
            {retryTarget && (
                <>
                    <EnginePathConsentModal
                        open={askInstallPath}
                        engines={[retryTarget.engineType]}
                        onAnswer={addToPath => {
                            setAskInstallPath(false)
                            rerun(() =>
                                window.pairApi.engines.install(
                                    retryTarget.engineType,
                                    retryTarget.nodeId,
                                    addToPath
                                )
                            )
                        }}
                        onCancel={() => setAskInstallPath(false)}
                    />
                    <ConfirmModal
                        open={askUninstallPath}
                        onOpenChange={setAskUninstallPath}
                        title="Uninstall"
                        message={`Retry uninstalling ${EngineDisplayNames[retryTarget.engineType]}?`}
                        confirmLabel="Uninstall"
                        confirmColor="danger"
                        option={{
                            label: `Also remove ${EngineDisplayNames[retryTarget.engineType]} from my PATH`,
                            checked: removePath,
                            onCheckedChange: setRemovePath
                        }}
                        onConfirm={() =>
                            rerun(() =>
                                window.pairApi.engines.uninstall(
                                    retryTarget.engineType,
                                    retryTarget.nodeId,
                                    removePath
                                )
                            )
                        }
                    />
                </>
            )}
        </InlineErrorBanner>
    )
}
