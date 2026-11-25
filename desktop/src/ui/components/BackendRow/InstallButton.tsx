// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import type { BackendInfo } from '@/ui/types/engine-info'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import { useDismissibleTooltipTrigger } from '@/ui/hooks/use-dismissible-tooltip-trigger'

export function InstallButton({
    backend,
    isLocalNode,
    disabled,
    onInstall
}: {
    backend: BackendInfo
    isLocalNode: boolean
    disabled: boolean
    onInstall: () => void
}) {
    // Whether NVPAIR can obtain the engine is the owning node's engine manager's
    // answer, reported with its state: only it knows whether that platform declares
    // an install at all, and whether the tool that install runs through is on that
    // machine. A missing answer means no button. The alternative is a button whose
    // only outcome is a refusal naming a missing package manager, by which point
    // the user has already decided they want the engine.
    const canInstall = backend.canInstall === true
    const isNotInstalled = backend.processStatus === 'not-installed'
    const missingPrereqs = (backend.prerequisites ?? []).filter(p => !p.installed)
    const prereqsMet = missingPrereqs.length === 0
    // Where NVPAIR cannot fetch the engine itself, the vendor's own download page
    // takes its place — for an engine like MTPLX, whose only other source is the
    // vendor's installer. Remote cards get no link: a download started here lands
    // here, not on the machine the card describes.
    const downloadUrl =
        !canInstall && isLocalNode && isNotInstalled ? backend.installUrl : undefined

    const openLink = useCallback((e: React.MouseEvent, url: string) => {
        e.preventDefault()
        e.stopPropagation()
        window.windowApi.window.openExternal(url)
    }, [])

    const onInstallAllClick = useDismissibleTooltipTrigger(onInstall)

    return (
        <>
            {downloadUrl && (
                <button
                    type="button"
                    onClick={e => openLink(e, downloadUrl)}
                    className="no-bg-link"
                >
                    <Button kind="primary" color="brand" size="small" asChild>
                        <span>Download</span>
                    </Button>
                </button>
            )}

            {canInstall &&
                isNotInstalled &&
                (!prereqsMet ? (
                    <DismissibleTooltip
                        slotContent={
                            <Stack gap="2">
                                <Text kind="body/semibold/sm">Missing dependencies:</Text>
                                <Stack gap="1">
                                    {missingPrereqs.map(p => (
                                        <Flex key={p.name} align="center" gap="1">
                                            <Text kind="body/regular/sm">{p.name}</Text>
                                            <button
                                                type="button"
                                                onClick={e => openLink(e, p.installUrl)}
                                                className="no-bg-link"
                                            >
                                                <Flex align="center" gap="1">
                                                    <Text kind="body/regular/sm">Install</Text>
                                                    <OpenInNew style={{ fontSize: 10 }} />
                                                </Flex>
                                            </button>
                                        </Flex>
                                    ))}
                                </Stack>
                                <Button
                                    onClick={onInstallAllClick}
                                    kind="primary"
                                    color="brand"
                                    size="small"
                                >
                                    Install all
                                </Button>
                            </Stack>
                        }
                    >
                        <span>
                            <Button
                                onClick={onInstall}
                                kind="primary"
                                color="brand"
                                size="small"
                                disabled
                            >
                                Install
                            </Button>
                        </span>
                    </DismissibleTooltip>
                ) : (
                    <Button
                        onClick={onInstall}
                        kind="primary"
                        color="brand"
                        size="small"
                        disabled={disabled}
                    >
                        Install
                    </Button>
                ))}
        </>
    )
}
