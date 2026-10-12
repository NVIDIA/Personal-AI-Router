// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestStartupOutputPreservesExplanationAndRedactsValues(t *testing.T) {
	o := newStartupOutput([]string{"--future-option", "private-value"}, map[string]string{"FUTURE_ENV": "private-env"})
	if _, err := o.Write([]byte("unknown option --future-option; input=private-value env=private-env\n")); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("exit status 1")
	err := o.failure(cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "unknown option --future-option") || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "private-env") {
		t.Fatalf("lost diagnostic or leaked supplied value: %v", err)
	}
}

func TestStartupOutputBoundsLargeWritesAndStopsCapturing(t *testing.T) {
	o := newStartupOutput([]string{"x", "x", "x"}, nil)
	data := []byte(strings.Repeat("x", maxStartupOutput*4) + "last diagnostic")
	if n, err := o.Write(data); n != len(data) || err != nil {
		t.Fatalf("writer did not consume all output: %d %v", n, err)
	}
	cause := errors.New("failed")
	err := o.failure(cause)
	if len(err.Error()) > maxStartupOutput+len(cause.Error())+2 || !strings.HasSuffix(err.Error(), "last diagnostic") {
		t.Fatalf("diagnostic tail exceeded bound or lost final message: %d bytes", len(err.Error()))
	}
	o.close()
	if _, err := o.Write([]byte("later engine log")); err != nil {
		t.Fatal(err)
	}
	if o.failure(cause) != cause {
		t.Fatal("startup capture retained output after successful startup")
	}
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
				if n, err := o.Write([]byte(chunk)); n != len(chunk) || err != nil {
					t.Fatalf("Write() = %d, %v", n, err)
				}
				if len(o.tail) > maxStartupOutput || len(o.pending) > o.lookback {
					t.Fatal("capture exceeded its diagnostic and boundary-context bounds")
				}
			}
			// Compare the entire result with redaction before truncation: checking
			// only for the full value would miss the original partial-value leak.
			want := strings.ReplaceAll(output, value, "[redacted]")
			if len(want) > maxStartupOutput {
				want = want[len(want)-maxStartupOutput:]
			}
			cause := errors.New("failed")
			got := o.failure(cause)
			if !errors.Is(got, cause) || got.Error() != "failed: "+want {
				t.Fatal("failure diagnostic differs from redact-before-truncate result")
			}
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
		if _, err := o.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if o.failure(errors.New("failed")).Error() != "failed: input=[redacted]; useful diagnostic" {
		t.Fatal("a shorter private value exposed the suffix of a longer value")
	}
}
