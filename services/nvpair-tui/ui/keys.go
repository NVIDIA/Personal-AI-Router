// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strconv"

	"github.com/charmbracelet/bubbles/key"
)

// globalKeyMap holds the bindings that work in every view. View-specific
// bindings are returned by each View's Help and handled inside its Update.
type globalKeyMap struct {
	NextTab key.Binding
	PrevTab key.Binding
	JumpTab key.Binding
	Help    key.Binding
	Quit    key.Binding
	Dismiss key.Binding
}

// While a text field owns the keyboard, these two are the only keys that do
// anything — so they are the only two a view should advertise.
//
// Every view previously kept listing its normal verbs mid-entry, which is the
// worst kind of help: it names six keys that all now type a character instead,
// and omits the two that work. The label is per-field because "save" and
// "apply filter" are not the same promise.
var (
	inputCancelKey = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel"))
)

// inputHelp is the footer for a view whose text field has the keyboard.
func inputHelp(submitLabel string) []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", submitLabel)),
		inputCancelKey,
	}
}

// newGlobalKeyMap builds the shell's bindings for a given number of tabs.
//
// The count is a parameter because the digit binding has to match the tab bar
// exactly: it advertised "1-9 go to tab" against five tabs, promising four
// shortcuts that did nothing.
func newGlobalKeyMap(tabs int) globalKeyMap {
	return globalKeyMap{
		// Deliberately not h/l or the arrows. Those are how you move *within*
		// content, and a view with a horizontal axis — switching between the
		// panes of a node's detail, say — cannot have them swallowed by the tab
		// bar. The digits below make tab switching direct anyway.
		NextTab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "prev"),
		),
		// The tab bar numbers every tab, so the digits have to select them —
		// otherwise the labels promise a shortcut that does nothing. No view
		// binds a digit, so these never shadow a view binding.
		JumpTab: key.NewBinding(
			key.WithKeys(tabDigits(tabs)...),
			key.WithHelp(tabDigitsHelp(tabs), "go to tab"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		// ctrl+x because every letter is already spoken for — the views between
		// them bind a through y, and the table and viewport add b, g, G, space,
		// and the ctrl+u / ctrl+d / ctrl+f / ctrl+b paging pairs. The shell
		// handles its own keys before the active view sees them, so a global on
		// any of those would silently shadow a verb.
		//
		// Not in the footer: it applies only while the banner is up, and the
		// banner names it. A permanent entry for a key that is usually inert
		// would cost a slot the footer truncates away from the right.
		Dismiss: key.NewBinding(
			key.WithKeys("ctrl+x"),
			key.WithHelp("ctrl+x", "dismiss notice"),
		),
	}
}

// tabDigits is the digit keys that select a tab, one per tab.
//
// Capped at nine because a tenth tab would need a two-key sequence, and the tab
// bar has no room for ten labels at eighty columns anyway.
func tabDigits(tabs int) []string {
	if tabs > 9 {
		tabs = 9
	}
	keys := make([]string, 0, tabs)
	for i := 1; i <= tabs; i++ {
		keys = append(keys, strconv.Itoa(i))
	}
	return keys
}

// tabDigitsHelp labels those keys: "1" alone, or "1-N".
func tabDigitsHelp(tabs int) string {
	if tabs > 9 {
		tabs = 9
	}
	if tabs <= 1 {
		return "1"
	}
	return "1-" + strconv.Itoa(tabs)
}
