// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const llamaTestCommit = "0123456789abcdef0123456789abcdef01234567"

func TestLlamaDownloadValidatesReturnedModelData(t *testing.T) {
	root := t.TempDir()
	header := make([]byte, 24)
	copy(header, "GGUF")
	binary.LittleEndian.PutUint32(header[4:8], 3)
	binary.LittleEndian.PutUint64(header[8:16], 1)
	for _, tc := range []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"model-Q4_K_M.gguf", header, true},
		{"preset.ini", []byte("[model]\nname=fixture\n"), false},
		{"configuration.gguf", []byte("[model]\nname=fixture\n"), false},
		{"empty.gguf", nil, false},
		{"truncated.gguf", []byte("GGUF"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.WriteFile(path, tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateLlamaDownload(root, path+"\n"); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	outside := filepath.Join(t.TempDir(), "other.gguf")
	if err := os.WriteFile(outside, header, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateLlamaDownload(root, outside); err == nil {
		t.Fatal("accepted external model")
	}
}

func TestLlamaCacheImportListDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	source := filepath.Join(t.TempDir(), "tiny-Q4_K_M.gguf")
	if err := os.WriteFile(source, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := llamaImport(context.Background(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	if id != "local/tiny-Q4_K_M:Q4_K_M" {
		t.Fatal(id)
	}
	models, err := llamaCacheModels(root)
	if err != nil || len(models) != 1 || models[0].ID != id {
		t.Fatalf("models=%v err=%v", models, err)
	}
	if _, err := llamaImport(context.Background(), root, source); err == nil {
		t.Fatal("duplicate overwrote model")
	}
	e := &Executor{}
	st := &engineState{installDir: filepath.Dir(root)}
	params, _ := json.Marshal(map[string]string{"model": id})
	if _, err := e.llamaModelAction(context.Background(), st, "delete_model", params); err != nil {
		t.Fatal(err)
	}
	models, err = llamaCacheModels(root)
	if err != nil || len(models) != 0 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source file modified", err)
	}
	if _, err := llamaImport(context.Background(), root, source); err != nil {
		t.Fatal("reimport after delete", err)
	}
}

func TestLlamaCacheIncompleteAndCancellation(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "models--owner--repo")
	if err := os.MkdirAll(filepath.Join(base, "refs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "refs", "main"), []byte(llamaTestCommit), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(base, "snapshots", llamaTestCommit)
	if err := os.MkdirAll(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(snapshot, name), []byte("GGUFfixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("tiny-Q4_K_M-00001-of-00002.gguf")
	write("mmproj-F16.gguf")
	write("tiny-Q8_0.gguf.downloadInProgress")
	models, err := llamaCacheModels(root)
	if err != nil || len(models) != 0 {
		t.Fatalf("incomplete listed: %v %v", models, err)
	}
	write("tiny-Q4_K_M-00002-of-00002.gguf")
	models, err = llamaCacheModels(root)
	if err != nil || len(models) != 1 || len(models[0].Files) != 2 {
		t.Fatalf("complete missing: %v %v", models, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := filepath.Join(t.TempDir(), "cancel-Q8_0.gguf")
	if err := os.WriteFile(source, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := llamaImport(ctx, root, source); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "models--local--cancel-Q8_0")); !os.IsNotExist(err) {
		t.Fatal("canceled import published")
	}
	outside := filepath.Join(t.TempDir(), "other.gguf")
	if err := os.WriteFile(outside, []byte("GGUF"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := llamaOwnedFile(root, outside); err == nil {
		t.Fatal("outside path admitted")
	}
}

func TestLlamaCacheSharedBlobDeletion(t *testing.T) {
	install := t.TempDir()
	root := filepath.Join(install, "models")
	base := filepath.Join(root, "models--owner--repo")
	snapshot := filepath.Join(base, "snapshots", llamaTestCommit)
	for _, dir := range []string{snapshot, filepath.Join(base, "refs"), filepath.Join(base, "blobs")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "refs", "main"), []byte(llamaTestCommit), 0600); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(base, "blobs", "content")
	if err := os.WriteFile(blob, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tiny-Q4_K_M.gguf", "tiny-Q8_0.gguf"} {
		if err := os.Symlink(blob, filepath.Join(snapshot, name)); err != nil {
			t.Skipf("symlink fixture unavailable: %v", err)
		}
	}
	e := &Executor{}
	st := &engineState{installDir: install}
	deleteModel := func(id string) {
		t.Helper()
		params, _ := json.Marshal(map[string]string{"model": id})
		if _, err := e.llamaModelAction(context.Background(), st, "delete_model", params); err != nil {
			t.Fatal(err)
		}
	}
	deleteModel("owner/repo:Q4_K_M")
	if _, err := os.Stat(blob); err != nil {
		t.Fatal("shared blob removed", err)
	}
	deleteModel("owner/repo:Q8_0")
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatal("orphaned blob retained", err)
	}
}

// installedLlamaFakeRuntime is an installed managed llama.cpp whose runtime binary is
// the fake engine (which implements the vendor's `download` shape), on a free
// port, with an isolated model library.
func installedLlamaFakeRuntime(t *testing.T) (*Executor, *engineState) {
	t.Helper()
	reg := NewRegistry()
	mf, _ := buildRegistry("").Get("llamacpp")
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := mf.Platforms[key]
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	p.Runtime.Port = port
	mf.Platforms[key] = p
	reg.engines["llamacpp"] = mf
	e := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(st.installDir, "runtime", llamaExecutable())
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, fakeEngineBin, bin)
	return e, st
}

func llamaFixtureOptions(t *testing.T, st *engineState, options map[string]any) {
	t.Helper()
	data, _ := json.Marshal(options)
	if err := os.WriteFile(filepath.Join(st.installDir, "runtime", ".llama-fixture.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func llamaDownloaded(t *testing.T, e *Executor, st *engineState) []string {
	t.Helper()
	raw, err := e.Action(context.Background(), "llamacpp", "list_downloaded", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := extractStringsResult(raw, st.manifest.Actions["list_downloaded"].Result)
	if !ok {
		t.Fatalf("invalid inventory %s", raw)
	}
	return ids
}

// pull_model runs the vendor's own `llama download` under the managed cache and
// publishes only a validated GGUF: a preset the vendor may return instead of
// weights is refused and never listed.
func TestLlamaPullUsesVendorDownloadAndValidatesResult(t *testing.T) {
	e, st := installedLlamaFakeRuntime(t)
	ctx := context.Background()
	params, _ := json.Marshal(map[string]string{"model": "owner/repo", "file": "fixture-Q4_0.gguf"})
	if _, err := e.Action(ctx, "llamacpp", "pull_model", params); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if ids := llamaDownloaded(t, e, st); !slices.Equal(ids, []string{"owner/repo:Q4_0"}) {
		t.Fatalf("downloaded = %v, want [owner/repo:Q4_0]", ids)
	}
	llamaFixtureOptions(t, st, map[string]any{"download_preset": true})
	params, _ = json.Marshal(map[string]string{"model": "owner/preset"})
	if _, err := e.Action(ctx, "llamacpp", "pull_model", params); err == nil || !strings.Contains(err.Error(), "GGUF") {
		t.Fatalf("a preset download was accepted: %v", err)
	}
	if ids := llamaDownloaded(t, e, st); !slices.Equal(ids, []string{"owner/repo:Q4_0"}) {
		t.Fatalf("downloaded after refused preset = %v", ids)
	}
}

// cancel_pull terminates the vendor download and nothing partial is published.
func TestLlamaCancelPullStopsTheVendorDownload(t *testing.T) {
	e, st := installedLlamaFakeRuntime(t)
	llamaFixtureOptions(t, st, map[string]any{"download_delay_ms": 20000})
	ctx := context.Background()
	params, _ := json.Marshal(map[string]string{"model": "owner/slow"})
	done := make(chan error, 1)
	go func() {
		_, err := e.Action(ctx, "llamacpp", "pull_model", params)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st.mu.Lock()
		active := st.pullCancel != nil
		st.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download never registered for cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := e.Action(ctx, "llamacpp", "cancel_pull", params); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled pull returned %v, want context canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop the download")
	}
	if ids := llamaDownloaded(t, e, st); len(ids) != 0 {
		t.Fatalf("cancelled download published %v", ids)
	}
}

// Uninstall removes only the runtime slots, keeps the model library and saves
// Off; while another process serves the engine's port it refuses instead.
func TestLlamaUninstallIsRuntimeOnlyAndRefusesExternalOwner(t *testing.T) {
	e, st := installedLlamaFakeRuntime(t)
	model := filepath.Join(llamaModelDir(st), "models--o--r", "snapshots", llamaTestCommit, "m-Q4_0.gguf")
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("GGUF"), 0600); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	external := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "llama.cpp")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	external.Listener.Close()
	external.Listener = listener
	external.Start()
	ctx := context.Background()
	if err := e.Uninstall(ctx, "llamacpp"); err == nil {
		external.Close()
		t.Fatal("uninstall proceeded while an external process served the engine port")
	}
	if _, err := os.Stat(filepath.Join(st.installDir, "runtime")); err != nil {
		external.Close()
		t.Fatalf("refused uninstall touched the runtime: %v", err)
	}
	external.Close()
	if err := e.Uninstall(ctx, "llamacpp"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.installDir, "runtime")); !os.IsNotExist(err) {
		t.Fatalf("runtime slot survived uninstall: %v", err)
	}
	if _, err := os.Stat(model); err != nil {
		t.Fatalf("uninstall touched the model library: %v", err)
	}
	if enabled, known, err := e.desired.get("llamacpp"); err != nil || !known || enabled {
		t.Fatalf("uninstall did not save Off: enabled=%v known=%v err=%v", enabled, known, err)
	}
}
