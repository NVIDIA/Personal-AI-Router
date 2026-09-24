// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"nvpair-shared/engines"
)

func TestBuildProxyEnginesUsesSharedDefaults(t *testing.T) {
	got := buildProxyEngines()
	want := engines.ProxyDefaults()
	if len(got) != len(want) {
		t.Fatalf("proxy tabs = %d, want %d", len(got), len(want))
	}
	for i, engine := range want {
		if got[i].label != engine.DisplayName || got[i].prefix != engine.ComponentName() {
			t.Errorf("proxy tab %d = (%q, %q), want (%q, %q)",
				i, got[i].label, got[i].prefix, engine.DisplayName, engine.ComponentName())
		}
	}
}
