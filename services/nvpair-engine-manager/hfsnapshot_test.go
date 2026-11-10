// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHub is a stand-in for the Hugging Face hub: one repository's metadata
// and its files, served with Range support.
type fakeHub struct {
	t     *testing.T
	repo  string
	files map[string][]byte
	// revision, when set, is the only commit served: metadata at
	// /api/models/<repo>/revision/<sha> and files at /<repo>/resolve/<sha>/.
	revision string
	// modelType, when set, is the config.json model_type the metadata reports.
	modelType string
	// meta overrides the metadata reply; empty means an open repo listing files.
	meta   string
	status int // metadata status; zero means 200

	mu         sync.Mutex
	ranges     []string
	fileHits   atomic.Int32
	failFirst  map[string]int // file → how many requests answer 503 first
	blockAfter map[string]int // file → bytes sent before the response stalls
	stalled    chan struct{}
}

func newFakeHub(t *testing.T, files map[string][]byte) *fakeHub {
	t.Helper()
	return &fakeHub{t: t, repo: "o/r", files: files, failFirst: map[string]int{}, blockAfter: map[string]int{}, stalled: make(chan struct{}, 1)}
}

func (h *fakeHub) metadata() string {
	if h.meta != "" {
		return h.meta
	}
	type sibling struct {
		RFilename string `json:"rfilename"`
		Size      int    `json:"size"`
	}
	siblings := []sibling{{RFilename: ".gitattributes", Size: 10}}
	for name, data := range h.files {
		siblings = append(siblings, sibling{RFilename: name, Size: len(data)})
	}
	meta := map[string]any{"private": false, "gated": false, "siblings": siblings}
	if h.modelType != "" {
		meta["config"] = map[string]string{"model_type": h.modelType}
	}
	out, _ := json.Marshal(meta)
	return string(out)
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metaPath, ref := "/api/models/"+h.repo, "main"
	if h.revision != "" {
		metaPath, ref = metaPath+"/revision/"+h.revision, h.revision
	}
	if r.URL.Path == metaPath {
		if r.URL.Query().Get("blobs") != "true" {
			h.t.Errorf("metadata requested without blobs=true: %s", r.URL)
		}
		if h.status != 0 {
			w.WriteHeader(h.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, h.metadata())
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/"+h.repo+"/resolve/"+ref+"/")
	data, found := h.files[name]
	if !ok || !found {
		http.NotFound(w, r)
		return
	}
	h.fileHits.Add(1)
	h.mu.Lock()
	h.ranges = append(h.ranges, r.Header.Get("Range"))
	fail := h.failFirst[name]
	if fail > 0 {
		h.failFirst[name] = fail - 1
	}
	block, blocking := h.blockAfter[name]
	h.mu.Unlock()
	if fail > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if blocking {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:block])
		w.(http.Flusher).Flush()
		select {
		case h.stalled <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// snapshotEngine is the Colibri-shaped fake engine with an hf_snapshot pull,
// pointed at a fake hub, recording every pull-progress frame.
func snapshotEngine(t *testing.T, hub *fakeHub) (*Executor, string, func() []map[string]any) {
	t.Helper()
	e, store, _ := modelEngine(t)
	st, _ := e.state("fake")
	st.manifest.Actions[pullModelAction] = Action{HFSnapshot: &ActionHFSnapshot{Dest: "{models_dir}/{model}"}}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	e.hfBaseURL = srv.URL
	var mu sync.Mutex
	var frames []map[string]any
	e.emit = func(method string, params any) {
		if method != "engine:pull-progress" {
			return
		}
		mp, _ := params.(map[string]any)
		mu.Lock()
		frames = append(frames, mp)
		mu.Unlock()
	}
	return e, store, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), frames...)
	}
}

func colibriFiles() map[string][]byte {
	return map[string][]byte{
		"config.json":             []byte(`{"model_type":"qwen3_moe"}`),
		"model-00000.safetensors": bytes.Repeat([]byte("w"), 64*1024),
		"tokenizer/vocab.json":    []byte(`{"a":1}`),
	}
}

func TestHFSnapshotDownloadsIntoStore(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, store, frames := snapshotEngine(t, hub)

	raw, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Model string `json:"model"`
		Files int    `json:"files"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Model != "o/r" || res.Files != 3 {
		t.Fatalf("result %s (err %v)", raw, err)
	}
	for name, want := range hub.files {
		got, err := os.ReadFile(filepath.Join(store, "o", "r", filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v, content match %t", name, err, bytes.Equal(got, want))
		}
	}
	if _, err := os.Stat(filepath.Join(store, "o", "r", ".gitattributes")); !os.IsNotExist(err) {
		t.Errorf(".gitattributes was downloaded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "o", "r"+partialSuffix)); !os.IsNotExist(err) {
		t.Errorf("the partial directory was left behind: %v", err)
	}
	// The first model downloaded becomes the launch model.
	if st, _ := e.state("fake"); st.plat.Runtime.Model != "o/r" {
		t.Errorf("launch model = %q, want the first download", st.plat.Runtime.Model)
	}

	got := frames()
	if len(got) < 3 {
		t.Fatalf("frames = %v", got)
	}
	if got[0]["stage"] != "pulling" || got[len(got)-1]["stage"] != "success" || got[len(got)-1]["percent"] != 100 {
		t.Fatalf("frames run %v … %v, want pulling … success 100", got[0], got[len(got)-1])
	}
	last := -1
	for _, f := range got[1 : len(got)-1] {
		pct, _ := f["percent"].(int)
		if f["stage"] != "downloading" || pct <= last || pct > 99 {
			t.Fatalf("progress frames are not one increasing frame per percent: %v", got)
		}
		last = pct
	}

	// A second pull of a complete model touches nothing.
	hits := hub.fileHits.Load()
	raw, err = e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err != nil || !strings.Contains(string(raw), `"already":true`) || hub.fileHits.Load() != hits {
		t.Fatalf("repeat pull = %s, %v; downloads %d → %d", raw, err, hits, hub.fileHits.Load())
	}
}

func TestHFSnapshotResumesPartialFile(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, store, _ := snapshotEngine(t, hub)
	weights := hub.files["model-00000.safetensors"]
	half := len(weights) / 2
	partial := filepath.Join(store, "o", "r"+partialSuffix)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "model-00000.safetensors.part"), weights[:half], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(store, "o", "r", "model-00000.safetensors"))
	if err != nil || !bytes.Equal(got, weights) {
		t.Fatalf("resumed file differs (err %v)", err)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if !slicesContains(hub.ranges, fmt.Sprintf("bytes=%d-", half)) {
		t.Fatalf("no ranged request resumed at %d: %v", half, hub.ranges)
	}
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestHFSnapshotRetriesTransientFailure(t *testing.T) {
	defer func(backoff time.Duration) { hfSnapshotRetryBackoff = backoff }(hfSnapshotRetryBackoff)
	hfSnapshotRetryBackoff = 0
	hub := newFakeHub(t, colibriFiles())
	hub.failFirst["model-00000.safetensors"] = 2
	e, _, _ := snapshotEngine(t, hub)
	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err != nil {
		t.Fatalf("two 503s within the retry budget failed the pull: %v", err)
	}
}

func TestHFSnapshotRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		model  string
		setup  func(*fakeHub)
		wanted string
	}{
		{"not a repo id", "../x", nil, "is not a Hugging Face repository id"},
		{"gated", "o/r", func(h *fakeHub) {
			h.meta = `{"private":false,"gated":"manual","siblings":[{"rfilename":"config.json","size":2}]}`
		}, "private or gated"},
		{"private", "o/r", func(h *fakeHub) {
			h.meta = `{"private":true,"gated":false,"siblings":[{"rfilename":"config.json","size":2}]}`
		}, "private or gated"},
		{"missing or unauthorized", "o/r", func(h *fakeHub) { h.status = http.StatusUnauthorized }, "not found on Hugging Face"},
		{"traversal", "o/r", func(h *fakeHub) {
			h.meta = `{"private":false,"gated":false,"siblings":[{"rfilename":"config.json","size":2},{"rfilename":"../evil","size":1}]}`
		}, "not safe to write"},
		{"drive letter", "o/r", func(h *fakeHub) {
			h.meta = `{"private":false,"gated":false,"siblings":[{"rfilename":"config.json","size":2},{"rfilename":"C:/evil","size":1}]}`
		}, "not safe to write"},
		{"not a model", "o/r", func(h *fakeHub) {
			h.meta = `{"private":false,"gated":false,"siblings":[{"rfilename":"README.md","size":2}]}`
		}, "has no config.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t, colibriFiles())
			if tc.setup != nil {
				tc.setup(hub)
			}
			e, store, _ := snapshotEngine(t, hub)
			_, err := e.PullModelStream(context.Background(), "fake", tc.model, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wanted) {
				t.Fatalf("pull = %v, want an error containing %q", err, tc.wanted)
			}
			if hub.fileHits.Load() != 0 {
				t.Fatal("a refused pull downloaded files")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(store), "evil")); err == nil {
				t.Fatal("a file escaped the store")
			}
		})
	}
}

func TestHFSnapshotChecksDiskSpace(t *testing.T) {
	defer func(f func(string) (uint64, error)) { freeDiskBytes = f }(freeDiskBytes)
	freeDiskBytes = func(string) (uint64, error) { return 1 << 20, nil }
	hub := newFakeHub(t, colibriFiles())
	e, _, _ := snapshotEngine(t, hub)
	_, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
		t.Fatalf("pull on a full disk = %v", err)
	}
	if hub.fileHits.Load() != 0 {
		t.Fatal("a pull refused for disk space downloaded files")
	}
}

func TestHFSnapshotCancelKeepsPartial(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	hub.blockAfter["model-00000.safetensors"] = 1000
	e, store, _ := snapshotEngine(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.PullModelStream(ctx, "fake", "o/r", nil)
		done <- err
	}()
	select {
	case <-hub.stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the download never started")
	}
	// The hub has sent its bytes, but the client may not have written them
	// yet; cancelling now would measure that race rather than the resume.
	partPath := filepath.Join(store, "o", "r"+partialSuffix, "model-00000.safetensors.part")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if fi, err := os.Stat(partPath); err == nil && fi.Size() == 1000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client never wrote the bytes it was sent")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled pull = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pull ignored cancellation")
	}
	part, err := os.Stat(partPath)
	if err != nil || part.Size() != 1000 {
		t.Fatalf("the partial file was not kept for resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "o", "r")); !os.IsNotExist(err) {
		t.Fatal("a cancelled download was moved into the store")
	}
	if models := scanModelDirs(store, "config.json", 2); len(models) != 0 {
		t.Fatalf("a cancelled download is listed: %v", models)
	}
}

// A remote initiator gets the same progress frames over the ec stream, then
// the result.
func TestHFSnapshotStreamsToRemoteInitiator(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, _, _ := snapshotEngine(t, hub)
	s := &controlServer{exec: e}
	srv := httptest.NewServer(http.HandlerFunc(s.handlePull))
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Post(srv.URL, "application/json", strings.NewReader(`{"opId":"op","engine":"fake","model":"o/r"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var types []string
	var stages []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var frame streamFrame
		if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		types = append(types, frame.Type)
		if frame.Type == "progress" {
			stages = append(stages, frame.Stage)
		}
	}
	if len(types) == 0 || types[len(types)-1] != "result" {
		t.Fatalf("stream frames %v, want progress then result", types)
	}
	if !reflect.DeepEqual(stages[:1], []string{"pulling"}) || stages[len(stages)-1] != "success" {
		t.Fatalf("relayed stages %v", stages)
	}
}

func TestHFSnapshotOnlyOnePullPerModel(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	hub.blockAfter["model-00000.safetensors"] = 10
	e, _, _ := snapshotEngine(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = e.PullModelStream(ctx, "fake", "o/r", nil)
		close(done)
	}()
	<-hub.stalled
	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err == nil || !strings.Contains(err.Error(), "already being downloaded") {
		t.Fatalf("concurrent pull = %v", err)
	}
	cancel()
	<-done
}

func TestSafeRepoFileName(t *testing.T) {
	for _, name := range []string{"config.json", "sub/dir/file.safetensors", "a b.txt"} {
		if !safeRepoFileName(name) {
			t.Errorf("safeRepoFileName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "/abs", "a//b", "../x", "a/../b", "./x", "C:/x", `a\b`, "x.part", " lead", "a/b?", "nul\x00"} {
		if safeRepoFileName(name) {
			t.Errorf("safeRepoFileName(%q) = true", name)
		}
	}
}
