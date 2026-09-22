// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
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

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)

	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("0")).
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
