// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex } from '@nvidia/foundations-react-core'
import { Check } from '@/ui/components/icons'
import { PortRow } from './PortRow'

export function BackendPorts({
    proxyPort,
    serverPort,
    changed,
    disabled,
    isLocalNode,
    showServerPort,
    onServerChange,
    onProxyChange,
    onApply
}: {
    proxyPort: string
    serverPort: string
    /** True when either port differs from the backend-reported value. */
    changed: boolean
    disabled: boolean
    isLocalNode: boolean
    showServerPort: boolean
    onServerChange: (v: string) => void
    onProxyChange: (v: string) => void
    onApply: () => void
}) {
    return (
        // align="end" bottom-aligns the (label-less) Apply button with the port
        // inputs, which sit below their labels.
        <Flex
            gap="6"
            align="end"
            style={{
                opacity: disabled ? 0.5 : 1,
                pointerEvents: disabled ? 'none' : 'auto'
            }}
        >
            {/* The proxy port persists via the broker's <engine>-proxy:set-port
                (steered clear of running engine ports), so it is editable on the
                local node. Only the proxy-fronted engines (Ollama, LM Studio)
                report a proxy port; other engines report none. Remote nodes are
                read-only — set-port only rebinds the local broker's proxy. */}
            {proxyPort &&
                (isLocalNode ? (
                    <PortRow
                        label="Proxy"
                        port={proxyPort}
                        disabled={disabled}
                        onPortChange={onProxyChange}
                    />
                ) : (
                    <PortRow label="Proxy" port={proxyPort} readOnly />
                ))}
            {showServerPort &&
                (isLocalNode ? (
                    <PortRow
                        label="Server"
                        port={serverPort}
                        disabled={disabled}
                        onPortChange={onServerChange}
                    />
                ) : (
                    // Remote nodes are read-only (the engine-manager only
                    // mutates the local node), so show the value statically
                    // instead of a disabled input.
                    <PortRow label="Server" port={serverPort} readOnly />
                ))}

            {/* One apply on the same row: the bridge diffs the ports and runs a
                single safe transaction (proxy and/or engine, incl. a swap), so
                the user never has to apply two rows in the right order. */}
            {isLocalNode && (
                <Button
                    kind="primary"
                    color="brand"
                    size="small"
                    onClick={onApply}
                    disabled={disabled || !changed}
                >
                    {disabled ? (
                        <Flex align="center" gap="2">
                            <span className="spinner-element" role="status" aria-label="" />
                            Applying…
                        </Flex>
                    ) : (
                        <Flex align="center" gap="2">
                            <Check style={{ fontSize: 16 }} />
                            Apply ports
                        </Flex>
                    )}
                </Button>
            )}
        </Flex>
    )
}
