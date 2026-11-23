// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const modelScopeURL = "https://modelscope.cn"

var errModelScopeListingTooLarge = errors.New("ModelScope repository exceeds the complete file-list limit")

type modelScopeFile struct {
	Path     string
	Type     string
	Revision string
	SHA256   string `json:"Sha256"`
	Size     int64
}

func validModelScopePath(path string) bool {
	return fs.ValidPath(path) && !strings.ContainsAny(path, "\\:\x00")
}

func modelScopeHash(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size
}

func fetchModelScopeFiles(ctx context.Context, client *http.Client, endpoint, repo, revision string) ([]modelScopeFile, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()
	query := url.Values{"Revision": {revision}, "Recursive": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/models/"+repo+"/repo/files?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build ModelScope metadata request: %w", withoutURL(err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ModelScope metadata request failed: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	var result struct {
		Success bool
		Data    struct {
			Files           []modelScopeFile
			LatestCommitter struct {
				ShortID string `json:"ShortId"`
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ModelScope metadata returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBody+1))
	if err != nil || len(data) > maxCatalogBody || json.Unmarshal(data, &result) != nil || !result.Success || result.Data.Files == nil {
		return nil, fmt.Errorf("invalid ModelScope file metadata")
	}
	if len(result.Data.Files) >= 3000 {
		return nil, errModelScopeListingTooLarge
	}
	files := make([]modelScopeFile, 0, len(result.Data.Files))
	for _, file := range result.Data.Files {
		if file.Type != "blob" {
			continue
		}
		if file.Revision == "" {
			file.Revision = result.Data.LatestCommitter.ShortID
		}
		if !validModelScopePath(file.Path) || (!modelScopeHash(file.Revision, 20) && !modelScopeHash(file.Revision, 4)) || !modelScopeHash(file.SHA256, 32) || file.Size < 0 {
			return nil, fmt.Errorf("invalid ModelScope file metadata")
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// The pinned llama.cpp downloader expects Hugging Face metadata. Translate
// metadata and stream files, leaving downloads, caching and progress to it.
func newModelScopeHandler(client *http.Client, endpoint string) http.Handler {
	var mu sync.Mutex
	snapshots := make(map[string][]modelScopeFile)
	var order []string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		metadata := len(parts) >= 5 && parts[0] == "api" && parts[1] == "models"
		if metadata {
			parts = parts[2:]
		}
		if len(parts) < 3 || !hfRepoPattern.MatchString(parts[0]+"/"+parts[1]) {
			http.NotFound(w, r)
			return
		}
		repo := parts[0] + "/" + parts[1]
		if metadata && len(parts) == 3 && parts[2] == "refs" {
			files, err := fetchModelScopeFiles(r.Context(), client, endpoint, repo, "master")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			// ModelScope supplies per-file or abbreviated repository revisions.
			// Identify the exact file set with the full-width snapshot ID llama.cpp
			// expects, so a moving branch cannot change an accepted download.
			data, _ := json.Marshal(files)
			digest := sha1.Sum(append([]byte("modelscope\x00"), data...))
			commit := hex.EncodeToString(digest[:])
			key := repo + "/" + commit
			mu.Lock()
			if _, exists := snapshots[key]; !exists {
				snapshots[key] = files
				order = append(order, key)
				if len(order) > 64 {
					delete(snapshots, order[0])
					order = order[1:]
				}
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"branches": []map[string]string{{"name": "main", "targetCommit": commit}}})
			return
		}
		if len(parts) < 4 || !modelScopeHash(parts[3], 20) {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		files, exists := snapshots[repo+"/"+parts[3]]
		mu.Unlock()
		if !exists {
			http.NotFound(w, r)
			return
		}
		if metadata && len(parts) == 4 && parts[2] == "tree" {
			rows := make([]map[string]any, 0, len(files))
			for _, file := range files {
				rows = append(rows, map[string]any{"type": "file", "path": file.Path, "oid": file.SHA256, "size": file.Size})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		path := strings.Join(parts[4:], "/")
		if !metadata && parts[2] == "resolve" && validModelScopePath(path) {
			for _, file := range files {
				if file.Path == path {
					query := url.Values{"Revision": {file.Revision}, "FilePath": {path}}
					// ModelScope can stall on HEAD for large files. Use GET headers;
					// downstream HEAD closes the response without reading its body.
					req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint+"/api/v1/models/"+repo+"/repo?"+query.Encode(), nil)
					if err != nil {
						http.Error(w, "invalid ModelScope download request", http.StatusBadGateway)
						return
					}
					for _, header := range []string{"If-None-Match", "If-Modified-Since"} {
						if value := r.Header.Get(header); value != "" {
							req.Header.Set(header, value)
						}
					}
					req.Header.Set("Accept-Encoding", "identity")
					resp, err := client.Do(req)
					if err != nil {
						http.Error(w, "ModelScope download request failed", http.StatusBadGateway)
						return
					}
					defer resp.Body.Close()
					serveModelScopeDownload(w, r, resp, file)
					return
				}
			}
		}
		http.NotFound(w, r)
	})
}

func serveModelScopeDownload(w http.ResponseWriter, r *http.Request, resp *http.Response, file modelScopeFile) {
	// A resumed suffix cannot authenticate bytes already in the engine's cache.
	// Always return a complete, checked file so a failed checksum cannot later
	// become a successful download by resuming only its withheld final byte.
	w.Header().Set("Accept-Ranges", "none")
	copyHeaders := func(headers ...string) {
		for _, header := range headers {
			if value := resp.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
	}
	if resp.StatusCode == http.StatusNotModified && (r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "") {
		copyHeaders("ETag", "Last-Modified")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode <= 599 {
		// Provider error pages are neither model bytes nor a trusted size source.
		http.Error(w, fmt.Sprintf("ModelScope download returned HTTP %d", resp.StatusCode), resp.StatusCode)
		return
	}
	valid := resp.Header.Get("Content-Encoding") == "" || resp.Header.Get("Content-Encoding") == "identity"
	// ModelScope also sends Content-Range on complete 200 responses. Validate
	// the full body against the accepted snapshot instead of trusting that header.
	if !valid || resp.StatusCode != http.StatusOK || (resp.ContentLength >= 0 && resp.ContentLength != file.Size) {
		http.Error(w, "invalid ModelScope download response", http.StatusBadGateway)
		return
	}
	copyHeaders("Content-Type", "ETag", "Last-Modified")
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if err := copyModelScopeDownload(w, r.Context(), resp.Body, file.Size, file.SHA256); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func copyModelScopeDownload(w io.Writer, ctx context.Context, body io.Reader, size int64, checksum string) error {
	digest := sha256.New()
	destination := io.MultiWriter(w, digest)
	// Hold the final byte until both EOF and integrity are confirmed. Otherwise
	// a client could accept Content-Length bytes before a checksum failure aborts
	// the connection. Memory stays constant even for multi-gigabyte model files.
	var last [1]byte
	if size > 0 {
		if _, err := io.CopyN(destination, body, size-1); err != nil {
			return err
		}
		if _, err := io.ReadFull(body, last[:]); err != nil {
			return err
		}
		_, _ = digest.Write(last[:])
	}
	var extra [1]byte
	if _, err := io.ReadFull(body, extra[:]); err != io.EOF {
		return fmt.Errorf("ModelScope download does not match its expected size")
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), checksum) {
		return fmt.Errorf("ModelScope download checksum mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if size > 0 {
		_, err := w.Write(last[:])
		return err
	}
	return nil
}
