// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState, type Dispatch, type SetStateAction } from 'react'
import { getLocalStorageJson, setLocalStorageJson } from '@/ui/utils/local-storage'

interface UseLocalStorageStateOptions<T> {
    /**
     * Validate / merge parsed JSON into `T`. Must not throw.
     */
    parse: (raw: unknown) => T
}

function readStored<T>(key: string, fallback: T, parse: (raw: unknown) => T): T {
    const parsed = getLocalStorageJson(key)
    if (parsed === undefined) return fallback
    return parse(parsed)
}

/**
 * State synced to `localStorage` under `key` via `setLocalStorageJson` / `getLocalStorageJson`.
 * Writes on every state change (including initial match after first paint).
 */
export function useLocalStorageState<T>(
    key: string,
    defaultValue: T,
    options: UseLocalStorageStateOptions<T>
): [T, Dispatch<SetStateAction<T>>] {
    const [state, setState] = useState<T>(() => readStored(key, defaultValue, options.parse))

    useEffect(() => {
        setLocalStorageJson(key, state)
    }, [key, state])

    return [state, setState]
}
