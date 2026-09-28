// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const nonNvidiaARM = `{"cpus":[{"Architecture":12,"Manufacturer":"Qualcomm"}],"device_count":20,"nvidia_hardware":false}`

func TestLlamaARMHardwarePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		cuda, fail bool
	}{
		{"generic", nonNvidiaARM, false, false},
		{"unbound-nvidia-hardware", `{"cpus":[{"Architecture":12,"Manufacturer":"Qualcomm"}],"device_count":20,"nvidia_hardware":true}`, true, false},
		{"nvidia-cpu-driver-absent", `{"cpus":[{"Architecture":12,"Manufacturer":"NVIDIA"}],"device_count":20,"nvidia_hardware":false}`, true, false},
		{"missing", "{}", false, true}, {"invalid", "not-json", false, true},
		{"wrong-arch", `{"cpus":[{"Architecture":9,"Manufacturer":"Qualcomm"}],"device_count":20,"nvidia_hardware":false}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cuda, err := llamaARMRequiresCUDA([]byte(tc.data))
			if (err != nil) != tc.fail || cuda != tc.cuda {
				t.Fatalf("cuda=%v err=%v", cuda, err)
			}
		})
	}
}

func cpuARMFixture(t *testing.T) *llamaUpstreamFixture {
	f := newLlamaUpstreamFixture(t, llamaRuntimeFixture{Version: "version: 0.4.0-dev (build 10826, commit fixture)", Licenses: "CPU fixture license"})
	script := []byte(`$ErrorActionPreference='Stop'
if ($env:LLAMA_VERSION -ne 'b10826' -or $env:SKIP_CUDA -ne '1' -or $env:SKIP_VULKAN -ne '1' -or $env:SKIP_INSTALL -ne '1') {throw 'CPU policy mismatch'}
$stagePath=Join-Path $env:USERPROFILE 'llama-app'
New-Item -ItemType Directory -Path $stagePath -Force | Out-Null
Copy-Item -LiteralPath $env:FAKE_LLAMA_BIN_SOURCE -Destination (Join-Path $stagePath 'llama.exe')
Copy-Item -LiteralPath $env:FAKE_LLAMA_FIXTURE_SOURCE -Destination (Join-Path $stagePath '.llama-fixture.json')
`)
	digest := sha256.Sum256(script)
	fetch := &Fetch{URL: "https://example.invalid/pinned-cpu.ps1", SHA256: hex.EncodeToString(digest[:])}
	f.st.plat.Install.CPUFetch = fetch
	f.e.armHardwareQuery = func(context.Context, map[string]string) (string, error) { return nonNvidiaARM, nil }
	f.e.client = &http.Client{Transport: llamaFixtureTransport(func(r *http.Request) (*http.Response, error) {
		f.requests[r.URL.String()]++
		if r.URL.String() != fetch.URL {
			return nil, errors.New("CPU path attempted an upstream/CUDA request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(script)), ContentLength: int64(len(script))}, nil
	})}
	return f
}

// Native inventory that positively identifies a non-NVIDIA ARM host selects the
// pinned CPU installer and nothing else; the resulting runtime is detected, and
// its later removal retains models.
func TestLlamaARMCPUInstallAndRetention(t *testing.T) {
	f := cpuARMFixture(t)
	if err := f.e.installLlamaApp(context.Background(), f.st); err != nil {
		t.Fatal(err)
	}
	receipt := f.receipt(t)
	if receipt["acceleration_policy"] != "cpu" || receipt["cuda_device_verified"] != false || receipt["installer_sha256"] != f.st.plat.Install.CPUFetch.SHA256 {
		t.Fatalf("wrong CPU provenance: %#v", receipt)
	}
	if len(f.requests) != 1 || f.requests[f.st.plat.Install.CPUFetch.URL] != 1 {
		t.Fatalf("CPU install did not fetch exactly its pinned installer: %v", f.requests)
	}
	if installed, err := f.e.Detect("llamacpp"); err != nil || !installed {
		t.Fatalf("CPU install not detected: %v %v", installed, err)
	}
	models := filepath.Join(f.st.installDir, "models")
	if err := os.MkdirAll(models, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(models, "keep.gguf"), []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeLlamaRuntime(f.st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.st.installDir, "runtime")); !os.IsNotExist(err) {
		t.Fatal("uninstall left the runtime slot")
	}
	data, _ := os.ReadFile(filepath.Join(models, "keep.gguf"))
	if string(data) != "model" {
		t.Fatal("model lost")
	}
}

// Without a complete native inventory the ARM policy cannot tell CPU-only from
// NVIDIA-with-a-broken-driver, so install refuses before any download.
func TestLlamaARMCPURefusesUncertainPolicy(t *testing.T) {
	for _, name := range []string{"query-error", "incomplete", "cancel"} {
		t.Run(name, func(t *testing.T) {
			f := cpuARMFixture(t)
			ctx := context.Background()
			switch name {
			case "query-error":
				f.e.armHardwareQuery = func(context.Context, map[string]string) (string, error) {
					return "", errors.New("inventory unavailable")
				}
			case "incomplete":
				f.e.armHardwareQuery = func(context.Context, map[string]string) (string, error) { return "{}", nil }
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := f.e.installLlamaApp(ctx, f.st); err == nil {
				t.Fatal("uncertainty accepted")
			}
			if len(f.requests) != 0 {
				t.Fatal("uncertainty started download/fallback")
			}
			if _, err := os.Stat(filepath.Join(f.st.installDir, "runtime")); !os.IsNotExist(err) {
				t.Fatal("refused install promoted a runtime")
			}
		})
	}
}
