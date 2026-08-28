// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import "github.com/charmbracelet/lipgloss"

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
)
