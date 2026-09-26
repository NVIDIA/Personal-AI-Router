// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func externalVLLMFixture(t *testing.T, owner string) (*Executor, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"0.10.0"}`))
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": "tiny", "owned_by": owner}},
		})
	}))
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect: []string{filepath.Join(t.TempDir(), "missing-vllm")},
			Runtime: Runtime{
				Bin: filepath.Join(t.TempDir(), "missing-vllm"), Port: port,
				Ready: &Probe{HTTP: "http://127.0.0.1:{port}/v1/models", Status: 200, Identity: "vllm"},
			},
		}},
		Actions: map[string]Action{
			"list_models": {HTTP: &ActionHTTP{Method: "GET", Path: "/v1/models"}},
			"pull_model":  {HTTP: &ActionHTTP{Method: "POST", Path: "/v1/models"}},
		},
	}
	return newTestExecutor(t, m), srv
}

func TestExternalVLLMRequiresExplicitRoutingIntentWithoutTakingAuthority(t *testing.T) {
	ex, srv := externalVLLMFixture(t, "vllm")
	defer srv.Close()

	st, err := ex.Status("vllm")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Adopted || st.Managed || st.Enabled || st.Routable || !st.Running || !st.Healthy {
		t.Fatalf("default external status = %+v", st)
	}
	if st.InstallSupported == nil || *st.InstallSupported {
		t.Fatalf("external vLLM unexpectedly installable: %+v", st)
	}

	if err := ex.Start(context.Background(), "vllm"); err != nil {
		t.Fatalf("enable external vLLM: %v", err)
	}
	st, _ = ex.Status("vllm")
	if !st.Enabled || !st.Routable || !st.Adopted || st.Managed {
		t.Fatalf("enabled external status = %+v", st)
	}

	for operation, run := range map[string]func() error{
		"install":   func() error { return ex.Install(context.Background(), "vllm") },
		"update":    func() error { return ex.Update(context.Background(), "vllm") },
		"uninstall": func() error { return ex.Uninstall(context.Background(), "vllm") },
		"restart":   func() error { return ex.Restart(context.Background(), "vllm") },
		"set-port": func() error {
			_, err := ex.SetPort(context.Background(), "vllm", st.Port+1)
			return err
		},
		"pull-model": func() error {
			_, err := ex.Action(context.Background(), "vllm", "pull_model", json.RawMessage(`{"model":"other"}`))
			return err
		},
	} {
		if err := run(); err == nil {
			t.Errorf("%s unexpectedly acquired authority over adopted vLLM", operation)
		}
	}

	if err := ex.Stop("vllm"); err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("stop error = %v, want external-owner refusal", err)
	}
	st, _ = ex.Status("vllm")
	if st.Enabled || st.Routable || !st.Running || !st.Adopted {
		t.Fatalf("disabled external status = %+v", st)
	}
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("external vLLM was disturbed: %v", err)
	}
	resp.Body.Close()
}

func TestVLLMIdentityRejectsGenericOpenAIListener(t *testing.T) {
	ex, srv := externalVLLMFixture(t, "openai")
	defer srv.Close()
	st, err := ex.Status("vllm")
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed || st.Running || st.Adopted || st.Routable {
		t.Fatalf("generic OpenAI listener was adopted as vLLM: %+v", st)
	}
}

func TestPAIRManagedVLLMLayoutIsReportedManaged(t *testing.T) {
	ex, srv := externalVLLMFixture(t, "vllm")
	defer srv.Close()
	st, err := ex.state("vllm")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(st.installDir, "bin", "vllm")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.installed, st.binPath, st.adopted = true, bin, false
	st.mu.Unlock()
	status := ex.snapshot("vllm", st)
	if !status.Managed || status.Adopted {
		t.Fatalf("PAIR layout status = %+v", status)
	}
}

func TestBundledVLLMManifestUsesOwnedDriverAndIsLinuxScoped(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("bundled vLLM manifest missing")
	}
	for key, platform := range m.Platforms {
		if key != "linux/amd64" && key != "linux/arm64" {
			t.Errorf("unexpected managed vLLM platform %q", key)
		}
		if platform.Install == nil || platform.Install.Driver != "vllm-python" || platform.Install.Fetch == nil {
			t.Errorf("%s lacks the owned pinned installer: %+v", key, platform.Install)
			continue
		}
		if len(platform.Install.Run) != 0 || len(platform.Install.Script) != 0 {
			t.Errorf("%s falls back to generic installer commands", key)
		}
		if !strings.HasPrefix(platform.Install.Fetch.URL, "https://github.com/astral-sh/uv/releases/download/0.12.17/") || !validSHA256(platform.Install.Fetch.SHA256) {
			t.Errorf("%s uv bootstrap is not release-and-digest pinned: %+v", key, platform.Install.Fetch)
		}
		for _, required := range []string{"getconf", "nvidia-smi"} {
			found := false
			for _, command := range platform.Install.Requires {
				found = found || command == required
			}
			if !found {
				t.Errorf("%s installer does not require %s", key, required)
			}
		}
		if platform.Uninstall == nil || platform.Uninstall.Driver != "vllm-python" || len(platform.Uninstall.Run) != 0 {
			t.Errorf("%s uninstall is not receipt-scoped: %+v", key, platform.Uninstall)
		}
		if platform.Runtime.Driver != "vllm-python" || !platform.Runtime.IsolatedEnv {
			t.Errorf("%s runtime is not isolated and receipt-owned: %+v", key, platform.Runtime)
		}
		if !strings.Contains(strings.Join(platform.Runtime.Args, " "), "{models_dir}/selected") {
			t.Errorf("%s runtime does not require the explicit retained selection", key)
		}
		if platform.Runtime.Env["VLLM_USE_FLASHINFER_SAMPLER"] != "0" || platform.Runtime.Env["VLLM_ALLREDUCE_USE_FLASHINFER"] != "0" {
			t.Errorf("%s runtime does not disable FlashInfer JIT paths under the spaced owned runtime", key)
		}
	}
	allowedActions := map[string]bool{"list_models": true, "loaded_models": true, "list_downloaded": true, "pull_model": true, "cancel_pull": true}
	for name := range m.Actions {
		if !allowedActions[name] {
			t.Errorf("unexpected mutating vLLM action %q", name)
		}
	}
	for _, name := range []string{"list_downloaded", "pull_model", "cancel_pull"} {
		if action := m.Actions[name]; action.Builtin != "vllm" {
			t.Errorf("%s is not owned by the managed vLLM builtin: %+v", name, action)
		}
	}
}
