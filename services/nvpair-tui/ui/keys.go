// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import "github.com/charmbracelet/bubbles/key"

// globalKeyMap holds the bindings that work in every view. View-specific
// bindings are returned by each View's Help and handled inside its Update.
type globalKeyMap struct {
	NextTab key.Binding
	PrevTab key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func newGlobalKeyMap() globalKeyMap {
	return globalKeyMap{
		NextTab: key.NewBinding(
			key.WithKeys("tab", "l", "right"),
			key.WithHelp("tab", "next"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("shift+tab", "h", "left"),
			key.WithHelp("shift+tab", "prev"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
	}
}
