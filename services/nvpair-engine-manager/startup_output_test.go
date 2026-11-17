// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStartupOutput_ExplainsMetalMemoryFailures(t *testing.T) {
	test := func(name, diagnostic string, chunkSize int) {
		t.Run(name, func(t *testing.T) {
			output := newStartupOutput(nil, nil)
			writeStartupOutputInChunks(t, output, diagnostic, chunkSize)
			cause := errors.New("process exited before readiness; check the launch options")
			err := output.failure(t.Context(), cause)
			require.ErrorIs(t, err, cause)
			require.ErrorContains(t, err, "Not enough available GPU memory to start this engine.")
			require.ErrorContains(t, err, "Unload models from other engines or free memory, then try again.")
			require.ErrorContains(t, err, "\n\nEngine details:\n")
			require.ErrorContains(t, err, diagnostic)
			require.NotContains(t, err.Error(), "check the launch options")
		})
	}
	// Captured vLLM-Metal diagnostic; the same Metal signature is reported at
	// https://github.com/ml-explore/mlx-examples/issues/481.
	test("reported Metal command-buffer failure",
		"RuntimeError: [METAL] Command buffer execution failed: Insufficient Memory (00000008:kIOGPUCommandBufferCallbackErrorOutOfMemory).",
		4096)
	test("Metal command-buffer failure without callback code", "[METAL] Command buffer execution failed: Insufficient Memory", 4096)
	test("Metal memory message without callback code", "RuntimeError: [METAL] Insufficient Memory", 4096)
	test("callback code split across writes", "kIOGPUCommandBufferCallbackErrorOutOfMemory", 1)
	test("case-insensitive Metal message", "[metal] insufficient memory", 4096)
}

func TestStartupOutput_RemembersMetalMemoryFailureAfterTheTailRolls(t *testing.T) {
	massiveLogMessage := "[METAL] Insufficient Memory\n" +
		strings.Repeat("follow-up traceback\n", maxStartupOutputBytes) +
		"Engine core initialization failed."
	output := newStartupOutput(nil, nil)
	_, err := output.Write([]byte(massiveLogMessage))
	require.NoError(t, err)
	// bringUpProcess produces this readiness error when its child exits. The
	// memory summary replaces its generic advice while retaining the cause.
	cause := errors.New("process exited before readiness; check the launch options")
	err = output.failure(t.Context(), cause)
	require.ErrorIs(t, err, cause)
	memoryAdvice := "Not enough available GPU memory to start this engine. Unload models from other engines or free memory, then try again."
	require.ErrorContains(t, err, memoryAdvice)
	require.ErrorContains(t, err, "Engine core initialization failed.")
	require.NotContains(t, err.Error(), "[METAL]", "the original line must have rolled out of the bounded diagnostic tail")
	maxErrorBytes := maxStartupOutputBytes + len(memoryAdvice) + len("\n\nEngine details:\n")
	require.LessOrEqual(t, len(err.Error()), maxErrorBytes)
}

func TestStartupOutput_PreservesOtherFailureExplanations(t *testing.T) {
	test := func(name, diagnostic string) {
		t.Run(name, func(t *testing.T) {
			output := newStartupOutput(nil, nil)
			_, err := output.Write([]byte(diagnostic))
			require.NoError(t, err)
			cause := errors.New("process exited before readiness; check the launch options")
			err = output.failure(t.Context(), cause)
			require.ErrorIs(t, err, cause)
			require.ErrorContains(t, err, cause.Error())
			require.ErrorContains(t, err, diagnostic)
			require.NotContains(t, err.Error(), "Not enough available GPU memory")
		})
	}
	// Synthetic negative controls check that unrelated diagnostics retain their
	// explanation, including CPU errors printed after an unrelated Metal line.
	test("unknown option", "unknown option --future-option")
	test("unavailable Metal device", "[METAL] device unavailable")
	test("CPU memory failure after a Metal log line", "[METAL] device available\nCPU: Insufficient Memory")
	test("CPU-only file memory failure", "Insufficient Memory while reading a CPU-only file")
	test("CUDA illegal memory access", "CUDA error: illegal memory access")
	test("generic engine initialization failure", "Engine core initialization failed.")
	// Real CUDA OOM example: https://github.com/pytorch/pytorch/issues/178057.
	// CUDA errors remain outside this Metal-specific classification.
	test("CUDA out of memory", "torch.OutOfMemoryError: CUDA out of memory.")
}

func TestStartupOutput_RedactsValuesInMemoryFailures(t *testing.T) {
	output := newStartupOutput(nil, map[string]string{"CUSTOM": "private-value"})
	_, err := output.Write([]byte("[METAL] Insufficient Memory; environment=private-value"))
	require.NoError(t, err)
	err = output.failure(t.Context(), errors.New("process exited before readiness"))
	require.ErrorContains(t, err, "Not enough available GPU memory")
	require.ErrorContains(t, err, "environment=[redacted]")
	require.NotContains(t, err.Error(), "private-value")
}

func TestStartupOutput_DoesNotClassifyPrivateValuesAsMemoryFailures(t *testing.T) {
	// An engine may echo a rejected argument value. That value must not become
	// evidence of a memory failure merely because it resembles one.
	private := "[METAL] Insufficient Memory"
	output := newStartupOutput([]string{"--future-value=" + private}, nil)
	_, err := output.Write([]byte("invalid value: " + private))
	require.NoError(t, err)
	require.EqualError(t, output.failure(t.Context(), errors.New("failed")), "failed: invalid value: [redacted]")
}

func TestStartupOutput_CloseDiscardsCapturedMemoryFailure(t *testing.T) {
	output := newStartupOutput(nil, nil)
	_, err := output.Write([]byte("[METAL] Insufficient Memory"))
	require.NoError(t, err)
	output.close()
	_, err = output.Write([]byte("later engine log"))
	require.NoError(t, err)
	cause := errors.New("later failure")
	require.Same(t, cause, output.failure(t.Context(), cause))
}

func TestStartupOutput_PreservesInterruptionAfterMetalMemoryOutput(t *testing.T) {
	test := func(name string, cause error) {
		t.Run(name, func(t *testing.T) {
			output := newStartupOutput(nil, nil)
			_, err := output.Write([]byte("[METAL] Insufficient Memory"))
			require.NoError(t, err)

			err = output.failure(t.Context(), cause)

			require.ErrorIs(t, err, cause)
			require.NotContains(t, err.Error(), "Not enough available GPU memory")
		})
	}
	test("cancelled operation", context.Canceled)
	test("expired deadline", context.DeadlineExceeded)
}

func TestStartupOutput_PreservesExplanationAndRedactsValues(t *testing.T) {
	o := newStartupOutput([]string{"--future-option", "private-value"}, map[string]string{"FUTURE_ENV": "private-env"})
	_, err := o.Write([]byte("unknown option --future-option; input=private-value env=private-env\n"))
	require.NoError(t, err)
	cause := errors.New("exit status 1")
	err = o.failure(t.Context(), cause)
	require.ErrorIs(t, err, cause, "lost diagnostic or leaked supplied value")
	require.ErrorContains(t, err, "unknown option --future-option", "lost diagnostic or leaked supplied value")
	require.NotContains(t, err.Error(), "private-value", "lost diagnostic or leaked supplied value (%v)", err)
	require.NotContains(t, err.Error(), "private-env", "lost diagnostic or leaked supplied value (%v)", err)
}

func TestStartupOutput_BoundsLargeWrites(t *testing.T) {
	o := newStartupOutput([]string{"x", "x", "x"}, nil)
	data := []byte(strings.Repeat("x", maxStartupOutputBytes*4) + "last diagnostic")
	n, err := o.Write(data)
	require.Equal(t, len(data), n, "writer did not consume all output (%v)", err)
	require.NoError(t, err, "writer did not consume all output (%v, %v)", n, err)
	cause := errors.New("failed")
	err = o.failure(t.Context(), cause)
	require.LessOrEqual(t, len(err.Error()), maxStartupOutputBytes+len(cause.Error())+len(": "), "diagnostic tail exceeded bound or lost final message")
	require.True(t, strings.HasSuffix(err.Error(), "last diagnostic"), "diagnostic tail exceeded bound or lost final message")
}

func TestStartupOutput_RedactsAcrossCaptureBoundaries(t *testing.T) {
	test := func(name, value, before, after string, chunkSize int) {
		t.Run(name, func(t *testing.T) {
			// Exercise both argument and environment collection without relying on
			// an engine-specific credential name.
			o := newStartupOutput([]string{"--future-value=" + value}, map[string]string{"CUSTOM": value})
			output := before + value + after
			writeStartupOutputInChunks(t, o, output, chunkSize)
			// Compare the entire result with redaction before truncation: checking
			// only for the full value would miss the original partial-value leak.
			want := strings.ReplaceAll(output, value, "[redacted]")
			if len(want) > maxStartupOutputBytes {
				want = want[len(want)-maxStartupOutputBytes:]
			}
			cause := errors.New("failed")
			got := o.failure(t.Context(), cause)
			require.ErrorIs(t, got, cause, "failure diagnostic differs from redact-before-truncate result")
			require.EqualError(t, got, "failed: "+want, "failure diagnostic differs from redact-before-truncate result")
		})
	}

	test("value longer than capture", strings.Repeat("private", 1500), "input=", "; useful diagnostic", maxStartupOutputBytes*4)
	test("tail begins inside value", "private-value", "input=", strings.Repeat("z", maxStartupOutputBytes-5), maxStartupOutputBytes*4)
	test("value split between writes", "private-value", "input=", "; useful diagnostic", 1)
	test("long value split between writes", strings.Repeat("private", 1500), "input=", "; useful diagnostic", 3071)
	test("literal regexp punctuation", "[private].*($value)", "input=", "; useful diagnostic", 3)
	test("expanding replacements", "x", strings.Repeat("x", maxStartupOutputBytes*2), "; useful diagnostic", 517)
}

func TestStartupOutput_PrefersLongestPrivateValueAcrossWrites(t *testing.T) {
	o := newStartupOutput([]string{"private", "private-longer-value"}, nil)
	for _, part := range []string{"input=private", "-longer-value; useful diagnostic"} {
		_, err := o.Write([]byte(part))
		require.NoError(t, err)
	}
	require.EqualError(t, o.failure(t.Context(), errors.New("failed")), "failed: input=[redacted]; useful diagnostic", "a shorter private value exposed the suffix of a longer value")
}

func writeStartupOutputInChunks(t *testing.T, output *startupOutput, text string, chunkSize int) {
	t.Helper()
	for chunk := range slices.Chunk([]byte(text), chunkSize) {
		n, err := output.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
		require.LessOrEqual(t, len(output.tail), maxStartupOutputBytes, "diagnostic tail exceeded its byte bound")
		require.LessOrEqual(t, len(output.pending), output.lookback, "pending output exceeded its redaction lookback")
	}
}

func TestStartupOutput_ReadyTimeoutShowsTheLastReadableLines(t *testing.T) {
	output := newStartupOutput(nil, nil)
	var log strings.Builder
	log.WriteString("tail of a line cut mid-way\n")
	log.WriteString(strings.Repeat("x", 500) + "\n")
	for i := range 20 {
		fmt.Fprintf(&log, "step %d\n", i)
	}
	writeStartupOutputInChunks(t, output, log.String(), 64)
	cause := &readyTimeoutError{timeout: 30 * time.Minute}
	err := output.failure(t.Context(), cause)
	require.ErrorIs(t, err, cause)
	require.ErrorContains(t, err, "not ready after 30m0s\n\nLast engine output:\nstep 8\n")
	require.ErrorContains(t, err, "step 19")
	require.NotContains(t, err.Error(), "step 7\n")
	require.NotContains(t, err.Error(), "mid-way")
	require.NotContains(t, err.Error(), strings.Repeat("x", 500))
	require.Equal(t, strings.Repeat("x", readyTimeoutLineWidth)+"…", recentLines(strings.Repeat("x", 500), 1, readyTimeoutLineWidth))
}
