// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnwrapPullCause(t *testing.T) {
	test := func(name string, err error, want string) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, unwrapPullCause(err))
		})
	}
	test("command prefix removed", errors.New("action command failed: exit status 1: Error: Download failed: Timed-out. Please try to resume."), "Download failed: Timed-out. Please try to resume.")
	test("engine detail preserved", errors.New(`engine "lmstudio" is not running`), `engine "lmstudio" is not running`)
	test("nil error", nil, "")
}

func TestFormatEnginePullError(t *testing.T) {
	err := errors.New("action command failed: exit status 1: Error: Download failed: Timed-out. Please try to resume.")
	got := formatEnginePullError("LM Studio", err)
	want := "LM Studio experienced an error while downloading a model: Download failed: Timed-out. Please try to resume."
	require.Equal(t, want, got)

	got = formatEnginePullError("LM Studio", errors.New(""))
	require.Equal(t, "LM Studio experienced an error while downloading a model.", got, "empty detail:")
}

func TestFormatEnginePullErrorNotRunning(t *testing.T) {
	err := errors.New(`engine "fake" is not running`)
	got := formatEnginePullError("Fake Engine", err)
	require.Contains(t, got, "Fake Engine experienced an error while downloading a model:", "unexpected prefix")
	require.Contains(t, got, `engine "fake" is not running`, "expected engine detail in")
}
