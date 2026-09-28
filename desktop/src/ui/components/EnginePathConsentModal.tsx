// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    Button,
    Flex,
    ModalContent,
    ModalDialog,
    ModalHeading,
    ModalRoot,
    Stack,
    Text
} from '@nvidia/foundations-react-core'
import { EngineDisplayNames } from '@/shared/constants/engines'
import type { EngineType } from '@/shared/types/engines'

const engineList = new Intl.ListFormat('en', { style: 'long', type: 'conjunction' })

/**
 * Asks before an install adds engines' command-line tools to the user's PATH.
 * One prompt covers every engine being installed together. Yes and No both
 * proceed with the install; dismissing the dialog cancels it.
 */
export function EnginePathConsentModal({
    open,
    engines,
    onAnswer,
    onCancel
}: {
    open: boolean
    engines: EngineType[]
    onAnswer: (addToPath: boolean) => void
    onCancel: () => void
}) {
    const names = engineList.format(engines.map(engine => EngineDisplayNames[engine]))
    const edited =
        window.windowApi.platform === 'Windows'
            ? 'your user environment variables'
            : 'your shell profile'

    return (
        <ModalRoot
            open={open}
            onOpenChange={next => {
                if (!next) onCancel()
            }}
        >
            <ModalDialog>
                <ModalContent className="no-drag-elements max-content-modal">
                    <ModalHeading>Add to PATH?</ModalHeading>
                    <Stack gap="4" className="-mt-2">
                        <Text kind="body/regular/sm" asChild>
                            <div>
                                Add the {names} command-line tools to your PATH so you can run them
                                from a terminal? PAIR will update {edited}. Open a new terminal
                                afterward for the change to take effect.
                            </div>
                        </Text>
                        <Flex justify="end" gap="2">
                            <Button kind="secondary" size="small" onClick={() => onAnswer(false)}>
                                No
                            </Button>
                            <Button kind="primary" size="small" onClick={() => onAnswer(true)}>
                                Yes
                            </Button>
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
