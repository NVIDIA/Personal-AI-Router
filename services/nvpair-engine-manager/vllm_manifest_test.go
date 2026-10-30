// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// vLLM is Linux (NVIDIA) and Apple Silicon only, installs through a pinned uv
// bootstrap, serves exactly one model per process and has no load, unload, pull or delete API. These
// are the properties the rest of the stack (UI capabilities, the model wire,
// the install gate) relies on, so a manifest edit that drops one fails here.
func TestBundledVLLMManifest(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("vllm manifest not loaded")
	}

	if got := len(manifest.Platforms); got != 3 {
		t.Fatalf("vllm declares %d platforms, want linux/amd64, linux/arm64 and darwin/arm64 only", got)
	}
	// The version each platform's install script must pin: Linux installs vLLM
	// itself from PyPI, macOS installs the vLLM core and vLLM-Metal wheels.
	pins := map[string]string{"linux/amd64": "vllm==", "linux/arm64": "vllm==", "darwin/arm64": "vllm-0.32.0"}
	for _, key := range []string{"linux/amd64", "linux/arm64", "darwin/arm64"} {
		platform, ok := manifest.Platforms[key]
		if !ok {
			t.Fatalf("vllm has no %s platform block", key)
		}
		// Our managed install must win over an external one.
		if len(platform.Detect) < 2 || !strings.HasPrefix(platform.Detect[0], "{install_dir}/") {
			t.Errorf("%s detect order = %v, want the managed install first", key, platform.Detect)
		}
		if platform.Install == nil || platform.Install.Fetch == nil || platform.Install.Fetch.SHA256 == "" {
			t.Errorf("%s: the uv bootstrap must be a sha256-pinned fetch", key)
			continue
		}
		if len(platform.Install.Script) != 0 {
			t.Errorf("%s: install must not use the unpinned script escape hatch", key)
		}
		script := strings.Join(platform.Install.Run, " ")
		if !strings.Contains(script, pins[key]) {
			t.Errorf("%s: the vLLM install must be pinned (want %q in the script)", key, pins[key])
		}
		if strings.HasPrefix(key, "darwin/") {
			// The macOS wheels are downloaded by the script itself, so each one
			// has to be checked against a digest before it is installed.
			if got := strings.Count(script, "shasum -a 256 -c"); got != 2 {
				t.Errorf("%s: %d wheel digest checks, want one for the vLLM core and one for vLLM-Metal", key, got)
			}
		}
		rt := platform.Runtime
		if rt.EditableLaunch == nil || !slices.Equal(rt.EditableLaunch.FixedArgs, []string{"serve"}) {
			t.Errorf("%s: launch must start with the fixed serve subcommand", key)
		}
		if platform.ModelsDir != "~/.cache/huggingface" {
			t.Errorf("%s: models_dir = %q, want the Hugging Face cache", key, platform.ModelsDir)
		}
		// vLLM compiles FlashInfer sampling kernels at first start and that build
		// cannot cope with the spaces in PAIR's app-data path
		// ("Nvidia Corporation/Personal AI Router"), so it must stay off. It also
		// keeps a system CUDA toolkit and C++ compiler from being required.
		if rt.Env["VLLM_USE_FLASHINFER_SAMPLER"] != "0" {
			t.Errorf("%s: the FlashInfer sampler must be disabled", key)
		}
		// vLLM reserves 92% of GPU memory by default and refuses a context above
		// the model's own limit. Both are decided per model at start, so the
		// manifest must not pin them in the saved launch.
		if rt.AutoLaunch == nil || rt.AutoLaunch.Kind != autoLaunchHFContext || rt.AutoLaunch.ContextTokens != 65536 {
			t.Errorf("%s: vllm must size its context and GPU share automatically for a 64K window, got %+v", key, rt.AutoLaunch)
		}
		for _, pinned := range []string{"--gpu-memory-utilization", "--max-model-len"} {
			if slices.Contains(rt.Args, pinned) {
				t.Errorf("%s: %s must not be pinned in the default launch", key, pinned)
			}
		}
		if rt.Port != 8001 {
			t.Errorf("%s: engine port = %d, want 8001 (the facade owns 8000)", key, rt.Port)
		}
	}

	for _, name := range []string{"list_models", "loaded_models"} {
		if _, ok := manifest.Actions[name]; !ok {
			t.Errorf("vllm must declare %s", name)
		}
	}
	// list_models is what is on disk, and all of it is advertised: a request for
	// a model that is not loaded switches the engine to it.
	if list := manifest.Actions["list_models"]; list.HFCache == nil || list.HFCache.Dir != "{models_dir}/hub" {
		t.Errorf("list_models must list the Hugging Face cache, got %+v", list)
	}
	if manifest.AdvertisedModels == advertisedModelsLoaded {
		t.Errorf("advertised_models = %q: the gateway switches models on request, so peers and the proxy must be offered every cached model", manifest.AdvertisedModels)
	}
	// The gateway's plain /v1/models names every downloaded model, so reading what
	// is resident has to ask for the resident one only, or every model shows as loaded.
	if loaded := manifest.Actions["loaded_models"]; loaded.HTTP == nil || loaded.HTTP.Path != "/v1/models?resident=1" {
		t.Errorf("loaded_models must read /v1/models?resident=1, got %+v", loaded.HTTP)
	}
	// Switching the model is a launch-argument change plus a restart, since the
	// engine has no load call.
	if load := manifest.Actions["load_model"]; load.LaunchModel == nil || load.LaunchModel.Flag != "--model" {
		t.Errorf("load_model must switch the model through --model, got %+v", load)
	}
	// vLLM cannot drop its model and keep running, so Eject stops the engine.
	if unload := manifest.Actions["unload_model"]; !unload.StopEngine {
		t.Errorf("unload_model must stop the engine, got %+v", unload)
	}
	// Deleting is a removal from the shared cache that needs no running engine.
	if del := manifest.Actions["delete_model"]; del.HFCache == nil || !del.HFCache.Delete || del.HFCache.Dir != "{models_dir}/hub" {
		t.Errorf("delete_model must remove from the hub cache, got %+v", del)
	}
	// Downloads go through the hf tool that ships in the engine's virtualenv.
	if pull := manifest.Actions["pull_model"]; pull.HFPull == nil || pull.HFPull.Tool != "hf" || pull.HFPull.Dir != "{models_dir}/hub" {
		t.Errorf("pull_model must download through hf into the hub cache, got %+v", pull)
	}
}

// The vLLM wheel is a CUDA 13 build, so an older driver installs cleanly and
// then fails at startup, after a multi-GB download. The install script has to
// refuse before downloading anything. This runs the manifest's own script, with
// its placeholders resolved the way the executor resolves them, against a fake
// nvidia-smi. Recent drivers print "CUDA UMD Version" and mark the older
// "CUDA Version" field deprecated, so both spellings have to be understood.
func TestVLLMInstallRefusesDriversWithoutCUDA13(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the vLLM install script is Linux only")
	}
	tools := map[string]string{}
	for _, name := range []string{"sh", "awk", "df", "mkdir", "tar"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not available", name)
		}
		tools[name] = path
	}

	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("vllm manifest not loaded")
	}

	cases := []struct {
		name       string
		smiOutput  *string
		wantCUDA   bool
		wantInText string
	}{
		{name: "no nvidia-smi", wantInText: "nvidia-smi not found"},
		{name: "old driver", smiOutput: ptr("CUDA Version                                           : 12.4\n"), wantInText: "CUDA 13 or newer (found: 12)"},
		{name: "unrecognised output", smiOutput: ptr("Driver Version : 1.2.3\n"), wantInText: "CUDA 13 or newer (found: none)"},
		{name: "current driver format", smiOutput: ptr("CUDA Version                                           : 13.4 [Deprecated; will be removed in CUDA 14.0. Use CUDA UMD Version instead]\nCUDA UMD Version                                       : 13.4\n"), wantCUDA: true},
		{name: "older CUDA 13 driver format", smiOutput: ptr("CUDA Version                                           : 13.0\n"), wantCUDA: true},
		{name: "UMD only", smiOutput: ptr("CUDA UMD Version                                       : 14.0\n"), wantCUDA: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			for name, path := range tools {
				if name != "sh" {
					if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.smiOutput != nil {
				script := "#!/bin/sh\nprintf '%s' '" + *tc.smiOutput + "'\n"
				if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			installDir := t.TempDir()
			argv, err := resolveArgs(manifest.Platforms["linux/amd64"].Install.Run, map[string]string{
				"download":    filepath.Join(installDir, "missing-uv.tar.gz"),
				"install_dir": installDir,
			})
			if err != nil {
				t.Fatal(err)
			}
			// The executor expands OS environment references in every install
			// argument after resolving placeholders (install.go), so "$name" in
			// the script would be replaced before the shell ever saw it. Apply
			// the same expansion, and require it to be a no-op: only positional
			// parameters and "${name:-default}" forms survive it.
			for i := range argv {
				if expanded := expandPath(argv[i]); expanded != argv[i] {
					t.Fatalf("the executor rewrites install argument %d, which breaks the script:\n%s\n-> %s", i, argv[i], expanded)
				}
			}
			cmd := exec.Command(tools["sh"], argv[1:]...)
			cmd.Env = []string{"PATH=" + bin}
			out, runErr := cmd.CombinedOutput()
			text := string(out)

			if tc.wantCUDA {
				// The driver check passed. The script goes on to fail later,
				// because the archive does not exist; it must not blame the driver.
				if strings.Contains(text, "NVIDIA driver") {
					t.Fatalf("a CUDA 13 driver was refused: %s", text)
				}
				return
			}
			if runErr == nil {
				t.Fatalf("install was not refused: %s", text)
			}
			if !strings.Contains(text, tc.wantInText) {
				t.Fatalf("output %q does not contain %q", text, tc.wantInText)
			}
			if _, err := os.Stat(filepath.Join(installDir, "uv")); err == nil {
				t.Fatal("the script started installing before it refused")
			}
		})
	}
}

// The executor expands OS environment references in every install argument
// after resolving placeholders, so a "$name" in a script would be replaced
// before the shell ran it. Each platform's script must survive that untouched.
func TestVLLMInstallScriptsSurviveArgumentExpansion(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("vllm manifest not loaded")
	}
	for key, platform := range manifest.Platforms {
		argv, err := resolveArgs(platform.Install.Run, map[string]string{
			"download":    "/tmp/uv.tar.gz",
			"install_dir": "/tmp/vllm",
		})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		for i := range argv {
			if expanded := expandPathForOS(argv[i], "linux"); expanded != argv[i] {
				t.Errorf("%s: the executor rewrites install argument %d, which breaks the script:\n%s\n-> %s", key, i, argv[i], expanded)
			}
		}
	}
}

func ptr(s string) *string { return &s }
