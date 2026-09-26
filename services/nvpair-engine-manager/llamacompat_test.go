// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const b580Devices = "Available devices:\r\n  Vulkan0: Intel(R) Arc(TM) B580 Graphics (12116 MiB, 11347 MiB free)\r\n  Vulkan1: AMD Radeon(TM) Graphics (16188 MiB, 15379 MiB free)\r\n"

// No profile marker is required: this is the receipt format from before the
// compatibility default. Runtime identity, not a PAIR version, admits it.
func b580OldReceipt(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"platform": "windows/amd64", "version": "version: 0.4.0-dev (build 10826, commit 73a43d1f6)",
		"binary_sha256":    llamaB580BinarySHA256,
		"installer_sha256": "455084203db0c864f4eb218bc82792b4304458a96211c385275d8337a5049851",
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLlamaB580Receipt(t *testing.T) {
	old := b580OldReceipt(t)
	for _, tc := range []struct {
		name string
		data []byte
		want bool
	}{
		{"old-install", old, true},
		{"new-build", bytes.ReplaceAll(old, []byte(llamaB580BinarySHA256), []byte(strings.Repeat("a", 64))), false},
		{"new-installer", bytes.ReplaceAll(old, []byte("45508420"), []byte("55508420")), false},
		{"windows-arm-cuda", bytes.ReplaceAll(old, []byte("windows/amd64"), []byte("windows/arm64")), false},
		{"apple-silicon", bytes.ReplaceAll(old, []byte("windows/amd64"), []byte("darwin/arm64")), false},
		{"linux", bytes.ReplaceAll(old, []byte("windows/amd64"), []byte("linux/amd64")), false},
		{"unknown", []byte(`{"version":"b10826-73a43d1f6"}`), false},
		{"invalid", []byte("{"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := llamaB580Receipt(tc.data); got != tc.want {
				t.Fatalf("receipt match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLlamaB580Selection(t *testing.T) {
	t.Setenv("LLAMA_ARG_DEVICE", "")
	t.Setenv("LLAMA_ARG_N_GPU_LAYERS", "")
	for _, tc := range []struct {
		name, devices string
		args          []string
		env           map[string]string
		want          bool
	}{
		{"automatic-mixed", b580Devices, nil, nil, true},
		{"explicit-b580", b580Devices, []string{"serve", "-dev", "Vulkan0"}, nil, true},
		{"explicit-mixed", b580Devices, []string{"--device=Vulkan1,Vulkan0"}, nil, true},
		{"explicit-other-vulkan", b580Devices, []string{"--device", "Vulkan1"}, nil, false},
		{"explicit-cuda", b580Devices, []string{"--device", "CUDA0"}, nil, false},
		{"explicit-cpu", b580Devices, nil, map[string]string{"LLAMA_ARG_DEVICE": "none"}, false},
		{"zero-gpu-layers", b580Devices, []string{"-ngl", "0", "-fa", "on"}, nil, false},
		{"zero-layers-env", b580Devices, nil, map[string]string{"LLAMA_ARG_N_GPU_LAYERS": "0"}, false},
		{"negative-auto-layers", b580Devices, []string{"--gpu-layers", "-1"}, nil, true},
		{"negative-all-layers", b580Devices, []string{"--gpu-layers=-2"}, nil, true},
		{"cli-over-env", b580Devices, []string{"-dev", "Vulkan0"}, map[string]string{"llama_arg_device": "CUDA0"}, true},
		{"last-cli-wins", b580Devices, []string{"-dev", "Vulkan0", "--device=Vulkan1"}, nil, false},
		{"renumbered", strings.ReplaceAll(b580Devices, "Vulkan0", "Vulkan12"), []string{"-dev", "Vulkan12"}, nil, true},
		{"other-intel", strings.ReplaceAll(b580Devices, "B580", "B570"), nil, nil, false},
		{"model-name-is-not-device", "model: Intel(R) Arc(TM) B580 Graphics", nil, nil, false},
		{"diagnostic-is-not-row", "error: Vulkan0: Intel(R) Arc(TM) B580 Graphics", nil, nil, false},
		{"cuda-only", "  CUDA0: NVIDIA GPU", nil, nil, false},
		{"empty", "", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := llamaB580Selected(tc.devices, tc.args, tc.env)
			if err != nil || got != tc.want {
				t.Fatalf("selection = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func clearB580Environment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GGML_VK_DISABLE_COOPMAT", "GGML_VK_DISABLE_COOPMAT2", "GGML_VK_DISABLE_INTEGER_DOT_PRODUCT", "GGML_VK_DISABLE_F16", "GGML_VK_DISABLE_BFLOAT16", "LLAMA_ARG_FLASH_ATTN", "LLAMA_ARG_MODELS_PRESET"} {
		t.Setenv(key, "") // Register restoration before making the key absent.
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLlamaB580Environment(t *testing.T) {
	clearB580Environment(t)
	for _, tc := range []struct {
		name        string
		args        []string
		env, parent map[string]string
		conflict    bool
	}{
		{"defaults", nil, map[string]string{"UNCHANGED": "keep"}, nil, false},
		{"compatible", []string{"-fa", "off"}, map[string]string{"ggml_vk_disable_f16": "1"}, nil, false},
		{"inherited-compatible", nil, nil, map[string]string{"GGML_VK_DISABLE_F16": "1"}, false},
		{"inherited-zero", nil, nil, map[string]string{"GGML_VK_DISABLE_F16": "0"}, false},
		{"manifest-over-parent", nil, map[string]string{"ggml_vk_disable_f16": "1"}, map[string]string{"GGML_VK_DISABLE_F16": "0"}, false},
		{"explicit-zero", nil, map[string]string{"GGML_VK_DISABLE_COOPMAT": "0"}, nil, false},
		{"explicit-empty", nil, map[string]string{"GGML_VK_DISABLE_COOPMAT2": ""}, nil, false},
		{"equivalent-presence-case", nil, map[string]string{"GGML_VK_DISABLE_F16": "1", "ggml_vk_disable_f16": "0"}, nil, false},
		{"ambiguous-flash-case", nil, map[string]string{"LLAMA_ARG_FLASH_ATTN": "off", "llama_arg_flash_attn": "on"}, nil, true},
		{"flash-env-on", nil, nil, map[string]string{"LLAMA_ARG_FLASH_ATTN": "on"}, true},
		{"flash-cli-on", []string{"--flash-attn=on"}, nil, nil, true},
		{"flash-cli-auto", []string{"-fa", "auto"}, nil, nil, true},
		{"flash-cli-missing", []string{"--flash-attn"}, nil, nil, true},
		{"flash-cli-over-env", []string{"--flash-attn", "off"}, map[string]string{"LLAMA_ARG_FLASH_ATTN": "on"}, nil, false},
		{"flash-inherited-zero", nil, nil, map[string]string{"LLAMA_ARG_FLASH_ATTN": "0"}, false},
		{"flash-manifest-disabled", nil, map[string]string{"LLAMA_ARG_FLASH_ATTN": "disabled"}, nil, false},
		{"flash-cli-false", []string{"-fa", "false"}, nil, nil, false},
		{"flash-inline-zero", []string{"--flash-attn=0"}, nil, nil, false},
		{"equivalent-flash-case", nil, map[string]string{"LLAMA_ARG_FLASH_ATTN": "off", "llama_arg_flash_attn": "false"}, nil, false},
		{"flash-last-cli-wins", []string{"-fa", "on", "--flash-attn=off"}, nil, nil, false},
		{"model-preset", []string{"--models-preset", "private.ini"}, nil, nil, true},
		{"inherited-preset", nil, nil, map[string]string{"LLAMA_ARG_MODELS_PRESET": "private.ini"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.parent {
				t.Setenv(k, v)
			}
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			before, _ := json.Marshal(env)
			parent := os.Environ()
			err := applyLlamaB580Defaults(tc.args, env)
			if (err != nil) != tc.conflict {
				t.Fatalf("conflict = %v, want %v", err, tc.conflict)
			}
			if !reflect.DeepEqual(os.Environ(), parent) {
				t.Fatal("changed parent environment")
			}
			if err != nil {
				after, _ := json.Marshal(env)
				if !bytes.Equal(before, after) || !strings.Contains(err.Error(), "retry Start") || strings.Contains(err.Error(), "private.ini") {
					t.Fatalf("conflict changed options or lacked private actionable error: %v", err)
				}
				return
			}
			for k, v := range tc.env {
				if env[k] != v {
					t.Fatalf("changed explicit option %s", k)
				}
			}
			for _, key := range []string{"GGML_VK_DISABLE_COOPMAT", "GGML_VK_DISABLE_COOPMAT2", "GGML_VK_DISABLE_INTEGER_DOT_PRODUCT", "GGML_VK_DISABLE_F16", "GGML_VK_DISABLE_BFLOAT16", "LLAMA_ARG_FLASH_ATTN"} {
				flags := []string{}
				want := "1"
				if key == "LLAMA_ARG_FLASH_ATTN" {
					want, flags = "off", []string{"--flash-attn", "-fa"}
				}
				got, present, err := llamaCompatibilityOption(tc.args, env, key, flags...)
				if err != nil || !present || got != want {
					t.Fatalf("effective %s = %q, %v", key, got, err)
				}
			}
			first, _ := json.Marshal(env)
			if err := applyLlamaB580Defaults(tc.args, env); err != nil {
				t.Fatal(err)
			}
			second, _ := json.Marshal(env)
			if !bytes.Equal(first, second) || env["GGML_VK_DISABLE_ASYNC"] != "" {
				t.Fatal("defaults are not idempotent or added ASYNC")
			}
		})
	}
}

func TestLlamaB580StartAdmission(t *testing.T) {
	clearB580Environment(t)
	for _, tc := range []struct {
		name, engine             string
		adopted, external, known bool
	}{
		{"other-engine", "ollama", false, false, true},
		{"adopted", "llamacpp", true, false, true},
		{"external-path", "llamacpp", false, true, true},
		{"unknown-receipt", "llamacpp", false, false, false},
		{"changed-binary", "llamacpp", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "runtime", "llama.exe")
			if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("never execute"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.known {
				if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "pair-install.json"), b580OldReceipt(t), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.external {
				bin = filepath.Join(t.TempDir(), "llama.exe")
			}
			st := &engineState{manifest: &Manifest{Engine: tc.engine}, installDir: root, adopted: tc.adopted}
			env := map[string]string{"UNCHANGED": "keep"}
			err := (&Executor{}).prepareLlamaCompatibility(context.Background(), st, bin, nil, env)
			wantErr := tc.name == "changed-binary" && runtime.GOOS == "windows" && runtime.GOARCH == "amd64"
			if (err != nil) != wantErr || len(env) != 1 {
				t.Fatalf("admission = %v, options = %v", err, env)
			}
			if wantErr {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := (&Executor{}).prepareLlamaCompatibility(ctx, st, bin, nil, env); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation = %v", err)
				}
			}
		})
	}
}

// An old B580 install the user turned Off must stay Off across restores, and a
// repeat Install over it must be a pure no-op: no fetch, no receipt rewrite, no
// runtime byte change and no staging, so the compatibility identity keeps
// qualifying on the next Start.
func TestLlamaB580OldInstallStaysOffAndInstallIsIdempotent(t *testing.T) {
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, t.TempDir())
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(st.installDir, "runtime", llamaExecutable())
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0700); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(filepath.Dir(bin), "pair-install.json")
	receipt := b580OldReceipt(t)
	if err := os.WriteFile(receiptPath, receipt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.setDesiredEnabled("llamacpp", false); err != nil {
		t.Fatal(err)
	}
	e.client = &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Error("repeat install over an existing runtime fetched an artifact")
		return nil, errors.New("unexpected acquisition")
	})}
	for range 2 {
		if err := e.Install(context.Background(), "llamacpp"); err != nil {
			t.Fatal(err)
		}
		if err := e.RestoreEnabled(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil || !bytes.Equal(receipt, after) || !llamaB580Receipt(after) {
		t.Fatal("old receipt changed or stopped qualifying")
	}
	after, err = os.ReadFile(bin)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("runtime bytes changed")
	}
	if enabled, known, err := e.desired.get("llamacpp"); err != nil || enabled || !known || st.running || st.proc != nil {
		t.Fatal("Off intent changed")
	}
	entries, err := os.ReadDir(st.installDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "runtime" {
		t.Fatal("repeat install created model or runtime stages")
	}
}

func TestLlamaB580ChildEnvironment(t *testing.T) {
	if os.Getenv("PAIR_B580_ENV_CHILD") == "1" {
		for _, key := range []string{"GGML_VK_DISABLE_COOPMAT", "GGML_VK_DISABLE_COOPMAT2", "GGML_VK_DISABLE_INTEGER_DOT_PRODUCT", "GGML_VK_DISABLE_F16", "GGML_VK_DISABLE_BFLOAT16", "LLAMA_ARG_FLASH_ATTN"} {
			fmt.Print(os.Getenv(key), "/")
		}
		os.Exit(0)
	}
	clearB580Environment(t)
	t.Setenv("GGML_VK_DISABLE_F16", "0")
	key := "GGML_VK_DISABLE_F16"
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	env := map[string]string{key: "1", "PAIR_B580_ENV_CHILD": "1"}
	if err := applyLlamaB580Defaults(nil, env); err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestLlamaB580ChildEnvironment$"}
	var output strings.Builder
	proc, err := startManagedProc(os.Args[0], args, env, func(_, line string) { output.WriteString(line) })
	if err != nil {
		t.Fatal(err)
	}
	<-proc.done
	if got := output.String(); got != "1/1/1/1/1/off/" {
		t.Fatalf("actual child environment = %q", got)
	}
	if os.Getenv("GGML_VK_DISABLE_F16") != "0" {
		t.Fatal("child options changed the parent")
	}
}
