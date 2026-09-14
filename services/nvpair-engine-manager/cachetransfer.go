// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LAN model transfer: copying a model from a node that already holds it to one
// that does not, over the same pin-gated cluster mTLS the other remote engine
// operations use.
//
// Why not just have the far node download it: because the far node may have no
// internet, because the bytes are already on this network, and because on a
// gigabit LAN this is roughly an order of magnitude faster than the Hub is from
// here. It is also the only option that works offline.
//
// Only the CACHE is transferred, never a model directory outside it. That is a
// deliberate limit: the cache is content-addressed, so every byte can be
// verified against the name it is stored under, and the layout is the one
// huggingface_hub's scan_cache_dir reads — which is what makes the copy show up
// in the destination's model list rather than merely existing on its disk.

const (
	controlCacheManifestPath = "/v1/models/cache-manifest"
	controlCacheBlobPath     = "/v1/models/cache-blob"
)

// hubRoot is the Hugging Face hub cache this node reads and writes.
func hubRoot() string {
	if v := strings.TrimSpace(os.Getenv("HF_HUB_CACHE")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("HF_HOME")); v != "" {
		return filepath.Join(v, "hub")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

// hubDir is the cache this node writes copies into.
func (e *Executor) hubDir() string {
	if e.hub != "" {
		return e.hub
	}
	return hubRoot()
}

// handleCacheManifest answers "what would it take to copy this model from you".
// Pin-gated by the caller, like every other ec route.
func (s *controlServer) handleCacheManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	var (
		m   *cacheManifest
		err error
	)
	if isDirModelRef(repo) {
		// A locally built model, addressed by path. resolveModelDir is the
		// boundary that keeps a peer from naming an arbitrary directory.
		var dir string
		if dir, err = resolveModelDir(repo); err == nil {
			m, err = readDirManifest(dir)
			if m != nil {
				m.Repo = repo // answer under the name the caller asked for
			}
		}
	} else {
		m, err = readCacheManifest(s.hubDir(), repo)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// handleCacheBlob streams one blob. The blob id is validated as a bare hex
// digest before it is used as a path, so a crafted id cannot read outside the
// repo's blobs directory.
func (s *controlServer) handleCacheBlob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repo, oid := r.URL.Query().Get("repo"), r.URL.Query().Get("oid")
	var (
		path string
		err  error
	)
	if isDirModelRef(repo) {
		// No blobs directory to address into: a directory model's files are
		// named by their path within it, and the receiver verifies each against
		// the digest the manifest gave.
		path, err = dirModelFilePath(repo, r.URL.Query().Get("path"))
	} else {
		path, err = blobPath(s.hubDir(), repo, oid)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "blob unreadable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, f)
}

// fetchCacheManifest asks a peer what it holds for repo.
func (c *remoteClient) fetchCacheManifest(ctx context.Context, repo string) (*cacheManifest, error) {
	// A directory model's manifest is computed by hashing every file, which for
	// a multi-gigabyte model outlasts the ordinary response-header budget.
	fetch := c.get
	if isDirModelRef(repo) {
		fetch = c.getSlow
	}
	raw, err := fetch(ctx, controlCacheManifestPath+"?repo="+url.QueryEscape(repo))
	if err != nil {
		return nil, err
	}
	var m cacheManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("peer sent an unreadable manifest: %w", err)
	}
	if m.Repo != repo || m.Revision == "" || len(m.Files) == 0 {
		return nil, fmt.Errorf("peer sent a manifest for %q with no usable files", m.Repo)
	}
	return &m, nil
}

// PullModelFromPeer copies a model from a peer's cache into this node's.
//
// Files are fetched over a few connections at once, and any file already present
// and digest-matching is skipped.
//
// The original serial loop argued the link was the bottleneck so parallelism
// could only cost. Measured, it is worth 1.7x: 24 MiB/s on one stream against
// 40 MiB/s on four, between two laptops on 802.11ac (see defaultCopyStreams).
// One flow cannot keep a Wi-Fi link busy on its own.
//
// Each file is independently verified and atomically renamed, so concurrency
// costs nothing in integrity -- and a run that dies leaves only complete,
// digest-checked files, which the next run skips instead of refetching.
func (e *Executor) PullModelFromPeer(ctx context.Context, c *remoteClient, repo string, onProgress func(stage string, percent int, message string)) error {
	m, err := c.fetchCacheManifest(ctx, repo)
	if err != nil {
		return err
	}
	dirModel := isDirModelRef(repo)
	var root, destDir string
	if dirModel {
		roots := modelRoots()
		if len(roots) == 0 {
			return fmt.Errorf("no model directory on this node to copy into")
		}
		// Same basename, this node's own root: the source path is the sender's
		// and means nothing here, but the catalogue keys on the path, so keeping
		// the leaf name is what makes the two nodes agree on the model's id.
		destDir = filepath.Join(roots[0], filepath.Base(filepath.Clean(repo)))
	} else {
		root = e.hubDir()
		if root == "" {
			return fmt.Errorf("no Hugging Face cache directory on this node")
		}
	}

	total := m.TotalBytes()
	var done int64
	onProgress("starting", 0, fmt.Sprintf("%d files, %.1f MiB", len(m.Files), float64(total)/(1<<20)))

	// One entry per transfer. A cache model points two paths at one blob, so it
	// moves once; a directory model has real files at both paths and needs both.
	seen := map[string]bool{}
	todo := make([]cacheFile, 0, len(m.Files))
	for _, f := range m.Files {
		if !dirModel {
			if seen[f.OID] {
				continue
			}
			seen[f.OID] = true
		}
		// Already here and already correct: a re-run after a failed transfer
		// resumes instead of re-fetching gigabytes it can verify locally.
		if dirModel && fileAlreadyPresent(filepath.Join(destDir, filepath.FromSlash(f.Path)), m.Sizes[f.OID], f.OID) {
			done += m.Sizes[f.OID]
			continue
		}
		todo = append(todo, f)
	}

	fetchOne := func(ctx context.Context, f cacheFile) error {
		size := m.Sizes[f.OID]
		q := controlCacheBlobPath + "?repo=" + url.QueryEscape(repo) + "&oid=" + url.QueryEscape(f.OID)
		if dirModel {
			q += "&path=" + url.QueryEscape(f.Path)
		}
		body, err := c.getStream(ctx, q)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", f.Path, err)
		}
		defer body.Close()
		if dirModel {
			return writeDirModelFile(filepath.Join(destDir, filepath.FromSlash(f.Path)), size, f.OID, body)
		}
		return writeCacheBlob(root, repo, f.OID, size, body)
	}

	if err := runTransfers(ctx, todo, fetchOne, func(f cacheFile) {
		// Progress is reported from every worker, so the running total and the
		// callback are both taken under the lock: two workers finishing together
		// must not race the percentage backwards.
		done += m.Sizes[f.OID]
		pct := 0
		if total > 0 {
			pct = int(done * 100 / total)
		}
		onProgress("downloading", pct, f.Path)
	}); err != nil {
		return err
	}

	if !dirModel {
		if err := linkSnapshot(root, m); err != nil {
			return err
		}
	}
	slog.Info("copied model from peer", "repo", repo, "dest", destDir, "files", len(m.Files), "bytes", total)
	onProgress("success", 100, repo)
	return nil
}

// controlCopyFromPath lets a peer ask THIS node to copy a model from a THIRD
// node. It is what makes "sitting at laptop 1, give laptop 2 this model" work:
// laptop 1 calls this on laptop 2, and laptop 2 pulls from laptop 1.
//
// The two-party endpoints above are the transport; this is the trigger. Both
// hops are pin-gated mTLS, so a node can only be asked to copy by a peer it has
// paired with, and can only copy from a peer it has paired with.
const controlCopyFromPath = "/v1/models/copy-from"

type copyFromRequest struct {
	OpID       string `json:"opId"`
	SourceNode string `json:"sourceNode"`
	Engine     string `json:"engine"`
	Model      string `json:"model"`
}

func (s *controlServer) handleCopyFrom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req copyFromRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.SourceNode == "" || req.Model == "" {
		http.Error(w, `"sourceNode" and "model" are required`, http.StatusBadRequest)
		return
	}
	if s.copyFrom == nil {
		http.Error(w, "this node cannot copy from a peer", http.StatusServiceUnavailable)
		return
	}
	// streamOp relays whatever the executor's progress hub publishes for this
	// engine, so the transfer's frames reach the caller with no extra plumbing.
	s.streamOp(w, r, req.OpID, req.Engine, "copy", func(ctx context.Context) (streamFrame, error) {
		if err := s.copyFrom(ctx, req.SourceNode, req.Engine, req.Model); err != nil {
			return streamFrame{}, err
		}
		return streamFrame{Stage: "success", Percent: 100, Message: req.Model}, nil
	})
}

// CopyModelFromPeer resolves sourceNode as a pinned peer and copies model from
// it into this node's cache, publishing progress on the engine's channel so a
// streaming caller sees it.
func (m *Manager) CopyModelFromPeer(ctx context.Context, sourceNode, engine, model string) error {
	peer, ok := m.peers.lookup(sourceNode)
	if !ok {
		return fmt.Errorf("node %s is not a discovered ec peer", sourceNode)
	}
	client, err := m.remoteClient(ctx, peer)
	if err != nil {
		return err
	}
	return m.exec.PullModelFromPeer(ctx, client, model, func(stage string, pct int, message string) {
		m.exec.progress.publish(ProgressEvent{
			Engine: engine, Op: "copy", Stage: stage, Percent: pct, Message: message,
		})
	})
}
