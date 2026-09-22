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

// SetAppearance fixes the terminal background, by detecting it now or by being
// told.
//
// Detection asks the terminal for its background colour and reads the reply
// from stdin. lipgloss does that once, lazily, the first time an adaptive
// colour is resolved — and that first resolution happens while rendering,
// which is after Bubble Tea has put the terminal in raw mode and started its
// own reader on stdin. The terminal answers, Bubble Tea's reader takes the
// reply, and the query times out having learned nothing.
//
// So detection is forced here instead, before the program starts, while stdin
// is still ours to read. The result is cached behind lipgloss's sync.Once, so
// every later render uses what was measured rather than re-asking at a moment
// when asking cannot work.
//
// Light and dark skip the question. They are for the terminal that does not
// answer at all — over SSH, inside tmux, in CI — where lipgloss would fall back
// to assuming dark and get a light terminal wrong.
func SetAppearance(a Appearance) {
	switch a {
	case AppearanceLight:
		lipgloss.SetHasDarkBackground(false)
	case AppearanceDark:
		lipgloss.SetHasDarkBackground(true)
	default:
		// The return value is deliberately unused: the point is to run the
		// query now and let the sync.Once keep the answer.
		_ = lipgloss.HasDarkBackground()
	}
}

// StartAppearance settles the terminal background off the startup path,
// returning a function that waits for it.
//
// The query costs nothing on a terminal that answers, and five seconds on one
// that does not: termenv's timeout is a constant, so it cannot be shortened.
// Rather than spend that before anything else happens, it runs while the
// broker starts — work the program has to do regardless — and is joined just
// before the first render, which is the first moment the answer is needed.
//
// Terminals that cannot answer are recognised without waiting at all. termenv
// refuses the query outright under screen, tmux, and TERM=dumb, because those
// can be attached to several terminals at once and there is no single
// background to report. Those sessions fall back to assuming dark, which is
// what --appearance is for.
func StartAppearance(a Appearance) (wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		SetAppearance(a)
	}()
	return func() { <-done }
}

// DetectedAppearance reports the background in force, for a log line that
// explains a colour scheme the operator did not expect.
func DetectedAppearance() Appearance {
	if lipgloss.HasDarkBackground() {
		return AppearanceDark
	}
	return AppearanceLight
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
