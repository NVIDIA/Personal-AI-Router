// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { UpdateStatus } from '@/shared/types/update'
import { isElectron } from '@/ui/api/bootstrap'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { DismissibleTooltip } from '@/ui/components/DismissibleTooltip/DismissibleTooltip'

const BROWSER_TOOLTIP = 'Only available in the desktop app'

function phaseLabel(status: UpdateStatus): string {
    switch (status.phase) {
        case 'idle':
            return 'Ready to check'
        case 'checking':
            return 'Checking for updates…'
        case 'available':
            return `Update available: ${status.latestVersion ?? ''}`
        case 'not-available':
            return 'You are on the latest version'
        case 'downloading':
            return `Downloading… ${status.downloadPercent !== null ? `${Math.round(status.downloadPercent)}%` : ''}`
        case 'downloaded':
            return `Ready to install ${status.latestVersion ?? ''}`
        case 'error':
            return status.error ?? 'Update failed'
    }
}

export default function ApplicationUpdatesCard() {
    const [status, setStatus] = useState<UpdateStatus | null>(null)

    useEffect(() => {
        if (!isElectron) return
        window.windowApi.update
            .getStatus()
            .then(setStatus)
            .catch(() => {})
        return window.windowApi.update.onStatusChanged(setStatus)
    }, [])

    const runCheck = useCallback(() => {
        if (!isElectron) return
        void window.windowApi.update.check()
    }, [])

    const runDownload = useCallback(() => {
        if (!isElectron) return
        void window.windowApi.update.download()
    }, [])

    const runInstall = useCallback(() => {
        if (!isElectron) return
        void window.windowApi.update.install()
    }, [])

    const current = status?.currentVersion ?? '—'
    const phase = status?.phase ?? 'idle'
    const showCheck = phase === 'idle' || phase === 'not-available' || phase === 'error'
    const showDownload = phase === 'available'
    const showInstall = phase === 'downloaded'

    return (
        <div className="settings-card settings-card-compact pair-paper p-4">
            {status?.error && phase === 'error' && (
                <Stack gap="3" className="mb-4">
                    <InlineErrorBanner severity="error" message={status.error} />
                </Stack>
            )}

            <Stack gap="4">
                <Stack gap="1">
                    <Text kind="body/semibold/md">Application updates</Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Version {current}. {status ? phaseLabel(status) : ''}
                    </Text>
                </Stack>

                <Flex gap="2" wrap="wrap">
                    {isElectron ? (
                        <>
                            {showCheck && (
                                <Button kind="secondary" size="small" onClick={runCheck}>
                                    Check for updates
                                </Button>
                            )}
                            {showDownload && (
                                <Button
                                    kind="primary"
                                    color="brand"
                                    size="small"
                                    onClick={runDownload}
                                >
                                    Download update
                                </Button>
                            )}
                            {showInstall && (
                                <Button
                                    kind="primary"
                                    color="brand"
                                    size="small"
                                    onClick={runInstall}
                                >
                                    Restart &amp; install
                                </Button>
                            )}
                        </>
                    ) : (
                        <DismissibleTooltip slotContent={BROWSER_TOOLTIP}>
                            <span className="inline-flex">
                                <Button kind="secondary" size="small" disabled>
                                    Check for updates
                                </Button>
                            </span>
                        </DismissibleTooltip>
                    )}
                </Flex>
            </Stack>
        </div>
    )
}
