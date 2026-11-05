// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vLLM is Linux (NVIDIA) and Apple Silicon only, installs through a pinned uv
// bootstrap, serves exactly one model per process and has no load, unload, pull or delete API. These
// are the properties the rest of the stack (UI capabilities, the model wire,
// the install gate) relies on, so a manifest edit that drops one fails here.
func TestBundledVLLMManifest_PinsTheInstallAndSizesTheLaunchOnEachPlatform(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")
	require.Len(t, manifest.Platforms, 3, "want linux/amd64, linux/arm64 and darwin/arm64 only")

	// wantPin is the version the platform's install script must pin: Linux
	// installs vLLM itself from PyPI, macOS installs the vLLM core and
	// vLLM-Metal wheels.
	test := func(key, wantPin string) {
		t.Run(key, func(t *testing.T) {
			platform, ok := manifest.Platforms[key]
			require.True(t, ok, "vllm has no %s platform block", key)

			// Our managed install must win over an external one.
			require.GreaterOrEqual(t, len(platform.Detect), 2, "detect must list the managed install and an external one")
			assert.True(t, strings.HasPrefix(platform.Detect[0], "{install_dir}/"), "detect order = %v, want the managed install first", platform.Detect)
			require.NotNil(t, platform.Install)
			require.NotNil(t, platform.Install.Fetch, "the uv bootstrap must be a fetch")
			require.NotEmpty(t, platform.Install.Fetch.SHA256, "the uv bootstrap must be sha256-pinned")
			assert.Empty(t, platform.Install.Script, "install must not use the unpinned script escape hatch")
			script := strings.Join(platform.Install.Run, " ")
			assert.Contains(t, script, wantPin, "the vLLM install must be pinned")
			// vLLM 0.32 asks only for mistral_common>=1.11.6. Release 1.13 made an
			// argument of get_lark_from_jinja required that vLLM 0.32 does not pass,
			// so with the Mistral tool-call parser every request to a Mistral model
			// fails. vLLM 0.33 makes the same call; drop the pin only with a vLLM
			// release that itself requires mistral_common 1.13.
			assert.Contains(t, script, "mistral_common==1.12.0", "the install must pin mistral_common==1.12.0 for vLLM 0.32")
			if strings.HasPrefix(key, "darwin/") {
				// The macOS wheels are downloaded by the script itself, so each one
				// has to be checked against a digest before it is installed.
				assert.Equal(t, 2, strings.Count(script, "shasum -a 256 -c"), "want one wheel digest check for the vLLM core and one for vLLM-Metal")
			}

			rt := platform.Runtime
			require.NotNil(t, rt.EditableLaunch)
			assert.Equal(t, []string{"serve"}, rt.EditableLaunch.FixedArgs, "launch must start with the fixed serve subcommand")
			assert.Equal(t, "~/.cache/huggingface", platform.ModelsDir, "models_dir must be the Hugging Face cache")
			// vLLM compiles FlashInfer sampling kernels at first start and that build
			// cannot cope with the spaces in PAIR's app-data path
			// ("Nvidia Corporation/Personal AI Router"), so it must stay off. It also
			// keeps a system CUDA toolkit and C++ compiler from being required.
			assert.Equal(t, "0", rt.Env["VLLM_USE_FLASHINFER_SAMPLER"], "the FlashInfer sampler must be disabled")
			// vLLM reserves 92% of GPU memory by default and refuses a context above
			// the model's own limit. Both are decided per model at start, so the
			// manifest must not pin them in the saved launch.
			require.NotNil(t, rt.AutoLaunch, "vllm must size its context and GPU share automatically")
			assert.Equal(t, autoLaunchHFContext, rt.AutoLaunch.Kind)
			assert.Equal(t, 65536, rt.AutoLaunch.ContextTokens, "vllm must aim for a 64K window")
			// vLLM's default of 256 concurrent requests does not fit a hybrid model
			// such as the default Qwen3.5-4B on a 16 GiB GPU.
			assert.Equal(t, 32, rt.AutoLaunch.MaxNumSeqs, "vllm must run at most 32 requests at a time")
			assert.NotContains(t, rt.Args, "--gpu-memory-utilization", "must not be pinned in the default launch")
			assert.NotContains(t, rt.Args, "--max-model-len", "must not be pinned in the default launch")
			assert.Equal(t, 8001, rt.Port, "the facade owns 8000")
		})
	}
	test("linux/amd64", "vllm==")
	test("linux/arm64", "vllm==")
	test("darwin/arm64", "vllm-0.32.0")
}

func TestBundledVLLMManifest_DeclaresTheModelActionsTheStackReliesOn(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")

	assert.Contains(t, manifest.Actions, "list_models")
	assert.Contains(t, manifest.Actions, "loaded_models")
	// list_models is what is on disk; only the model being served is advertised.
	if list := manifest.Actions["list_models"]; assert.NotNil(t, list.HFCache, "list_models must list the Hugging Face cache") {
		assert.Equal(t, "{models_dir}/hub", list.HFCache.Dir)
	}
	assert.Equal(t, advertisedModelsLoaded, manifest.AdvertisedModels, "peers and the proxy must only be offered the served model")
	if loaded := manifest.Actions["loaded_models"]; assert.NotNil(t, loaded.HTTP, "loaded_models must read vLLM's /v1/models") {
		assert.Equal(t, "/v1/models", loaded.HTTP.Path)
	}
	// Switching the model is a launch-argument change plus a restart, since the
	// engine has no load call.
	if load := manifest.Actions["load_model"]; assert.NotNil(t, load.LaunchModel, "load_model must switch the model through a launch argument") {
		assert.Equal(t, "--model", load.LaunchModel.Flag)
	}
	// vLLM cannot drop its model and keep running, so Eject stops the engine.
	assert.True(t, manifest.Actions["unload_model"].StopEngine, "unload_model must stop the engine")
	// Deleting is a removal from the shared cache that needs no running engine.
	if del := manifest.Actions["delete_model"]; assert.NotNil(t, del.HFCache, "delete_model must remove from the hub cache") {
		assert.True(t, del.HFCache.Delete)
		assert.Equal(t, "{models_dir}/hub", del.HFCache.Dir)
	}
	// Downloads go through the hf tool that ships in the engine's virtualenv.
	if pull := manifest.Actions["pull_model"]; assert.NotNil(t, pull.HFPull, "pull_model must download through hf into the hub cache") {
		assert.Equal(t, "hf", pull.HFPull.Tool)
		assert.Equal(t, "{models_dir}/hub", pull.HFPull.Dir)
	}
}

// The vLLM wheel is a CUDA 13 build, so an older driver installs cleanly and
// then fails at startup, after a multi-GB download. The install script has to
// refuse before downloading anything. This runs the manifest's own script, with
// its placeholders resolved the way the executor resolves them, against a fake
// nvidia-smi. Recent drivers print "CUDA UMD Version" and mark the older
// "CUDA Version" field deprecated, so both spellings have to be understood.
func TestVLLMInstall_RefusesDriversWithoutCUDA13(t *testing.T) {
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
	require.True(t, ok, "vllm manifest not loaded")

	// runInstall runs the Linux install script with only the tools above and,
	// unless smiOutput is nil, a fake nvidia-smi that prints it.
	runInstall := func(t *testing.T, smiOutput *string) (text string, runErr error, installDir string) {
		t.Helper()
		bin := t.TempDir()
		for name, path := range tools {
			if name != "sh" {
				require.NoError(t, os.Symlink(path, filepath.Join(bin, name)))
			}
		}
		if smiOutput != nil {
			script := "#!/bin/sh\nprintf '%s' '" + *smiOutput + "'\n"
			require.NoError(t, os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(script), 0o755))
		}
		installDir = t.TempDir()
		argv, err := resolveArgs(manifest.Platforms["linux/amd64"].Install.Run, map[string]string{
			"download":    filepath.Join(installDir, "missing-uv.tar.gz"),
			"install_dir": installDir,
		})
		require.NoError(t, err)
		// The executor expands OS environment references in every install
		// argument after resolving placeholders (install.go), so "$name" in
		// the script would be replaced before the shell ever saw it. Apply
		// the same expansion, and require it to be a no-op: only positional
		// parameters and "${name:-default}" forms survive it.
		for i := range argv {
			require.Equal(t, argv[i], expandPath(argv[i]), "the executor rewrites install argument %d, which breaks the script", i)
		}
		cmd := exec.Command(tools["sh"], argv[1:]...)
		cmd.Env = []string{"PATH=" + bin}
		out, runErr := cmd.CombinedOutput()
		return string(out), runErr, installDir
	}
	accepted := func(name, smiOutput string) {
		t.Run(name, func(t *testing.T) {
			text, _, _ := runInstall(t, &smiOutput)

			// The driver check passed. The script goes on to fail later,
			// because the archive does not exist; it must not blame the driver.
			assert.NotContains(t, text, "NVIDIA driver", "a CUDA 13 driver was refused")
		})
	}
	refused := func(name string, smiOutput *string, wantInText string) {
		t.Run(name, func(t *testing.T) {
			text, runErr, installDir := runInstall(t, smiOutput)

			require.Error(t, runErr, "install was not refused: %s", text)
			assert.Contains(t, text, wantInText)
			assert.NoFileExists(t, filepath.Join(installDir, "uv"), "the script started installing before it refused")
		})
	}
	accepted("current driver format", "CUDA Version                                           : 13.4 [Deprecated; will be removed in CUDA 14.0. Use CUDA UMD Version instead]\nCUDA UMD Version                                       : 13.4\n")
	accepted("older CUDA 13 driver format", "CUDA Version                                           : 13.0\n")
	accepted("UMD only", "CUDA UMD Version                                       : 14.0\n")
	refused("no nvidia-smi", nil, "nvidia-smi not found")
	refused("old driver", ptr("CUDA Version                                           : 12.4\n"), "CUDA 13 or newer (found: 12)")
	refused("unrecognised output", ptr("Driver Version : 1.2.3\n"), "CUDA 13 or newer (found: none)")
}

// The executor expands OS environment references in every install argument
// after resolving placeholders, so a "$name" in a script would be replaced
// before the shell ran it. Each platform's script must survive that untouched.
func TestVLLMInstallScripts_SurviveArgumentExpansion(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")
	for key, platform := range manifest.Platforms {
		t.Run(key, func(t *testing.T) {
			argv, err := resolveArgs(platform.Install.Run, map[string]string{
				"download":    "/tmp/uv.tar.gz",
				"install_dir": "/tmp/vllm",
			})

			require.NoError(t, err)
			for i := range argv {
				assert.Equal(t, argv[i], expandPathForOS(argv[i], "linux"), "the executor rewrites install argument %d, which breaks the script", i)
			}
		})
	}
}

func ptr(s string) *string { return &s }
