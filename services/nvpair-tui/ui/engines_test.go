// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func runeKey(r string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)} }

// Install always asks before touching PATH; uninstall asks only when PAIR owns
// an entry to remove, so there is never a question with nothing behind it.
func TestLifecycleAsksBeforeChangingPath(t *testing.T) {
	view := func(e engineStatus) *enginesView {
		v := newEnginesView(nil)
		v.SetSize(80, 20)
		v.merge(e)
		return v
	}
	t.Run("install waits for an answer", func(t *testing.T) {
		v := view(engineStatus{Engine: "ollama", DisplayName: "Ollama"})
		if cmd := v.handleKey(runeKey("i")); cmd != nil {
			t.Fatal("install ran before the PATH question was answered")
		}
		if v.confirm == nil || !strings.Contains(v.confirm.question, "Ollama") || !v.CapturingInput() {
			t.Fatalf("confirm = %+v, want a PATH question that captures input", v.confirm)
		}
		if cmd := v.handleKey(runeKey("y")); cmd == nil {
			t.Fatal("answering yes did not run the install")
		}
		if v.confirm != nil {
			t.Fatal("the question is still showing after the answer")
		}
	})
	t.Run("esc cancels the install", func(t *testing.T) {
		v := view(engineStatus{Engine: "ollama"})
		v.handleKey(runeKey("i"))
		if cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
			t.Fatal("esc still ran the install")
		}
		if v.confirm != nil || !strings.Contains(v.status, "cancelled") {
			t.Fatalf("confirm = %+v, status = %q", v.confirm, v.status)
		}
	})
	t.Run("uninstall asks only when PAIR owns the entry", func(t *testing.T) {
		managed := view(engineStatus{Engine: "lmstudio", Installed: true, PathManaged: true})
		if cmd := managed.handleKey(runeKey("u")); cmd != nil || managed.confirm == nil {
			t.Fatal("uninstall of a PAIR-owned PATH entry did not ask first")
		}
		unmanaged := view(engineStatus{Engine: "lmstudio", Installed: true})
		if cmd := unmanaged.handleKey(runeKey("u")); cmd == nil || unmanaged.confirm != nil {
			t.Fatal("uninstall asked about a PATH entry PAIR never added")
		}
	})
}

func TestLifecycleParamsCarryThePathAnswer(t *testing.T) {
	for _, answer := range []bool{true, false} {
		p := lifecycleParams("ollama", answer)
		if p["engine"] != "ollama" || p["path"] != answer {
			t.Errorf("lifecycleParams(ollama, %v) = %v", answer, p)
		}
	}
	if _, ok := unaskedParams("ollama")["path"]; ok {
		t.Error("an op the user was not asked about sent a PATH answer")
	}
}

// TestPullParamsSendsBothKeys guards the LM Studio pull fix: the pull params
// must carry the model under BOTH "name" (Ollama's /api/pull body key) and
// "model" (LM Studio's `lms get {model}` CLI placeholder). Sending only "name"
// silently ran `lms get "" --yes`, so a TUI pull never reached LM Studio.
func TestPullParamsSendsBothKeys(t *testing.T) {
	p := pullParams("lmstudio", "owner/model")

	if p["engine"] != "lmstudio" {
		t.Fatalf("engine = %v, want lmstudio", p["engine"])
	}
	if p["action"] != "pull_model" {
		t.Fatalf("action = %v, want pull_model", p["action"])
	}
	inner, ok := p["params"].(map[string]string)
	if !ok {
		t.Fatalf("params = %T, want map[string]string", p["params"])
	}
	if inner["name"] != "owner/model" {
		t.Fatalf(`params["name"] = %q, want "owner/model"`, inner["name"])
	}
	if inner["model"] != "owner/model" {
		t.Fatalf(`params["model"] = %q, want "owner/model" (LM Studio reads this key)`, inner["model"])
	}
}
