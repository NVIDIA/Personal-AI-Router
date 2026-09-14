// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildFakeRepo writes a cache entry the way the Hugging Face client does:
// content-addressed blobs plus snapshot symlinks.
func buildFakeRepo(t *testing.T, hub, repo, rev string, files map[string][]byte) {
	t.Helper()
	base := filepath.Join(hub, repoDirName(repo))
	for name, data := range files {
		var oid string
		if len(data) > 32 { // pretend the big ones are LFS: sha256-named
			s := sha256.Sum256(data)
			oid = hex.EncodeToString(s[:])
		} else {
			h := sha1.New()
			fmt.Fprintf(h, "blob %d\x00", len(data))
			h.Write(data)
			oid = hex.EncodeToString(h.Sum(nil))
		}
		blob := filepath.Join(base, "blobs", oid)
		os.MkdirAll(filepath.Dir(blob), 0o755)
		os.WriteFile(blob, data, 0o644)
		link := filepath.Join(base, "snapshots", rev, name)
		os.MkdirAll(filepath.Dir(link), 0o755)
		rel, _ := filepath.Rel(filepath.Dir(link), blob)
		os.Symlink(rel, link)
	}
}

func TestCacheManifestRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	const repo, rev = "mlx-community/Fake-1B", "abc123"
	files := map[string][]byte{
		"config.json":       []byte(`{"model_type":"fake"}`),
		"model.safetensors": bytes.Repeat([]byte("W"), 4096), // "large": sha256-named
	}
	buildFakeRepo(t, src, repo, rev, files)

	m, err := readCacheManifest(src, repo)
	if err != nil {
		t.Fatalf("readCacheManifest: %v", err)
	}
	if m.Revision != rev || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.TotalBytes() != int64(len(files["config.json"])+len(files["model.safetensors"])) {
		t.Errorf("TotalBytes = %d", m.TotalBytes())
	}

	// Transfer every blob, then rebuild the links.
	for _, f := range m.Files {
		p, _ := blobPath(src, repo, f.OID)
		data, _ := os.ReadFile(p)
		if err := writeCacheBlob(dst, repo, f.OID, m.Sizes[f.OID], bytes.NewReader(data)); err != nil {
			t.Fatalf("writeCacheBlob %s: %v", f.OID, err)
		}
	}
	if err := linkSnapshot(dst, m); err != nil {
		t.Fatalf("linkSnapshot: %v", err)
	}

	// The copy must be readable as a cache entry in its own right.
	back, err := readCacheManifest(dst, repo)
	if err != nil {
		t.Fatalf("re-reading the copy: %v", err)
	}
	if back.Revision != m.Revision || len(back.Files) != len(m.Files) {
		t.Fatalf("round trip changed the manifest: %+v vs %+v", back, m)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, repoDirName(repo), "snapshots", rev, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: content differs (%v)", name, err)
		}
	}
	if ref, _ := os.ReadFile(filepath.Join(dst, repoDirName(repo), "refs", "main")); string(ref) != rev {
		t.Errorf("refs/main = %q, want %q", ref, rev)
	}
}

// The blob name is the only integrity check on the wire, so corrupted bytes
// must be refused rather than installed.
func TestWriteCacheBlobRejectsCorruption(t *testing.T) {
	dst := t.TempDir()
	good := []byte("hello world")
	s := sha256.Sum256(good)
	oid := hex.EncodeToString(s[:])

	if err := writeCacheBlob(dst, "org/name", oid, int64(len(good)), bytes.NewReader(good)); err != nil {
		t.Fatalf("good blob rejected: %v", err)
	}
	bad := []byte("hello w0rld")
	p, _ := blobPath(dst, "org/name", oid)
	os.Remove(p)
	err := writeCacheBlob(dst, "org/name", oid, int64(len(bad)), bytes.NewReader(bad))
	if err == nil {
		t.Fatal("a blob that does not match its own name must be refused")
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Error("a failed transfer must not leave a blob in place")
	}
}

// A peer supplies these strings; they become filenames.
func TestCachePathsRejectTraversal(t *testing.T) {
	for _, oid := range []string{"../etc/passwd", "abc", "", "zz" + "0123456789abcdef0123456789abcdef01234567"[2:]} {
		if safeOID(oid) {
			t.Errorf("safeOID(%q) = true", oid)
		}
	}
	if !safeOID("0123456789abcdef0123456789abcdef01234567") {
		t.Error("a 40-hex oid must be accepted")
	}
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b", ""} {
		if safeRelPath(p) {
			t.Errorf("safeRelPath(%q) = true", p)
		}
	}
	if !safeRelPath("weights/model.safetensors") {
		t.Error("a normal nested path must be accepted")
	}
}
