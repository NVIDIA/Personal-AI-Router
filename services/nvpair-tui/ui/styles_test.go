// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestTextOnAnAdaptiveBackgroundAdaptsToo is the regression guard for a light
// terminal rendering the selected row unreadable.
//
// A style whose background changes with the terminal but whose foreground does
// not is only legible in one of the two. Both of these paired a fixed black
// with the accent, which is a pale blue on a dark terminal — correct — and a
// dark blue on a light one, where black on dark blue is what the operator was
// left reading on every tab.
//
// Asserted structurally rather than by rendering, because the colour profile in
// a test process is unset and lipgloss then drops colour from the output
// entirely: the rendered strings for a right and a wrong pairing are identical.
func TestTextOnAnAdaptiveBackgroundAdaptsToo(t *testing.T) {
	cases := map[string]lipgloss.Style{
		"active tab":         tabActiveStyle,
		"selected table row": tableStyles().Selected,
	}
	for name, style := range cases {
		t.Run(name, func(t *testing.T) {
			bg, bgAdaptive := style.GetBackground().(lipgloss.AdaptiveColor)
			if !bgAdaptive {
				// Not a failure in itself: a fixed background with a fixed
				// foreground is a deliberate pair. There is just nothing here
				// for this test to check.
				t.Skipf("background is %T, not adaptive", style.GetBackground())
			}
			fg, fgAdaptive := style.GetForeground().(lipgloss.AdaptiveColor)
			if !fgAdaptive {
				t.Fatalf("background adapts (%+v) but the foreground is fixed (%v); "+
					"one of the two terminals gets unreadable text", bg, style.GetForeground())
			}
			if fg.Light == fg.Dark {
				t.Errorf("foreground is the same colour either way (%q), so it cannot "+
					"suit both the light and dark backgrounds", fg.Light)
			}
		})
	}
}

// TestAppearanceOverrideIsExplicit checks the flag accepts exactly the three
// words it documents, and that anything else is refused rather than quietly
// treated as auto.
//
// A typo that silently means "auto" is the worst outcome: the operator who
// reached for this flag is the one whose terminal was already detected wrongly,
// so falling back to detection leaves them exactly where they started with no
// indication why.
func TestAppearanceOverrideIsExplicit(t *testing.T) {
	good := map[string]Appearance{
		"auto":    AppearanceAuto,
		"":        AppearanceAuto,
		"light":   AppearanceLight,
		"dark":    AppearanceDark,
		"  Dark ": AppearanceDark,
		"LIGHT":   AppearanceLight,
	}
	for in, want := range good {
		got, ok := ParseAppearance(in)
		if !ok || got != want {
			t.Errorf("ParseAppearance(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}

	for _, in := range []string{"lite", "black", "white", "true", "1", "no"} {
		if _, ok := ParseAppearance(in); ok {
			t.Errorf("ParseAppearance(%q) was accepted; it should be refused", in)
		}
	}
}

// TestSetAppearanceLeavesDetectionAloneOnAuto checks auto does not assert a
// background of its own.
func TestSetAppearanceLeavesDetectionAloneOnAuto(t *testing.T) {
	before := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(before) })

	SetAppearance(AppearanceAuto)
	if got := lipgloss.HasDarkBackground(); got != before {
		t.Errorf("auto changed the background assumption from %v to %v", before, got)
	}

	SetAppearance(AppearanceLight)
	if lipgloss.HasDarkBackground() {
		t.Error("light did not take effect")
	}
	SetAppearance(AppearanceDark)
	if !lipgloss.HasDarkBackground() {
		t.Error("dark did not take effect")
	}
}
