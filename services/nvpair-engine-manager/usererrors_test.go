// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"testing"

	"github.com/stretchr/testify/require"
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
		{
			got := unwrapPullCause(err)
			require.True(t, got == c.want, "unwrapPullCause (%v)", got)
		}
	}
}

func TestFormatEnginePullError(t *testing.T) {
	err := errors.New("action command failed: exit status 1: Error: Download failed: Timed-out. Please try to resume.")
	got := formatEnginePullError("LM Studio", err)
	want := "LM Studio experienced an error while downloading a model: Download failed: Timed-out. Please try to resume."
	require.True(t, got == want, "got (%v, %v)", got, want)

	got = formatEnginePullError("LM Studio", errors.New(""))
	require.Equal(t, "LM Studio experienced an error while downloading a model.", got, "empty detail:")
}

func TestFormatEnginePullErrorNotRunning(t *testing.T) {
	err := errors.New(`engine "fake" is not running`)
	got := formatEnginePullError("Fake Engine", err)
	require.Contains(t, got, "Fake Engine experienced an error while downloading a model:", "unexpected prefix")
	require.Contains(t, got, `engine "fake" is not running`, "expected engine detail in")
}
