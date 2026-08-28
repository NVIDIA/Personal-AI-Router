// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback } from 'react'
import type { CSSProperties } from 'react'
import { Banner, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import type { BackendInfo } from '@/ui/types/engine-info'

const BANNER_LINK_STYLE: CSSProperties = {
    alignItems: 'center',
    color: 'inherit',
    display: 'inline-flex',
    lineHeight: 1
}

function displayVersion(version: string): string {
    return version.startsWith('v') ? version : `v${version}`
}

export function BackendUpdateBanner({
    backend,
    disabled,
    onUpdate
}: {
    backend: BackendInfo
    disabled: boolean
    onUpdate: () => void
}) {
    const openLink = useCallback((e: React.MouseEvent, url: string) => {
        e.preventDefault()
        e.stopPropagation()
        window.windowApi.window.openExternal(url)
    }, [])

    const isTransitioning =
        backend.processStatus === 'installing' || backend.processStatus === 'uninstalling'
    const showUpdateBanner =
        backend.processStatus !== 'not-installed' && !isTransitioning && !!backend.updateAvailable

    if (!showUpdateBanner || !backend.updateAvailable) return null
    const updateAvailable = backend.updateAvailable

    return (
        <Banner status="info" attributes={{ BannerHeader: { className: 'min-w-0 flex-1' } }}>
            <Flex align="center" justify="between" gap="3" className="w-full min-w-0 flex-1">
                <Stack gap="0" className="min-w-0 flex-1">
                    <Text kind="body/semibold/sm">
                        {updateAvailable.installType === 'managed'
                            ? `Update available: ${displayVersion(updateAvailable.latestVersion)}`
                            : `New ${backend.displayName} release: ${displayVersion(updateAvailable.latestVersion)}`}
                    </Text>
                    <Flex align="center" gap="2">
                        <button
                            type="button"
                            onClick={e => openLink(e, updateAvailable.releaseUrl)}
                            className="no-bg-link cursor-pointer"
                            style={BANNER_LINK_STYLE}
                        >
                            <Flex align="center" gap="1">
                                <Text kind="body/regular/sm">Release notes</Text>
                                <OpenInNew style={{ fontSize: 12 }} />
                            </Flex>
                        </button>
                    </Flex>
                </Stack>
                {updateAvailable.installType === 'managed' && (
                    <Button kind="secondary" size="small" onClick={onUpdate} disabled={disabled}>
                        Update
                    </Button>
                )}
            </Flex>
        </Banner>
    )
}
