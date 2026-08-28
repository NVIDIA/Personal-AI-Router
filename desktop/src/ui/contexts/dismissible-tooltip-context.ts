// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createContext } from 'react'

type DismissibleTooltipContextValue = {
    dismiss: () => void
}

export const DismissibleTooltipContext = createContext<DismissibleTooltipContextValue | null>(null)
