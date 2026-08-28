// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex } from '@nvidia/foundations-react-core'

interface WelcomeModalActionsProps {
    step: number
    installing: boolean
    allEnginesInstalled: boolean
    onStepIntroNext: () => void
    onClose: () => void
    onInstall: () => void
}

export function WelcomeModalActions({
    step,
    installing,
    allEnginesInstalled,
    onStepIntroNext,
    onClose,
    onInstall
}: WelcomeModalActionsProps) {
    return (
        <Flex align="center" justify="end" wrap="wrap" gap="2" className="mt-3">
            {step === 0 && (
                <Button
                    color="brand"
                    size="small"
                    onClick={allEnginesInstalled ? onClose : onStepIntroNext}
                >
                    {allEnginesInstalled ? 'OK' : 'Next'}
                </Button>
            )}
            {step === 1 && (
                <>
                    <Button kind="secondary" size="small" onClick={onClose}>
                        Close
                    </Button>
                    <Button
                        color="brand"
                        size="small"
                        onClick={() => void onInstall()}
                        disabled={installing}
                    >
                        Install
                    </Button>
                </>
            )}
        </Flex>
    )
}
