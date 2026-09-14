// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A pool that never spawns anything real: enough to exercise the bookkeeping,
// which is where the concurrency bugs live.
func testPool(t *testing.T, max int) *Pool {
	t.Helper()
	return NewPool("/nonexistent", nil, max, 19000, time.Second)
}

func (p *Pool) addFake(model string, port int, used time.Time) {
	p.mu.Lock()
	p.children[model] = &child{model: model, port: port, used: used}
	p.mu.Unlock()
}

func TestResidentIsMostRecentlyUsedFirst(t *testing.T) {
	p := testPool(t, 3)
	now := time.Now()
	p.addFake("old", 1, now.Add(-time.Hour))
	p.addFake("new", 2, now)
	p.addFake("mid", 3, now.Add(-time.Minute))
	got := p.Resident()
	want := []string{"new", "mid", "old"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Resident() = %v, want %v", got, want)
		}
	}
}

// The eviction victim must be the least recently USED, not the first loaded --
// a FIFO would evict a model that is being alternated with.
func TestEvictsLeastRecentlyUsedNotOldestLoaded(t *testing.T) {
	p := testPool(t, 2)
	now := time.Now()
	p.addFake("a", 1, now.Add(-time.Hour)) // loaded first, but touched recently below
	p.addFake("b", 2, now.Add(-time.Minute))
	p.mu.Lock()
	p.children["a"].used = now // "a" was just used
	victim := p.evictableLocked()
	p.mu.Unlock()
	if victim == nil || victim.model != "b" {
		t.Fatalf("victim = %v, want b (the least recently used)", victim)
	}
}

// A child serving a request must never be chosen for eviction: at max-models=1
// a second request for another model would otherwise kill a live generation.
func TestBusyChildIsNeverEvicted(t *testing.T) {
	p := testPool(t, 1)
	p.addFake("busy", 1, time.Now().Add(-time.Hour))
	p.mu.Lock()
	p.children["busy"].inuse = 1
	victim := p.evictableLocked()
	p.mu.Unlock()
	if victim != nil {
		t.Fatalf("victim = %q, want none: the only child is serving a request", victim.model)
	}
}

// Acquire on a resident model must not touch the slow path at all, so /health
// and other models stay responsive while something big is loading.
func TestAcquireResidentIsImmediateAndRefcounts(t *testing.T) {
	p := testPool(t, 2)
	p.addFake("hot", 4321, time.Now())

	done := make(chan struct{})
	go func() {
		defer close(done)
		port, release, err := p.Acquire(context.Background(), "hot")
		if err != nil || port != 4321 {
			t.Errorf("Acquire(hot) = %d, %v", port, err)
			return
		}
		p.mu.Lock()
		inuse := p.children["hot"].inuse
		p.mu.Unlock()
		if inuse != 1 {
			t.Errorf("inuse = %d, want 1 while the request is in flight", inuse)
		}
		release()
		release() // must be idempotent; a double release would underflow
		p.mu.Lock()
		inuse = p.children["hot"].inuse
		p.mu.Unlock()
		if inuse != 0 {
			t.Errorf("inuse = %d after release, want 0", inuse)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire on a resident model blocked; it must not take the load path")
	}
}

// The catalogue must list what mlx_lm.server would list, and nothing else: an
// embedding or BERT repo also carries a config and a tokenizer config, so the
// weight index is what separates a servable model from one that cannot load.
func TestScanHFCacheRequiresAWeightIndex(t *testing.T) {
	root := t.TempDir()
	mk := func(repo string, files ...string) {
		dir := filepath.Join(root, repo, "snapshots", "abc")
		os.MkdirAll(dir, 0o755)
		for _, f := range files {
			os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644)
		}
	}
	mk("models--mlx-community--Good", "config.json", "model.safetensors.index.json", "tokenizer_config.json")
	mk("models--org--BertLike", "config.json", "tokenizer_config.json") // no index
	mk("models--org--Empty")

	got := scanHFCache(root)
	if len(got) != 1 || got[0] != "mlx-community/Good" {
		t.Fatalf("scanHFCache = %v, want [mlx-community/Good]", got)
	}
}

// A model you quantized yourself never enters the Hugging Face cache, so a
// cache-only catalogue cannot show it — which is exactly what made a local 27B
// build invisible in the UI while it was the model actually being served.
func TestScanModelsDirFindsLocalBuilds(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, files ...string) {
		d := filepath.Join(root, name)
		os.MkdirAll(d, 0o755)
		for _, f := range files {
			os.WriteFile(filepath.Join(d, f), []byte("{}"), 0o644)
		}
	}
	// A local quantization: single shard, so no weight index. Requiring one
	// here (as the cache scan does) would hide it.
	mk("Qwen3.8-27B-3bit", "config.json", "tokenizer_config.json", "model.safetensors")
	mk("TokenizerJsonOnly", "config.json", "tokenizer.json")
	mk("NotAModel", "README.md")
	mk("ConfigButNoTokenizer", "config.json")
	os.WriteFile(filepath.Join(root, "loose-file.bin"), []byte("x"), 0o644)

	got := scanModelsDir(root)
	want := map[string]bool{
		filepath.Join(root, "Qwen3.8-27B-3bit"):  true,
		filepath.Join(root, "TokenizerJsonOnly"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("scanModelsDir = %v, want %d entries", got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %q", g)
		}
	}
	// Advertised by absolute path, because that is what a request must name.
	for _, g := range got {
		if !filepath.IsAbs(g) {
			t.Errorf("%q is not an absolute path", g)
		}
	}
}

// A path the user registered by hand must be offered exactly when it is still
// servable: MLX has no catalogue, so this list is the only way a model outside
// the cache and outside a scanned directory can be named at all.
func TestReadRegisteredSkipsEntriesThatAreNoLongerModels(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, files ...string) string {
		d := filepath.Join(root, name)
		os.MkdirAll(d, 0o755)
		for _, f := range files {
			os.WriteFile(filepath.Join(d, f), []byte("{}"), 0o644)
		}
		return d
	}
	good := mk("Good", "config.json", "tokenizer_config.json")
	noTok := mk("NoTokenizer", "config.json")
	gone := filepath.Join(root, "Deleted")

	reg := filepath.Join(root, "registered-models.txt")
	os.WriteFile(reg, []byte(
		"# a comment\n\n"+good+"\n"+noTok+"\n"+gone+"\n  "+good+"  \n"), 0o644)

	got := readRegistered(reg)
	// good appears twice in the file (once padded with whitespace); the
	// catalogue dedupes, so both are returned here and collapse in handleModels.
	for _, g := range got {
		if g != good {
			t.Errorf("offered %q, which is not a servable model directory", g)
		}
	}
	if len(got) == 0 {
		t.Fatalf("readRegistered dropped a valid entry")
	}
	// A missing file is not an error: the engine must still list everything else.
	if r := readRegistered(filepath.Join(root, "nope.txt")); r != nil {
		t.Errorf("missing registry file returned %v, want nil", r)
	}
}

// Unload is the Eject the UI offers. mlx-lm has no unload API, so the pool's
// answer is to end the child holding the model -- which must actually free it,
// and must not cut off a request that is mid-flight.
func TestUnloadReleasesAModelButNotOneInUse(t *testing.T) {
	p := NewPool("/nonexistent", nil, 2, 8300, time.Second)
	busy := &child{model: "busy", port: 8301, used: time.Now(), inuse: 1}
	idle := &child{model: "idle", port: 8302, used: time.Now()}
	p.children["busy"] = busy
	p.children["idle"] = idle

	if err := p.Unload("busy"); err == nil {
		t.Error("unloaded a model with a request in flight; that truncates the response")
	}
	if _, still := p.children["busy"]; !still {
		t.Error("an in-use model was dropped from the pool anyway")
	}

	if err := p.Unload("idle"); err != nil {
		t.Errorf("Unload(idle) = %v, want nil", err)
	}
	if _, still := p.children["idle"]; still {
		t.Error("an unloaded model is still resident; its weights were not freed")
	}

	// Asking for memory back that is already back is what the caller wanted.
	if err := p.Unload("never-loaded"); err != nil {
		t.Errorf("Unload of a model that is not resident = %v, want nil", err)
	}
}
