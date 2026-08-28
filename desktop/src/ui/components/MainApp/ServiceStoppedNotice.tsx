// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'

interface ServiceStoppedNoticeProps {
    /** Set when the service exited on its own instead of being stopped. */
    error?: string
}

/**
 * Shown on Overview while the connector reports `disconnected`.
 *
 * The service owns every node, engine, and model view, so there is nothing to
 * render without it. This replaces the spinner that state used to show
 * indefinitely and offers the two ways back: start the service, or open the
 * Service tab for logs and log level.
 */
export default function ServiceStoppedNotice({ error }: ServiceStoppedNoticeProps) {
    const openSettings = useOverviewUiStore(state => state.openSettings)
    const [starting, setStarting] = useState(false)
    const [startError, setStartError] = useState<string | null>(null)

    const handleStart = (): void => {
        setStarting(true)
        setStartError(null)
        void (async () => {
            try {
                await window.windowApi.service.start()
            } catch (err) {
                setStartError(getErrorString(err))
            } finally {
                setStarting(false)
            }
        })()
    }

    return (
        <Flex
            align="center"
            justify="center"
            direction="col"
            gap="4"
            className="flex-1 relative w-full"
        >
            <Stack gap="2" className="max-w-md text-center">
                <Text kind="body/semibold/md">The {APP_DISPLAY_NAME} service is stopped</Text>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    Nodes, engines, and models are unavailable until it is running again.
                </Text>
            </Stack>

            {error && <InlineErrorBanner severity="error" message={error} />}
            {startError && startError !== error && (
                <InlineErrorBanner severity="error" message={startError} />
            )}

            <Flex gap="2">
                <Button kind="secondary" size="small" onClick={handleStart} disabled={starting}>
                    {starting ? (
                        <span className="spinner-element" role="status" aria-label="" />
                    ) : (
                        'Start service'
                    )}
                </Button>
                <Button kind="tertiary" size="small" onClick={() => openSettings('service')}>
                    Service settings
                </Button>
            </Flex>
        </Flex>
    )
}
