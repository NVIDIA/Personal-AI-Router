// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type windowsCUDAFixture struct {
	*llamaUpstreamFixture
	vendor      *Fetch
	failArchive bool
	onArchive   func()
}

// newWindowsCUDAFixture shapes the recipe like the bundled Windows x64 one: a
// pinned vendor installer plus a pinned CUDA app and runtime bundle.
func newWindowsCUDAFixture(t *testing.T, capabilities string, queryErr error, cuda llamaRuntimeFixture) *windowsCUDAFixture {
	t.Helper()
	upstream := newLlamaUpstreamFixture(t, llamaRuntimeFixture{Version: "version: 0.4.0-dev (build 10826, commit fixture)", Licenses: "vendor fixture license", Devices: "Available devices:\n  Vulkan0: Fixture GPU"})
	script := []byte(`$ErrorActionPreference='Stop'
if ($env:LLAMA_VERSION -ne 'b10826' -or $env:SKIP_INSTALL -ne '1' -or $env:SKIP_CUDA -or $env:SKIP_VULKAN) {throw 'vendor selection was restricted'}
$stagePath=Join-Path $env:USERPROFILE 'llama-app'
New-Item -ItemType Directory -Path $stagePath -Force | Out-Null
Copy-Item -LiteralPath $env:FAKE_LLAMA_BIN_SOURCE -Destination (Join-Path $stagePath 'llama.exe')
Copy-Item -LiteralPath $env:FAKE_LLAMA_FIXTURE_SOURCE -Destination (Join-Path $stagePath '.llama-fixture.json')
`)
	digest := sha256.Sum256(script)
	f := &windowsCUDAFixture{llamaUpstreamFixture: upstream, vendor: &Fetch{URL: "https://example.invalid/pinned-vendor.ps1", SHA256: hex.EncodeToString(digest[:])}}
	bin, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatal(err)
	}
	cudaJSON, _ := json.Marshal(cuda)
	bundles := [][]byte{llamaFixtureZip(t, map[string][]byte{"llama.exe": bin, ".llama-fixture.json": cudaJSON}), llamaFixtureZip(t, map[string][]byte{"cudart64_13.dll": []byte("fixture CUDA runtime")})}
	install := f.st.plat.Install
	install.UpstreamFirst, install.CPUFetch, install.Archives, install.Fetch, install.CUDAArchives = false, nil, nil, f.vendor, nil
	for i, bundle := range bundles {
		h := sha256.Sum256(bundle)
		install.CUDAArchives = append(install.CUDAArchives, Fetch{URL: fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/b10826/fixture-cuda-%d.zip", i), SHA256: hex.EncodeToString(h[:])})
	}
	f.e.nvidiaComputeQuery = func(context.Context, map[string]string) (string, error) { return capabilities, queryErr }
	f.e.client = &http.Client{Transport: llamaFixtureTransport(func(r *http.Request) (*http.Response, error) {
		url := r.URL.String()
		f.requests[url]++
		body := []byte(nil)
		if url == f.vendor.URL {
			body = script
		}
		for i, fetch := range install.CUDAArchives {
			if fetch.URL != url {
				continue
			}
			if f.onArchive != nil {
				f.onArchive()
			}
			if f.failArchive {
				return nil, errors.New("fixture CUDA archive unavailable")
			}
			body = bundles[i]
		}
		if body == nil {
			return nil, errors.New("unexpected fixture request")
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	return f
}

func (f *windowsCUDAFixture) archiveRequests() int {
	n := 0
	for _, fetch := range f.st.plat.Install.CUDAArchives {
		n += f.requests[fetch.URL]
	}
	return n
}

// Every NVIDIA GPU at 7.5 or newer installs the pinned CUDA build; anything that
// rules CUDA out installs the vendor build and records why, without mixing the
// two stages.
func TestLlamaWindowsCUDASelection(t *testing.T) {
	noDevice := cudaLlamaFixture(10826)
	noDevice.Devices = "Available devices:\n  CPU0: Fixture CPU"
	for _, tc := range []struct {
		name, capabilities, note string
		queryErr                 error
		cuda                     llamaRuntimeFixture
		failArchive              bool
		archiveRequests          int
	}{
		{name: "turing-or-newer", capabilities: "8.9\n12.0\n", cuda: cudaLlamaFixture(10826), archiveRequests: 2},
		{name: "no-nvidia", queryErr: errors.New("executable file not found"), cuda: cudaLlamaFixture(10826), note: "no NVIDIA GPU"},
		{name: "pre-turing", capabilities: "8.9\n6.1\n", cuda: cudaLlamaFixture(10826), note: "below the 7.5"},
		{name: "unreadable", capabilities: "[N/A]\n", cuda: cudaLlamaFixture(10826), note: "unreadable"},
		{name: "no-cuda-device", capabilities: "8.9\n", cuda: noDevice, note: "CUDA device", archiveRequests: 2},
		{name: "archive-unavailable", capabilities: "8.9\n", cuda: cudaLlamaFixture(10826), failArchive: true, note: "official CUDA archives", archiveRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsCUDAFixture(t, tc.capabilities, tc.queryErr, tc.cuda)
			f.failArchive = tc.failArchive
			if err := f.e.installLlamaApp(context.Background(), f.st); err != nil {
				t.Fatal(err)
			}
			receipt := f.receipt(t)
			if got := f.archiveRequests(); got != tc.archiveRequests {
				t.Fatalf("CUDA archive requests = %d, want %d", got, tc.archiveRequests)
			}
			_, runtimeErr := os.Stat(filepath.Join(f.st.installDir, "runtime", "cudart64_13.dll"))
			if tc.note == "" {
				capabilities, _ := receipt["compute_capabilities"].([]any)
				if receipt["source"] != "pinned-cuda-archives" || receipt["acceleration_policy"] != "cuda" || receipt["cuda_device_verified"] != true ||
					len(capabilities) != 2 || receipt["recipe_sha256"] != llamaArchiveRecipeHash(f.st.plat.Install.CUDAArchives) {
					t.Fatalf("wrong CUDA provenance: %#v", receipt)
				}
				if f.requests[f.vendor.URL] != 0 || runtimeErr != nil {
					t.Fatal("CUDA install ran the vendor installer or lost the CUDA runtime")
				}
				return
			}
			note, _ := receipt["cuda_not_used"].(string)
			if receipt["installer_sha256"] != f.vendor.SHA256 || !strings.Contains(note, tc.note) {
				t.Fatalf("vendor install lacks its provenance or CUDA reason: %#v", receipt)
			}
			if f.requests[f.vendor.URL] != 1 || !os.IsNotExist(runtimeErr) {
				t.Fatal("vendor install skipped its installer or merged CUDA bytes")
			}
		})
	}
}

// A parent cancellation during the CUDA download ends the install cancelled; it
// never authorizes the vendor installer.
func TestLlamaWindowsCUDACancelDoesNotFallBack(t *testing.T) {
	f := newWindowsCUDAFixture(t, "8.9\n", nil, cudaLlamaFixture(10826))
	model := f.retainModels(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onArchive = cancel
	if err := f.e.installLlamaApp(ctx, f.st); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if f.requests[f.vendor.URL] != 0 {
		t.Fatal("cancelled CUDA install ran the vendor installer")
	}
	f.assertNothingPromoted(t, model)
}

func TestLlamaWindowsCUDARecipe(t *testing.T) {
	mf, _ := buildRegistry("").Get("llamacpp")
	want := []Fetch{
		{URL: "https://github.com/ggml-org/llama.cpp/releases/download/b10826/llama-b10826-bin-win-cuda-13.3-x64.zip", SHA256: "34549bd7c46717333277d7bfc4c8617a12be0fad33a5253748e59e1085ffda4f"},
		{URL: "https://github.com/ggml-org/llama.cpp/releases/download/b10826/cudart-llama-bin-win-cuda-13.3-x64.zip", SHA256: "1462a050eb4c684921ba51dcc4cc488a036674c3e73e9945ee705b854808d03e"},
	}
	if p := mf.Platforms["windows/amd64"]; !reflect.DeepEqual(p.Install.CUDAArchives, want) || p.Install.Fetch == nil {
		t.Fatalf("Windows x64 lost its pinned CUDA build or vendor fallback: %+v", p.Install)
	}
	for key, p := range mf.Platforms {
		if key != "windows/amd64" && len(p.Install.CUDAArchives) > 0 {
			t.Fatalf("%s acquired the Windows x64 CUDA recipe", key)
		}
	}
	if _, reason := llamaInstallSupport("windows", "amd64"); !strings.Contains(reason, "CUDA") {
		t.Fatalf("Windows x64 support reason omits the CUDA policy: %q", reason)
	}
	for _, mutation := range []string{"other-platform", "no-fallback", "unpinned-fallback", "unpinned-archive", "upstream-first"} {
		t.Run(mutation, func(t *testing.T) {
			fresh, _ := buildRegistry("").Get("llamacpp")
			p, key := fresh.Platforms["windows/amd64"], "windows/amd64"
			switch mutation {
			case "other-platform":
				key = "linux/amd64"
			case "no-fallback":
				p.Install.Fetch = nil
			case "unpinned-fallback":
				p.Install.Fetch = &Fetch{URL: p.Install.Fetch.URL}
			case "unpinned-archive":
				p.Install.CUDAArchives[0].SHA256 = ""
			case "upstream-first":
				p.Install.UpstreamFirst = true
			}
			if err := p.validate(key); err == nil {
				t.Fatal("invalid Windows x64 CUDA recipe accepted")
			}
		})
	}
}
