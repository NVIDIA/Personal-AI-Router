// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EnabledEngineTypes, EngineDisplayNames } from '@/shared/constants/engines'
import { EngineType } from '@/shared/types/engines'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { useVllmGroupStore, vllmGroupHoldsNode } from '@/ui/stores/vllm-group.store'
import { getEnginesForNode } from '@/ui/utils/get-engines-for-node'
import { isExternalRuntime } from '@/ui/utils/engine-ownership'
import { Button, Flex, Switch, Text } from '@nvidia/foundations-react-core'
import { Download } from '@/ui/components/icons'
import { useCallback, useMemo } from 'react'
import {
    engineEnabled,
    engineInstallAllowed,
    engineLifecycleAllowed
} from '@/ui/utils/engine-control-policy'

export default function NodeEnginesInline({ nodeId }: { nodeId: string }) {
    const statusByNode = useEngineStatusStore(s => s.statusByNode)
    const selfId = useConnectionStore(s => s.selfId)
    const isRemote = selfId !== nodeId
    const isDisconnected = useNodesStore(s => isRemote && s.nodes.get(nodeId)?.status === 'offline')
    // Shared target-aware hold, including an unsettled serving-group Start.
    const vllmGroupBlocked = useVllmGroupStore(state =>
        vllmGroupHoldsNode(state, nodeId, selfId, window.windowApi.platform === 'Linux')
    )
    // Re-render when a lifecycle command for this node begins/clears; the
    // per-engine pending state is read below via getState().
    const lifecyclePendingFingerprint = usePendingActionsStore(state => {
        const parts: string[] = []
        for (const [key, p] of state.pending) {
            if (p.nodeId === nodeId && key.endsWith(':lifecycle')) {
                parts.push(`${p.engineType}:${p.action}`)
            }
        }
        return parts.join('|')
    })
    void lifecyclePendingFingerprint
    const selfEngines = useMemo(
        () => getEnginesForNode(nodeId ?? '', statusByNode, EnabledEngineTypes),
        [nodeId, statusByNode]
    )

    const allBackends = useMemo(() => {
        return EnabledEngineTypes.flatMap(type => {
            const backend = selfEngines.find(b => b.engineType === type)

            // Show every engine the node has actually reported — not just ones
            // with a proxy port. The broker fronts only Ollama with a proxy, so
            // gating on `proxyPort` hid LM Studio (managed by nvpair-engine-manager,
            // loopback-only, no proxy). Skip the padded `initializing`
            // placeholder so engines a node never reports (e.g. LM Studio on a
            // remote, undiscovered node) don't show a perpetual spinner.
            if (!backend || backend.processStatus === 'initializing') {
                return []
            }

            return [
                {
                    type,
                    name: EngineDisplayNames[type],
                    status: backend.processStatus,
                    enabled: engineEnabled({ type, ...backend }),
                    managed: backend.managed,
                    adopted: backend.adopted,
                    installSupported: backend.installSupported,
                    // The same facts the engine row uses: a runtime PAIR only
                    // observes gets no lifecycle control, and an engine the
                    // node reports as not installable gets no Install button.
                    external: isExternalRuntime(type, backend.processStatus, backend.managed),
                    installable: type !== 'llamacpp' || backend.installSupported === true
                }
            ]
        })
    }, [selfEngines])

    const handleToggle = useCallback(
        (engineType: EngineType) => {
            window.pairApi.engines.toggle(engineType, nodeId)
        },
        [nodeId]
    )

    const handleInstall = useCallback(
        (engineType: EngineType) => {
            window.pairApi.engines.install(engineType, nodeId)
        },
        [nodeId]
    )

    return (
        <>
            {allBackends.map(b => {
                const pending = Boolean(
                    usePendingActionsStore.getState().getLifecyclePending(nodeId, b.type)
                )
                const isTransitioning =
                    pending ||
                    b.status === 'installing' ||
                    b.status === 'uninstalling' ||
                    b.status === 'starting' ||
                    b.status === 'stopping'

                const controlFacts = {
                    type: b.type,
                    processStatus: b.status,
                    enabled: b.enabled,
                    managed: b.managed,
                    adopted: b.adopted,
                    installSupported: b.installSupported
                }
                const lifecycleAllowed = engineLifecycleAllowed(controlFacts)
                const installAllowed = engineInstallAllowed(controlFacts)
                const groupBlocked = b.type === 'vllm' && vllmGroupBlocked

                if (!lifecycleAllowed && b.status !== 'not-installed') {
                    return (
                        <Text key={b.name} kind="body/regular/sm" className="text-subtle-color">
                            {b.name}
                        </Text>
                    )
                }

                if (b.status === 'not-installed' && !pending) {
                    if (!b.installable) return null
                    if (!installAllowed) {
                        return (
                            <Text key={b.name} kind="body/regular/sm" className="text-subtle-color">
                                {b.name}
                            </Text>
                        )
                    }
                    return (
                        <Button
                            key={b.name}
                            kind="secondary"
                            size="tiny"
                            className="shrink-0 no-drag-elements px-2"
                            onClick={e => {
                                e.preventDefault()
                                e.stopPropagation()
                                handleInstall(b.type)
                            }}
                            disabled={(isRemote && isDisconnected) || groupBlocked}
                            title={
                                groupBlocked
                                    ? 'Serving-group ownership holds vLLM actions.'
                                    : undefined
                            }
                            aria-label={`Install ${b.name}`}
                        >
                            <Download style={{ fontSize: 14 }} />
                            <Text kind="body/regular/sm" className="ml-1">
                                {b.name}
                            </Text>
                        </Button>
                    )
                }

                const toggleHeld = groupBlocked || b.external
                const toggleTitle = groupBlocked
                    ? 'Serving-group ownership holds vLLM actions'
                    : b.external
                      ? `${b.name} is managed outside PAIR`
                      : isRemote && isDisconnected
                        ? 'Node is disconnected'
                        : `${b.enabled ? 'Disable' : 'Enable'} ${b.name}`
                return (
                    <Flex
                        key={b.name}
                        align="center"
                        gap="2"
                        className="shrink-0 no-drag-elements pair-engines-inline"
                    >
                        <Flex align="center" justify="center" className="h-4 w-7 shrink-0 -mr-1">
                            {isTransitioning ? (
                                <span
                                    className="spinner-element-medium"
                                    role="status"
                                    aria-label=""
                                />
                            ) : (
                                <Switch
                                    size="small"
                                    checked={b.enabled}
                                    onCheckedChange={() => handleToggle(b.type)}
                                    disabled={(isRemote && isDisconnected) || toggleHeld}
                                    title={toggleTitle}
                                    aria-label={toggleTitle}
                                />
                            )}
                        </Flex>
                        <Text
                            kind="body/regular/sm"
                            className={toggleHeld ? 'text-subtle-color' : 'cursor-pointer'}
                            onClick={() =>
                                !isTransitioning && !toggleHeld ? handleToggle(b.type) : undefined
                            }
                        >
                            {b.name}
                        </Text>
                    </Flex>
                )
            })}
        </>
    )
}
