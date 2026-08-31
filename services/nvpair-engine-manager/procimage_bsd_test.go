// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package main

import (
	"os"
	"testing"
)

// TestProcImageSurvivesAnEmptyPath is the guard for the deployment shape rather
// than the developer one. This process inherits whatever PATH the desktop app
// was launched with, and nothing between Electron, the broker and this worker
// sets one. If lsof were resolved by bare name, a narrowed PATH would return
// this to the original bug — orphan reclaim refused, with the same misleading
// "external management" message and nothing to say a tool was missing.
func TestProcImageSurvivesAnEmptyPath(t *testing.T) {
	if _, err := os.Stat(macOSLsof); err != nil {
		t.Skipf("no lsof at %s on this host: %v", macOSLsof, err)
	}
	t.Setenv("PATH", "")

	got := procImage(os.Getpid())
	if got == "" {
		t.Fatal("procImage found nothing with an empty PATH; a launcher that narrows " +
			"PATH would silently disable orphan reclaim again")
	}

	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if normalizeEngineImage(got) != normalizeEngineImage(want) {
		t.Errorf("procImage = %q, want %q", got, want)
	}
}

// TestLsofPathPrefersTheAbsoluteLocation pins the resolution order: the known
// macOS location when it is there, PATH otherwise so the other BSDs this file
// builds for still work.
func TestLsofPathPrefersTheAbsoluteLocation(t *testing.T) {
	got := lsofPath()
	if _, err := os.Stat(macOSLsof); err == nil {
		if got != macOSLsof {
			t.Errorf("lsofPath() = %q, want the absolute %q", got, macOSLsof)
		}
		return
	}
	if got != "lsof" {
		t.Errorf("lsofPath() = %q, want a PATH lookup where %q is absent", got, macOSLsof)
	}
}

// TestProcImageFailsClosedOnAMissingTool checks the direction that matters for
// safety: if lsof cannot be run at all, the image must come back empty so the
// ownership check declines rather than matching something.
func TestProcImageFailsClosedOnAMissingTool(t *testing.T) {
	if got := procImageVia("/nonexistent/lsof", os.Getpid()); got != "" {
		t.Errorf("procImageVia with a missing tool = %q, want empty", got)
	}
	if isOurEngineImage("", "/opt/nvpair/ollama") {
		t.Error("an unresolvable image matched; the check must fail closed")
	}
}
