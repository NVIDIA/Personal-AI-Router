// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { ThemeProvider, TooltipProvider } from '@nvidia/foundations-react-core'

interface FoundationsThemeProviderProps {
    children: ReactNode
}

export function FoundationsThemeProvider({ children }: FoundationsThemeProviderProps) {
    return (
        <ThemeProvider
            global
            target="html"
            theme="dark"
            density="compact"
            className="h-full w-full"
        >
            <TooltipProvider>{children}</TooltipProvider>
        </ThemeProvider>
    )
}
