// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Reading and writing a Hugging Face cache entry, so a model one node already
// holds can be copied to another over the LAN instead of re-downloaded.
//
// The layout is content-addressed and must be reproduced exactly, not merely
// approximated: mlx-lm lists models via huggingface_hub's scan_cache_dir, which
// reads this structure. Writing the files as plain paths would serve inference
// but leave the model invisible in the catalogue.
//
//	models--<org>--<name>/
//	  blobs/<oid>                     the bytes, named by content
//	  snapshots/<revision>/<path>     symlinks into ../../blobs/<oid>
//	  refs/main                       the revision the branch points at
//
// The oid IS the integrity check, which is why none is transmitted: a 40-hex
// oid is git's blob SHA-1 (sha1("blob <len>\0" + content)) and a 64-hex one is
// the LFS SHA-256 of the content. Both were verified against a real cache entry
// before this relied on them. Computing digests sender-side instead would mean
// reading every byte of an 11 GB model just to answer "what do you have".

// cacheFile is one path inside a snapshot and the blob it resolves to.
type cacheFile struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
}

// cacheManifest describes one cached repo well enough to rebuild it elsewhere.
type cacheManifest struct {
	Repo     string      `json:"repo"`
	Revision string      `json:"revision"`
	Files    []cacheFile `json:"files"`
	Sizes    map[string]int64 `json:"sizes"` // oid -> bytes, for progress and preflight
}

// TotalBytes is what a transfer will move: distinct blobs, so a repo that
// points two paths at one blob is not counted twice.
func (m *cacheManifest) TotalBytes() int64 {
	var n int64
	for _, size := range m.Sizes {
		n += size
	}
	return n
}

// repoDirName converts "org/name" to the cache's "models--org--name".
func repoDirName(repo string) string {
	return "models--" + strings.ReplaceAll(repo, "/", "--")
}

// repoFromDirName is the inverse.
func repoFromDirName(dir string) string {
	return strings.ReplaceAll(strings.TrimPrefix(dir, "models--"), "--", "/")
}

// safeOID rejects anything that is not a bare hex digest. The oid arrives from a
// peer and is used as a filename, so this is the boundary that keeps a crafted
// value from escaping the blobs directory.
func safeOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

// safeRelPath rejects a snapshot path that would escape its snapshot directory.
func safeRelPath(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "..") {
		return false
	}
	clean := filepath.Clean(p)
	return clean == p && !strings.Contains(clean, ".."+string(filepath.Separator))
}

// readCacheManifest describes a repo held in the given hub root.
func readCacheManifest(hubRoot, repo string) (*cacheManifest, error) {
	base := filepath.Join(hubRoot, repoDirName(repo))
	snapsDir := filepath.Join(base, "snapshots")
	snaps, err := os.ReadDir(snapsDir)
	if err != nil {
		return nil, fmt.Errorf("repo %q is not in this cache: %w", repo, err)
	}
	var revision string
	for _, s := range snaps {
		if s.IsDir() {
			revision = s.Name() // one revision per cached repo in practice; last wins
		}
	}
	if revision == "" {
		return nil, fmt.Errorf("repo %q has no snapshot", repo)
	}

	m := &cacheManifest{Repo: repo, Revision: revision, Sizes: map[string]int64{}}
	snapRoot := filepath.Join(snapsDir, revision)
	err = filepath.WalkDir(snapRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(snapRoot, p)
		if relErr != nil {
			return relErr
		}
		// Resolve the symlink to learn which blob backs this path. A plain file
		// (some tools materialise rather than link) is still transferable: it is
		// named by its own content once hashed, so it is skipped rather than
		// guessed at.
		target, linkErr := os.Readlink(p)
		if linkErr != nil {
			return nil
		}
		oid := filepath.Base(target)
		if !safeOID(oid) {
			return nil
		}
		info, statErr := os.Stat(p) // follows the link: the blob's real size
		if statErr != nil {
			return nil
		}
		m.Files = append(m.Files, cacheFile{Path: filepath.ToSlash(rel), OID: oid})
		m.Sizes[oid] = info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("repo %q has no transferable files", repo)
	}
	return m, nil
}

// blobPath is where a blob lives, refusing anything that is not a clean digest.
func blobPath(hubRoot, repo, oid string) (string, error) {
	if !safeOID(oid) {
		return "", fmt.Errorf("invalid blob id %q", oid)
	}
	return filepath.Join(hubRoot, repoDirName(repo), "blobs", oid), nil
}

// hasherFor returns the digest the oid encodes, and the prefix git puts in front
// of a blob's contents before hashing it.
func hasherFor(oid string, size int64) (hash.Hash, []byte) {
	if len(oid) == 64 {
		return sha256.New(), nil
	}
	return sha1.New(), []byte(fmt.Sprintf("blob %d\x00", size))
}

// writeCacheBlob streams a blob in, verifying it against its own name, and only
// then puts it in place. A partial or corrupted transfer leaves a temp file that
// is removed, never a blob that looks complete.
func writeCacheBlob(hubRoot, repo, oid string, size int64, r io.Reader) error {
	dst, err := blobPath(hubRoot, repo, oid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Already present and intact: a re-run after an interrupted transfer skips
	// what it already has.
	if info, statErr := os.Stat(dst); statErr == nil && info.Size() == size {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	h, prefix := hasherFor(oid, size)
	h.Write(prefix)
	written, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, size))
	tmp.Close()
	if err != nil {
		return fmt.Errorf("blob %s: %w", oid, err)
	}
	if written != size {
		return fmt.Errorf("blob %s: got %d bytes, expected %d", oid, written, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != oid {
		return fmt.Errorf("blob %s failed verification (computed %s); refusing to install it", oid, got)
	}
	return os.Rename(tmp.Name(), dst)
}

// linkSnapshot rebuilds the snapshot symlinks and the branch ref, which is what
// makes the copied repo visible to scan_cache_dir and therefore to mlx-lm.
func linkSnapshot(hubRoot string, m *cacheManifest) error {
	base := filepath.Join(hubRoot, repoDirName(m.Repo))
	snapRoot := filepath.Join(base, "snapshots", m.Revision)
	for _, f := range m.Files {
		if !safeRelPath(f.Path) || !safeOID(f.OID) {
			return fmt.Errorf("refusing unsafe entry %q -> %q", f.Path, f.OID)
		}
		link := filepath.Join(snapRoot, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		// Relative, exactly as the Hugging Face client writes it, so the cache
		// survives being moved and looks native to every tool that reads it.
		rel, err := filepath.Rel(filepath.Dir(link), filepath.Join(base, "blobs", f.OID))
		if err != nil {
			return err
		}
		_ = os.Remove(link)
		if err := os.Symlink(rel, link); err != nil {
			return err
		}
	}
	refs := filepath.Join(base, "refs")
	if err := os.MkdirAll(refs, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(refs, "main"), []byte(m.Revision), 0o644)
}
