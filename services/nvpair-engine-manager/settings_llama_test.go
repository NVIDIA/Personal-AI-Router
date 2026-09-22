// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
)

// The llama manifest resolves its cache environment from {model_dir}. The
// settings display path must supply that placeholder like doStart does, and
// the reviewed --host/--port controls make the launch editable, so a llama.cpp
// row shows its argument text instead of a display failure.
func TestLlamaLaunchSettingsResolveModelDirAndAreEditable(t *testing.T) {
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, t.TempDir())
	state, err := e.LaunchSettings("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state.Reason, "cannot be displayed") {
		t.Fatalf("settings display failed to resolve the llama launch: %q", state.Reason)
	}
	if !state.Editable {
		t.Fatalf("llama.cpp declares reviewed networking controls and must be editable: reason %q", state.Reason)
	}
	if state.ServerPort != 8081 {
		t.Fatalf("server port = %d, want the managed default 8081", state.ServerPort)
	}
	// Fixed startup arguments (serve, --no-models-autoload) stay out of the
	// editable text; the managed loopback bind and port are what the user sees.
	if state.LaunchText != "--host 127.0.0.1 --port 8081" {
		t.Fatalf("launch text = %q", state.LaunchText)
	}
	if strings.Contains(state.LaunchText, "no-models-autoload") || strings.Contains(state.LaunchText, "LLAMA_CACHE") {
		t.Fatalf("fixed arguments or owned cache environment leaked into editable text: %q", state.LaunchText)
	}
}

// The model cache location is owned by the engine manager and injected on every
// launch. Accepting it in the editor would save a value that never reaches the
// engine, so the preview refuses it instead of dropping it silently.
func TestLlamaLaunchSettingsRefuseOwnedCacheEnvironment(t *testing.T) {
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, t.TempDir())
	for _, text := range []string{`LLAMA_CACHE="C:/elsewhere" --host 127.0.0.1 --port 8081`, `hf_hub_cache=/elsewhere --port 8081`} {
		preview, err := e.PreviewLaunch(settings.Request{Engine: "llamacpp", Settings: settings.Config{ServerPort: 8081, ProxyPort: 8080, LaunchText: text}})
		if err != nil {
			t.Fatal(err)
		}
		if len(preview.Errors) == 0 {
			t.Fatalf("accepted owned cache environment %q: %+v", text, preview)
		}
		if len(preview.Env) != 0 {
			t.Fatalf("owned cache environment leaked into saved env for %q: %v", text, preview.Env)
		}
	}
	// An unrelated variable is still an ordinary editable assignment.
	preview, err := e.PreviewLaunch(settings.Request{Engine: "llamacpp", Settings: settings.Config{ServerPort: 8081, ProxyPort: 8080, LaunchText: `GGML_EXAMPLE=1 --host 127.0.0.1 --port 8081 --ctx-size 4096`}})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Errors) != 0 || len(preview.Env) != 1 || preview.Env[0] != "GGML_EXAMPLE=1" || len(preview.Args) != 2 || preview.Args[0] != "--ctx-size" {
		t.Fatalf("unexpected preview for an ordinary llama launch edit: %+v", preview)
	}
}
