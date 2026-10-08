// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			_, bgAdaptive := style.GetBackground().(lipgloss.AdaptiveColor)
			if !bgAdaptive {
				// Not a failure in itself: a fixed background with a fixed
				// foreground is a deliberate pair. There is just nothing here
				// for this test to check.
				t.Skipf("background is %T, not adaptive", style.GetBackground())
			}
			fg, fgAdaptive := style.GetForeground().(lipgloss.AdaptiveColor)
			require.True(t, fgAdaptive, "an adaptive background needs an adaptive foreground so both terminals have readable text")
			assert.NotEqual(t, fg.Light, fg.Dark, "foreground must suit both the light and dark backgrounds")
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
		assert.True(t, ok, "appearance %q", in)
		assert.Equal(t, want, got, "appearance %q", in)
	}

	for _, in := range []string{"lite", "black", "white", "true", "1", "no"} {
		_, ok := ParseAppearance(in)
		assert.False(t, ok, "appearance %q should be refused", in)
	}
}

// TestStartAppearanceSettlesBeforeTheFirstFrame checks the background is
// decided by the time the waiter returns.
//
// The whole point of resolving it off the startup path is that the answer is
// ready before anything renders. A waiter that returned early would put the
// query back where it was — resolved during the first frame, when Bubble Tea
// owns stdin and the terminal's reply goes to its reader instead.
func TestStartAppearanceSettlesBeforeTheFirstFrame(t *testing.T) {
	before := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(before) })

	wait := StartAppearance(AppearanceLight)
	wait()
	assert.False(t, lipgloss.HasDarkBackground(), "the waiter returned before the appearance was settled")

	// Waiting twice is not an error: the caller joins it on one path, and a
	// second join must not block forever on a closed channel.
	wait()
}

// TestSetAppearanceLeavesDetectionAloneOnAuto checks auto does not assert a
// background of its own.
func TestSetAppearanceLeavesDetectionAloneOnAuto(t *testing.T) {
	before := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(before) })

	SetAppearance(AppearanceAuto)
	assert.Equal(t, before, lipgloss.HasDarkBackground(), "auto must preserve the background assumption")

	SetAppearance(AppearanceLight)
	assert.False(t, lipgloss.HasDarkBackground(), "light did not take effect")
	SetAppearance(AppearanceDark)
	assert.True(t, lipgloss.HasDarkBackground(), "dark did not take effect")
}
