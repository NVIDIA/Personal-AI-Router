// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, Stack, Text, TextInput } from '@nvidia/foundations-react-core'
import { Check } from '@/ui/components/icons'

/**
 * The model an engine serves, for an engine that runs one model per process
 * (vLLM). It is a start-time setting, not a model operation: the engine manager
 * persists it and restarts the engine onto it, and the engine downloads the
 * weights itself on that first start. The local node only — the engine manager
 * writes manifest overrides for its own host and the backend exposes no remote
 * served-model control, so `PortsSection` does not render this for a peer.
 */
export function ServedModelRow({
    model,
    changed,
    disabled,
    onChange,
    onApply
}: {
    model: string
    /** True when the draft differs from the engine-reported value. */
    changed: boolean
    disabled: boolean
    onChange: (value: string) => void
    onApply: () => void
}) {
    return (
        <Stack gap="2" className="pt-4">
            <Stack gap="1">
                <Text kind="body/regular/sm">Model to serve</Text>
                <Text kind="body/regular/xs" style={{ opacity: 0.7 }}>
                    A Hugging Face model id, for example Qwen/Qwen3-8B. The engine serves this one
                    model and downloads it on the next start, which can take several minutes.
                </Text>
            </Stack>
            <Flex
                gap="6"
                align="end"
                style={{
                    opacity: disabled ? 0.5 : 1,
                    pointerEvents: disabled ? 'none' : 'auto'
                }}
            >
                <TextInput
                    value={model}
                    onValueChange={onChange}
                    disabled={disabled}
                    size="small"
                    placeholder="Qwen/Qwen3-8B"
                    className="grow"
                />
                <Button
                    kind="primary"
                    color="brand"
                    size="small"
                    onClick={onApply}
                    disabled={disabled || !changed}
                >
                    <Flex align="center" gap="2">
                        <Check style={{ fontSize: 16 }} />
                        Apply model
                    </Flex>
                </Button>
            </Flex>
        </Stack>
    )
}
