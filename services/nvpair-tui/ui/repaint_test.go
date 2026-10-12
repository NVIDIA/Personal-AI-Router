// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The shell sizes the active view inside View, so SetSize runs on every frame
// rather than only on a resize. That is what keeps the content budget and the
// frame in agreement, but it makes idempotence a requirement: anything SetSize
// touches is touched again on every tick and every keystroke.
//
// These pin the two pieces of state a repaint must not disturb. Both are easy
// to break — logsView.SetSize calls render, which calls the viewport's
// SetContent — and neither would show up in a frame-size test.
//
// Cost measured at the time of writing: about 0.5ms per frame with a full
// 5000-line log buffer, which is why the simplicity is worth the repeated work.
func TestScrollSurvivesEveryFrame(t *testing.T) {
	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = 120, 40
	m.resizeViews()
	m.selectTab(4)

	logs, ok := m.views[4].(*logsView)
	if !ok {
		t.Fatal("view 4 is not the logs tab")
	}
	for i := range 500 {
		logs.Update(LogLineMsg{Line: "line " + strings.Repeat("x", i%20)})
	}
	_ = m.View()

	// Scroll up, which also releases follow.
	for range 10 {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		m = updated.(Model)
	}
	afterScroll := logs.vp.YOffset
	if afterScroll == 0 {
		t.Fatal("scrolling up did not move the viewport")
	}
	if logs.follow {
		t.Error("scrolling up did not release follow")
	}

	// Repaint several times, as a tick would.
	for range 5 {
		_ = m.View()
	}
	if got := logs.vp.YOffset; got != afterScroll {
		t.Errorf("repainting moved the viewport from %d to %d; scrolling back is impossible",
			afterScroll, got)
	}

	// A new line arriving must not yank a scrolled-back operator to the bottom.
	logs.Update(LogLineMsg{Line: "a new line"})
	_ = m.View()
	if got := logs.vp.YOffset; got != afterScroll {
		t.Errorf("a new log line moved the viewport from %d to %d while follow was off",
			afterScroll, got)
	}
}

// The same question for the tables: the cursor must survive a repaint.
func TestTableCursorSurvivesEveryFrame(t *testing.T) {
	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = 120, 40
	m.resizeViews()

	nodes, ok := m.views[0].(*nodesView)
	if !ok {
		t.Fatal("first view is not the nodes tab")
	}
	discovered := make([]availableNode, 20)
	for i := range discovered {
		discovered[i] = availableNode{
			HostUUID:  string(rune('a' + i)),
			Name:      "node-" + string(rune('a'+i)),
			IPAddress: "10.0.0.1",
		}
	}
	nodes.feeds.discovered = discovered
	nodes.rebuild()
	_ = m.View()

	for range 5 {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = updated.(Model)
	}
	after := nodes.table.Cursor()
	if after == 0 {
		t.Fatal("moving down did not move the cursor")
	}

	for range 5 {
		_ = m.View()
	}
	if got := nodes.table.Cursor(); got != after {
		t.Errorf("repainting moved the cursor from %d to %d", after, got)
	}
}
