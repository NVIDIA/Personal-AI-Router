// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { version as appPackageVersion } from '@/../package.json'
import type { ServiceVersions } from '@/shared/types/ipc-channels'
import { isElectron } from '@/ui/api/bootstrap'
import { OpenInNew } from '@/ui/components/icons'

function VersionRow({ label, value }: { label: string; value: string }) {
    return (
        <Flex justify="between" align="center" gap="4">
            {label && <Text kind="body/regular/sm">{label}</Text>}
            {value && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {`v${value}`}
                </Text>
            )}
        </Flex>
    )
}

export default function VersionsCard() {
    const [versions, setVersions] = useState<ServiceVersions | null>(null)

    useEffect(() => {
        if (!isElectron) return
        window.windowApi.service
            .getVersions()
            .then(setVersions)
            .catch(() => {})
    }, [])

    const appVersion = versions?.appVersion ?? appPackageVersion
    const binaries = versions?.binaries ?? []
    const licenseType = versions?.licenseType ?? ''

    const openLicense = () => {
        void window.windowApi.service.openLicense().catch(() => {})
    }

    const openThirdPartyLicenses = () => {
        void window.windowApi.service.openThirdPartyLicenses().catch(() => {})
    }

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="4">
                <Stack gap="1">
                    <Text kind="body/semibold/md">About</Text>
                    {isElectron && (
                        <Flex align="center" gap="4">
                            {licenseType && <Text kind="body/bold/sm">{licenseType}</Text>}
                            <button
                                type="button"
                                onClick={openLicense}
                                className="no-bg-link cursor-pointer pair-link self-start"
                            >
                                <Flex align="center" gap="1">
                                    <Text kind="body/regular/sm">View license</Text>
                                    <OpenInNew style={{ fontSize: 14, marginTop: -2 }} />
                                </Flex>
                            </button>
                            <button
                                type="button"
                                onClick={openThirdPartyLicenses}
                                className="no-bg-link cursor-pointer pair-link self-start"
                            >
                                <Flex align="center" gap="1">
                                    <Text kind="body/regular/sm">Third-party licenses</Text>
                                    <OpenInNew style={{ fontSize: 14, marginTop: -2 }} />
                                </Flex>
                            </button>
                        </Flex>
                    )}
                </Stack>

                <Stack gap="2">
                    <Stack gap="2">
                        <VersionRow label="Application" value={appVersion} />
                        {versions?.modularProduct && (
                            <VersionRow label="Services" value={versions.modularProduct} />
                        )}
                    </Stack>

                    {binaries.length > 0 && (
                        <Stack gap="2">
                            {binaries.map(binary => (
                                <VersionRow
                                    key={binary.name}
                                    label={binary.name}
                                    value={binary.version}
                                />
                            ))}
                        </Stack>
                    )}
                </Stack>
            </Stack>
        </div>
    )
}
