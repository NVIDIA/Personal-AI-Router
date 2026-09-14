// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Mirrors a REAL cached model into a scratch hub, so the result can be checked
// against huggingface_hub's own scan_cache_dir rather than against my beliefs.
func TestMirrorRealModelForInspection(t *testing.T) {
	src := os.Getenv("MIRROR_SRC")
	dst := os.Getenv("MIRROR_DST")
	repo := os.Getenv("MIRROR_REPO")
	if src == "" || dst == "" || repo == "" {
		t.Skip("set MIRROR_SRC/MIRROR_DST/MIRROR_REPO")
	}
	m, err := readCacheManifest(src, repo)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	t.Logf("manifest: %d files, %.1f MiB", len(m.Files), float64(m.TotalBytes())/(1<<20))
	for _, f := range m.Files {
		p, _ := blobPath(src, repo, f.OID)
		in, err := os.Open(p)
		if err != nil {
			t.Fatalf("open %s: %v", f.OID, err)
		}
		err = writeCacheBlob(dst, repo, f.OID, m.Sizes[f.OID], in)
		in.Close()
		if err != nil {
			t.Fatalf("write %s: %v", f.OID, err)
		}
	}
	if err := linkSnapshot(dst, m); err != nil {
		t.Fatalf("link: %v", err)
	}
	t.Logf("mirrored to %s", filepath.Join(dst, repoDirName(repo)))
}
