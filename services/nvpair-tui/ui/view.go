// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// View is one tab in the TUI. Each view owns a slice of the broker's
// surface area (errors, discovery, proxies, ...). Views are long-lived
// and pointer-based: Update mutates the receiver in place and returns any
// follow-up command, so background tabs keep their state current from the
// broker's notification stream even while another tab is on screen.
type View interface {
	// Title is the short label shown in the tab bar.
	Title() string
	// Init returns a command to run when the program starts (e.g. issuing
	// the view's initial RPC calls and subscriptions).
	Init() tea.Cmd
	// SetSize tells the view the content area it may render into (the
	// region between the tab bar and the footer).
	SetSize(width, height int)
	// Update handles a message. Notification and tick messages are
	// delivered to every view; key messages only to the active view.
	Update(msg tea.Msg) tea.Cmd
	// View renders the content area.
	View() string
	// Help returns the contextual key bindings shown in the footer while
	// this view is active.
	Help() []key.Binding
}

// inputCapturer is an optional interface a View implements when it can
// enter a text-entry mode. While CapturingInput reports true, the root
// model routes every key to the view (bypassing global bindings) so the
// field receives characters like 'q' or tab verbatim.
type inputCapturer interface {
	CapturingInput() bool
}
