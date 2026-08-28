// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { MoreHoriz } from '@/ui/components/icons'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useActiveWorkloads } from '@/ui/stores/workloads.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useServiceStatusStore } from '@/ui/stores/service-status.store'
import { resolveShellView } from '@/ui/utils/shell-view'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import { useOverviewNodes } from '@/ui/hooks/useOverviewNodes'
import TrayNodeRow from './TrayNodeRow'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

export default function TrayApp() {
    const nodes = useOverviewNodes()
    const fetchedNodes = useNodesStore(state => state.fetchedNodes)
    const connected = useConnectionStore(state => state.connected)
    const connectorStatus = useServiceStatusStore(state => state.status.connectorStatus)
    const activeWorkloads = useActiveWorkloads()
    const pendingInviteCount = useClusterInvitationsStore(s => s.pendingInvites.length)
    const contentRef = useRef<HTMLDivElement>(null)
    const previousClusterPromptCountRef = useRef<number | null>(null)
    const [maxHeight, setMaxHeight] = useState(0)

    const onlineNodes = useMemo(() => nodes.filter(n => n.status !== 'offline'), [nodes])
    const shellView = resolveShellView({ connectorStatus, connected, fetchedNodes })

    useEffect(() => {
        const nextPromptCount = pendingInviteCount
        const previousPromptCount = previousClusterPromptCountRef.current
        previousClusterPromptCountRef.current = nextPromptCount
        if (previousPromptCount === null || nextPromptCount <= previousPromptCount) return

        void window.windowApi.window.openOverview()
    }, [pendingInviteCount])

    useEffect(() => {
        const el = contentRef.current
        if (!el) return

        const sendResize = (): void => {
            const height = Math.ceil(el.getBoundingClientRect().height)
            if (height > 0) {
                window.windowApi.window.resizeTray(height).then(mh => {
                    if (mh > 0) setMaxHeight(mh)
                })
            }
        }

        const observer = new ResizeObserver(sendResize)
        observer.observe(el)
        sendResize()

        const onFocus = (): void => sendResize()
        window.addEventListener('focus', onFocus)

        return () => {
            observer.disconnect()
            window.removeEventListener('focus', onFocus)
        }
    }, [])

    const scrollMaxHeight = maxHeight > 0 ? maxHeight - 52 : undefined

    return (
        <Stack ref={contentRef} className="bg-black w-full select-none relative pb-0.5">
            <div className="absolute top-0 right-0 w-full h-full app-bg"></div>
            <Flex
                justify="between"
                align="center"
                className="px-3 py-2 shrink-0 relative"
                style={{ borderBottom: '1px solid var(--color-translucent-white-100)' }}
            >
                <Text kind="body/bold/md" className="truncate mt-1">
                    {APP_DISPLAY_NAME}
                </Text>
                <Button
                    kind="tertiary"
                    size="small"
                    className="cursor-pointer"
                    onClick={() => window.windowApi.window.showTrayMenu()}
                    aria-label="Open tray menu"
                >
                    <MoreHoriz style={{ fontSize: 16 }} />
                </Button>
            </Flex>

            <Stack
                className="overflow-y-auto relative"
                style={scrollMaxHeight ? { maxHeight: scrollMaxHeight } : undefined}
            >
                {shellView === 'service-stopped' ? (
                    <Flex align="center" justify="center" direction="col" gap="2" className="py-6">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Service stopped
                        </Text>
                        <Button
                            kind="tertiary"
                            size="small"
                            className="cursor-pointer"
                            onClick={() => void window.windowApi.window.openOverview()}
                        >
                            Open {APP_DISPLAY_NAME}
                        </Button>
                    </Flex>
                ) : shellView === 'loading' ? (
                    <Flex align="center" justify="center" className="py-6">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Connecting...
                        </Text>
                    </Flex>
                ) : onlineNodes.length === 0 ? (
                    <Flex align="center" justify="center" className="py-6">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            No nodes available
                        </Text>
                    </Flex>
                ) : (
                    onlineNodes.map(node => (
                        <TrayNodeRow key={node.id} node={node} activeWorkloads={activeWorkloads} />
                    ))
                )}
            </Stack>
        </Stack>
    )
}
