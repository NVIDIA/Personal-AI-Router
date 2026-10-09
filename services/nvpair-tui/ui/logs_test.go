// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		assert.NotContains(t, logFollowKey.Keys(), reserved, "the viewport uses this key for scrolling")
	}

	// And it still toggles.
	v := logsWith(t, 50)
	before := v.follow
	logsKey(v, logFollowKey.Keys()[0])
	assert.NotEqual(t, before, v.follow, "the follow key did not toggle follow")
}

// TestLogsFilterRoundTrip covers opening the filter, applying it, and clearing
// it — the whole reason the tab is useful when something has gone wrong.
func TestLogsFilterRoundTrip(t *testing.T) {
	v := logsWith(t, 0)
	logLine(v, "engine started ok")
	logLine(v, "cluster pairing failed")
	logLine(v, "engine stopped")

	logsKey(v, "/")
	require.True(t, v.editing, "/ did not open the filter")
	for _, r := range "engine" {
		logsKey(v, string(r))
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, v.editing, "enter did not close the filter field")
	assert.Equal(t, "engine", v.filter)
	body := v.View()
	assert.Contains(t, body, "engine started", "filter must narrow the buffer")
	assert.NotContains(t, body, "pairing failed", "filter must narrow the buffer")

	logsKey(v, "c")
	assert.Empty(t, v.filter, "c did not clear the filter")
	assert.Contains(t, v.View(), "pairing failed", "clearing the filter must restore the hidden lines")
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

	assert.False(t, v.editing, "esc did not close the field")
	assert.Equal(t, "engine", v.filter, "esc must preserve the filter when abandoning text")
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
	assert.Contains(t, joined, "enter")
	assert.Contains(t, joined, "esc")
	assert.NotContains(t, joined, "/", "filter-mode help must omit keys that now type characters")
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
	require.True(t, ok, "save should produce logsSavedMsg")
	require.NoError(t, msg.err, "save failed")

	body, err := os.ReadFile(msg.path)
	require.NoError(t, err, "read back")
	assert.Contains(t, string(body), "engine started", "the filter should not narrow the saved file")
	assert.Contains(t, string(body), "something failed", "the filter should not narrow the saved file")
	assert.Equal(t, home, filepath.Dir(msg.path), "save should use the home directory")
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
	require.True(t, ok, "first save should produce logsSavedMsg")
	require.NoError(t, first.err, "first save")

	logLine(v, "second")
	second, ok := v.saveCmd()().(logsSavedMsg)
	require.True(t, ok, "second save should produce logsSavedMsg")
	require.NoError(t, second.err, "second save")

	require.NotEqual(t, first.path, second.path, "the second save must preserve the first file")
	body, err := os.ReadFile(first.path)
	require.NoError(t, err, "the first file is gone")
	assert.NotContains(t, string(body), "second", "the first file must not be overwritten")
}

var _ View = (*logsView)(nil)
