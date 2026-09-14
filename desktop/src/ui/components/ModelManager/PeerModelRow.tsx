// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, Text } from '@nvidia/foundations-react-core'
import type { EngineType } from '@/shared/types/engines'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'

/**
 * A model this node does not have, offered by a peer that does.
 *
 * The action copies it across the LAN rather than downloading it again: the
 * bytes are already on this network, the transfer is verified against each
 * blob's own content hash, and it works with no internet at all. That last
 * point is the reason this exists rather than reusing the ordinary pull.
 */
export function PeerModelRow({
    model,
    engineType,
    sourceNodeId,
    sourceNodeName,
    busy,
    onCopy
}: {
    model: string
    engineType: EngineType
    sourceNodeId: string
    sourceNodeName: string
    busy: boolean
    onCopy: (model: string, sourceNodeId: string) => void
}) {
    return (
        <Flex align="center" justify="between" gap="2" className="pl-2 min-w-0">
            <Text kind="body/regular/sm" className="text-subtle-color">
                {formatModelDisplayName(model, engineType)}
            </Text>
            <Button
                size="small"
                kind="secondary"
                disabled={busy}
                onClick={() => onCopy(model, sourceNodeId)}
            >
                {busy ? 'Copying…' : `Get from ${sourceNodeName}`}
            </Button>
        </Flex>
    )
}
