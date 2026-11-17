// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartupOutput_ExplainsMetalMemoryFailures(t *testing.T) {
	test := func(name, diagnostic string, chunkSize int) {
		t.Run(name, func(t *testing.T) {
			output := newStartupOutput(nil, nil)
			for offset := 0; offset < len(diagnostic); offset += chunkSize {
				_, err := output.Write([]byte(diagnostic[offset:min(offset+chunkSize, len(diagnostic))]))
				require.NoError(t, err)
			}
			cause := errors.New("process exited before readiness; check the launch options")
			err := output.failure(cause)
			require.ErrorIs(t, err, cause)
			require.True(t, strings.HasPrefix(err.Error(), "Not enough available GPU memory to start this engine."))
			require.ErrorContains(t, err, "Unload models from other engines or free memory, then try again.")
			require.ErrorContains(t, err, "\n\nEngine details:\n")
			require.ErrorContains(t, err, diagnostic)
			require.NotContains(t, err.Error(), "check the launch options")
		})
	}
	test("reported Metal command-buffer failure",
		"RuntimeError: [METAL] Command buffer execution failed: Insufficient Memory (00000008:kIOGPUCommandBufferCallbackErrorOutOfMemory).",
		4096)
	test("Metal memory message without callback code", "RuntimeError: [METAL] Insufficient Memory", 4096)
	test("callback code split across writes", "kIOGPUCommandBufferCallbackErrorOutOfMemory", 1)
	test("case-insensitive Metal message", "[metal] insufficient memory", 4096)
}

func TestStartupOutput_RemembersMetalMemoryFailureAfterTheTailRolls(t *testing.T) {
	output := newStartupOutput(nil, nil)
	_, err := output.Write([]byte("[METAL] Insufficient Memory\n" + strings.Repeat("follow-up traceback\n", maxStartupOutput) + "Engine core initialization failed."))
	require.NoError(t, err)
	cause := errors.New("process exited before readiness")
	err = output.failure(cause)
	require.ErrorIs(t, err, cause)
	require.ErrorContains(t, err, "Not enough available GPU memory")
	require.ErrorContains(t, err, "Engine core initialization failed.")
	require.NotContains(t, err.Error(), "[METAL]", "the original line must have rolled out of the bounded diagnostic tail")
	require.Less(t, len(err.Error()), maxStartupOutput+256)
}

func TestStartupOutput_PreservesOtherFailureExplanations(t *testing.T) {
	test := func(name, diagnostic string) {
		t.Run(name, func(t *testing.T) {
			output := newStartupOutput(nil, nil)
			_, err := output.Write([]byte(diagnostic))
			require.NoError(t, err)
			cause := errors.New("process exited before readiness; check the launch options")
			err = output.failure(cause)
			require.ErrorIs(t, err, cause)
			require.EqualError(t, err, cause.Error()+": "+diagnostic)
		})
	}
	test("unknown option", "unknown option --future-option")
	test("unavailable Metal device", "[METAL] device unavailable")
	test("CPU memory failure after a Metal log line", "[METAL] device available\nCPU: Insufficient Memory")
	test("CPU-only file memory failure", "Insufficient Memory while reading a CPU-only file")
	test("CUDA illegal memory access", "CUDA error: illegal memory access")
	test("generic engine initialization failure", "Engine core initialization failed.")
}

func TestStartupOutput_RedactsValuesInMemoryFailures(t *testing.T) {
	output := newStartupOutput(nil, map[string]string{"CUSTOM": "private-value"})
	_, err := output.Write([]byte("[METAL] Insufficient Memory; environment=private-value"))
	require.NoError(t, err)
	err = output.failure(errors.New("process exited before readiness"))
	require.ErrorContains(t, err, "Not enough available GPU memory")
	require.ErrorContains(t, err, "environment=[redacted]")
	require.NotContains(t, err.Error(), "private-value")
}

func TestStartupOutput_DoesNotClassifyPrivateValuesAsMemoryFailures(t *testing.T) {
	private := "[METAL] Insufficient Memory"
	output := newStartupOutput([]string{"--future-value=" + private}, nil)
	_, err := output.Write([]byte("invalid value: " + private))
	require.NoError(t, err)
	require.EqualError(t, output.failure(errors.New("failed")), "failed: invalid value: [redacted]")
}

func TestStartupOutput_ClearsTheMemoryReasonAfterSuccessfulStartup(t *testing.T) {
	output := newStartupOutput(nil, nil)
	_, err := output.Write([]byte("[METAL] Insufficient Memory"))
	require.NoError(t, err)
	output.close()
	cause := errors.New("later failure")
	require.Same(t, cause, output.failure(cause))
}

func TestStartupOutput_PreservesCancellationAfterMetalMemoryOutput(t *testing.T) {
	output := newStartupOutput(nil, nil)
	_, err := output.Write([]byte("[METAL] Insufficient Memory"))
	require.NoError(t, err)
	err = output.failure(context.Canceled)
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "Not enough available GPU memory")
}

func TestStartupOutput_PreservesExplanationAndRedactsValues(t *testing.T) {
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

func TestStartupOutput_BoundsLargeWritesAndStopsCapturing(t *testing.T) {
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

func TestStartupOutput_RedactsAcrossCaptureBoundaries(t *testing.T) {
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

func TestStartupOutput_PrefersLongestPrivateValueAcrossWrites(t *testing.T) {
	o := newStartupOutput([]string{"private", "private-longer-value"}, nil)
	for _, part := range []string{"input=private", "-longer-value; useful diagnostic"} {
		_, err := o.Write([]byte(part))
		require.NoError(t, err)
	}
	require.Equal(t, "failed: input=[redacted]; useful diagnostic", o.failure(errors.New("failed")).Error(), "a shorter private value exposed the suffix of a longer value")
}
