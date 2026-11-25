// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { Stack, Switch, Text } from '@nvidia/foundations-react-core'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import getErrorString from '@/shared/utils/get-error-string'

export default function MacAppearanceSettings() {
    const [enabled, setEnabled] = useState(false)
    const [busy, setBusy] = useState(true)
    const [error, setError] = useState<string | null>(null)

    useEffect(() => {
        let active = true
        window.windowApi
            .getHideDockIconInBackground()
            .then(value => {
                if (active) setEnabled(value)
            })
            .catch(err => {
                if (active) setError(getErrorString(err))
            })
            .finally(() => {
                if (active) setBusy(false)
            })
        return () => {
            active = false
        }
    }, [])

    const changePreference = async (value: boolean): Promise<void> => {
        setBusy(true)
        setError(null)
        try {
            await window.windowApi.setHideDockIconInBackground(value)
            setEnabled(value)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setBusy(false)
        }
    }

    return (
        <Stack gap="2">
            <Text kind="body/semibold/md">Appearance</Text>
            <Switch
                slotLabel="Hide Dock icon when the window is hidden"
                checked={enabled}
                disabled={busy}
                onCheckedChange={value => void changePreference(value)}
            />
            <Text kind="body/regular/sm" className="text-subtle-color">
                PAIR keeps running in the menu bar. Reopening the window restores the Dock icon.
            </Text>
            {error && <InlineErrorBanner severity="error" message={error} />}
        </Stack>
    )
}
