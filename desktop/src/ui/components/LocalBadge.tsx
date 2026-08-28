// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Text } from '@nvidia/foundations-react-core'

/**
 * Small brand-tinted pill marking the local (this-machine) node in node lists.
 */
export function LocalBadge() {
    return (
        <Text
            kind="body/regular/xs"
            className="shrink-0 rounded-full px-1.5 py-0.5 uppercase"
            style={{
                color: 'var(--color-brand, #76b900)',
                backgroundColor: 'color-mix(in srgb, var(--color-brand, #76b900) 18%, transparent)',
                letterSpacing: '0.04em',
                lineHeight: 1
            }}
        >
            Local
        </Text>
    )
}
