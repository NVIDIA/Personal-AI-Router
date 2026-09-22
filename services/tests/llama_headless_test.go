// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFreshLlamaManagerRefusesExternalListenerMutation(t *testing.T) {
	var reads, mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
		} else {
			mutations.Add(1)
		}
		w.Header().Set("Server", "llama.cpp")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()
	home := t.TempDir()
	base := home
	if runtime.GOOS == "darwin" {
		base = filepath.Join(home, "Library", "Application Support")
	}
	app := filepath.Join(base, "Nvidia Corporation", "Personal AI Router")
	name := "llama"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(app, "engine-bin", "llamacpp", "runtime", name)
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("managed image fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(app, "engines", "llamacpp.json")
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"engine": "llamacpp", "runtime": map[string]int{"port": portOfURL(t, server.URL)}})
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	stdin, msgs, stop := startEngineManagerStdio(t, home)
	t.Cleanup(stop)
	waitForMethod(t, msgs, "engine:ready", 10*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"engine:action","params":{"engine":"llamacpp","action":"load_model","params":{"model":"not-owned"}}}`)
	response := waitForResponse(t, msgs, 10*time.Second)
	if response.Error == nil || !strings.Contains(response.Error.Message, "external") {
		t.Fatalf("fresh manager failed to refuse the external owner: %+v", response)
	}
	if reads.Load() == 0 || mutations.Load() != 0 {
		t.Fatalf("identity reads=%d mutation requests=%d; want observed identity and no mutation", reads.Load(), mutations.Load())
	}
}
