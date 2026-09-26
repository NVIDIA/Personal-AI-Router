// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestInstallSupportRequiresDeclaredCommands(t *testing.T) {
	platform := &Platform{Install: &Install{Requires: []string{"definitely-not-an-nvpair-command"}}}
	supported, reason := installSupport("fake", platform)
	if supported || !strings.Contains(reason, "definitely-not-an-nvpair-command") {
		t.Fatalf("installSupport = (%v, %q)", supported, reason)
	}
}

func TestInstallRequirementValidationFailsClosed(t *testing.T) {
	manifest := testEngineManifest(fakeEngineBin)
	for key, platform := range manifest.Platforms {
		platform.Install = &Install{Requires: []string{" "}}
		manifest.Platforms[key] = platform
	}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("Validate() = %v", err)
	}
}
