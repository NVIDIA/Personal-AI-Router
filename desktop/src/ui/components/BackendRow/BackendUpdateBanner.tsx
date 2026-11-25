// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback } from 'react'
import type { CSSProperties } from 'react'
import { Banner, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { OpenInNew } from '@/ui/components/icons'
import { displayEngineVersion } from '@/ui/utils/status'
import type { BackendInfo } from '@/ui/types/engine-info'

const BANNER_LINK_STYLE: CSSProperties = {
    alignItems: 'center',
    color: 'inherit',
    display: 'inline-flex',
    lineHeight: 1
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

    const { processStatus, updateAvailable } = backend
    if (
        !updateAvailable ||
        processStatus === 'unsupported' ||
        processStatus === 'uninstalling' ||
        processStatus === 'installing' ||
        (processStatus === 'not-installed' && updateAvailable.installType === 'managed')
    )
        return null

    return (
        <Banner
            status="info"
            className="mt-4"
            attributes={{ BannerHeader: { className: 'min-w-0 flex-1' } }}
        >
            <Flex align="center" justify="between" gap="3" className="w-full min-w-0 flex-1">
                <Stack gap="0" className="min-w-0 flex-1">
                    <Text kind="body/semibold/sm">
                        {backend.displayName} {displayEngineVersion(updateAvailable.latestVersion)}{' '}
                        is available
                    </Text>
                    <Text kind="body/regular/sm">
                        {displayEngineVersion(updateAvailable.currentVersion)} installed
                    </Text>
                    {(updateAvailable.installType === 'managed' ||
                        updateAvailable.releaseUrl !== updateAvailable.downloadUrl) && (
                        <Flex align="center" gap="2">
                            <button
                                type="button"
                                onClick={e => openLink(e, updateAvailable.releaseUrl)}
                                className="no-bg-link cursor-pointer"
                                style={BANNER_LINK_STYLE}
                            >
                                <Flex align="center" gap="1">
                                    <Text kind="body/regular/sm">Update details</Text>
                                    <OpenInNew style={{ fontSize: 12 }} />
                                </Flex>
                            </button>
                        </Flex>
                    )}
                </Stack>
                {updateAvailable.installType === 'managed' ? (
                    <Button kind="secondary" size="small" onClick={onUpdate} disabled={disabled}>
                        Update
                    </Button>
                ) : (
                    <Button
                        kind="secondary"
                        size="small"
                        onClick={e => openLink(e, updateAvailable.downloadUrl)}
                        disabled={disabled}
                    >
                        Get update <OpenInNew style={{ fontSize: 12 }} />
                    </Button>
                )}
            </Flex>
        </Banner>
    )
}
