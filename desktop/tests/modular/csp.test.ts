// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { buildCspHeader } from '@/electron/csp'

describe('content security policy', () => {
    it('keeps security-critical directives restrictive', () => {
        const directives = buildCspHeader()
            .split(';')
            .map(directive => directive.trim())

        expect(directives).toContain("default-src 'none'")
        expect(directives).toContain("script-src 'self'")
        expect(directives).toContain("connect-src 'self'")
        expect(directives).toContain("worker-src 'none'")
        expect(directives).toContain("base-uri 'self'")
        expect(directives).toContain("object-src 'none'")
    })
})
