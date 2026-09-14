// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End to end over real HTTP: one node's cache is served, another node pulls it,
// and the result must be a cache entry in its own right. The pin gate and TLS
// are the ec surface's, exercised by its own tests; what is new here is the
// manifest/blob protocol and the writing.
func TestPullModelFromPeerCopiesACacheEntry(t *testing.T) {
	srcHub, dstHub := t.TempDir(), t.TempDir()
	const repo, rev = "mlx-community/Fake-1B", "deadbeef"
	files := map[string][]byte{
		"config.json":                  []byte(`{"model_type":"fake"}`),
		"tokenizer_config.json":        []byte(`{"tok":true}`),
		"model.safetensors":            bytes.Repeat([]byte("W"), 8192),
		"model.safetensors.index.json": []byte(`{"weight_map":{}}`),
	}
	buildFakeRepo(t, srcHub, repo, rev, files)

	// The server reads whatever hubRoot() resolves to, so point it at the source.
	s := &controlServer{hub: srcHub}
	mux := http.NewServeMux()
	mux.HandleFunc(controlCacheManifestPath, s.handleCacheManifest)
	mux.HandleFunc(controlCacheBlobPath, s.handleCacheBlob)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &remoteClient{http: srv.Client(), base: srv.URL, forget: func() {}}

	var stages []string
	// The pull writes into the DESTINATION cache. Both "nodes" are this one
	// process, which is exactly why the cache is a field and not an env lookup.
	err := (&Executor{hub: dstHub}).PullModelFromPeer(context.Background(), client, repo,
		func(stage string, _ int, _ string) { stages = append(stages, stage) })
	if err != nil {
		t.Fatalf("PullModelFromPeer: %v", err)
	}

	if len(stages) == 0 || stages[0] != "starting" || stages[len(stages)-1] != "success" {
		t.Errorf("progress stages = %v, want starting…success", stages)
	}
	// The destination must be readable as a cache entry, which is what makes it
	// visible to scan_cache_dir and therefore to mlx-lm's /v1/models.
	got, err := readCacheManifest(dstHub, repo)
	if err != nil {
		t.Fatalf("copy is not a valid cache entry: %v", err)
	}
	if got.Revision != rev || len(got.Files) != len(files) {
		t.Fatalf("copied manifest = %+v", got)
	}
	for name, want := range files {
		p := filepath.Join(dstHub, repoDirName(repo), "snapshots", rev, name)
		if data, err := os.ReadFile(p); err != nil || !bytes.Equal(data, want) {
			t.Errorf("%s differs after transfer (%v)", name, err)
		}
	}
	if ref, _ := os.ReadFile(filepath.Join(dstHub, repoDirName(repo), "refs", "main")); string(ref) != rev {
		t.Errorf("refs/main = %q", ref)
	}
}

// A peer supplies repo and oid; both land in filesystem paths.
func TestCacheBlobHandlerRejectsTraversal(t *testing.T) {
	s := &controlServer{hub: t.TempDir()}
	for _, q := range []string{
		"?repo=org/name&oid=../../../../etc/passwd",
		"?repo=org/name&oid=",
		"?repo=../../etc&oid=0123456789abcdef0123456789abcdef01234567",
	} {
		req := httptest.NewRequest(http.MethodGet, controlCacheBlobPath+q, nil)
		rec := httptest.NewRecorder()
		s.handleCacheBlob(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s served 200; it must be refused", q)
		}
	}
}

// A model the node does not have must be a clean 404, not a panic or an empty
// manifest the caller would treat as "nothing to copy, done".
func TestCacheManifestMissingRepo(t *testing.T) {
	s := &controlServer{hub: t.TempDir()}
	req := httptest.NewRequest(http.MethodGet, controlCacheManifestPath+"?repo=org/absent", nil)
	rec := httptest.NewRecorder()
	s.handleCacheManifest(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not in this cache") {
		t.Errorf("body = %q", rec.Body.String())
	}
}
