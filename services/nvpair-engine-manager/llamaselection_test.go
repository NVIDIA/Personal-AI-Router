// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The receipt classifies whatever `llama cli --list-devices` printed, so a
// vendor build that landed on Vulkan or CPU is visible instead of silent.
// TestBoundedCaptureKeepsHeadAndTail: a diagnostic written after a long
// progress stream must survive for llamaDownloadError to classify.
func TestBoundedCaptureKeepsHeadAndTail(t *testing.T) {
	b := newBoundedCapture(8)
	b.Write([]byte("abcdef"))
	if got := string(b.Bytes()); got != "abcdef" {
		t.Fatalf("short capture = %q", got)
	}
	b.Write([]byte("ghijkl")) // 12 bytes: head and tail overlap
	if got := string(b.Bytes()); got != "abcdefghijkl" {
		t.Fatalf("overlapping capture = %q", got)
	}
	for range 100 {
		b.Write([]byte("0123456789"))
	}
	b.Write([]byte("No space left on device"))
	got := string(b.Bytes())
	if !strings.HasPrefix(got, "abcdefgh") || !strings.HasSuffix(got, "n device") || !strings.Contains(got, "\n[...]\n") || len(got) != 8+8+len("\n[...]\n") {
		t.Fatalf("long capture = %q", got)
	}
}

func TestLlamaAccelerationClassification(t *testing.T) {
	for _, tc := range []struct {
		name, listing, policy string
		devices               int
	}{
		{"cuda", "Available devices:\n  CUDA0: NVIDIA GB10 (122564 MiB, 512 MiB free)\n", "cuda", 1},
		{"cuda-and-vulkan", "Available devices:\n  CUDA0: NVIDIA RTX 5070 (12226 MiB, 11017 MiB free)\n  Vulkan1: AMD Radeon RX 7900 XTX (24560 MiB, 24000 MiB free)\n", "cuda", 2},
		{"vulkan-multi", "Available devices:\r\n  Vulkan0: Intel(R) Arc(TM) A770 Graphics (16256 MiB, 15487 MiB free)\r\n  Vulkan1: Intel(R) Arc(TM) B580 Graphics (12116 MiB, 11347 MiB free)\r\n", "vulkan", 2},
		{"vulkan-tegra", "Available devices:\n  Vulkan0: NVIDIA Tegra NVIDIA Thor (94329 MiB, 94328 MiB free)\n", "vulkan", 1},
		{"metal", "Available devices:\n  MTL0: Apple M1 (5461 MiB, 5460 MiB free)\n  BLAS: Accelerate (0 MiB, 0 MiB free)\n", "metal", 2},
		{"metal-long-name", "Available devices:\n  Metal0: Apple M4 Max (49152 MiB, 49000 MiB free)\n", "metal", 1},
		{"rocm", "Available devices:\n  ROCm0: AMD Radeon RX 7900 XTX (24560 MiB, 24000 MiB free)\n", "rocm", 1},
		{"accelerate-cpu", "Available devices:\n  BLAS: Accelerate (0 MiB, 0 MiB free)\n", "cpu", 1},
		{"none", "Available devices:\n  (none)\n", "cpu", 0},
		{"empty", "", "cpu", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, devices := llamaAcceleration(tc.listing)
			if policy != tc.policy || len(devices) != tc.devices {
				t.Fatalf("llamaAcceleration = %q %v, want %q with %d rows", policy, devices, tc.policy, tc.devices)
			}
			for _, row := range devices {
				if strings.Contains(row, "\r") || strings.HasPrefix(row, " ") {
					t.Fatalf("device row not trimmed: %q", row)
				}
			}
		})
	}
	dir := t.TempDir()
	if policy, devices := llamaReceiptAcceleration(dir); policy != "" || devices != nil {
		t.Fatal("missing receipt must report nothing")
	}
	if err := os.MkdirAll(filepath.Join(dir, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(map[string]any{"acceleration_policy": "vulkan", "devices": []string{"Vulkan0: Fixture GPU"}})
	if err := os.WriteFile(filepath.Join(dir, "runtime", "pair-install.json"), receipt, 0o600); err != nil {
		t.Fatal(err)
	}
	if policy, devices := llamaReceiptAcceleration(dir); policy != "vulkan" || !reflect.DeepEqual(devices, []string{"Vulkan0: Fixture GPU"}) {
		t.Fatalf("receipt facts = %q %v", policy, devices)
	}
}

// A transfer that keeps moving is never cut off; one that stops is cancelled
// with a reason the operator can act on.
func TestStallContextCancelsOnlyWithoutProgress(t *testing.T) {
	restore := stallPollEvery
	stallPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { stallPollEvery = restore })

	live := newStallContext(context.Background(), 80*time.Millisecond, time.Minute)
	defer live.Stop()
	stopTouching := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopTouching:
				return
			case <-time.After(20 * time.Millisecond):
				live.Touch()
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if live.Err() != nil {
		t.Fatalf("progressing transfer was cancelled: %v", context.Cause(live))
	}
	close(stopTouching)
	select {
	case <-live.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stalled transfer was not cancelled")
	}
	if cause := context.Cause(live); cause == nil || !strings.Contains(cause.Error(), "no download progress") {
		t.Fatalf("cause = %v", cause)
	}
	err := transferError(live, errors.New("signal: killed"))
	if !strings.Contains(err.Error(), "no download progress") || !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("transferError = %v", err)
	}
	if !hasStall(live) || hasStall(context.Background()) {
		t.Fatal("hasStall misreports")
	}

	capped := newStallContext(context.Background(), time.Minute, 30*time.Millisecond)
	defer capped.Stop()
	select {
	case <-capped.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("absolute cap did not fire")
	}
	if cause := context.Cause(capped); cause == nil || !strings.Contains(cause.Error(), "did not finish") {
		t.Fatalf("cap cause = %v", cause)
	}
	plain := context.Background()
	if transferError(plain, errors.New("boom")).Error() != "boom" {
		t.Fatal("live context must return the error unchanged")
	}
}

// The vendor installer downloads silently; growth of its staging tree is the
// progress signal and drives the UI heartbeat.
func TestWatchTreeGrowthReportsProgress(t *testing.T) {
	root := t.TempDir()
	stall := newStallContext(context.Background(), time.Minute, time.Minute)
	defer stall.Stop()
	stop := make(chan struct{})
	defer close(stop)
	grew := make(chan int64, 8)
	go watchTreeGrowth(stall, root, 10*time.Millisecond, func(b int64) { grew <- b }, stop)
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case b := <-grew:
			if b == 5 {
				return
			}
		case <-deadline:
			t.Fatal("growth was not reported")
		}
	}
}

type linuxCUDAFixture struct {
	*windowsCUDAFixture
	installerRuns int
	installerEnv  []map[string]string
	failInstalls  int
}

// newLinuxCUDAFixture shapes the recipe like the bundled Linux one: a pinned
// vendor installer and nothing else. The Linux gate runs that installer through
// the injected hook, which stands in for `sh install.sh` on this host.
func newLinuxCUDAFixture(t *testing.T, capabilities string, queryErr error, devices string) *linuxCUDAFixture {
	t.Helper()
	restore := llamaLinuxCUDAGate
	llamaLinuxCUDAGate = true
	t.Cleanup(func() { llamaLinuxCUDAGate = restore })
	cuda := cudaLlamaFixture(10826)
	cuda.Devices = devices
	f := &linuxCUDAFixture{windowsCUDAFixture: newWindowsCUDAFixture(t, capabilities, queryErr, cuda)}
	f.st.plat.Install.CUDAArchives = nil // the Linux recipe has no pinned archive set
	bin, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatal(err)
	}
	cudaJSON, _ := json.Marshal(cuda)
	vulkan := cudaLlamaFixture(10826)
	vulkan.Devices = "Available devices:\n  Vulkan0: Fixture GPU"
	vulkanJSON, _ := json.Marshal(vulkan)
	// The same hook stands in for both installer runs: restricted (SKIP_VULKAN)
	// produces the CUDA build or fails like a missing payload; unrestricted lands
	// on Vulkan, as the vendor script does on this hardware without CUDA.
	f.e.runLlamaInstaller = func(ctx context.Context, script string, env map[string]string) error {
		fixture := vulkanJSON
		if env["SKIP_VULKAN"] == "1" {
			f.installerRuns++
			f.installerEnv = append(f.installerEnv, env)
			if f.failInstalls > 0 {
				f.failInstalls--
				return errors.New("fixture: CUDA payload download failed")
			}
			fixture = cudaJSON
		}
		// The restricted attempt always stages under the Linux installer's
		// ".llama-app"; the unrestricted run lands wherever this host's installer
		// path expects (the PowerShell installer's "llama-app" on Windows).
		home := filepath.Join(env["HOME"], ".llama-app")
		if env["SKIP_VULKAN"] != "1" && runtime.GOOS == "windows" {
			home = filepath.Join(env["HOME"], "llama-app")
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(home, llamaExecutable()), bin, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(home, ".llama-fixture.json"), fixture, 0o600)
	}
	return f
}

// Linux NVIDIA hosts install the CUDA build through the pinned installer,
// restricted so it cannot quietly land on Vulkan; when CUDA cannot be had the
// unrestricted installer runs and the receipt says why and what it produced.
func TestLlamaLinuxCUDAGate(t *testing.T) {
	cudaDevices := "Available devices:\n  CUDA0: NVIDIA GB10 (122564 MiB, 512 MiB free)"
	for _, tc := range []struct {
		name, capabilities, wantSource, wantPolicy, wantNote string
		queryErr                                             error
		failInstalls, wantRuns                               int
	}{
		{name: "gb10-cuda", capabilities: "12.1\n", wantSource: "official-installer-cuda", wantPolicy: "cuda", wantRuns: 1},
		{name: "transient-payload-failure-retries", capabilities: "12.1\n", failInstalls: 1, wantSource: "official-installer-cuda", wantPolicy: "cuda", wantRuns: 2},
		{name: "payload-missing-falls-back", capabilities: "11.0\n", failInstalls: 2, wantPolicy: "vulkan", wantNote: "official CUDA build", wantRuns: 2},
		{name: "pre-turing", capabilities: "6.1\n", wantPolicy: "vulkan", wantNote: "below the 7.5", wantRuns: 0},
		{name: "no-nvidia", queryErr: errors.New("executable file not found"), wantPolicy: "vulkan", wantNote: "no NVIDIA GPU", wantRuns: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinuxCUDAFixture(t, tc.capabilities, tc.queryErr, cudaDevices)
			f.failInstalls = tc.failInstalls
			if err := f.e.installLlamaApp(context.Background(), f.st); err != nil {
				t.Fatal(err)
			}
			if f.installerRuns != tc.wantRuns {
				t.Fatalf("restricted installer runs = %d, want %d", f.installerRuns, tc.wantRuns)
			}
			for _, env := range f.installerEnv {
				if env["SKIP_VULKAN"] != "1" || env["SKIP_ROCM"] != "1" || env["LLAMA_VERSION"] != "b10826" || env["SKIP_INSTALL"] != "1" {
					t.Fatalf("restricted installer env = %v", env)
				}
			}
			receipt := f.receipt(t)
			if tc.wantSource != "" {
				if receipt["source"] != tc.wantSource || receipt["cuda_device_verified"] != true || receipt["cuda_not_used"] != nil {
					t.Fatalf("CUDA receipt = %v", receipt)
				}
				if caps, _ := receipt["compute_capabilities"].([]any); len(caps) != 1 {
					t.Fatalf("receipt lacks compute capabilities: %v", receipt)
				}
				// A retried install keeps the first attempt's rejection for diagnosis; a clean one records nothing.
				if retried, _ := receipt["retried_after"].(string); (tc.failInstalls > 0) != (retried != "") {
					t.Fatalf("retried_after = %q with %d failed attempts", retried, tc.failInstalls)
				}
			} else {
				note, _ := receipt["cuda_not_used"].(string)
				if !strings.Contains(note, tc.wantNote) || receipt["source"] != "official-installer" {
					t.Fatalf("fallback receipt = %v", receipt)
				}
			}
			if receipt["acceleration_policy"] != tc.wantPolicy {
				t.Fatalf("acceleration_policy = %v, want %s", receipt["acceleration_policy"], tc.wantPolicy)
			}
			if rows, _ := receipt["devices"].([]any); len(rows) != 1 {
				t.Fatalf("receipt devices = %v", receipt["devices"])
			}
			var found bool
			for _, status := range f.e.GetInstalled() {
				if status.Engine == "llamacpp" {
					found = true
					if status.Acceleration != tc.wantPolicy || len(status.Devices) != 1 {
						t.Fatalf("engine status acceleration = %q %v", status.Acceleration, status.Devices)
					}
				}
			}
			if !found {
				t.Fatal("llamacpp missing from inventory")
			}
		})
	}
}

// A parent cancellation during the restricted attempt neither retries nor
// falls back to the unrestricted installer.
func TestLlamaLinuxCUDAGateCancelDoesNotFallBack(t *testing.T) {
	f := newLinuxCUDAFixture(t, "12.1\n", nil, "Available devices:\n  CUDA0: NVIDIA GB10")
	ctx, cancel := context.WithCancel(context.Background())
	f.e.runLlamaInstaller = func(context.Context, string, map[string]string) error {
		f.installerRuns++
		cancel()
		return errors.New("fixture: interrupted")
	}
	err := f.e.installLlamaApp(ctx, f.st)
	if !errors.Is(err, context.Canceled) || f.installerRuns != 1 {
		t.Fatalf("err = %v, runs = %d", err, f.installerRuns)
	}
	if _, err := os.Stat(filepath.Join(f.st.installDir, "runtime", llamaExecutable())); err == nil {
		t.Fatal("cancelled install promoted a runtime")
	}
}

// The Windows x64 installer path now records the accelerator the vendor build
// actually has, so the receipt of a non-NVIDIA host says "vulkan".
func TestLlamaVendorInstallerReceiptRecordsAcceleration(t *testing.T) {
	f := newWindowsCUDAFixture(t, "", errors.New("executable file not found"), cudaLlamaFixture(10826))
	if err := f.e.installLlamaApp(context.Background(), f.st); err != nil {
		t.Fatal(err)
	}
	receipt := f.receipt(t)
	if receipt["acceleration_policy"] != "vulkan" || receipt["devices_error"] != nil {
		t.Fatalf("vendor receipt = %v", receipt)
	}
	if rows, _ := receipt["devices"].([]any); len(rows) != 1 || !strings.HasPrefix(rows[0].(string), "Vulkan0:") {
		t.Fatalf("vendor receipt devices = %v", receipt["devices"])
	}
}

// NVIDIA Windows ARM64 installs the pinned CUDA archives directly; the
// upstream-latest attempt is opt-in and no longer costs a failed round trip.
func TestLlamaARMPinnedCUDAIsTheDefault(t *testing.T) {
	f := newLlamaUpstreamFixture(t, cudaLlamaFixture(10900))
	// The bundled recipe: pinned archives plus the CPU installer, no upstream opt-in,
	// on a host whose inventory reports NVIDIA hardware.
	f.st.plat.Install.UpstreamFirst = false
	f.st.plat.Install.CPUFetch = &Fetch{URL: "https://example.invalid/pinned-cpu.ps1", SHA256: strings.Repeat("a", 64)}
	f.e.armHardwareQuery = func(context.Context, map[string]string) (string, error) {
		return `{"cpus":[{"Architecture":12,"Manufacturer":"NVIDIA"}],"device_count":40,"nvidia_hardware":true}`, nil
	}
	if err := f.e.installLlamaApp(context.Background(), f.st); err != nil {
		t.Fatal(err)
	}
	if f.requests[llamaLatestVersionURL] != 0 || f.requests[llamaLatestInstallerURL] != 0 {
		t.Fatal("default ARM64 install contacted the upstream-latest endpoints")
	}
	receipt := f.receipt(t)
	if receipt["source"] != "pinned-cuda-archives" || receipt["acceleration_policy"] != "cuda" || receipt["fallback_reason"] != nil || receipt["cuda_device_verified"] != true {
		t.Fatalf("direct pinned receipt = %v", receipt)
	}
	for _, fetch := range f.st.plat.Install.Archives {
		if f.requests[fetch.URL] != 1 {
			t.Fatalf("pinned archive %s fetched %d times", fetch.URL, f.requests[fetch.URL])
		}
	}
	if _, err := os.Stat(filepath.Join(f.st.installDir, "runtime", "cuda.dll")); err != nil {
		t.Fatal("CUDA companion archive was not promoted")
	}
}
