// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ClusterModelAliasViewEntry, ModelAlias } from '@/shared/types/cluster'
import type { JsonObject, JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'

/**
 * The cluster model alias document, checked at the stdio boundary.
 *
 * An alias is two names: the one a caller types and the model it stands for.
 * `nvpair-node-settings` stores those two fields under `model_aliases` and the
 * settings view edits the same pair, so nothing here translates spellings. What
 * it does instead is refuse, and there are two ways a document deserves that.
 *
 * One is a document that does not decode: a non-list, an entry with no name, an
 * entry with no model. The alias editor saves its whole list, so entries hidden
 * by a lenient parse would be deleted by the next save; the editor shows the
 * defect instead of editing behind it.
 *
 * The other is a document that decodes *cleanly* into something inert, which is
 * what the reads check for by requiring a model. A payload written for the
 * earlier provider-citing shape still decodes, into aliases that stand for no
 * model, and delivering those leaves a cluster that lists the names and routes
 * none of them.
 */

function asObject(value: JsonValue | undefined): JsonObject | null {
    if (value === undefined || value === null) return null
    if (typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

/** A usable name: present, a string, and not only spaces. */
function readName(container: JsonObject, key: string): string | null {
    const value = container[key]
    return typeof value === 'string' && value.trim() !== '' ? value : null
}

/**
 * Decode the `aliases` key of a `settings/get-cluster-model-aliases` reply.
 * Absence (`null` or a reply without the key) is a legitimately unconfigured
 * node, not an error.
 */
export function parseModelAliasesFromWire(raw: JsonValue | undefined): ModelAlias[] {
    if (raw === undefined || raw === null) return []
    if (!Array.isArray(raw)) {
        throw new Error('malformed model alias list: expected a list')
    }

    const aliases: ModelAlias[] = []
    for (const [index, entry] of raw.entries()) {
        const alias = asObject(entry)
        if (!alias) {
            throw new Error(`malformed model alias at index ${index}: expected an object`)
        }
        const name = readName(alias, 'name')
        if (name === null) {
            throw new Error(`malformed model alias at index ${index}: missing name`)
        }
        const model = readName(alias, 'model')
        if (model === null) {
            throw new Error(`malformed model alias "${name}": missing model`)
        }
        aliases.push({ name, model })
    }
    return aliases
}

/**
 * Decode the `aliases` key of a `router/get-aliases` reply into the cluster
 * alias view. Permissive where `parseModelAliasesFromWire` is strict, and the
 * editor behind this read is why: nothing writes back from the view, so one
 * malformed row — an older peer, a field that gained a shape — costs one row
 * on screen, not the whole card. The list itself must still be a list: a body
 * that cannot be a view at all is a failed read, and staying a rejection keeps
 * "cannot read" from being drawn as "no aliases anywhere".
 */
export function parseClusterModelAliasView(
    raw: JsonValue | undefined
): ClusterModelAliasViewEntry[] {
    if (raw === undefined || raw === null) return []
    if (!Array.isArray(raw)) {
        throw new Error('malformed cluster alias view: expected a list')
    }

    const entries: ClusterModelAliasViewEntry[] = []
    for (const entry of raw) {
        const alias = asObject(entry)
        if (!alias) continue
        const name = readName(alias, 'name')
        const model = readName(alias, 'model')
        if (name === null || model === null) continue
        entries.push({
            name,
            model,
            originNode: typeof alias.originNode === 'string' ? alias.originNode : '',
            originHost: typeof alias.originHost === 'string' ? alias.originHost : ''
        })
    }
    return entries
}

/**
 * Refuse an incomplete list before it reaches the settings service, so a
 * half-typed editor row cannot persist and then be announced to peers. The
 * rules and their wording mirror `validateModelAliases` in
 * `nvpair-node-settings` (which stays the authority for writes from other
 * origins); the checks run in the same two passes, so the first complaint the
 * editor shows is the one the service would have made.
 */
export function validateModelAliases(aliases: ModelAlias[]): string | null {
    // Two passes over the list, in the service's order: names are claimed
    // first, then targets are judged against them. Keeping the order keeps the
    // first complaint the editor shows the same one the service would make.
    const names = new Set<string>()
    for (const [index, alias] of aliases.entries()) {
        const name = alias.name.trim()
        if (name === '') {
            return `aliases[${index}]: name must not be empty`
        }
        if (alias.name !== name) {
            return (
                `aliases[${index}] (${alias.name}): name must not have leading or trailing ` +
                'spaces; the cluster endpoint matches the name a caller types exactly, so a ' +
                'padded name is a row that answers nothing'
            )
        }
        if (names.has(alias.name)) {
            return `aliases[${index}]: duplicate name "${alias.name}"`
        }
        names.add(alias.name)
    }

    for (const [index, alias] of aliases.entries()) {
        const target = alias.model.trim()
        if (target === '') {
            return (
                `aliases[${index}] (${alias.name}): model must name the model this alias ` +
                'stands for; an alias with no target is not an alias, it is a typo'
            )
        }
        if (alias.model !== target) {
            return (
                `aliases[${index}] (${alias.name}): model "${alias.model}" must not have ` +
                'leading or trailing spaces; an engine is asked for that name exactly as ' +
                'stored, so a padded target resolves nowhere'
            )
        }
        if (target === alias.name) {
            return (
                `aliases[${index}] (${alias.name}): model must name a different model; ` +
                'an alias that stands for itself routes nothing'
            )
        }
        if (names.has(target)) {
            return (
                `aliases[${index}] (${alias.name}): model "${target}" is another alias's ` +
                'name; an alias stands for a model, not for another alias'
            )
        }
    }
    return null
}

/**
 * Encode the editor's draft as the document the settings service stores. Both
 * sides spell the two fields the same way, so this copies rather than renames:
 * it keeps the persisted document a document — one built here, not the
 * draft's own objects handed to the transport by reference.
 */
export function toModelAliasesWire(aliases: ModelAlias[]): JsonValue[] {
    return aliases.map(alias => ({ name: alias.name, model: alias.model }))
}
