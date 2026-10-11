// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The Logs view had no behavioural coverage at all, which is how a keybinding
// collision with the viewport's own paging keys survived four review rounds in
// the one view whose purpose is scrolling.

// logsWith builds a sized view holding n lines.
func logsWith(t *testing.T, n int) *logsView {
	t.Helper()
	v := newLogsView(nil)
	v.SetSize(80, 20)
	for i := range n {
		logLine(v, "line "+string(rune('a'+i%26)))
	}
	return v
}

// logLine feeds one captured stderr line in, the way the shell does.
func logLine(v *logsView, s string) { v.Update(LogLineMsg{Line: s}) }

func logsKey(v *logsView, k string) {
	if k == "esc" {
		v.Update(tea.KeyMsg{Type: tea.KeyEsc})
		return
	}
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
}

// TestLogsFollowKeyDoesNotCollideWithPaging is the regression guard for the
// binding that made the tab unusable for its own purpose.
//
// The viewport binds f to page-down. Binding the follow toggle to the same key
// meant the standard paging key threw the operator to the bottom of the buffer
// on first press, in the view they had opened to scroll back through.
func TestLogsFollowKeyDoesNotCollideWithPaging(t *testing.T) {
	for _, reserved := range []string{"f", "b", "u", "d", "g", "G", " "} {
		for _, bound := range logFollowKey.Keys() {
			if bound == reserved {
				t.Errorf("follow is bound to %q, which the viewport uses for scrolling", bound)
			}
		}
	}

	// And it still toggles.
	v := logsWith(t, 50)
	before := v.follow
	logsKey(v, logFollowKey.Keys()[0])
	if v.follow == before {
		t.Error("the follow key did not toggle follow")
	}
}

// TestLogsFilterRoundTrip covers opening the filter, applying it, and clearing
// it — the whole reason the tab is useful when something has gone wrong.
func TestLogsFilterRoundTrip(t *testing.T) {
	v := logsWith(t, 0)
	logLine(v, "engine started ok")
	logLine(v, "cluster pairing failed")
	logLine(v, "engine stopped")

	logsKey(v, "/")
	if !v.editing {
		t.Fatal("/ did not open the filter")
	}
	for _, r := range "engine" {
		logsKey(v, string(r))
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if v.editing {
		t.Error("enter did not close the filter field")
	}
	if v.filter != "engine" {
		t.Errorf("filter = %q", v.filter)
	}
	body := v.View()
	if !contains(body, "engine started") || contains(body, "pairing failed") {
		t.Errorf("filter did not narrow the buffer:\n%s", body)
	}

	logsKey(v, "c")
	if v.filter != "" {
		t.Errorf("c did not clear the filter, got %q", v.filter)
	}
	if !contains(v.View(), "pairing failed") {
		t.Error("clearing the filter did not restore the hidden lines")
	}
}

// TestLogsFilterCanBeAbandoned checks esc leaves the previous filter alone
// rather than committing a half-typed one.
func TestLogsFilterCanBeAbandoned(t *testing.T) {
	v := logsWith(t, 5)
	v.filter = "engine"

	logsKey(v, "/")
	for _, r := range "zzz" {
		logsKey(v, string(r))
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if v.editing {
		t.Error("esc did not close the field")
	}
	if v.filter != "engine" {
		t.Errorf("esc committed the abandoned text: filter = %q", v.filter)
	}
}

// TestLogsHelpReflectsTheFieldState checks the footer names the keys that work
// while the filter has the keyboard, rather than the ones that now type.
func TestLogsHelpReflectsTheFieldState(t *testing.T) {
	v := logsWith(t, 5)
	logsKey(v, "/")

	keys := make([]string, 0, 2)
	for _, b := range v.Help() {
		keys = append(keys, b.Help().Key)
	}
	joined := strings.Join(keys, ",")
	if !contains(joined, "enter") || !contains(joined, "esc") {
		t.Errorf("filter-mode help = %v, want enter and esc", keys)
	}
	if contains(joined, "/") {
		t.Errorf("filter-mode help still advertises %v, which now type characters", keys)
	}
}

// TestLogsSaveWritesTheWholeBuffer checks the file contains every line, not
// just what a transient filter was showing — a saved log is evidence.
func TestLogsSaveWritesTheWholeBuffer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows

	v := logsWith(t, 0)
	logLine(v, "engine started")
	logLine(v, "something failed")
	v.filter = "engine" // showing one line

	msg, ok := v.saveCmd()().(logsSavedMsg)
	if !ok {
		t.Fatalf("save produced %T", v.saveCmd()())
	}
	if msg.err != nil {
		t.Fatalf("save failed: %v", msg.err)
	}

	body, err := os.ReadFile(msg.path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{"engine started", "something failed"} {
		if !contains(string(body), want) {
			t.Errorf("saved file is missing %q; the filter should not narrow it", want)
		}
	}
	if filepath.Dir(msg.path) != home {
		t.Errorf("saved to %q, want the home directory", msg.path)
	}
}

// TestLogsSaveNeverOverwrites checks a second save in the same second does not
// destroy the first, which is exactly the capture-twice case.
func TestLogsSaveNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	v := logsWith(t, 0)
	logLine(v, "first")
	first, ok := v.saveCmd()().(logsSavedMsg)
	if !ok || first.err != nil {
		t.Fatalf("first save: %+v", first)
	}

	logLine(v, "second")
	second, ok := v.saveCmd()().(logsSavedMsg)
	if !ok || second.err != nil {
		t.Fatalf("second save: %+v", second)
	}

	if first.path == second.path {
		t.Fatal("the second save reused the first file's name and destroyed it")
	}
	body, err := os.ReadFile(first.path)
	if err != nil {
		t.Fatalf("the first file is gone: %v", err)
	}
	if contains(string(body), "second") {
		t.Error("the first file was overwritten")
	}
}

var _ View = (*logsView)(nil)
