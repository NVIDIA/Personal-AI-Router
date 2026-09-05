// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"testing"
)

func TestBundledLlamaCppManifestAdoptsAndReportsModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"Ornith-1.5-9B","object":"model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	bundled := buildRegistry("")
	m, ok := bundled.Get("llamacpp")
	if !ok {
		t.Fatal("bundled registry does not contain llamacpp")
	}

	platformKey := runtime.GOOS + "/" + runtime.GOARCH
	p, ok := m.Platforms[platformKey]
	if !ok {
		t.Fatalf("llamacpp manifest has no platform block for %s", platformKey)
	}
	p.Runtime.Port = port
	m.Platforms[platformKey] = p

	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())

	status, err := ex.Status("llamacpp")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Installed || !status.Running || !status.Healthy {
		t.Fatalf("adopted status = %+v, want installed, running, healthy", status)
	}
	if status.Port != port {
		t.Fatalf("adopted port = %d, want %d", status.Port, port)
	}

	result := ex.ModelsResult(context.Background())
	want := []string{"Ornith-1.5-9B"}
	if !reflect.DeepEqual(result.Models, want) {
		t.Fatalf("models = %v, want %v", result.Models, want)
	}
	if !reflect.DeepEqual(result.ByEngine["llamacpp"], want) {
		t.Fatalf("modelsByEngine[llamacpp] = %v, want %v", result.ByEngine["llamacpp"], want)
	}
	if !reflect.DeepEqual(result.LoadedByEngine["llamacpp"], want) {
		t.Fatalf("loadedByEngine[llamacpp] = %v, want %v", result.LoadedByEngine["llamacpp"], want)
	}
}

func TestBundledLlamaCppManifestIsAdoptOnly(t *testing.T) {
	m, ok := buildRegistry("").Get("llamacpp")
	if !ok {
		t.Fatal("bundled registry does not contain llamacpp")
	}

	for key, p := range m.Platforms {
		if len(p.Detect) != 0 || p.Install != nil || p.Uninstall != nil {
			t.Fatalf("platform %s declares managed lifecycle fields", key)
		}
		if p.Runtime.Port != 8080 {
			t.Fatalf("platform %s port = %d, want 8080", key, p.Runtime.Port)
		}
		if p.Runtime.Ready == nil || p.Runtime.Health == nil {
			t.Fatalf("platform %s is missing readiness or health probe", key)
		}
	}
}
