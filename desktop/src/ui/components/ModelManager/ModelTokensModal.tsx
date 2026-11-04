// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useState, useEffect } from 'react'
import {
    Button,
    Flex,
    FormField,
    ModalContent,
    ModalDialog,
    ModalHeading,
    ModalRoot,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'

const TOKEN_PRESETS = [
    { label: '2K (2,048)', value: 2048, desc: 'Default / Fast' },
    { label: '4K (4,096)', value: 4096, desc: 'Balanced' },
    { label: '8K (8,192)', value: 8192, desc: 'Coding & Long Chat' },
    { label: '16K (16,384)', value: 16384, desc: 'Large Context' },
    { label: '32K (32,768)', value: 32768, desc: 'Deep Analysis' }
]

function getVramEstimate(tokens: number): string {
    if (tokens <= 2048) return '~0.5 GB KV Cache'
    if (tokens <= 4096) return '~1.0 GB KV Cache'
    if (tokens <= 8192) return '~1.8 - 2.5 GB KV Cache'
    if (tokens <= 16384) return '~3.5 - 5.0 GB KV Cache'
    return '~7+ GB KV Cache (requires high VRAM)'
}

export function ModelTokensModal({
    open,
    onOpenChange,
    modelName,
    initialTokens = 2048,
    onApply
}: {
    open: boolean
    onOpenChange: (open: boolean) => void
    modelName: string
    initialTokens?: number
    onApply: (tokens: number) => void
}) {
    const [tokenValue, setTokenValue] = useState<string>(String(initialTokens))

    useEffect(() => {
        if (open) {
            setTokenValue(String(initialTokens))
        }
    }, [open, initialTokens])

    const numericTokens = parseInt(tokenValue, 10) || 2048
    const isValid = numericTokens >= 512 && numericTokens <= 131072

    const handleApply = () => {
        if (!isValid) return
        onApply(numericTokens)
        onOpenChange(false)
    }

    return (
        <ModalRoot open={open} onOpenChange={onOpenChange}>
            <ModalDialog className="max-w-lg w-full">
                <ModalHeading>
                    <Flex direction="column" gap="1">
                        <Text kind="body/bold/md">Configure Model Tokens</Text>
                        <Text kind="body/regular/sm" className="text-subtle-color truncate">
                            {modelName}
                        </Text>
                    </Flex>
                </ModalHeading>

                <ModalContent>
                    <Stack gap="4">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            Adjust the context window token limit (<code>num_ctx</code> /{' '}
                            <code>--ctx-size</code>). A larger context allows longer prompts and
                            broader history, but uses more GPU memory.
                        </Text>

                        <Stack gap="2">
                            <Text kind="body/semibold/sm">Quick Presets</Text>
                            <Flex wrap="wrap" gap="2">
                                {TOKEN_PRESETS.map(preset => {
                                    const isSelected = numericTokens === preset.value
                                    return (
                                        <Button
                                            key={preset.value}
                                            size="small"
                                            kind={isSelected ? 'primary' : 'secondary'}
                                            onClick={() => setTokenValue(String(preset.value))}
                                        >
                                            {preset.label}
                                        </Button>
                                    )
                                })}
                            </Flex>
                        </Stack>

                        <FormField slotLabel="Custom Context Size (Tokens)">
                            <TextInput
                                size="small"
                                inputMode="numeric"
                                value={tokenValue}
                                onValueChange={setTokenValue}
                                placeholder="e.g. 8192"
                            />
                        </FormField>
                        <Text kind="body/regular/xs" className="text-subtle-color -mt-2">
                            {`Estimated memory overhead: ${getVramEstimate(numericTokens)}`}
                        </Text>

                        <Flex justify="end" gap="2" className="mt-2">
                            <Button
                                kind="secondary"
                                size="small"
                                onClick={() => onOpenChange(false)}
                            >
                                Cancel
                            </Button>
                            <Button
                                kind="primary"
                                size="small"
                                disabled={!isValid}
                                onClick={handleApply}
                            >
                                Apply &amp; Load
                            </Button>
                        </Flex>
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
