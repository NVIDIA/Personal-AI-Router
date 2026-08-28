// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Text } from '@nvidia/foundations-react-core'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

interface WelcomeIntroStepProps {
    allEnginesInstalled: boolean
}

export function WelcomeIntroStep({ allEnginesInstalled }: WelcomeIntroStepProps) {
    return (
        <>
            <Text kind="body/regular/sm" className="text-subtle-color">
                {APP_DISPLAY_NAME} connects your machines into one pool so you can run inference
                across a cluster.
            </Text>
            <Text kind="body/regular/sm" className="text-subtle-color">
                You can pair other machines anytime from Settings → Cluster.
            </Text>
            <Text kind="body/regular/sm" className="text-subtle-color">
                {allEnginesInstalled
                    ? 'Your engines are ready.'
                    : 'Next, optionally install inference engines in the background.'}
            </Text>
        </>
    )
}
