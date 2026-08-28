// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { detectLicenseType } from '@/shared/utils/detect-license'

describe('detectLicenseType', () => {
    it('detects Apache-2.0 from the standard header', () => {
        const text = `
                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/
        `
        expect(detectLicenseType(text)).toBe('Apache-2.0')
    })

    it('detects MIT from the permission notice', () => {
        const text =
            'MIT License\n\nPermission is hereby granted, free of charge, to any person obtaining a copy'
        expect(detectLicenseType(text)).toBe('MIT')
    })

    it('returns Unknown for empty or unrecognized text', () => {
        expect(detectLicenseType('')).toBe('Unknown')
        expect(detectLicenseType('some proprietary blob')).toBe('Unknown')
    })

    it('detects a single-block BSD-2-Clause license', () => {
        const text = [
            'Copyright (c) 2013, Dustin L. Howett. All rights reserved.',
            '',
            'Redistribution and use in source and binary forms, with or without',
            'modification, are permitted provided that the following conditions are met:'
        ].join('\n')
        expect(detectLicenseType(text)).toBe('BSD-2-Clause')
    })

    it('detects a single-block BSD-3-Clause license', () => {
        const text = [
            'Copyright (c) 2012 The Go Authors. All rights reserved.',
            '',
            'Redistribution and use in source and binary forms, with or without',
            'modification, are permitted provided that the following conditions are met:',
            '',
            '   * Neither the name of Google Inc. nor the names of its contributors may be'
        ].join('\n')
        expect(detectLicenseType(text)).toBe('BSD-3-Clause')
    })

    it('detects a compound BSD-2 + appended BSD-3 (Go Authors) license file', () => {
        const text = [
            'Copyright (c) 2013, Dustin L. Howett. All rights reserved.',
            '',
            'Redistribution and use in source and binary forms, with or without',
            'modification, are permitted provided that the following conditions are met:',
            '',
            '------------------------------------------------------------------------------',
            'Parts of this package were made available under the license covering',
            'the Go language and all attended core libraries. That license follows.',
            '------------------------------------------------------------------------------',
            '',
            'Copyright (c) 2012 The Go Authors. All rights reserved.',
            '',
            'Redistribution and use in source and binary forms, with or without',
            'modification, are permitted provided that the following conditions are met:',
            '',
            '   * Neither the name of Google Inc. nor the names of its contributors may be'
        ].join('\n')
        expect(detectLicenseType(text)).toBe('BSD-2-Clause AND BSD-3-Clause')
    })
})
