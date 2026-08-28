// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import getErrorString from '@/shared/utils/get-error-string'

// `fetch` gives every transport failure the same message and puts the syscall
// that actually failed in `cause`. Reading only the top message is why a field
// capture of an endpoint that had stopped answering held 74 failures that all
// read "fetch failed" — a refused connection, an unreachable host and a reset
// were one indistinguishable string.
describe('getErrorString', () => {
    it('reads the syscall a fetch failure buries in its cause', () => {
        const err = Object.assign(new Error('fetch failed'), {
            cause: Object.assign(new Error('connect ECONNREFUSED 10.0.0.5:14318'), {
                code: 'ECONNREFUSED'
            })
        })

        // The code is not repeated: the cause's own message already names it.
        expect(getErrorString(err)).toBe('fetch failed: connect ECONNREFUSED 10.0.0.5:14318')
    })

    it('names a code the message left out', () => {
        expect(getErrorString(Object.assign(new Error('read'), { code: 'ECONNRESET' }))).toBe(
            'read (ECONNRESET)'
        )
    })

    it('does not repeat a code the message already carries', () => {
        expect(
            getErrorString(
                Object.assign(new Error('connect EHOSTUNREACH'), { code: 'EHOSTUNREACH' })
            )
        ).toBe('connect EHOSTUNREACH')
    })

    it('terminates on a cause that points back at itself', () => {
        const looped: Error & { cause?: unknown } = new Error('looped')
        looped.cause = looped

        // Collapsed rather than repeated: a cause that says what its parent said
        // adds nothing, and that alone ends a self-cycle.
        expect(getErrorString(looped)).toBe('looped')
    })

    it('terminates on a cycle the repeat guard cannot see', () => {
        const outer: Error & { cause?: unknown } = new Error('outer')
        const inner: Error & { cause?: unknown } = new Error('inner')
        outer.cause = inner
        inner.cause = outer

        // Two alternating messages never repeat their immediate parent, so depth
        // is what stops this one — bounded output instead of a hang.
        expect(getErrorString(outer)).toBe('outer: inner: outer: inner: outer')
    })

    it('still reports the plain cases it always did', () => {
        expect(getErrorString(new Error('plain'))).toBe('plain')
        expect(getErrorString({ stderr: 'command said this' })).toBe('command said this')
        expect(getErrorString({ message: 'object with a message' })).toBe('object with a message')
        expect(getErrorString('a bare string')).toBe('a bare string')
        expect(getErrorString(undefined)).toBe('')
    })
})
