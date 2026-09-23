// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
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

// A pull's own request routinely outlives callTimeout, and that deadline is
// ignored on purpose because the download carries on. The terminal progress
// frame is then the only thing that can retire the entry — without it a
// finished download stays in active for the rest of the session and keeps
// being offered as the cancel target.
func TestTerminalPullProgressRetiresTheDownload(t *testing.T) {
	test := func(name, params string) {
		t.Run(name, func(t *testing.T) {
			v := newEnginesView(nil)
			v.active = []enginePull{
				{engine: "ollama", model: "llama3.2"},
				{engine: "ollama", model: "qwen3:8b"},
			}
			v.Update(NotificationMsg{Msg: &rpc.Message{
				Method: "engine:pull-progress",
				Params: json.RawMessage(params),
			}})
			pull, ok := v.newestPull("ollama")
			if !ok {
				t.Fatal("the other download was retired too")
			}
			if pull.model != "llama3.2" {
				t.Fatalf("newestPull = %q, want llama3.2 once qwen3:8b has finished", pull.model)
			}
		})
	}
	test("success", `{"engine":"ollama","model":"qwen3:8b","op":"pull","stage":"success"}`)
	test("error", `{"engine":"ollama","model":"qwen3:8b","op":"pull","stage":"error","percent":-1,"message":"no space left on device"}`)
}

// The engine-manager acknowledges a cancel only after the transfer has stopped
// and its partial files are cleaned up, so the reply can outlast callTimeout.
// Retiring the entry on that deadline dropped the only cancel target for a
// download that was still running: a second press reported no active download
// on the engine while the transfer carried on.
func TestCancelKeepsTheDownloadWhenTheCallTimesOut(t *testing.T) {
	v := newEnginesView(nil)
	pull := enginePull{engine: "ollama", model: "llama3.2"}
	v.active = []enginePull{pull}

	if msg := v.decodeCancel(pull)(nil, context.DeadlineExceeded); msg != nil {
		t.Fatalf("a timed-out cancel produced %#v, want no message", msg)
	}
	if _, ok := v.newestPull("ollama"); !ok {
		t.Fatal("the download is no longer cancelable after its cancel timed out")
	}
}

// A cancel that actually failed is a different matter: it reports, and leaves
// the entry so it can be tried again.
func TestCancelReportsARealFailure(t *testing.T) {
	v := newEnginesView(nil)
	pull := enginePull{engine: "ollama", model: "llama3.2"}
	v.active = []enginePull{pull}

	msg := v.decodeCancel(pull)(nil, errors.New("engine ollama is not running"))
	op, ok := msg.(engineOpMsg)
	if !ok {
		t.Fatalf("msg = %#v, want engineOpMsg", msg)
	}
	if !strings.Contains(op.what, pull.model) {
		t.Errorf("what = %q, want it to name %q", op.what, pull.model)
	}
	if _, ok := v.newestPull("ollama"); !ok {
		t.Error("a failed cancel left the download unselectable, so it cannot be retried")
	}
}

// A cancel the engine-manager confirmed is the one case that retires the entry
// on the reply itself.
func TestConfirmedCancelRetiresTheDownload(t *testing.T) {
	v := newEnginesView(nil)
	pull := enginePull{engine: "ollama", model: "llama3.2"}
	v.active = []enginePull{pull}

	msg := v.decodeCancel(pull)(nil, nil)
	if _, ok := msg.(enginePullDoneMsg); !ok {
		t.Fatalf("msg = %#v, want enginePullDoneMsg", msg)
	}
	v.Update(msg)
	if _, ok := v.newestPull("ollama"); ok {
		t.Error("a confirmed cancel left the download offered as a cancel target")
	}
}

// A non-terminal frame says the download is still going, so it must leave the
// entry alone.
func TestProgressFrameDoesNotRetireARunningDownload(t *testing.T) {
	v := newEnginesView(nil)
	running := enginePull{engine: "ollama", model: "llama3.2"}
	v.active = []enginePull{running}
	v.Update(NotificationMsg{Msg: &rpc.Message{
		Method: "engine:pull-progress",
		Params: json.RawMessage(`{"engine":"ollama","model":"llama3.2","op":"pull","stage":"downloading","percent":42}`),
	}})
	if _, ok := v.newestPull("ollama"); !ok {
		t.Fatal("a running download was retired on a progress frame")
	}
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
