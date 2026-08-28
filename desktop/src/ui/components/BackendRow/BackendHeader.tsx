// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState } from 'react'
import { Button, Flex, Stack, Switch, Text } from '@nvidia/foundations-react-core'
import { Check, ContentCopy, ExpandMore, OpenInNew } from '@/ui/components/icons'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { PlatformDisplayName } from '@/shared/types/platform'
import { InstallButton } from './InstallButton'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import { gatewayEndpointDisplayUrl } from '@/ui/utils/gateway-inference-paths'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { statusLabel } from '@/ui/utils/status'

/** Install/uninstall lines include asset names + percentages — allow more room than generic status. */
const INSTALL_STATUS_MAX_LEN = 52

function truncate(text: string, max: number): string {
    return text.length > max ? text.substring(0, max) + '...' : text
}

export function BackendHeader({
    backend,
    isTransitioning,
    isUnavailable,
    disabled,
    onToggle,
    onExpand,
    expanded,
    isLocalNode,
    targetOs,
    onInstall
}: {
    backend: BackendInfo
    isTransitioning: boolean
    /** Remote node never reported this engine — render a stable read-only label, no controls. */
    isUnavailable: boolean
    disabled: boolean
    onToggle: () => void
    onExpand: () => void
    expanded: boolean
    isLocalNode: boolean
    targetOs: PlatformDisplayName
    onInstall: () => void
}) {
    const caps = EngineCapabilities[backend.type]
    // The proxy URL / web UI live on this machine's 127.0.0.1 — meaningless for a
    // remote node's engine, so only offer them on the local node.
    const showWebUI =
        isLocalNode &&
        caps?.hasProxyWebUI &&
        backend.processStatus === 'running' &&
        backend.proxyPort

    const handleOpenWebUI = useCallback(() => {
        if (backend.proxyPort) {
            window.windowApi.window.openExternal(`http://127.0.0.1:${backend.proxyPort}`)
        }
    }, [backend.proxyPort])

    const [copied, setCopied] = useState(false)

    const proxyUrl = useMemo(() => {
        if (!backend.proxyPort) {
            return ''
        }

        return gatewayEndpointDisplayUrl(backend.proxyPort, backend.type)
    }, [backend.proxyPort, backend.type])

    const handleCopy = useCallback(() => {
        if (!proxyUrl) {
            return
        }

        window.windowApi.window.copyToClipboard(proxyUrl).then(() => {
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
        })
    }, [proxyUrl])

    const handleHeaderClick = useCallback(() => {
        onExpand()
    }, [onExpand])

    return (
        <Flex
            align="center"
            justify="between"
            onClick={handleHeaderClick}
            className={`${isUnavailable ? 'cursor-default' : 'cursor-pointer'} p-4 -m-4`}
        >
            <Flex align="center" gap="2" className=" grow">
                <Flex align="center" gap="1">
                    {!isUnavailable && (
                        <ExpandMore
                            style={{ fontSize: 14 }}
                            className={`transition-transform ${expanded ? 'rotate-180' : ''}`}
                        />
                    )}
                    <Text kind="body/semibold/md">{backend.displayName}</Text>
                </Flex>

                <Flex align="center">
                    {isLocalNode &&
                        backend.proxyPort &&
                        !isTransitioning &&
                        backend.processStatus !== 'not-installed' && (
                            <Button
                                kind="tertiary"
                                size="small"
                                onClick={e => {
                                    e.preventDefault()
                                    e.stopPropagation()
                                    handleCopy()
                                }}
                                title={`Copy ${backend.displayName} API http://127.0.0.1:${backend.proxyPort}`}
                                style={{ padding: '2px 6px', minWidth: 'auto' }}
                                aria-label={`Copy ${backend.displayName} API URL`}
                            >
                                {copied ? (
                                    <Check style={{ fontSize: 14 }} />
                                ) : (
                                    <ContentCopy style={{ fontSize: 14 }} />
                                )}
                            </Button>
                        )}

                    {showWebUI && (
                        <Button
                            kind="tertiary"
                            size="small"
                            onClick={e => {
                                e.preventDefault()
                                e.stopPropagation()
                                handleOpenWebUI()
                            }}
                            title={`Open ${backend.displayName} in browser`}
                            style={{ padding: '2px 6px', minWidth: 'auto' }}
                            aria-label={`Open ${backend.displayName} in browser`}
                        >
                            <OpenInNew style={{ fontSize: 14 }} />
                        </Button>
                    )}
                </Flex>
            </Flex>

            {isUnavailable && (
                <DismissibleTooltip slotContent="This node hasn't reported this engine's status.">
                    <Flex align="center" className="ml-2" style={{ minHeight: 32 }}>
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Unavailable
                        </Text>
                    </Flex>
                </DismissibleTooltip>
            )}

            {isTransitioning &&
                (() => {
                    const baseStatus =
                        backend.installProgress?.status ?? statusLabel[backend.processStatus]
                    const pct = backend.installProgress?.percent
                    const pctRounded = pct != null && Number.isFinite(pct) ? Math.round(pct) : null
                    const pctSuffix = pctRounded != null ? ` · ${pctRounded}%` : ''
                    const baseWithoutDuplicatePercent =
                        pctRounded != null
                            ? baseStatus.replace(/\s*\d+\.?\d*\s*%.*$/, '').trim()
                            : baseStatus
                    const maxBase =
                        pctSuffix.length > 0
                            ? INSTALL_STATUS_MAX_LEN - pctSuffix.length
                            : INSTALL_STATUS_MAX_LEN
                    const baseForLine = truncate(baseWithoutDuplicatePercent, maxBase)
                    const fullStatus = pctSuffix ? `${baseForLine}${pctSuffix}` : baseForLine
                    const isTruncated = baseWithoutDuplicatePercent.length > maxBase
                    return (
                        <DismissibleTooltip
                            slotContent={fullStatus}
                            {...(isTruncated ? {} : { open: false })}
                        >
                            <Flex
                                align="center"
                                gap="1"
                                className="ml-2 min-w-0 max-w-[min(100%,28rem)]"
                                style={{ minHeight: 32 }}
                            >
                                <span className="min-w-0">
                                    <Text
                                        kind="body/regular/sm"
                                        className="truncate block mt-1 capitalize"
                                    >
                                        {fullStatus}
                                    </Text>
                                </span>
                                <span className="spinner-element" role="status" aria-label="" />
                            </Flex>
                        </DismissibleTooltip>
                    )
                })()}

            {!isUnavailable &&
                !isTransitioning &&
                backend.processStatus !== 'not-installed' &&
                (() => {
                    const missingPrereqs = (backend.prerequisites ?? []).filter(p => !p.installed)
                    const prereqsMet = missingPrereqs.length === 0
                    const isStartDisabled = !prereqsMet && backend.processStatus !== 'running'

                    const toggle = (
                        <Switch
                            size="small"
                            checked={backend.processStatus === 'running'}
                            onCheckedChange={onToggle}
                            onClick={e => e.stopPropagation()}
                            disabled={disabled || isStartDisabled}
                            aria-label={`${backend.processStatus === 'running' ? 'Stop' : 'Start'} ${backend.displayName}`}
                        />
                    )

                    if (isStartDisabled) {
                        return (
                            <DismissibleTooltip
                                slotContent={
                                    <Stack gap="1">
                                        <Text kind="body/semibold/sm">Missing prerequisites:</Text>
                                        {missingPrereqs.map(p => (
                                            <Text key={p.name} kind="body/regular/sm">
                                                {p.name}
                                                {p.installUrl
                                                    ? ` — install from ${p.installUrl}`
                                                    : ''}
                                            </Text>
                                        ))}
                                    </Stack>
                                }
                            >
                                {toggle}
                            </DismissibleTooltip>
                        )
                    }

                    return toggle
                })()}

            {!isUnavailable && !isTransitioning && backend.processStatus === 'not-installed' && (
                <InstallButton
                    backend={backend}
                    targetOs={targetOs}
                    isLocalNode={isLocalNode}
                    disabled={disabled}
                    onInstall={onInstall}
                />
            )}
        </Flex>
    )
}
