// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Minimal JSON value type for safely narrowing `JSON.parse` results without
 * casts or `unknown` in signatures. Shared by the CLI client and the in-app
 * control server (the Electron service bridge has its own copy in
 * `json-rpc-subprocess.ts` for the stdio plane).
 */
export type JsonValue =
    | string
    | number
    | boolean
    | null
    | JsonValue[]
    | { [key: string]: JsonValue }
