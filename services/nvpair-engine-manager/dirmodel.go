// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Transfer of a model that lives as a plain DIRECTORY rather than as a Hugging
// Face cache entry.
//
// A model you quantized yourself never enters the cache: it has no repo id, so
// mlx-pool advertises it by absolute path (see scanModelsDir) and the catalogue
// offers it like any other. The cache transfer cannot carry it -- it addresses
// blobs by the cache's own oids -- so "Get from <node>" on such a model failed
// with the path mangled into a cache directory name that could never exist:
//
//	models----Users--alice--models--Qwen3.8-27B-3bit/snapshots: no such file or directory
//
// The fix keeps the property that made the cache transfer safe: every byte is
// verified against a digest named in the manifest before it lands. A cache blob
// is self-naming, so nothing had to be computed; a directory has no such digest,
// so the sender hashes the files when it builds the manifest. That is one extra
// read of the model at manifest time, which is the price of being able to verify
// at all.

// modelRoots are the directories this node will serve a directory model from
// and copy one into.
//
// Mirrors mlx-pool's --models-dir default so both agree on where a locally built
// model lives; MLX_MODELS_DIRS overrides it in the same os.PathListSeparator
// form. This list IS the security boundary for the transfer: a peer names the
// model by absolute path, so without it a peer could name any directory on this
// machine.
func modelRoots() []string {
	if raw := strings.TrimSpace(os.Getenv("MLX_MODELS_DIRS")); raw != "" {
		var out []string
		for _, p := range strings.Split(raw, string(os.PathListSeparator)) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, expandPath(p))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "models")}
}

// engineModelsDir is the {models_dir} placeholder, per engine.
//
// It was LM Studio's directory unconditionally, which was harmless while LM
// Studio was the only engine declaring a path-based action. MLX now deletes by
// path too, and pointing its confinement root at another engine's directory
// would make every delete fail the containment check.
func engineModelsDir(engine string) string {
	if engine == "mlx" {
		if roots := modelRoots(); len(roots) > 0 {
			return roots[0]
		}
		return ""
	}
	return lmstudioModelsDir()
}

// isDirModelRef reports whether a catalogue id names a directory rather than a
// Hugging Face repo. Repo ids are "org/name" and never absolute.
func isDirModelRef(repo string) bool {
	return filepath.IsAbs(repo)
}

// withinRoot reports whether p is root itself or sits underneath it. Compared on
// cleaned paths with a separator boundary so "/Users/me/models-evil" does not
// pass for root "/Users/me/models".
func withinRoot(p, root string) bool {
	p, root = filepath.Clean(p), filepath.Clean(root)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// resolveModelDir turns a peer-supplied absolute path into a directory this node
// is willing to serve, or an error.
//
// Symlinks are resolved BEFORE the containment check: a symlink inside a model
// root pointing at /etc would otherwise pass a prefix test while reading
// somewhere else entirely.
func resolveModelDir(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("model path %q is not absolute", p)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("model %q is not on this node: %w", p, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("model %q is not a directory on this node", p)
	}
	roots := modelRoots()
	for _, root := range roots {
		if r, err := filepath.EvalSymlinks(root); err == nil && withinRoot(resolved, r) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("model %q is outside this node's model directories %v", p, roots)
}

// readDirManifest describes a directory model well enough to rebuild it
// elsewhere, reusing the cache manifest shape so the transfer loop is shared.
//
// The oid is the SHA-256 of the file's contents, which is what the receiver
// verifies against. Revision is a constant: a directory has no revisions, and
// the field only has to be non-empty for the manifest to be accepted.
func readDirManifest(dir string) (*cacheManifest, error) {
	m := &cacheManifest{Repo: dir, Revision: "local", Sizes: map[string]int64{}}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Regular files only: a symlink or device node in a model directory is
		// not something to reproduce on the far side.
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || !safeRelPath(rel) {
			return nil
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		m.Files = append(m.Files, cacheFile{Path: filepath.ToSlash(rel), OID: sum})
		m.Sizes[sum] = info.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read model directory %q: %w", dir, err)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("model directory %q holds no files", dir)
	}
	// Deterministic order so a transfer resumes over the same sequence.
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dirModelFilePath resolves one file of a directory model for serving. Both the
// directory and the relative path arrive from a peer, so both are validated and
// the join is re-checked against the resolved directory.
func dirModelFilePath(repo, rel string) (string, error) {
	dir, err := resolveModelDir(repo)
	if err != nil {
		return "", err
	}
	if !safeRelPath(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("invalid file path %q", rel)
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if !withinRoot(full, dir) {
		return "", fmt.Errorf("invalid file path %q", rel)
	}
	return full, nil
}

// writeDirModelFile writes one verified file of a directory model.
//
// Written to a temporary name and renamed only after the digest matches, so an
// interrupted transfer leaves no file that a catalogue scan would offer as a
// complete model.
func writeDirModelFile(dest string, size int64, wantOID string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".partial-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: got %d bytes, expected %d", filepath.Base(dest), n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantOID {
		return fmt.Errorf("%s: content does not match its digest", filepath.Base(dest))
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}
