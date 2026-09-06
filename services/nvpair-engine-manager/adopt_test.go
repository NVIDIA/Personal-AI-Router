// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestAdoptModeStartsWithoutSpawning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		// Identity probes send X-NVPAIR-Engine-Identity-Probe; still answer 200.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "m1", "status": map[string]string{"value": "unloaded"}}},
		})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := adoptManifest(port)
	ex := newTestExecutor(t, m)
	if err := ex.Start(context.Background(), "llamacpp"); err != nil {
		t.Fatalf("adopt start: %v", err)
	}
	st, err := ex.Status("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || !st.Healthy || st.Port != port {
		t.Fatalf("status = %+v", st)
	}
	state, _ := ex.state("llamacpp")
	state.mu.Lock()
	proc := state.proc
	state.mu.Unlock()
	if proc != nil {
		t.Fatal("adopt mode spawned a process")
	}
}

func TestAdoptModeDoesNotSpawnWhenDown(t *testing.T) {
	m := adoptManifest(1) // nothing listens on :1
	ex := newTestExecutor(t, m)
	err := ex.Start(context.Background(), "llamacpp")
	if err == nil {
		t.Fatal("expected start error when probe fails")
	}
	if !strings.Contains(err.Error(), "PAIR will not launch llama-server") {
		t.Fatalf("start error = %v, want adopt-only refusal", err)
	}
	st, _ := ex.Status("llamacpp")
	if st.Running {
		t.Fatal("must not mark running")
	}
	state, _ := ex.state("llamacpp")
	state.mu.Lock()
	proc := state.proc
	state.mu.Unlock()
	if proc != nil {
		t.Fatal("adopt mode spawned a process when the probe was down")
	}
}

func adoptManifest(port int) *Manifest {
	key := runtime.GOOS + "/" + runtime.GOARCH
	return &Manifest{
		Engine:          "llamacpp",
		DisplayName:     "llama.cpp",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Runtime: Runtime{
					Mode:  "adopt",
					Port:  port,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/v1/models", Status: 200},
				},
			},
		},
	}
}
