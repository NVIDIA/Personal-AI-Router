// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import "testing"

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
