// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Stack, Text, TextInput } from '@nvidia/foundations-react-core'

export function PortRow({
    label,
    port,
    disabled,
    onPortChange,
    readOnly
}: {
    label: string
    port: string
    /** Unused when `readOnly`. Disables the input while a port op is in flight. */
    disabled?: boolean
    /** Unused when `readOnly`. */
    onPortChange?: (value: string) => void
    /** Render the value as static text (no input) — e.g. a remote node's ports. */
    readOnly?: boolean
}) {
    const controls = readOnly ? (
        // Match the editable input's height (size="small" ≈ 30px) so a static
        // value lines up vertically with an adjacent editable field.
        <Flex align="center" style={{ minHeight: 30 }}>
            <Text kind="body/semibold/sm">{port || '—'}</Text>
        </Flex>
    ) : (
        <TextInput
            value={port}
            onValueChange={onPortChange}
            disabled={disabled}
            size="small"
            className="w-18 min-w-18"
        />
    )

    if (!label) {
        return controls
    }

    return (
        <Stack gap="1">
            <Text kind="body/regular/sm" style={{ minWidth: 42 }}>
                {label}
            </Text>

            {controls}
        </Stack>
    )
}
