// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react'
import {
    Button,
    Flex,
    FormField,
    ModalContent,
    ModalDialog,
    ModalHeading,
    ModalRoot,
    Stack,
    Text,
    TextArea,
    TextInput
} from '@nvidia/foundations-react-core'
import type { EngineType } from '@/shared/types/engines'
import type {
    EngineSettingsConfig,
    EngineSettingsRequest,
    EngineSettingsSnapshot
} from '@/shared/types/engine-settings'
import {
    engineSettingsKey,
    useEngineSettingsStore,
    watchEngineSettings
} from '@/ui/stores/engine-settings.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { ConfirmModal } from '@/ui/components/ConfirmModal'
import getErrorString from '@/shared/utils/get-error-string'
import { engineManagerName } from '@/shared/utils/engines'

interface Draft {
    serverPort: string
    proxyPort: string
    launchText: string
}
const asDraft = (config: EngineSettingsConfig): Draft => ({
    ...config,
    serverPort: String(config.serverPort),
    proxyPort: String(config.proxyPort)
})
const sameDraft = (a: Draft, b: Draft): boolean =>
    a.serverPort === b.serverPort && a.proxyPort === b.proxyPort && a.launchText === b.launchText

export function EngineSettingsSection({
    nodeId,
    engineType,
    disabled
}: {
    nodeId: string
    engineType: EngineType
    disabled: boolean
}) {
    const engine = engineManagerName(engineType)
    const target = { nodeId, engine }
    const entry = useEngineSettingsStore(state => state.entries[engineSettingsKey(target)])
    const snapshot = entry?.snapshot
    const connected = useConnectionStore(state => state.connected)
    const isLocalNode = useConnectionStore(state => state.selfId === nodeId)
    const [base, setBase] = useState<EngineSettingsSnapshot>()
    const [draft, setDraft] = useState<Draft>({ serverPort: '', proxyPort: '', launchText: '' })
    const [errorDialog, setErrorDialog] = useState('')
    const [confirmation, setConfirmation] = useState<EngineSettingsRequest>()
    const [checking, setChecking] = useState(false)
    const generation = useRef(0)
    // Response correlation, not loading state: which of this editor's requests
    // a terminal snapshot belongs to, so another editor's result never resets
    // this draft. Operation progress lives in the pending-actions store.
    const submitted = useRef('')
    const serverPortEdited = useRef(false)
    const dirty = !!base && !sameDraft(draft, asDraft(base.settings))
    const stale =
        !!base &&
        !!snapshot &&
        (base.revision !== snapshot.revision || base.epoch !== snapshot.epoch)
    const applying =
        usePendingActionsStore(state => state.isSettingsPending(nodeId, engineType)) ||
        snapshot?.phase === 'applying'
    const readOnly =
        !connected ||
        !!entry?.unavailable ||
        !snapshot?.editable ||
        snapshot.format !== 'pair-arguments-v1'
    const locked = readOnly || disabled || applying

    useEffect(() => watchEngineSettings(), [])
    useEffect(() => {
        generation.current++
        if (connected) void useEngineSettingsStore.getState().reload({ nodeId, engine })
        else useEngineSettingsStore.getState().disconnect()
    }, [nodeId, engine, connected])

    useEffect(() => {
        if (!snapshot) return
        const ownResult =
            !!submitted.current &&
            submitted.current === snapshot.requestId &&
            (snapshot.phase === 'succeeded' || snapshot.phase === 'failed')
        if (!base || !dirty || ownResult) {
            setBase(snapshot)
            setDraft(asDraft(snapshot.settings))
            if (ownResult) {
                submitted.current = ''
                if (snapshot.error) setErrorDialog(snapshot.error)
            }
        }
    }, [snapshot, base, dirty])

    function change(field: keyof Draft, value: string): void {
        generation.current++
        if (field === 'serverPort') serverPortEdited.current = true
        setDraft(current => ({ ...current, [field]: value }))
    }
    function reportError(message: string): void {
        generation.current++
        setChecking(false)
        setErrorDialog(message)
    }
    async function check(commit = false): Promise<void> {
        if (!base || locked || errorDialog || confirmation || checking) return
        if (stale) {
            if (commit)
                reportError('Settings changed elsewhere. Reload saved settings before applying.')
            return
        }
        const id = ++generation.current
        const serverPort = Number(draft.serverPort)
        const proxyPort = Number(draft.proxyPort)
        if (
            ![serverPort, proxyPort].every(
                port => Number.isInteger(port) && port >= 1 && port <= 65535
            )
        ) {
            if (commit) reportError('Enter ports from 1 to 65535.')
            return
        }
        if (commit) setChecking(true)
        const useServerPort = serverPortEdited.current
        const request: EngineSettingsRequest = {
            ...target,
            expectedRevision: base.revision,
            settings: { serverPort, proxyPort, launchText: draft.launchText },
            resolution: useServerPort ? 'server' : 'launch'
        }
        try {
            const result = await window.pairApi.engines.previewSettings(request)
            if (generation.current !== id) return
            if (result.conflict) {
                if (commit)
                    reportError(
                        'The server ports do not match. Update the server port and try again.'
                    )
                return
            }
            if (Object.keys(result.errors).length) {
                if (commit) reportError(Object.values(result.errors).join(' '))
                return
            }
            // Keep the user's command formatting unless the port field needs
            // to update it. Quiet blur validation never adds text or dialogs.
            setDraft(current => ({
                ...asDraft(result.settings),
                launchText: useServerPort ? result.settings.launchText : current.launchText
            }))
            serverPortEdited.current = false
            if (commit) {
                const confirmedRequest = {
                    ...request,
                    settings: result.settings,
                    resolution: undefined,
                    requestId: crypto.randomUUID()
                }
                if (result.restart) setConfirmation(confirmedRequest)
                else await apply(confirmedRequest)
            }
        } catch (cause) {
            if (commit && generation.current === id) reportError(getErrorString(cause))
        } finally {
            if (commit) setChecking(false)
        }
    }
    function reloadDraft(): void {
        generation.current++
        serverPortEdited.current = false
        if (snapshot) {
            setBase(snapshot)
            setDraft(asDraft(snapshot.settings))
        }
        setConfirmation(undefined)
        void useEngineSettingsStore.getState().reload(target)
    }
    async function apply(request: EngineSettingsRequest): Promise<void> {
        const requestId = request.requestId ?? crypto.randomUUID()
        submitted.current = requestId
        const pending = usePendingActionsStore.getState()
        // Hold the controls from the send until the backend's first snapshot
        // for this request, after which snapshot.phase is authoritative.
        pending.beginSettings(nodeId, engineType, requestId)
        try {
            await useEngineSettingsStore.getState().apply({ ...request, requestId })
        } catch (cause) {
            pending.endSettings(nodeId, engineType, requestId)
            reportError(getErrorString(cause))
        }
    }

    return (
        <details className="pair-accordion translucent-bg-accordion">
            <summary className="pair-accordion-summary">
                <Text kind="body/semibold/sm">Settings</Text>
            </summary>
            <Stack gap="3" className="p-3">
                {!applying && (entry?.unavailable || readOnly) && (
                    <Text kind="body/regular/sm">
                        {entry?.unavailable ||
                            snapshot?.reason ||
                            'Waiting for this device’s settings. Older devices require an update to support editing.'}
                    </Text>
                )}
                {!readOnly && !isLocalNode && (
                    <Text kind="body/regular/sm">
                        Ports and options can be changed from here. Managed CORS origin settings
                        must be changed on the device running the engine.
                    </Text>
                )}
                <Flex gap="3">
                    <FormField slotLabel="Server port">
                        <TextInput
                            size="small"
                            aria-label="Server port"
                            value={draft.serverPort}
                            onValueChange={value => change('serverPort', value)}
                            onBlur={() => {
                                if (dirty) void check()
                            }}
                            disabled={locked}
                            inputMode="numeric"
                        />
                    </FormField>
                    <FormField slotLabel="Proxy port">
                        <TextInput
                            size="small"
                            aria-label="Proxy port"
                            value={draft.proxyPort}
                            onValueChange={value => change('proxyPort', value)}
                            onBlur={() => {
                                if (dirty) void check()
                            }}
                            disabled={locked}
                            inputMode="numeric"
                        />
                    </FormField>
                </Flex>
                <FormField slotLabel="Engine arguments">
                    <TextArea
                        size="small"
                        aria-label="Engine arguments"
                        placeholder="Engine arguments; optional NAME=value assignments first"
                        value={draft.launchText}
                        onValueChange={value => change('launchText', value)}
                        onBlur={() => {
                            if (dirty) void check()
                        }}
                        resizeable="auto"
                        rows={4}
                        maxLength={16384}
                        disabled={locked}
                        className="font-mono"
                    />
                </FormField>
                <Flex gap="2" justify="end">
                    <Button kind="secondary" size="small" disabled={applying} onClick={reloadDraft}>
                        {dirty || stale ? 'Reload saved settings' : 'Refresh'}
                    </Button>
                    <Button
                        kind="primary"
                        color="brand"
                        size="small"
                        disabled={locked || checking || (!dirty && snapshot?.phase !== 'failed')}
                        className="relative"
                        aria-label="Apply"
                        aria-busy={applying || checking}
                        // Keep focus in the field being edited. A primary-button
                        // press would otherwise blur it first, and that blur
                        // fires its own `check()` — the click then arrives while
                        // `checking` is true and is dropped.
                        onPointerDown={event => {
                            if (event.button === 0) event.preventDefault()
                        }}
                        onClick={() => void check(true)}
                    >
                        <span className={applying || checking ? 'invisible' : undefined}>
                            Apply
                        </span>
                        {(applying || checking) && (
                            <span
                                className="absolute inset-0 flex items-center justify-center"
                                aria-hidden="true"
                            >
                                <span className="spinner-element-small" />
                            </span>
                        )}
                    </Button>
                </Flex>
            </Stack>
            <ModalRoot
                open={!!errorDialog}
                onOpenChange={open => {
                    if (!open) setErrorDialog('')
                }}
            >
                <ModalDialog>
                    <ModalContent className="no-drag-elements max-content-modal">
                        <ModalHeading>Could not apply engine settings</ModalHeading>
                        <Stack gap="4">
                            <Text kind="body/regular/sm" role="alert">
                                {errorDialog}
                            </Text>
                            <Flex justify="end">
                                <Button
                                    kind="primary"
                                    color="brand"
                                    size="small"
                                    onClick={() => setErrorDialog('')}
                                >
                                    OK
                                </Button>
                            </Flex>
                        </Stack>
                    </ModalContent>
                </ModalDialog>
            </ModalRoot>
            <ConfirmModal
                open={!!confirmation}
                onOpenChange={open => {
                    if (!open) setConfirmation(undefined)
                }}
                title="Restart engine?"
                confirmLabel="Restart"
                message="Applying these arguments will restart your engine, continue?"
                onConfirm={() => {
                    if (confirmation) void apply(confirmation)
                }}
            />
        </details>
    )
}
