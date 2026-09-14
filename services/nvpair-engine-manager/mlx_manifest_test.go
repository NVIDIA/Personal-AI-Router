// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// The two schema additions the MLX manifest needs, and the bundled manifest
// that uses them. mlx-lm reports its single resident model as a scalar and has
// no load endpoint, so without these the manifest cannot express residency or a
// load at all.

func TestExtractScalarResult(t *testing.T) {
	spec := &ActionResult{Scalar: "model"}
	for _, tc := range []struct {
		name string
		body string
		want []string
		ok   bool
	}{
		{"loaded", `{"status":"ok","model":"mlx-community/Qwen3-VL-8B-Instruct-4bit"}`, []string{"mlx-community/Qwen3-VL-8B-Instruct-4bit"}, true},
		// null is mlx-lm's answer both before anything is loaded and while a
		// load is in flight: it sets its model key only once the weights are
		// in. Authoritative empty, so the node is not advertised as an owner.
		{"nothing loaded", `{"status":"ok","model":null}`, []string{}, true},
		{"empty string", `{"status":"ok","model":""}`, []string{}, true},
		// Unknown, not empty: a response we cannot read must never be reported
		// as an authoritative "serving nothing".
		{"field absent", `{"status":"ok"}`, nil, false},
		{"wrong type", `{"status":"ok","model":["a"]}`, nil, false},
		{"not an object", `["a"]`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractStringsResult(json.RawMessage(tc.body), spec)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestActionBodyTemplate(t *testing.T) {
	act := Action{HTTP: &ActionHTTP{
		Method: "POST",
		Path:   "/v1/chat/completions",
		Body:   json.RawMessage(`{"model":"{model}","max_tokens":1}`),
	}}

	r, err := actionBody(act, json.RawMessage(`{"model":"mlx-community/x"}`))
	if err != nil {
		t.Fatalf("actionBody: %v", err)
	}
	b, _ := io.ReadAll(r)
	if string(b) != `{"model":"mlx-community/x","max_tokens":1}` {
		t.Fatalf("body = %s", b)
	}

	// A model name carrying a quote must be escaped into the template, not
	// allowed to terminate the JSON string and rewrite the request.
	r, err = actionBody(act, json.RawMessage(`{"model":"a\",\"max_tokens\":9999,\"x\":\"b"}`))
	if err != nil {
		t.Fatalf("actionBody with quoted model: %v", err)
	}
	b, _ = io.ReadAll(r)
	var got struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("resolved body is not valid JSON: %v (%s)", err, b)
	}
	if got.MaxTokens != 1 {
		t.Fatalf("injected model name changed max_tokens to %d: %s", got.MaxTokens, b)
	}

	// No body declared: the caller's params are the body, as before.
	r, err = actionBody(Action{HTTP: &ActionHTTP{Method: "POST", Path: "/x"}}, json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("actionBody without template: %v", err)
	}
	b, _ = io.ReadAll(r)
	if string(b) != `{"a":1}` {
		t.Fatalf("params body = %s", b)
	}
}

// The bundled manifest has to survive the same validation every manifest does,
// and has to keep the residency/catalogue split the routing policy depends on.
func TestBundledMLXManifest(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("bundled manifests failed validation: %v", err)
	}
	m, ok := reg.Get("mlx")
	if !ok {
		t.Fatal("mlx manifest not bundled")
	}
	plat, ok := m.PlatformFor("darwin", "arm64")
	if !ok {
		t.Fatal("mlx must be installable on darwin/arm64")
	}
	if _, ok := m.PlatformFor("linux", "amd64"); ok {
		t.Error("mlx claims a non-Apple-Silicon platform; MLX cannot run there")
	}
	// loaded_models reads a LIST now: mlx-pool can hold several models at once
	// (MLX_MAX_MODELS), and reporting only the most recent would make PAIR route
	// as if the others were not resident.
	lm := m.Actions["loaded_models"].Result
	if lm.Array != "models" || lm.Field != "id" {
		t.Errorf("loaded_models must read models[].id, got array=%q field=%q", lm.Array, lm.Field)
	}
	if lm.Scalar != "" {
		t.Error("loaded_models must not use the single-model scalar form any more")
	}
	// The launcher is what makes detect-vs-run differ: detection proves the
	// virtualenv exists, but what PAIR starts is the pool.
	if plat.Runtime.Launcher == "" {
		t.Error("mlx must launch mlx-pool rather than the detected mlx_lm.server")
	}
	if plat.Runtime.Env["MLX_MAX_MODELS"] == "" {
		t.Error("MLX_MAX_MODELS must be set in runtime.env so a per-user override can deep-merge it")
	}
	if m.Actions["list_models"].Result.Scalar != "" {
		t.Error("list_models must stay the full downloaded catalogue, not the resident model")
	}
	if len(m.Actions["load_model"].HTTP.Body) == 0 {
		t.Error("load_model needs a templated body: mlx-lm has no load endpoint")
	}
}

// runtime.cli for MLX is itself templated ("{install_dir}/venv/bin/hf"), unlike
// LM Studio's fixed ~/.lmstudio/bin/lms. resolveArgs substitutes once, so an
// unresolved nested placeholder reaches exec as a literal path — which is
// exactly how pull_model and delete_model were silently broken.
func TestTemplatedCLIResolvesForCmdActions(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("bundled manifests: %v", err)
	}
	m, _ := reg.Get("mlx")
	plat, ok := m.PlatformFor("darwin", "arm64")
	if !ok {
		t.Fatal("no darwin/arm64 platform")
	}
	if !strings.Contains(plat.Runtime.CLI, "{install_dir}") {
		t.Skip("cli is no longer templated; this regression cannot occur")
	}
	resolved, err := resolvePlaceholders(plat.Runtime.CLI, map[string]string{
		"install_dir": "/tmp/enginedir", "port": "8081",
	})
	if err != nil {
		t.Fatalf("cli must resolve from runner-owned vars: %v", err)
	}
	if strings.Contains(resolved, "{") {
		t.Errorf("cli still holds an unresolved placeholder: %s", resolved)
	}
	if resolved != "/tmp/enginedir/venv/bin/hf" {
		t.Errorf("resolved cli = %s", resolved)
	}
}
