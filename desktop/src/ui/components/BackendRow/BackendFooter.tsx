// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback } from 'react'
import type { CSSProperties } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import type { BackendInfo } from '@/ui/types/engine-info'
import type { PlatformDisplayName } from '@/shared/types/platform'
import { canAutoInstallBackendForOs } from '@/ui/utils/backend-target-os'

const INLINE_LINK_STYLE: CSSProperties = {
    alignItems: 'center',
    color: 'var(--color-brand, #76b900)',
    display: 'inline-flex',
    lineHeight: 1
}

function displayVersion(version: string): string {
    return version.startsWith('v') ? version : `v${version}`
}

export function BackendFooter({
    backend,
    targetOs,
    showUninstall,
    disabled,
    onUninstall
}: {
    backend: BackendInfo
    targetOs: PlatformDisplayName
    showUninstall: boolean
    disabled: boolean
    onUninstall: () => void
}) {
    const autoInstall = canAutoInstallBackendForOs(backend.type, targetOs)
    const isTransitioning =
        backend.processStatus === 'installing' || backend.processStatus === 'uninstalling'
    const isNotInstalled = backend.processStatus === 'not-installed'
    const isInstalled = !isNotInstalled && !isTransitioning

    const missingPrereqs = (backend.prerequisites ?? []).filter(p => !p.installed)

    const openLink = useCallback((e: React.MouseEvent, url: string) => {
        e.preventDefault()
        e.stopPropagation()
        window.windowApi.window.openExternal(url)
    }, [])

    const installedVersion = backend.installedVersion
        ? displayVersion(backend.installedVersion)
        : backend.updateAvailable
          ? displayVersion(backend.updateAvailable.currentVersion)
          : null

    return (
        <Stack gap="2">
            {isNotInstalled && missingPrereqs.length > 0 && (
                <Stack gap="1">
                    {missingPrereqs.map(p => (
                        <Flex key={p.name} align="center" gap="1">
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Requires {p.name}
                            </Text>
                            <button
                                type="button"
                                onClick={e => openLink(e, p.installUrl)}
                                className="no-bg-link"
                            >
                                <Flex align="center" gap="1">
                                    <Text kind="body/regular/sm">Install</Text>
                                    <OpenInNew style={{ fontSize: 12 }} />
                                </Flex>
                            </button>
                        </Flex>
                    ))}
                </Stack>
            )}

            <Flex align="center" justify="between" gap="3">
                <Flex align="center" gap="2" className="min-w-0 ml-1">
                    {backend.docsUrl && (
                        <button
                            type="button"
                            onClick={e => openLink(e, backend.docsUrl!)}
                            className="no-bg-link cursor-pointer pair-link"
                            style={INLINE_LINK_STYLE}
                        >
                            <Flex align="center" gap="1">
                                <Text kind="body/regular/sm">Docs</Text>
                                <OpenInNew style={{ fontSize: 14, marginTop: -2 }} />
                            </Flex>
                        </button>
                    )}
                    {installedVersion && (
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {installedVersion}
                        </Text>
                    )}
                </Flex>

                {autoInstall && showUninstall && isInstalled && (
                    <Button onClick={onUninstall} kind="tertiary" size="small" disabled={disabled}>
                        Uninstall
                    </Button>
                )}
            </Flex>
        </Stack>
    )
}
