// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { PlatformDisplayName } from '@/shared/types/platform'
import { canAutoInstallBackendForOs } from '@/ui/utils/backend-target-os'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'
import { useDismissibleTooltipTrigger } from '@/ui/hooks/use-dismissible-tooltip-trigger'

export function InstallButton({
    backend,
    targetOs,
    isLocalNode,
    disabled,
    onInstall
}: {
    backend: BackendInfo
    targetOs: PlatformDisplayName
    isLocalNode: boolean
    disabled: boolean
    onInstall: () => void
}) {
    const autoInstall = canAutoInstallBackendForOs(backend.type, targetOs)
    const isNotInstalled = backend.processStatus === 'not-installed'
    const missingPrereqs = (backend.prerequisites ?? []).filter(p => !p.installed)
    const prereqsMet = missingPrereqs.length === 0

    const openLink = useCallback((e: React.MouseEvent, url: string) => {
        e.preventDefault()
        e.stopPropagation()
        window.windowApi.window.openExternal(url)
    }, [])

    const onInstallAllClick = useDismissibleTooltipTrigger(onInstall)

    return (
        <>
            {!autoInstall && isLocalNode && isNotInstalled && backend.installUrl && (
                <button
                    type="button"
                    onClick={e => openLink(e, backend.installUrl!)}
                    className="no-bg-link"
                >
                    <Button kind="primary" color="brand" size="small" asChild>
                        <span>Download</span>
                    </Button>
                </button>
            )}

            {autoInstall &&
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
