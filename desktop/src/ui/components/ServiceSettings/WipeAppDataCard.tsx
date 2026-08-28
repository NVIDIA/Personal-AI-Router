// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import getErrorString from '@/shared/utils/get-error-string'
import { useErrorsStore } from '@/ui/stores/errors.store'

function wipeMessage() {
    return (
        <Stack gap="3" className="mt-3">
            <Text kind="body/regular/sm">
                This deletes settings, logs, cluster identity, certificates, and {APP_DISPLAY_NAME}{' '}
                engine installations under the app data folder.
            </Text>
            <Text kind="body/regular/sm">
                Third-party model libraries (for example <code>~/.ollama</code> and{' '}
                <code>~/.lmstudio</code>) are <strong>not</strong> deleted.
            </Text>
        </Stack>
    )
}

export default function WipeAppDataCard() {
    const [confirmOpen, setConfirmOpen] = useState(false)
    const [wiping, setWiping] = useState(false)
    const addLocalError = useErrorsStore(state => state.addLocalError)

    const handleWipe = useCallback(async () => {
        setWiping(true)
        try {
            await window.windowApi.wipeAppData()
        } catch (err) {
            addLocalError(getErrorString(err))
            setWiping(false)
        }
    }, [addLocalError])

    return (
        <>
            <Stack gap="1">
                <Flex align="center" justify="between" gap="4">
                    <Text kind="body/semibold/md">Reset app data</Text>
                    <Button
                        kind="secondary"
                        size="small"
                        color="danger"
                        disabled={wiping}
                        onClick={() => setConfirmOpen(true)}
                    >
                        {wiping ? (
                            <span className="spinner-element" role="status" aria-label="" />
                        ) : (
                            'Reset'
                        )}
                    </Button>
                </Flex>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    Deletes {APP_DISPLAY_NAME} settings and state, then restarts the app.
                </Text>
            </Stack>

            <ConfirmModal
                open={confirmOpen}
                onOpenChange={setConfirmOpen}
                title={`Remove ${APP_DISPLAY_NAME} data and restart?`}
                message={wipeMessage()}
                confirmLabel="Remove and restart"
                confirmColor="danger"
                onConfirm={handleWipe}
            />
        </>
    )
}
