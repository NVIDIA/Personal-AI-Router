// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useId, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import {
    Button,
    Flex,
    FormField,
    Panel,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import type { ModelAlias } from '@/shared/types/cluster'
import { AddCircle } from '@/ui/components/icons'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import deepEqual from '@/ui/utils/deep-equal'

/**
 * The model alias list editor: one box per synonym, each naming the model that
 * name stands for, over a document that is saved whole.
 *
 * Exactly one surface edits an alias document — a node's card in the Overview,
 * which edits that node's list whether the node is this computer or a paired
 * one — so the rows, the validation and the save semantics are written once
 * here, and the difference between the two lists stays in whose list it is and
 * what saving it costs.
 *
 * A row deliberately names no node and no engine. Each computer's cluster
 * router decides which hosts to try from what its own catalog sees, which is
 * why a list written on one machine routes on machines that machine knows
 * nothing about. What a row cannot do is guess: a synonym whose model no
 * reachable host advertises stays out of the endpoint's model list, so a name
 * typed here is answered by a machine that really has that model or refused,
 * never rerouted onto whatever else happens to be free. That is why the Model
 * name field suggests the models this computer's catalog has seen and still
 * accepts any free text — an alias may name a model that is merely not loaded
 * yet, and refusing the write would block that legitimate case.
 *
 * Saving replaces everything the node held, so every row that will survive is
 * on screen, and the rules shown here only mirror the settings service's: that
 * service is the authority on whatever machine owns the list, and a refusal
 * from there is shown in its own words over a draft left open to fix.
 */

function cloneAliases(aliases: ModelAlias[]): ModelAlias[] {
    return aliases.map(alias => ({ ...alias }))
}

interface ModelAliasEditorProps {
    /**
     * The document as the node owns it. A list saying something different is
     * adopted — a save that persisted, a newer read — so what is on screen
     * follows the node rather than whatever was last typed at it. The same rows
     * in a new array are not a different document, and do not touch a draft in
     * progress.
     */
    initial: ModelAlias[]
    /** A read or a write is in flight: every control waits with it. */
    busy: boolean
    error: string | null
    onDismissError: () => void
    /**
     * Store this whole list. Resolves with what the node holds afterwards —
     * what the caller read back, or the list it wrote when the node confirms
     * without repeating it — and with `null` when the write did not land and
     * the caller has already said why. Only the first ends the draft: a failed
     * write leaves the rows on screen to fix, not to retype.
     */
    onSave: (aliases: ModelAlias[]) => Promise<ModelAlias[] | null>
    /** Whose document this is and how current it is, in the caller's words. */
    notice?: ReactNode
}

export default function ModelAliasEditor({
    initial,
    busy,
    error,
    onDismissError,
    onSave,
    notice
}: ModelAliasEditorProps): React.JSX.Element {
    const [base, setBase] = useState<ModelAlias[]>(() => cloneAliases(initial))
    const [draft, setDraft] = useState<ModelAlias[]>(() => cloneAliases(initial))

    // Id of the one suggestion list every Model name field points at. Two node
    // cards can be open at once, so a fixed id would hand one card's suggestions
    // to the other's fields.
    const suggestionsId = useId()

    // The picker lists every model name the engine-models feed has seen — this
    // node's engines and the cluster peers' announced inventories, deduplicated
    // across nodes and engines. The selector flattens to a joined string so the
    // subscription only re-renders when the name set actually changes, following
    // the fingerprint pattern in NodeList/NodeEngineSettings.tsx.
    const knownModelNamesKey = useEngineModelsStore(state => {
        const names = new Set<string>()
        for (const entry of state.models.values()) {
            for (const model of entry.models) names.add(model.name)
        }
        return Array.from(names).sort().join('\n')
    })

    // The rows are re-seeded when the list behind them genuinely changed, not
    // whenever the caller hands over a new array. A peer's list is read from its
    // node record, and that record is rebuilt on every poll for models, loaded
    // state and hardware, so adopting on identity alone would wipe a half-typed
    // draft on a refresh that has nothing to do with aliases — and disable Save
    // while it did. Content is what "a newer read" means here. `seededFrom` is
    // only ever compared, never displayed, so it needs no clone.
    const seededFrom = useRef<ModelAlias[]>(initial)
    useEffect(() => {
        if (deepEqual(seededFrom.current, initial)) return
        seededFrom.current = initial
        setBase(cloneAliases(initial))
        setDraft(cloneAliases(initial))
    }, [initial])

    const dirty = !deepEqual(draft, base)
    // A row is only worth saving when both halves are typed. Which models that
    // target turns out to reach is the router's answer, not a precondition of
    // editing one, so nothing here waits for a catalog.
    const rowsComplete = draft.every(alias => alias.name.trim() !== '' && alias.model.trim() !== '')
    const uniqueNames = new Set(draft.map(alias => alias.name)).size === draft.length

    function patchAlias(index: number, patch: Partial<ModelAlias>): void {
        setDraft(current =>
            current.map((alias, i) => (i === index ? { ...alias, ...patch } : alias))
        )
    }

    function removeAlias(index: number): void {
        setDraft(current => current.filter((_, i) => i !== index))
    }

    async function save(): Promise<void> {
        const written = cloneAliases(draft)
        let persisted: ModelAlias[] | null
        try {
            persisted = await onSave(written)
        } catch {
            // onSave is expected to answer a failed write with null, not with a
            // rejection; either way the draft stays, because the operator's next
            // move is to fix the row, not to type it again.
            return
        }
        if (persisted === null) return
        setBase(cloneAliases(persisted))
        setDraft(cloneAliases(persisted))
    }

    function discard(): void {
        setDraft(cloneAliases(base))
        onDismissError()
    }

    return (
        <Stack gap="3">
            {notice && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {notice}
                </Text>
            )}

            {error && (
                <InlineErrorBanner severity="error" message={error} onClose={onDismissError} />
            )}

            {/* One suggestion list for every row: a native datalist keeps the
                field a text input, so a name outside the list stays typeable,
                while typing filters it by substring. */}
            <datalist id={suggestionsId}>
                {knownModelNamesKey === ''
                    ? null
                    : knownModelNamesKey
                          .split('\n')
                          .map(name => <option key={name} value={name} />)}
            </datalist>

            {draft.length === 0 && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    No aliases yet.
                </Text>
            )}

            {draft.map((alias, index) => (
                <Panel key={`alias-${index}`} density="compact" elevation="high">
                    <Stack gap="2">
                        {/* font-mono pairs with pair-underscore-safe (styles/index.css):
                            the stock small shell leaves too little height for the
                            monospace underscore glyph to render whole. */}
                        <FormField slotLabel="Model alias">
                            <TextInput
                                size="small"
                                aria-label={`Alias name ${index + 1}`}
                                placeholder="Friendly model name, e.g. qwen38-flash-next"
                                value={alias.name}
                                onValueChange={value => patchAlias(index, { name: value })}
                                className="font-mono pair-underscore-safe"
                            />
                        </FormField>
                        <FormField slotLabel="Model name">
                            <TextInput
                                size="small"
                                aria-label={`Target model ${index + 1}`}
                                list={suggestionsId}
                                placeholder="e.g. qwen38-fn-ud-q4_k_xl"
                                value={alias.model}
                                onValueChange={value => patchAlias(index, { model: value })}
                                className="font-mono pair-underscore-safe"
                            />
                        </FormField>
                        <Flex justify="end">
                            <Button
                                kind="tertiary"
                                color="danger"
                                size="small"
                                aria-label={`Delete alias ${index + 1}`}
                                disabled={busy}
                                onClick={() => removeAlias(index)}
                            >
                                Delete alias
                            </Button>
                        </Flex>
                    </Stack>
                </Panel>
            ))}

            <Button
                kind="primary"
                color="brand"
                size="small"
                disabled={busy}
                onClick={() => setDraft(current => [...current, { name: '', model: '' }])}
            >
                <AddCircle style={{ fontSize: 16 }} />
                Add alias
            </Button>

            {(!rowsComplete || !uniqueNames) && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {!uniqueNames
                        ? 'Model aliases must be unique.'
                        : 'Fill in both the model alias and the model name — or delete the box.'}
                </Text>
            )}

            <Flex gap="2" justify="end">
                <Button kind="secondary" size="small" disabled={!dirty || busy} onClick={discard}>
                    Discard
                </Button>
                <Button
                    kind="primary"
                    color="brand"
                    size="small"
                    disabled={!dirty || !rowsComplete || !uniqueNames || busy}
                    onClick={() => void save()}
                >
                    Save
                </Button>
            </Flex>
        </Stack>
    )
}
