// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import {
    Button,
    Checkbox,
    Flex,
    FormField,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import {
    validateCustomEndpointForm,
    validateCustomEndpointKeyFields
} from '@/ui/utils/custom-endpoints'
import type { CustomEndpointDraft } from '@/ui/api/custom-endpoints-api'
import type { CustomEndpointInfo } from '@/shared/types/custom-endpoints'
import getErrorString from '@/shared/utils/get-error-string'

/**
 * Add or edit form for one custom endpoint. The URL and Port fields stay
 * separate in the UI even though the broker composes them into one baseUrl:
 * the port is a mandatory field that is never inferred, and a port embedded
 * in a pasted url must exactly agree with the Port field. The API key field
 * masks by default; it rides the add request — local IPC, or the pinned
 * control channel when a peer is addressed — and is never logged.
 *
 * With `editing` set the same request edits that endpoint in place
 * (`replaceId`) instead of forking a second entry for the address. Key
 * semantics: a blank key field keeps the stored credential, a typed key
 * replaces it, and the Remove saved key checkbox (visible only when a key is
 * stored) clears it via `keyAction:"clear"`. The Show/Hide control belongs to
 * the field's own text, not to the stored credential: a stored key never
 * enters this input, so with nothing typed there is nothing to unmask and the
 * control stays dark rather than offering a reveal that can only return an
 * empty field. Typing a key and clearing in
 * one submit is refused up front by `validateCustomEndpointKeyFields` — the
 * broker would otherwise have to pick a winner. A rejected save keeps the
 * editor open with the broker's reason, so nothing the user typed is lost;
 * a successful one calls `onDone` with the stored entry and it is the
 * owner's call whether the editor closes.
 */

interface CustomEndpointFormProps {
    /** The node whose broker stores the endpoint: this node, or — in a
     *  peer's node card — the peer itself, over the control channel. */
    nodeId?: string
    /** Present in edit mode: the endpoint this form replaces. */
    editing?: CustomEndpointInfo
    /** Called after a successful add or save, with the stored entry as the
     *  broker replied (its id is the one worth re-probing). */
    onDone: (stored: CustomEndpointInfo) => void
}

/**
 * Port-field text for an existing baseUrl: its explicit port, else the
 * scheme's default. An edit starts from the address the endpoint is actually
 * dialled at — the field stays truth-prefilled rather than re-asking, and
 * the url/port agreement check still applies to anything typed over it.
 */
function portTextOf(baseUrl: string): string {
    try {
        const parsed = new URL(baseUrl)
        if (parsed.port) return parsed.port
        return parsed.protocol === 'http:' ? '80' : '443'
    } catch {
        return ''
    }
}

export function CustomEndpointAddForm({ nodeId, editing, onDone }: CustomEndpointFormProps) {
    const keySet = editing?.keySet ?? false
    const [url, setUrl] = useState(editing?.baseUrl ?? '')
    const [port, setPort] = useState(() => (editing ? portTextOf(editing.baseUrl) : '8000'))
    const [name, setName] = useState(editing?.name ?? '')
    const [apiKey, setApiKey] = useState('')
    const [showKey, setShowKey] = useState(false)
    const [clearKey, setClearKey] = useState(false)
    const [submitting, setSubmitting] = useState(false)
    const [error, setError] = useState<string | null>(null)

    const submit = useCallback(async () => {
        if (submitting) return
        const formErrors = validateCustomEndpointForm({ url, port })
        const keyErrors = validateCustomEndpointKeyFields({ keySet, apiKey, clearKey })
        const shown = [formErrors.mismatch, formErrors.url, formErrors.port, keyErrors.key]
            .filter(Boolean)
            .join(' ')
        if (shown) {
            setError(shown)
            return
        }
        setError(null)
        setSubmitting(true)
        try {
            const draft: CustomEndpointDraft = {
                url: url.trim(),
                port: Number(port.trim())
            }
            if (name.trim()) draft.name = name.trim()
            if (apiKey) draft.apiKey = apiKey
            if (editing) {
                draft.replaceId = editing.id
                if (clearKey) draft.keyAction = 'clear'
            }
            const stored = await window.pairApi.customEndpoints.add(draft, nodeId)
            setUrl('')
            setPort('8000')
            setName('')
            setApiKey('')
            setShowKey(false)
            setClearKey(false)
            onDone(stored)
        } catch (cause) {
            // The broker's reason (e.g. loopback self-forward, port conflict)
            // is user-facing; show it rather than silently dropping the add.
            setError(getErrorString(cause))
        } finally {
            setSubmitting(false)
        }
    }, [url, port, name, apiKey, clearKey, keySet, submitting, nodeId, editing, onDone])

    const canSubmit = url.trim() !== '' && port.trim() !== '' && !submitting
    // Show/Hide acts on the text in the field, not on the endpoint's stored
    // credential, so it follows what has been typed: a saved key never enters
    // this input, which means an empty field has nothing to unmask and an
    // enabled control promised a reveal that can only return a blank field.
    const keyEntered = apiKey.trim() !== ''
    const keyRevealed = keyEntered && showKey

    return (
        <Stack gap="2">
            {error ? <InlineErrorBanner message={error} onClose={() => setError(null)} /> : null}
            <Flex gap="2" align="end">
                <FormField slotLabel="Endpoint URL" className="flex-1">
                    <TextInput
                        value={url}
                        onValueChange={setUrl}
                        placeholder="https://api.example.com/v1"
                        disabled={submitting}
                        onKeyDown={event => {
                            if (event.key === 'Enter' && canSubmit) void submit()
                        }}
                    />
                </FormField>
                <FormField slotLabel="Port" className="w-24">
                    <TextInput
                        value={port}
                        onValueChange={setPort}
                        inputMode="numeric"
                        disabled={submitting}
                        onKeyDown={event => {
                            if (event.key === 'Enter' && canSubmit) void submit()
                        }}
                    />
                </FormField>
            </Flex>
            <Flex gap="2" align="end">
                <FormField slotLabel="Name (optional)" className="flex-1">
                    <TextInput
                        value={name}
                        onValueChange={setName}
                        placeholder="Shown in model lists instead of the address"
                        disabled={submitting}
                    />
                </FormField>
                <FormField slotLabel="API key (optional)" className="flex-1">
                    <TextInput
                        value={apiKey}
                        onValueChange={setApiKey}
                        // A stored credential reads as the field's placeholder,
                        // not helper text: "leave blank to keep it" is exactly
                        // the empty-field meaning, and a placeholder clears
                        // itself the moment the user types a replacement key.
                        placeholder={keySet ? 'Key set — leave blank to keep it' : undefined}
                        type={keyRevealed ? 'text' : 'password'}
                        autoComplete="off"
                        disabled={submitting}
                    />
                </FormField>
                <Button
                    kind="secondary"
                    size="small"
                    onClick={() => setShowKey(value => !value)}
                    disabled={!keyEntered}
                    aria-label={keyRevealed ? 'Hide API key' : 'Show API key'}
                >
                    {keyRevealed ? 'Hide' : 'Show'}
                </Button>
            </Flex>
            {keySet ? (
                <Flex gap="2" align="center">
                    <Checkbox
                        checked={clearKey}
                        // The library's checked state can be indeterminate; a
                        // click always lands on checked or unchecked, and
                        // only `true` means "remove the saved key".
                        onCheckedChange={checked => setClearKey(checked === true)}
                        aria-label="Remove saved key"
                    />
                    <Text kind="body/regular/sm">Remove saved key</Text>
                </Flex>
            ) : null}
            <Flex justify="end">
                <Button
                    kind="primary"
                    color="brand"
                    size="small"
                    disabled={!canSubmit}
                    onClick={() => void submit()}
                >
                    <span className={submitting ? 'invisible' : undefined}>
                        {editing ? 'Save changes' : 'Add endpoint'}
                    </span>
                    {submitting && (
                        <span className="spinner-element-small" role="status" aria-label="" />
                    )}
                </Button>
            </Flex>
        </Stack>
    )
}
