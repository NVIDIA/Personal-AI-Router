// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { Badge, Flex, Stack, Text } from '@nvidia/foundations-react-core'

interface SetupStepperProps {
    activeStep: number
    steps: readonly string[]
}

export function SetupStepper({ activeStep, steps }: SetupStepperProps) {
    return (
        <nav aria-label="Device setup progress">
            <ol className="m-0 grid list-none grid-cols-1 gap-2 p-0 sm:grid-cols-2 lg:grid-cols-4">
                {steps.map((step, index) => {
                    const complete = index < activeStep
                    const current = index === activeStep
                    return (
                        <li
                            key={step}
                            aria-current={current ? 'step' : undefined}
                            className="border border-subtle-color rounded p-2"
                        >
                            <Flex align="center" gap="2">
                                <Badge
                                    color={complete || current ? 'green' : 'gray'}
                                    kind={current ? 'solid' : 'outline'}
                                >
                                    {index + 1}
                                </Badge>
                                <Text kind={current ? 'body/semibold/sm' : 'body/regular/sm'}>
                                    {step}
                                </Text>
                            </Flex>
                        </li>
                    )
                })}
            </ol>
        </nav>
    )
}

interface SetupStatusProps {
    title: string
    tone: 'neutral' | 'progress' | 'success' | 'error'
    busy?: boolean
    children?: ReactNode
}

export function SetupStatus({ title, tone, busy = false, children }: SetupStatusProps) {
    const color =
        tone === 'success'
            ? 'green'
            : tone === 'error'
              ? 'red'
              : tone === 'progress'
                ? 'yellow'
                : 'gray'
    return (
        <section
            role="status"
            aria-live="polite"
            aria-busy={busy}
            className="border border-subtle-color rounded p-3"
        >
            <Stack gap="1">
                <Flex align="center" gap="2">
                    <Badge color={color} kind="solid">
                        {tone === 'progress' ? 'In progress' : tone}
                    </Badge>
                    <Text kind="body/semibold/sm">{title}</Text>
                </Flex>
                {children && (
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {children}
                    </Text>
                )}
            </Stack>
        </section>
    )
}
