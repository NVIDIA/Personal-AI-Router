// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/lipgloss"
)

// Styles are intentionally restrained: adaptive colors that degrade
// gracefully on a bare SSH terminal, no background fills that depend on
// 24-bit color. lipgloss downsamples to the detected color profile.
var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#1d4ed8", Dark: "#7dd3fc"}
	colorMuted  = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#9ca3af"}
	colorErr    = lipgloss.AdaptiveColor{Light: "#b91c1c", Dark: "#f87171"}
	colorOK     = lipgloss.AdaptiveColor{Light: "#15803d", Dark: "#86efac"}

	// colorOnAccent is text drawn on top of colorAccent, and it has to flip
	// with it rather than be a fixed colour.
	//
	// The accent is a dark blue on a light terminal and a pale blue on a dark
	// one, so one foreground cannot serve both: this was black, which is
	// correct on the pale blue and unreadable on the dark. It applied to the
	// selected table row — the row the operator is looking at, on every tab.
	//
	// The pairing is the point. A fixed foreground over an adaptive background
	// is only ever right for one of the two terminals, and which one it is
	// depends on a detection the program does not control.
	colorOnAccent = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#000000"}

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)

	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorOnAccent).
			Background(colorAccent).
			Padding(0, 1)

	tabInactiveStyle = lipgloss.NewStyle().
				Foreground(colorMuted).
				Padding(0, 1)

	footerStyle = lipgloss.NewStyle().Foreground(colorMuted)

	statusOKStyle  = lipgloss.NewStyle().Foreground(colorOK)
	statusErrStyle = lipgloss.NewStyle().Foreground(colorErr)

	// helpKeyStyle and helpDescStyle separate the key you press from what it
	// does. Undifferentiated, the footer reads as one run of words — "enter
	// details p pair n pair by address" — and the reader has to know the
	// convention to parse it. Bold marks the keys; the descriptions take the
	// same muted tone as the rest of the chrome.
	//
	// Bold rather than a color because this has to survive a bare SSH terminal:
	// lipgloss downsamples color, but bold is an SGR attribute that terminals
	// honour even at two colors, and it stays legible on any background.
	helpKeyStyle  = lipgloss.NewStyle().Bold(true)
	helpDescStyle = lipgloss.NewStyle().Foreground(colorMuted)
)

// Appearance is how the interface should colour itself, when the operator has
// to say so rather than let the terminal be asked.
type Appearance string

const (
	// AppearanceAuto asks the terminal for its background colour.
	AppearanceAuto Appearance = "auto"
	// AppearanceLight and AppearanceDark state it instead.
	AppearanceLight Appearance = "light"
	AppearanceDark  Appearance = "dark"
)

// SetAppearance overrides the detected terminal background.
//
// Detection works by asking the terminal for its background colour and waiting
// for the reply. A terminal that does not answer — which is common enough over
// SSH, inside tmux, and in CI — leaves lipgloss assuming a dark background, and
// every adaptive colour then picks the variant for the wrong one. On a light
// terminal that is not a cosmetic difference: the pale-blue accent chosen for a
// dark background is close to invisible on white.
//
// Nothing here guesses. Auto leaves the detection alone, and the other two say
// what the terminal is.
func SetAppearance(a Appearance) {
	switch a {
	case AppearanceLight:
		lipgloss.SetHasDarkBackground(false)
	case AppearanceDark:
		lipgloss.SetHasDarkBackground(true)
	}
}

// ParseAppearance narrows a flag value, reporting whether it is one of the
// three accepted words.
func ParseAppearance(v string) (Appearance, bool) {
	switch Appearance(strings.ToLower(strings.TrimSpace(v))) {
	case AppearanceAuto, "":
		return AppearanceAuto, true
	case AppearanceLight:
		return AppearanceLight, true
	case AppearanceDark:
		return AppearanceDark, true
	}
	return AppearanceAuto, false
}

// styleHelp applies the key/description split to a help model.
//
// Done once, on the shell's single help model, so every view's footer and the
// full-help overlay share it. bubbles keeps separate styles for the short and
// full renderings and defaults both to the same faint tone.
func styleHelp(m *help.Model) {
	m.Styles.ShortKey = helpKeyStyle
	m.Styles.FullKey = helpKeyStyle
	m.Styles.ShortDesc = helpDescStyle
	m.Styles.FullDesc = helpDescStyle
}
