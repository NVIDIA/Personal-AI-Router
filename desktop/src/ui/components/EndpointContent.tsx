// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { ContentCopy, Route } from './icons'

import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { EnabledEngineTypes, EngineDisplayNames } from '@/shared/constants/engines'
import { useCallback, useMemo, useState } from 'react'
import EngineIcon from './EngineIcon'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import { getEnginesForNode, isEngineTypeRunningClusterWide } from '@/ui/utils/get-engines-for-node'
import { gatewayEndpointDisplayUrl } from '@/ui/utils/gateway-inference-paths'
import type { EngineType } from '@/shared/types/engines'

function createCopiedState(enabledEngines: EngineType[]): Record<string, boolean> {
    const copiedState: Record<string, boolean> = {}
    for (const type of enabledEngines) {
        copiedState[EngineDisplayNames[type]] = false
    }
    return copiedState
}

/** Doubles as the copy-state key for the cluster routing row, so it is one name. */
const CLUSTER_ENDPOINT_NAME = 'Cluster routing'

export default function EndpointContent({
    selfId,
    clusterPort,
    className
}: {
    selfId: string
    /**
     * Port the cluster routing endpoint actually answers on, or `null` when this
     * computer does not serve one. Reported by the proxy rather than read from
     * the configured port, so a port somebody else holds is not listed here.
     */
    clusterPort: number | null
    className?: string
}) {
    const [copied, setCopied] = useState<Record<string, boolean>>(() =>
        createCopiedState(EnabledEngineTypes)
    )
    const statusByNode = useEngineStatusStore(s => s.statusByNode)
    const selfEngines = useMemo(
        () => getEnginesForNode(selfId, statusByNode, EnabledEngineTypes),
        [selfId, statusByNode]
    )
    const allBackends = useMemo(() => {
        const endpoints = EnabledEngineTypes.map(type => {
            const backend = selfEngines.find(b => b.engineType === type)
            const isAvailable = isEngineTypeRunningClusterWide(statusByNode, type)

            if (!backend || !backend.proxyPort || !isAvailable) {
                return null
            }

            const port = backend.proxyPort
            return {
                name: EngineDisplayNames[type],
                proxyPort: port,
                url: gatewayEndpointDisplayUrl(port),
                icon: <EngineIcon type={type} />
            }
        })

        // One endpoint that is not an engine: a single loopback port that routes
        // to any engine on any node. It is therefore not in the engine table and
        // arrives as a reported port, and it stands on its own — a computer with
        // no engine facade still routes from its endpoint, so the "no engines"
        // empty state must not swallow it.
        if (clusterPort !== null) {
            endpoints.push({
                name: CLUSTER_ENDPOINT_NAME,
                proxyPort: clusterPort,
                url: gatewayEndpointDisplayUrl(clusterPort),
                icon: <Route />
            })
        }

        return endpoints.filter(v => !!v)
    }, [selfEngines, statusByNode, clusterPort])

    const handleCopy = useCallback(async (value: string, backend: string) => {
        const electronCopy = window.windowApi?.window?.copyToClipboard

        const doTimer = () => {
            setCopied(prev => ({ ...prev, [backend]: true }))
            setTimeout(() => setCopied(prev => ({ ...prev, [backend]: false })), 1500)
        }

        if (typeof electronCopy === 'function') {
            try {
                await electronCopy(value)
                doTimer()
                return
            } catch {
                /* fall through */
            }
        }

        navigator.clipboard.writeText(value)
        doTimer()
    }, [])

    const items = useMemo(() => {
        if (allBackends.length === 0) {
            return [
                {
                    id: 'no-engines',
                    children: <Text kind="body/regular/sm">No engines are running</Text>,
                    disabled: true
                }
            ]
        }

        return allBackends.map(endpoint => ({
            id: endpoint?.name ?? '',
            children: (
                <Flex
                    align="center"
                    gap="4"
                    justify="start"
                    onClick={() =>
                        endpoint?.url
                            ? handleCopy(endpoint?.url ?? '', endpoint?.name ?? '')
                            : undefined
                    }
                >
                    {endpoint?.icon}

                    <Stack className="grow min-w-0">
                        <Text kind="body/semibold/sm">{endpoint?.name}</Text>
                        {endpoint?.url && (
                            <Text kind="body/regular/sm" className="text-subtle-color truncate">
                                {endpoint?.url}
                            </Text>
                        )}
                    </Stack>

                    <DismissibleTooltip slotContent="Copy" placement="right">
                        <Button kind="tertiary" size="small" aria-label={`Copy ${endpoint?.name}`}>
                            {copied[endpoint?.name ?? ''] ? (
                                <Text kind="body/regular/sm">Copied</Text>
                            ) : (
                                <ContentCopy style={{ fontSize: 16 }} />
                            )}
                        </Button>
                    </DismissibleTooltip>
                </Flex>
            )
        }))
    }, [allBackends, handleCopy, copied])

    return (
        <Stack gap="4" className={`overflow-y-auto ${className ?? ''}`}>
            {items.map(item => (
                <Stack key={item.id}>{item.children}</Stack>
            ))}
        </Stack>
    )
}
