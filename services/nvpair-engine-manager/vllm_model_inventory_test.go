// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func retainedModelReceipt(t *testing.T, dir string) vllmModel {
	t.Helper()
	var model vllmModel
	if err := readVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), vllmModelReceiptMaxBytes, &model); err != nil {
		t.Fatal(err)
	}
	return model
}

func writeRawRetainedModelReceipt(t *testing.T, dir string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pair-model.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireRetainedInventoryRefusal(t *testing.T, st *engineState) {
	t.Helper()
	if models, err := retainedVLLMModels(context.Background(), st); err == nil {
		t.Fatalf("retained inventory accepted %v", models)
	}
}

func TestRetainedVLLMModelsListsReceiptsWithoutHashingModelBytes(t *testing.T) {
	f := vllmResourceFixture(t)
	ids := []string{
		"owner/alpha@" + strings.Repeat("a", 40),
		"owner/zeta@" + strings.Repeat("b", 40),
	}
	dirs := make([]string, 0, len(ids))
	for _, id := range ids {
		dirs = append(dirs, writeRetainedVLLMModel(t, f.st, id))
	}
	for _, name := range []string{".hf-home", ".hf-xet", "huggingface"} {
		if err := os.Mkdir(filepath.Join(f.st.modelDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Listing validates receipt/file metadata, not payload hashes. Selection is
	// the effect boundary that rejects these changed bytes.
	config := filepath.Join(dirs[0], "config.json")
	original, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, bytes.Repeat([]byte("x"), len(original)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := retainedVLLMModels(context.Background(), f.st); err != nil || !reflect.DeepEqual(got, ids) {
		t.Fatalf("retained inventory = %v, %v; want %v", got, err, ids)
	}

	t.Run("selected link names an accepted receipt", func(t *testing.T) {
		selected := filepath.Join(f.st.modelDir, "selected")
		if err := os.Symlink(filepath.Base(dirs[1]), selected); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		if got, err := retainedVLLMModels(context.Background(), f.st); err != nil || !reflect.DeepEqual(got, ids) {
			t.Fatalf("inventory with selection = %v, %v; want %v", got, err, ids)
		}
	})
}

func TestRetainedVLLMModelsIgnoresUnownedDigestDirectoriesBesideLiveSidecars(t *testing.T) {
	f := vllmResourceFixture(t)
	id := "owner/model@" + strings.Repeat("9", 40)
	dir := writeRetainedVLLMModel(t, f.st, id)
	for _, name := range []string{".hf-home", ".hf-xet", "huggingface", strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if err := os.Mkdir(filepath.Join(f.st.modelDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.st.modelDir, strings.Repeat("a", 64), "cache-data"), []byte("unowned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := retainedVLLMModels(context.Background(), f.st); err != nil || !reflect.DeepEqual(got, []string{id}) {
		t.Fatalf("live-shaped retained inventory = %v, %v; want only %v", got, err, id)
	}

	t.Run("selected receipt remains accepted", func(t *testing.T) {
		selected := filepath.Join(f.st.modelDir, "selected")
		if err := os.Symlink(filepath.Base(dir), selected); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		if got, err := retainedVLLMModels(context.Background(), f.st); err != nil || !reflect.DeepEqual(got, []string{id}) {
			t.Fatalf("live-shaped selected inventory = %v, %v; want only %v", got, err, id)
		}
	})
}

func TestVLLMStateUsesPersistentSiblingCatalogForInventoryAndRuntime(t *testing.T) {
	vendorRoot := filepath.Join(t.TempDir(), "Nvidia Corporation")
	installBase := filepath.Join(vendorRoot, "Personal AI Router", "engine-bin")
	manifest := &Manifest{
		Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1,
		Platforms: map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: {
			Runtime: Runtime{Driver: "vllm-python", Args: []string{"serve", "{models_dir}/selected"}},
		}},
	}
	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	executor := NewExecutor(reg, NewReporter(nil), nil, installBase)
	st, err := executor.state("vllm")
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(vendorRoot, "Personal AI Router Models", "vllm")
	if st.modelDir != wantRoot || st.installDir != filepath.Join(installBase, "vllm") {
		t.Fatalf("vLLM state paths = install %q models %q, want install under engine-bin and models %q", st.installDir, st.modelDir, wantRoot)
	}
	id := "owner/model@" + strings.Repeat("7", 40)
	writeRetainedVLLMModel(t, st, id)
	for _, name := range []string{".hf-home", ".hf-xet", strings.Repeat("c", 64)} {
		if err := os.Mkdir(filepath.Join(st.modelDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := retainedVLLMModels(context.Background(), st); err != nil || !reflect.DeepEqual(got, []string{id}) {
		t.Fatalf("persistent sibling catalog = %v, %v; want %v", got, err, id)
	}
	wantHFHome := "HF_HOME=" + filepath.Join(wantRoot, "huggingface")
	foundHFHome := false
	for _, value := range managedVLLMRuntimeEnv(st, filepath.Join(st.installDir, "runtime")) {
		foundHFHome = foundHFHome || value == wantHFHome
	}
	resolved, err := resolveArgs(st.plat.Runtime.Args, map[string]string{"models_dir": st.modelDir})
	if err != nil || len(resolved) != 2 || filepath.Clean(resolved[1]) != filepath.Join(wantRoot, "selected") || !foundHFHome {
		t.Fatalf("vLLM runtime paths = argv %v HF_HOME found=%v err=%v", resolved, foundHFHome, err)
	}
}

func TestRetainedVLLMModelsRefusesUnownedOrMalformedInventory(t *testing.T) {
	t.Run("missing root is an empty library", func(t *testing.T) {
		f := vllmResourceFixture(t)
		if got, err := retainedVLLMModels(context.Background(), f.st); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("missing root inventory = %v, %v; want non-nil empty", got, err)
		}
	})

	t.Run("unknown root entry", func(t *testing.T) {
		f := vllmResourceFixture(t)
		if err := os.MkdirAll(f.st.modelDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.st.modelDir, "unknown"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("malformed receipt", func(t *testing.T) {
		f := vllmResourceFixture(t)
		dir := filepath.Join(f.st.modelDir, strings.Repeat("a", 64))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pair-model.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("unknown receipt field", func(t *testing.T) {
		f := vllmResourceFixture(t)
		id := "owner/model@" + strings.Repeat("c", 40)
		dir := writeRetainedVLLMModel(t, f.st, id)
		model := retainedModelReceipt(t, dir)
		row := map[string]any{}
		raw, _ := json.Marshal(model)
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		row["unknown"] = true
		writeRawRetainedModelReceipt(t, dir, row)
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("digest directory mismatch", func(t *testing.T) {
		f := vllmResourceFixture(t)
		id := "owner/model@" + strings.Repeat("d", 40)
		dir := writeRetainedVLLMModel(t, f.st, id)
		wrong := filepath.Join(f.st.modelDir, strings.Repeat("f", 64))
		if err := os.Rename(dir, wrong); err != nil {
			t.Fatal(err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("invalid exact id", func(t *testing.T) {
		f := vllmResourceFixture(t)
		id := "owner/model@" + strings.Repeat("e", 40)
		dir := writeRetainedVLLMModel(t, f.st, id)
		model := retainedModelReceipt(t, dir)
		model.ID = "not-an-exact-id"
		writeRawRetainedModelReceipt(t, dir, model)
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("file count overflow", func(t *testing.T) {
		f := vllmResourceFixture(t)
		id := "owner/model@" + strings.Repeat("1", 40)
		dir := writeRetainedVLLMModel(t, f.st, id)
		model := retainedModelReceipt(t, dir)
		model.Files = make([]vllmModelFile, vllmModelReceiptMaxFiles+1)
		writeRawRetainedModelReceipt(t, dir, model)
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("receipt byte overflow", func(t *testing.T) {
		f := vllmResourceFixture(t)
		id := "owner/model@" + strings.Repeat("8", 40)
		dir := writeRetainedVLLMModel(t, f.st, id)
		if err := os.WriteFile(filepath.Join(dir, "pair-model.json"), bytes.Repeat([]byte(" "), vllmModelReceiptMaxBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("model count overflow", func(t *testing.T) {
		f := vllmResourceFixture(t)
		if err := os.MkdirAll(f.st.modelDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= vllmRetainedModelLimit; i++ {
			if err := os.Mkdir(filepath.Join(f.st.modelDir, fmt.Sprintf("%064x", i)), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("selected is not a symlink", func(t *testing.T) {
		f := vllmResourceFixture(t)
		writeRetainedVLLMModel(t, f.st, "owner/model@"+strings.Repeat("2", 40))
		if err := os.WriteFile(filepath.Join(f.st.modelDir, "selected"), []byte("bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("selected redirects outside the library", func(t *testing.T) {
		f := vllmResourceFixture(t)
		writeRetainedVLLMModel(t, f.st, "owner/model@"+strings.Repeat("7", 40))
		if err := os.Symlink(t.TempDir(), filepath.Join(f.st.modelDir, "selected")); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("selected points at an unowned digest directory", func(t *testing.T) {
		f := vllmResourceFixture(t)
		unowned := strings.Repeat("6", 64)
		if err := os.MkdirAll(filepath.Join(f.st.modelDir, unowned), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(unowned, filepath.Join(f.st.modelDir, "selected")); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("redirected auxiliary directory", func(t *testing.T) {
		f := vllmResourceFixture(t)
		if err := os.MkdirAll(f.st.modelDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(f.st.modelDir, ".hf-home")); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("redirected root", func(t *testing.T) {
		f := vllmResourceFixture(t)
		realRoot := f.st.modelDir
		writeRetainedVLLMModel(t, f.st, "owner/model@"+strings.Repeat("3", 40))
		link := filepath.Join(filepath.Dir(realRoot), "redirected-vllm")
		if err := os.Symlink(realRoot, link); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		f.st.modelDir = link
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("redirected model directory", func(t *testing.T) {
		f := vllmResourceFixture(t)
		if err := os.MkdirAll(f.st.modelDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(f.st.modelDir, strings.Repeat("4", 64))); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})

	t.Run("redirected retained file", func(t *testing.T) {
		f := vllmResourceFixture(t)
		id := "owner/model@" + strings.Repeat("5", 40)
		dir := writeRetainedVLLMModel(t, f.st, id)
		config := filepath.Join(dir, "config.json")
		data, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, config); err != nil {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		requireRetainedInventoryRefusal(t, f.st)
	})
}

func TestModelsResultSeparatesStoppedRetainedVLLMCatalogFromServedModels(t *testing.T) {
	f := vllmResourceFixture(t)
	id := "owner/model@" + strings.Repeat("6", 40)
	writeRetainedVLLMModel(t, f.st, id)

	result := f.e.ModelsResult(context.Background())
	if len(result.Models) != 0 || result.ByEngine != nil || result.LoadedByEngine != nil {
		t.Fatalf("stopped retained model became served or loaded: %+v", result)
	}
	if want := map[string][]string{"vllm": {id}}; !reflect.DeepEqual(result.RetainedByEngine, want) {
		t.Fatalf("ModelsResult.RetainedByEngine = %v, want %v", result.RetainedByEngine, want)
	}
}

func TestModelsResultRetainedVLLMDoesNotQueueBehindLifecycle(t *testing.T) {
	f := vllmResourceFixture(t)
	id := "owner/model@" + strings.Repeat("8", 40)
	writeRetainedVLLMModel(t, f.st, id)

	for _, running := range []bool{false, true} {
		f.st.mu.Lock()
		f.st.running, f.st.healthy = running, running
		f.st.mu.Unlock()
		f.st.opMu.Lock()
		result := make(chan ModelsResult, 1)
		go func() { result <- f.e.ModelsResult(context.Background()) }()
		select {
		case got := <-result:
			f.st.opMu.Unlock()
			if want := map[string][]string{"vllm": {id}}; !reflect.DeepEqual(got.RetainedByEngine, want) {
				t.Fatalf("running=%t retained catalog = %v, want %v", running, got.RetainedByEngine, want)
			}
			if len(got.Models) != 0 || got.ByEngine != nil || got.LoadedByEngine != nil {
				t.Fatalf("running=%t made an unverified served-model claim: %+v", running, got)
			}
		case <-time.After(time.Second):
			f.st.opMu.Unlock()
			t.Fatalf("running=%t model listing queued behind vLLM lifecycle lock", running)
		}
	}
}

// A served model must survive routine status traffic: a reader that loses the
// lifecycle lock to another status refresh takes that refresh's result rather
// than withdrawing the model from routing until the next scanner sweep.
func TestVLLMStatusReadSharesAConcurrentStatusRefresh(t *testing.T) {
	type read struct {
		status EngineStatus
		fresh  bool
	}
	serving := EngineStatus{Engine: "vllm", Installed: true, Running: true, Healthy: true, Routable: true, Port: 8001}
	for _, tc := range []struct {
		name      string
		completed bool
		wantFresh bool
	}{
		{name: "completed refresh", completed: true, wantFresh: true},
		{name: "cancelled refresh", completed: false, wantFresh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vllmResourceFixture(t)
			f.st.opMu.Lock()
			defer f.st.opMu.Unlock()
			refresh := &vllmStatusRefresh{done: make(chan struct{})}
			f.st.mu.Lock()
			f.st.statusRefresh = refresh
			f.st.mu.Unlock()
			result := make(chan read, 1)
			go func() {
				status, fresh := f.e.vllmStatusForRead(context.Background(), f.st)
				result <- read{status, fresh}
			}()
			select {
			case got := <-result:
				t.Fatalf("status read returned %+v before the in-flight refresh finished", got)
			case <-time.After(50 * time.Millisecond):
			}
			refresh.status, refresh.ok = serving, tc.completed
			close(refresh.done)
			got := <-result
			if got.fresh != tc.wantFresh || tc.wantFresh && !reflect.DeepEqual(got.status, serving) {
				t.Fatalf("shared status = %+v fresh=%t, want fresh=%t", got.status, got.fresh, tc.wantFresh)
			}
		})
	}

	t.Run("lifecycle operation", func(t *testing.T) {
		f := vllmResourceFixture(t)
		f.st.opMu.Lock()
		defer f.st.opMu.Unlock()
		result := make(chan read, 1)
		go func() {
			status, fresh := f.e.vllmStatusForRead(context.Background(), f.st)
			result <- read{status, fresh}
		}()
		select {
		case got := <-result:
			if got.fresh {
				t.Fatalf("a lifecycle-held lock produced a fresh status: %+v", got.status)
			}
		case <-time.After(time.Second):
			t.Fatal("status read queued behind a lifecycle operation")
		}
	})
}

func TestModelsResultReportsAuthoritativeEmptyRetainedVLLMCatalog(t *testing.T) {
	f := vllmResourceFixture(t)
	result := f.e.ModelsResult(context.Background())
	want := map[string][]string{"vllm": {}}
	if !reflect.DeepEqual(result.RetainedByEngine, want) || len(result.Models) != 0 || result.ByEngine != nil {
		t.Fatalf("empty retained catalog result = %+v, want retained vllm:[] only", result)
	}
}

func TestAssembleModelsResultKeepsRetainedCatalogOutOfRoutingTruth(t *testing.T) {
	result := assembleModelsResult(
		[]string{"vllm"},
		[][]string{{"model-b"}}, []bool{true},
		[][]string{{"model-b"}}, []bool{true},
		[][]string{{"model-a", "model-b"}}, []bool{true},
	)
	if want := []string{"model-b"}; !reflect.DeepEqual(result.Models, want) {
		t.Fatalf("served Models = %v, want %v", result.Models, want)
	}
	if want := map[string][]string{"vllm": {"model-b"}}; !reflect.DeepEqual(result.ByEngine, want) || !reflect.DeepEqual(result.LoadedByEngine, want) {
		t.Fatalf("served/loaded maps = %v / %v, want %v", result.ByEngine, result.LoadedByEngine, want)
	}
	if want := map[string][]string{"vllm": {"model-a", "model-b"}}; !reflect.DeepEqual(result.RetainedByEngine, want) {
		t.Fatalf("retained catalog = %v, want %v", result.RetainedByEngine, want)
	}

	// Either source can fail without promoting or erasing the independent one.
	liveFailed := assembleModelsResult(
		[]string{"vllm"}, [][]string{nil}, []bool{false}, [][]string{nil}, []bool{false},
		[][]string{{"model-a"}}, []bool{true},
	)
	if len(liveFailed.Models) != 0 || liveFailed.ByEngine != nil || !reflect.DeepEqual(liveFailed.RetainedByEngine, map[string][]string{"vllm": {"model-a"}}) {
		t.Fatalf("live failure crossed inventory boundaries: %+v", liveFailed)
	}
	retainedFailed := assembleModelsResult(
		[]string{"vllm"}, [][]string{{"model-b"}}, []bool{true}, [][]string{nil}, []bool{false},
		[][]string{nil}, []bool{false},
	)
	if !reflect.DeepEqual(retainedFailed.Models, []string{"model-b"}) || !reflect.DeepEqual(retainedFailed.ByEngine, map[string][]string{"vllm": {"model-b"}}) || retainedFailed.RetainedByEngine != nil {
		t.Fatalf("retained failure crossed inventory boundaries: %+v", retainedFailed)
	}
}
