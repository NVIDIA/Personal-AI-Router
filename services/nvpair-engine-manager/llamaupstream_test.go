// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/zip"
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
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type llamaRuntimeFixture struct {
	Version      string `json:"version"`
	Licenses     string `json:"licenses"`
	Devices      string `json:"devices"`
	DelayCommand string `json:"delay_command,omitempty"`
	DelayMS      int    `json:"delay_ms,omitempty"`
}

func cudaLlamaFixture(build int) llamaRuntimeFixture {
	return llamaRuntimeFixture{Version: fmt.Sprintf("version: 0.4.0-dev (build %d, commit fixture)", build), Licenses: "fixture third-party license", Devices: "Available devices:\n  CUDA0: Fixture NVIDIA device"}
}

// llamaFixtureZip builds an official-style flat bundle for the injected transport.
func llamaFixtureZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0700)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type llamaUpstreamFixture struct {
	e        *Executor
	st       *engineState
	requests map[string]int
	latest   string
	script   string
	fail     string
	onScript func()
}

// useScript serves script as the pinned installer. The fixture stands in for an
// installer these tests cannot run, so the expected digest has to follow the
// substituted bytes; every other caller of the real URL still gets the pin.
func (f *llamaUpstreamFixture) useScript(t *testing.T, script string) {
	t.Helper()
	before := llamaInstallerSHA256
	t.Cleanup(func() { llamaInstallerSHA256 = before })
	f.script = script
	sum := sha256.Sum256([]byte(script))
	llamaInstallerSHA256 = hex.EncodeToString(sum[:])
}

func newLlamaUpstreamFixture(t *testing.T, primary llamaRuntimeFixture) *llamaUpstreamFixture {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell fixture; other-platform pin tests are portable")
	}
	reg := NewRegistry()
	mf, _ := buildRegistry("").Get("llamacpp")
	mf.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = mf.Platforms["windows/arm64"]
	reg.engines["llamacpp"] = mf
	e := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	// Policy fixtures use commands and injected HTTP bodies only. Public Install
	// also reconciles presence; leaving the vendor readiness probe here would
	// cause a real loopback dial outside the injected HTTP transport.
	st.plat.Runtime.Ready, st.plat.Runtime.Health = nil, nil
	// This existing suite exercises the CUDA-required policy, not host inventory,
	// and the opt-in upstream-latest path rather than the default pinned archives.
	st.plat.Install.CPUFetch = nil
	st.plat.Install.UpstreamFirst = true
	fixtureSource := filepath.Join(t.TempDir(), "primary.json")
	data, _ := json.Marshal(primary)
	if err := os.WriteFile(fixtureSource, data, 0600); err != nil {
		t.Fatal(err)
	}
	// The manifest no longer declares runtime env (childEnv injects the owned cache paths).
	if st.plat.Runtime.Env == nil {
		st.plat.Runtime.Env = map[string]string{}
	}
	st.plat.Runtime.Env["FAKE_LLAMA_BIN_SOURCE"] = fakeEngineBin
	st.plat.Runtime.Env["FAKE_LLAMA_FIXTURE_SOURCE"] = fixtureSource
	f := &llamaUpstreamFixture{e: e, st: st, requests: map[string]int{}, latest: "b10900"}
	f.useScript(t, `$ErrorActionPreference = 'Stop'
if ($env:LLAMA_VERSION -ne 'b10900' -or $env:SKIP_INSTALL -ne '1' -or $env:SKIP_VULKAN -ne '1' -or -not [string]::IsNullOrEmpty($env:SKIP_CUDA)) { throw 'installer env mismatch' }
$stagePath = Join-Path $env:USERPROFILE 'llama-app'
New-Item -ItemType Directory -Path $stagePath -Force | Out-Null
Copy-Item -LiteralPath $env:FAKE_LLAMA_BIN_SOURCE -Destination (Join-Path $stagePath 'llama.exe')
Copy-Item -LiteralPath $env:FAKE_LLAMA_FIXTURE_SOURCE -Destination (Join-Path $stagePath '.llama-fixture.json')
Set-Content -LiteralPath (Join-Path $stagePath 'primary-only.txt') -Value 'primary'
`)
	bin, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatal(err)
	}
	fallbackJSON, _ := json.Marshal(cudaLlamaFixture(10826))
	bundles := [][]byte{llamaFixtureZip(t, map[string][]byte{"llama.exe": bin, ".llama-fixture.json": fallbackJSON, "fallback-only.txt": []byte("fallback")}), llamaFixtureZip(t, map[string][]byte{"cuda.dll": []byte("fixture CUDA companion")})}
	st.plat.Install.Archives = nil
	for i, bundle := range bundles {
		h := sha256.Sum256(bundle)
		st.plat.Install.Archives = append(st.plat.Install.Archives, Fetch{URL: fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/b10826/fixture-%d.zip", i), SHA256: hex.EncodeToString(h[:])})
	}
	e.client = &http.Client{Transport: llamaFixtureTransport(func(r *http.Request) (*http.Response, error) {
		url := r.URL.String()
		f.requests[url]++
		var body []byte
		switch url {
		case llamaLatestVersionURL:
			if f.fail == "version" {
				return nil, errors.New("fixture latest unavailable")
			}
			body = []byte(f.latest)
		case llamaLatestInstallerURL:
			if f.onScript != nil {
				f.onScript()
			}
			if f.fail == "download" {
				return nil, errors.New("fixture script unavailable")
			}
			if f.fail == "timeout" {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			body = []byte(f.script)
		default:
			if f.fail == "both" {
				return nil, errors.New("fixture fallback unavailable")
			}
			for i, fetch := range st.plat.Install.Archives {
				if fetch.URL == url {
					body = bundles[i]
				}
			}
			if body == nil {
				return nil, errors.New("unexpected fixture request")
			}
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	return f
}

func (f *llamaUpstreamFixture) seed(t *testing.T, spec llamaRuntimeFixture, receipt map[string]any) {
	t.Helper()
	dir := filepath.Join(f.st.installDir, "runtime")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, llamaExecutable()), bin, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, ".llama-fixture.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if receipt != nil {
		h := sha256.Sum256(bin)
		receipt["binary_sha256"] = hex.EncodeToString(h[:])
		if err := writeJSONAtomic(filepath.Join(dir, "pair-install.json"), receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.st.installDir, "models"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.st.installDir, "models", "retained.gguf"), []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *llamaUpstreamFixture) receipt(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.st.installDir, "runtime", "pair-install.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLlamaUpstreamInstallAndFallback(t *testing.T) {
	for _, failure := range []string{"", "download", "version", "wrong-version", "cpu", "licenses", "timeout", "child-timeout", "oversized-script", "oversized-version", "invalid-version", "old-version"} {
		t.Run(map[bool]string{true: "primary-success", false: failure}[failure == ""], func(t *testing.T) {
			primary := cudaLlamaFixture(10900)
			if failure == "wrong-version" {
				primary.Version = cudaLlamaFixture(10901).Version
			}
			if failure == "cpu" {
				primary.Devices = "CPU0: Fixture CPU\nCUDA support compiled but no devices"
			}
			if failure == "licenses" {
				primary.Licenses = ""
			}
			if failure == "child-timeout" {
				primary.DelayCommand, primary.DelayMS = "cli", 10000
			}
			f := newLlamaUpstreamFixture(t, primary)
			f.fail = failure
			if failure == "oversized-script" {
				f.useScript(t, strings.Repeat("#", maxLlamaInstallerBytes+1))
			}
			if failure == "oversized-version" {
				f.latest = "b10900" + strings.Repeat(" ", 64)
			}
			if failure == "invalid-version" {
				f.latest = "b10900;untrusted"
			}
			if failure == "old-version" {
				f.latest = "b10825"
			}
			if failure == "timeout" || failure == "child-timeout" {
				before := llamaUpstreamAttemptTimeout
				llamaUpstreamAttemptTimeout = 2 * time.Second
				t.Cleanup(func() { llamaUpstreamAttemptTimeout = before })
			}
			if err := f.e.installLlamaApp(context.Background(), f.st); err != nil {
				t.Fatal(err)
			}
			receipt := f.receipt(t)
			wantSource, wantBuild := "official-upstream", "b10900"
			if failure != "" {
				wantSource, wantBuild = "pinned-cuda-archives", "b10826"
			}
			if receipt["source"] != wantSource || receipt["selected_build"] != wantBuild || receipt["cuda_device_verified"] != true {
				t.Fatalf("incorrect outcome: %+v", receipt)
			}
			if failure == "" {
				if len(f.requests) != 2 || receipt["installer_sha256"] != llamaInstallerSHA256 {
					t.Fatal("primary fetched fallback or lacks the verified script hash")
				}
			} else {
				if receipt["fallback_reason"] == "" || receipt["upstream_attempt"] == nil {
					t.Fatal("fallback did not record attempted upstream provenance")
				}
				wantReason := map[string]string{"wrong-version": "selected build", "cpu": "CUDA device", "licenses": "licenses", "timeout": "deadline exceeded", "child-timeout": "deadline exceeded", "oversized-script": "1048576-byte limit", "oversized-version": "response is invalid", "invalid-version": "response is invalid", "old-version": "older than supported"}[failure]
				if wantReason != "" && !strings.Contains(receipt["fallback_reason"].(string), wantReason) {
					t.Fatalf("fallback was not caused by %s: %v", failure, receipt["fallback_reason"])
				}
				if _, err := os.Stat(filepath.Join(f.st.installDir, "runtime", "primary-only.txt")); !os.IsNotExist(err) {
					t.Fatal("primary bytes merged into fallback")
				}
			}
		})
	}
}

// retainModels seeds a model file beneath the install root without a runtime:
// an earlier uninstall retains models, so a later install, failure, or
// cancellation must leave them exactly as found.
func (f *llamaUpstreamFixture) retainModels(t *testing.T) string {
	t.Helper()
	model := filepath.Join(f.st.installDir, "models", "retained.gguf")
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	return model
}

// assertNothingPromoted checks the outcome every non-success install shares:
// neither runtime slot exists, the engine is not detected, and models survive.
func (f *llamaUpstreamFixture) assertNothingPromoted(t *testing.T, model string) {
	t.Helper()
	for _, slot := range []string{"runtime", "previous"} {
		if _, err := os.Stat(filepath.Join(f.st.installDir, slot)); !os.IsNotExist(err) {
			t.Fatalf("install left a %s slot", slot)
		}
	}
	if installed, err := f.e.Detect("llamacpp"); err != nil || installed {
		t.Fatalf("install reported as installed: %v %v", installed, err)
	}
	if data, err := os.ReadFile(model); err != nil || string(data) != "model" {
		t.Fatalf("install changed models: %v", err)
	}
}

func TestLlamaUpstreamInstallFailsClosedWhenBothSourcesFail(t *testing.T) {
	f := newLlamaUpstreamFixture(t, cudaLlamaFixture(10900))
	f.fail = "both"
	f.useScript(t, "throw 'fixture primary failed'")
	model := f.retainModels(t)
	err := f.e.installLlamaApp(context.Background(), f.st)
	if err == nil || !strings.Contains(err.Error(), "upstream attempt failed") || !strings.Contains(err.Error(), "official CUDA fallback failed") {
		t.Fatalf("both-fail outcome did not name both sources: %v", err)
	}
	if f.requests[llamaLatestInstallerURL] != 1 {
		t.Fatal("install skipped the upstream attempt")
	}
	f.assertNothingPromoted(t, model)
	if matches, _ := filepath.Glob(filepath.Join(f.st.installDir, ".llama-install-*")); len(matches) != 1 {
		t.Fatalf("failed stage was not retained for diagnosis: %v", matches)
	}
	found := false
	for _, se := range f.e.Errors() {
		if se.ID == installFailedID("llamacpp") && se.Operation == "install" {
			found = true
		}
	}
	if !found {
		t.Fatal("both-fail install did not report an install failure")
	}
}

// A parent cancellation at any point of a fresh install, including after the
// candidate is fully validated, must end the install cancelled without a
// fallback attempt and without promoting anything into the runtime slot.
func TestLlamaUpstreamParentCancelDoesNotPromote(t *testing.T) {
	for _, point := range []string{"download", "execution", "validation", "before-promotion"} {
		t.Run(point, func(t *testing.T) {
			primary := cudaLlamaFixture(10900)
			if point == "validation" {
				primary.DelayCommand, primary.DelayMS = "cli", 10000
			}
			f := newLlamaUpstreamFixture(t, primary)
			model := f.retainModels(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var blocked chan bool
			if point == "download" {
				f.onScript = cancel
			} else if point == "execution" || point == "validation" {
				if point == "execution" {
					f.useScript(t, f.script+"Set-Content -LiteralPath (Join-Path $stagePath '.llama-delay-started') -Value 'installer'\nStart-Sleep -Seconds 10\n")
				}
				blocked = make(chan bool, 1)
				go func() {
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					deadline := time.NewTimer(10 * time.Second)
					defer deadline.Stop()
					for {
						select {
						case <-ctx.Done():
							blocked <- false
							return
						case <-deadline.C:
							cancel()
							blocked <- false
							return
						case <-ticker.C:
							matches, _ := filepath.Glob(filepath.Join(f.st.installDir, ".llama-install-*", "upstream", "llama-app", ".llama-delay-started"))
							if len(matches) > 0 {
								cancel()
								blocked <- true
								return
							}
						}
					}
				}()
			} else {
				f.e.emit = func(method string, value any) {
					if method == "engine:install-progress" && value.(map[string]any)["stage"] == "installing" {
						// The second installing event occurs after the complete primary
						// candidate is validated, immediately before receipt/promotion.
						if matches, _ := filepath.Glob(filepath.Join(f.st.installDir, ".llama-install-*", "upstream", "llama-app", "THIRD-PARTY-LICENSES.txt")); len(matches) > 0 {
							cancel()
						}
					}
				}
			}
			err := f.e.installLlamaApp(ctx, f.st)
			cancel()
			if blocked != nil && !<-blocked {
				t.Fatal("did not reach blocked subprocess before parent cancellation")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("parent cancellation lost: %v", err)
			}
			if len(f.requests) != 2 {
				t.Fatal("cancelled parent attempted fallback")
			}
			f.assertNothingPromoted(t, model)
		})
	}
}

// Stop and shutdown cancel an in-flight public Install through the mutation
// cancel it registers. When that cancellation lands after the candidate is
// complete but before promotion, the install must still end cancelled with an
// empty runtime slot, Stop must not stay blocked behind it, and admission must
// reopen afterwards.
func TestLlamaInstallStoppedBeforePromotionDoesNotPromote(t *testing.T) {
	for _, reason := range []string{"operation", "stop", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			f := newLlamaUpstreamFixture(t, cudaLlamaFixture(10900))
			model := f.retainModels(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancelled := make(chan struct{})
			stopped := make(chan error, 1)
			var once sync.Once
			f.e.emit = func(method string, value any) {
				if method != "engine:install-progress" || value.(map[string]any)["stage"] != "installing" {
					return
				}
				// The second installing event occurs after the complete primary
				// candidate is validated, immediately before receipt/promotion.
				if matches, _ := filepath.Glob(filepath.Join(f.st.installDir, ".llama-install-*", "upstream", "llama-app", "THIRD-PARTY-LICENSES.txt")); len(matches) == 0 {
					return
				}
				once.Do(func() {
					if reason == "operation" {
						cancel()
						close(cancelled)
						return
					}
					// Install registered its cancel under st.mu. Observe the exact
					// moment Stop/StopAll invokes it, then let the install proceed
					// to its pre-promotion check with the cancellation in effect.
					f.st.mu.Lock()
					registered := f.st.mutationCancel
					f.st.mutationCancel = func() { registered(); close(cancelled) }
					f.st.mu.Unlock()
					go func() {
						if reason == "shutdown" {
							f.e.StopAll()
							stopped <- nil
							return
						}
						stopped <- f.e.Stop("llamacpp")
					}()
					<-cancelled
				})
			}
			err := f.e.Install(ctx, "llamacpp")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s: cancellation lost: %v", reason, err)
			}
			select {
			case <-cancelled:
			default:
				t.Fatal("install finished before reaching the pre-promotion point")
			}
			if reason != "operation" {
				select {
				case err := <-stopped:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Stop blocked behind the cancelled install")
				}
			}
			if len(f.requests) != 2 {
				t.Fatal("cancelled install attempted fallback")
			}
			f.assertNothingPromoted(t, model)
			f.st.mu.Lock()
			pending, registered := f.st.stopPending, f.st.mutationCancel != nil
			f.st.mu.Unlock()
			if pending != 0 || registered {
				t.Fatal("cancelled install left admission blocked or its cancel registered")
			}
		})
	}
}

// The upstream installer is the one artifact PAIR executes rather than merely
// unpacks, so a branch ref here would let upstream change what runs between
// review and a user's install. Guarded on every platform: the Windows fixtures
// above skip elsewhere, and they substitute this digest anyway.
func TestUpstreamInstallerIsPinnedAndVerified(t *testing.T) {
	if !regexp.MustCompile(`/llama-install\.sh/[0-9a-f]{40}/`).MatchString(llamaLatestInstallerURL) {
		t.Fatalf("installer URL is not pinned to a commit: %s", llamaLatestInstallerURL)
	}
	if !strings.Contains(llamaLatestInstallerURL, llamaInstallerCommit) {
		t.Fatalf("installer URL does not carry llamaInstallerCommit: %s", llamaLatestInstallerURL)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(llamaInstallerSHA256) {
		t.Fatalf("installer digest is not a SHA256: %q", llamaInstallerSHA256)
	}
}

func TestLlamaUpstreamRegistryAndOtherPlatformPins(t *testing.T) {
	mf, _ := buildRegistry("").Get("llamacpp")
	for key, p := range mf.Platforms {
		if key == "windows/arm64" {
			if p.Install.UpstreamFirst || p.Install.Fetch != nil || len(p.Install.Archives) != 2 || p.Install.CPUFetch == nil {
				t.Fatal("Windows ARM64 recipe must install the pinned CUDA archives directly, with upstream-latest opt-in only")
			}
		} else if key == "darwin/amd64" {
			if p.Install.UpstreamFirst || p.Install.Fetch != nil || p.Install.ArchiveRoot != "llama-b10826" || len(p.Install.Archives) != 1 ||
				p.Install.Archives[0].URL != "https://github.com/ggml-org/llama.cpp/releases/download/b10826/llama-b10826-bin-macos-x64.tar.gz" ||
				p.Install.Archives[0].SHA256 != "adcd2066b2a1a3d8e774e36c8f97d166defccd01d947d9e39667b0435e8361b0" {
				t.Fatal("Intel Mac recipe lost its exact pinned CPU archive/root")
			}
		} else if p.Install.UpstreamFirst || p.Install.Fetch == nil || len(p.Install.Fetch.SHA256) != 64 || !strings.Contains(p.Install.Fetch.URL, "27a82f3a6e0f259f88c2c31cd6b20d858a975f27") {
			t.Fatalf("%s lost its existing pin", key)
		}
	}
	for _, mutation := range []string{"other-platform", "other-driver", "no-archive", "unpinned-archive", "unpinned-fetch", "custom-script"} {
		t.Run(mutation, func(t *testing.T) {
			fresh, _ := buildRegistry("").Get("llamacpp")
			p := fresh.Platforms["windows/arm64"]
			key := "windows/arm64"
			switch mutation {
			case "other-platform":
				key = "linux/arm64"
			case "other-driver":
				p.Install.Driver = ""
			case "no-archive":
				p.Install.Archives = nil
			case "unpinned-archive":
				p.Install.Archives[0].SHA256 = ""
			case "unpinned-fetch":
				p.Install.Archives = nil
				p.Install.Fetch = &Fetch{URL: llamaLatestInstallerURL}
			case "custom-script":
				p.Install.Script = []string{"powershell", "untrusted"}
			}
			if err := p.validate(key); err == nil {
				t.Fatal("invalid latest recipe accepted")
			}
		})
	}
	for _, version := range []string{"b108260-fixture", "version: 0.4 (build 108260, commit fixture)"} {
		if llamaBuildNumber(version) != 108260 {
			t.Fatal("numeric build comparison truncated a prefix")
		}
	}
}
