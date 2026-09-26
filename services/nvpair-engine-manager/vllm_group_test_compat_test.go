// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// vllmInstallFixture is the selected-lifecycle equivalent of the sealed
// component fixture. Group tests need ownership/state seams, not the sealed
// 0.28 installer implementation.
type vllmInstallFixture struct {
	e        *Executor
	st       *engineState
	commands [][]string
	arch     string
}

type remoteActionTransport func(*http.Request) (*http.Response, error)

type actionBodyTransport func(*http.Request) (*http.Response, error)

func (f actionBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f remoteActionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func remoteActionResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func remoteActionPin(t *testing.T) (string, *clustertrust.Mesh, *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "fixture-cluster", "fixture-self", "fixture-peer")
	raw, err := os.ReadFile(filepath.Join(dir, "trusted", "fixture-peer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin struct {
		CertPEM string `json:"certPem"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(pin.CertPEM))
	if block == nil {
		t.Fatal("missing fixture certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return dir, clustertrust.Open(dir), cert
}

func vllmResourceFixture(t *testing.T) *vllmInstallFixture {
	t.Helper()
	base := t.TempDir()
	manifest := &Manifest{Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1}
	platform := &Platform{Runtime: Runtime{Mode: "process", Port: 8001}}
	manifest.Platforms = map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: *platform}
	reg := NewRegistry()
	reg.engines["vllm"] = manifest
	e := NewExecutor(reg, NewReporter(nil), func(string, any) {}, base)
	st := &engineState{
		manifest: manifest, plat: platform, logs: newLogBuffer(), port: 8001,
		installDir: filepath.Join(base, "vllm"), modelDir: filepath.Join(base, "models", "vllm"),
	}
	e.engines["vllm"] = st
	e.vllmNodeID = "node-a"
	return &vllmInstallFixture{e: e, st: st, arch: runtime.GOARCH}
}
