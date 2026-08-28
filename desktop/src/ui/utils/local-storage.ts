// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Canonical renderer `localStorage` I/O (Electron + browser).
 *
 * **Pattern**
 * - This module: safe get/set (guards missing `localStorage`, quota, private mode).
 * - Domain files (`*-storage.ts`): storage keys + `parse(raw: unknown) => T` validators that do not throw.
 * - `useLocalStorageState`: React state mirrored to one JSON key via `getLocalStorageJson` / `setLocalStorageJson`.
 *
 * Zustand `persist` is not required for small prefs; use it only when the state already lives in a zustand store
 * and you want middleware-based hydration — still call `getLocalStorageItem` / `setLocalStorageItem` in a
 * custom storage adapter if you need the same guards as here.
 */

function getLocalStorageItem(key: string): string | null {
    if (typeof localStorage === 'undefined') return null
    try {
        return localStorage.getItem(key)
    } catch {
        return null
    }
}

function setLocalStorageItem(key: string, value: string): void {
    if (typeof localStorage === 'undefined') return
    try {
        localStorage.setItem(key, value)
    } catch {
        /* quota / private mode */
    }
}

/** Returns `undefined` if missing, empty, or invalid JSON. */
export function getLocalStorageJson(key: string): unknown | undefined {
    const raw = getLocalStorageItem(key)
    if (raw === null || raw === '') return undefined
    try {
        const parsed: unknown = JSON.parse(raw)
        return parsed
    } catch {
        return undefined
    }
}

export function setLocalStorageJson(key: string, value: unknown): void {
    try {
        setLocalStorageItem(key, JSON.stringify(value))
    } catch {
        /* circular structure / quota */
    }
}

/** Own-property read for `parse(raw: unknown)` helpers (no prototype chain walk). */
export function readOwnProperty(obj: object, key: string): unknown {
    if (!Object.prototype.hasOwnProperty.call(obj, key)) return undefined
    const d = Object.getOwnPropertyDescriptor(obj, key)
    return d === undefined ? undefined : d.value
}
