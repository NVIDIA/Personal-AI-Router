// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartupOutputPreservesExplanationAndRedactsValues(t *testing.T) {
	o := newStartupOutput([]string{"--future-option", "private-value"}, map[string]string{"FUTURE_ENV": "private-env"})
	_, err := o.Write([]byte("unknown option --future-option; input=private-value env=private-env\n"))
	require.NoError(t, err)
	cause := errors.New("exit status 1")
	err = o.failure(cause)
	require.ErrorIs(t, err, cause, "lost diagnostic or leaked supplied value")
	require.ErrorContains(t, err, "unknown option --future-option", "lost diagnostic or leaked supplied value")
	require.NotContains(t, err.Error(), "private-value", "lost diagnostic or leaked supplied value (%v)", err)
	require.NotContains(t, err.Error(), "private-env", "lost diagnostic or leaked supplied value (%v)", err)
}

func TestStartupOutputBoundsLargeWritesAndStopsCapturing(t *testing.T) {
	o := newStartupOutput([]string{"x", "x", "x"}, nil)
	data := []byte(strings.Repeat("x", maxStartupOutput*4) + "last diagnostic")
	n, err := o.Write(data)
	require.Equal(t, len(data), n, "writer did not consume all output (%v)", err)
	require.NoError(t, err, "writer did not consume all output (%v, %v)", n, err)
	cause := errors.New("failed")
	err = o.failure(cause)
	require.LessOrEqual(t, len(err.Error()), maxStartupOutput+len(cause.Error())+2, "diagnostic tail exceeded bound or lost final message")
	require.True(t, strings.HasSuffix(err.Error(), "last diagnostic"), "diagnostic tail exceeded bound or lost final message")
	o.close()
	_, err = o.Write([]byte("later engine log"))
	require.NoError(t, err)
	require.Same(t, cause, o.failure(cause), "startup capture retained output after successful startup")
}

func TestStartupOutputRedactsAcrossCaptureBoundaries(t *testing.T) {
	test := func(name, value, before, after string, chunkSize int) {
		t.Run(name, func(t *testing.T) {
			// Exercise both argument and environment collection without relying on
			// an engine-specific credential name.
			o := newStartupOutput([]string{"--future-value=" + value}, map[string]string{"CUSTOM": value})
			output := before + value + after
			for offset := 0; offset < len(output); offset += chunkSize {
				chunk := output[offset:min(offset+chunkSize, len(output))]
				n, err := o.Write([]byte(chunk))
				require.Equal(t, len(chunk), n, "Write() (%v)", err)
				require.NoError(t, err, "Write() (%v, %v)", n, err)
				require.LessOrEqual(t, len(o.tail), maxStartupOutput, "capture exceeded its diagnostic and boundary-context bounds")
				require.LessOrEqual(t, len(o.pending), o.lookback, "capture exceeded its diagnostic and boundary-context bounds")
			}
			// Compare the entire result with redaction before truncation: checking
			// only for the full value would miss the original partial-value leak.
			want := strings.ReplaceAll(output, value, "[redacted]")
			if len(want) > maxStartupOutput {
				want = want[len(want)-maxStartupOutput:]
			}
			cause := errors.New("failed")
			got := o.failure(cause)
			require.ErrorIs(t, got, cause, "failure diagnostic differs from redact-before-truncate result")
			require.EqualError(t, got, "failed: "+want, "failure diagnostic differs from redact-before-truncate result")
		})
	}

	test("value longer than capture", strings.Repeat("private", 1500), "input=", "; useful diagnostic", maxStartupOutput*4)
	test("tail begins inside value", "private-value", "input=", strings.Repeat("z", maxStartupOutput-5), maxStartupOutput*4)
	test("value split between writes", "private-value", "input=", "; useful diagnostic", 1)
	test("long value split between writes", strings.Repeat("private", 1500), "input=", "; useful diagnostic", 3071)
	test("literal regexp punctuation", "[private].*($value)", "input=", "; useful diagnostic", 3)
	test("expanding replacements", "x", strings.Repeat("x", maxStartupOutput*2), "; useful diagnostic", 517)
}

func TestStartupOutputPrefersLongestPrivateValueAcrossWrites(t *testing.T) {
	o := newStartupOutput([]string{"private", "private-longer-value"}, nil)
	for _, part := range []string{"input=private", "-longer-value; useful diagnostic"} {
		_, err := o.Write([]byte(part))
		require.NoError(t, err)
	}
	require.Equal(t, "failed: input=[redacted]; useful diagnostic", o.failure(errors.New("failed")).Error(), "a shorter private value exposed the suffix of a longer value")
}
