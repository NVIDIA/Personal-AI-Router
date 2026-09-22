// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nvpair-tui/rpc"
)

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

// The engine-manager queues concurrent pulls per engine and attributes every
// engine:pull-progress frame by model, so the status line must name the model
// rather than credit one download's percent to another.
func TestPullProgressRendersModelAndQueuedStage(t *testing.T) {
	test := func(name, params, want string) {
		t.Run(name, func(t *testing.T) {
			v := newEnginesView(nil)
			v.Update(NotificationMsg{Msg: &rpc.Message{
				Method: "engine:pull-progress",
				Params: json.RawMessage(params),
			}})
			if v.status != want {
				t.Fatalf("status = %q, want %q", v.status, want)
			}
		})
	}
	test("downloading",
		`{"engine":"ollama","model":"llama3.2","op":"pull","stage":"downloading","percent":42}`,
		"pull ollama llama3.2: downloading (42%)")
	test("queued behind another download",
		`{"engine":"ollama","model":"qwen3:8b","op":"pull","stage":"queued"}`,
		"pull ollama qwen3:8b: queued")
	test("success",
		`{"engine":"lmstudio","model":"owner/model","op":"pull","stage":"success"}`,
		"pull lmstudio owner/model: done")
}

// The cancel key needs the model, not just the engine: several downloads can be
// registered against one engine at a time.
func TestCancelTargetsNewestPullOnEngine(t *testing.T) {
	v := newEnginesView(nil)
	v.active = []enginePull{
		{engine: "ollama", model: "llama3.2"},
		{engine: "lmstudio", model: "owner/model"},
		{engine: "ollama", model: "qwen3:8b"},
	}
	pull, ok := v.newestPull("ollama")
	if !ok || pull.model != "qwen3:8b" {
		t.Fatalf("newestPull = %+v (ok=%v), want qwen3:8b", pull, ok)
	}
	v.retire(pull)
	if pull, ok = v.newestPull("ollama"); !ok || pull.model != "llama3.2" {
		t.Fatalf("after retiring, newestPull = %+v (ok=%v), want llama3.2", pull, ok)
	}
	v.retire(pull)
	if _, ok = v.newestPull("ollama"); ok {
		t.Fatal("ollama still reports a download after both were retired")
	}
	if pull, ok = v.newestPull("lmstudio"); !ok || pull.model != "owner/model" {
		t.Fatalf("another engine's download was retired: %+v (ok=%v)", pull, ok)
	}
}

// A pull whose request failed is over. Leaving it tracked kept it selectable,
// so the next cancel key would target a download that was never running.
func TestFailedPullIsRetiredAndNotCancelable(t *testing.T) {
	v := newEnginesView(nil)
	failed := enginePull{engine: "ollama", model: "llama3.2"}
	v.active = []enginePull{failed}

	v.Update(enginePullDoneMsg{pull: failed, err: errors.New("engine ollama is not running")})

	if _, ok := v.newestPull("ollama"); ok {
		t.Fatal("a failed download is still offered as the cancel target")
	}
	if !strings.Contains(v.status, "engine ollama is not running") {
		t.Fatalf("status = %q, want the failure reason", v.status)
	}
}
