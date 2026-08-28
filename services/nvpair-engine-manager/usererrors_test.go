// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestUnwrapPullCause(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"action command failed: exit status 1: Error: Download failed: Timed-out. Please try to resume.",
			"Download failed: Timed-out. Please try to resume.",
		},
		{`engine "lmstudio" is not running`, `engine "lmstudio" is not running`},
		{"", ""},
	}
	for _, c := range cases {
		var err error
		if c.in != "" {
			err = errors.New(c.in)
		}
		if got := unwrapPullCause(err); got != c.want {
			t.Fatalf("unwrapPullCause(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatEnginePullError(t *testing.T) {
	err := errors.New("action command failed: exit status 1: Error: Download failed: Timed-out. Please try to resume.")
	got := formatEnginePullError("LM Studio", err)
	want := "LM Studio experienced an error while downloading a model: Download failed: Timed-out. Please try to resume."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	got = formatEnginePullError("LM Studio", errors.New(""))
	if got != "LM Studio experienced an error while downloading a model." {
		t.Fatalf("empty detail: got %q", got)
	}
}

func TestFormatEnginePullErrorNotRunning(t *testing.T) {
	err := errors.New(`engine "fake" is not running`)
	got := formatEnginePullError("Fake Engine", err)
	if !strings.Contains(got, "Fake Engine experienced an error while downloading a model:") {
		t.Fatalf("unexpected prefix: %q", got)
	}
	if !strings.Contains(got, `engine "fake" is not running`) {
		t.Fatalf("expected engine detail in %q", got)
	}
}
