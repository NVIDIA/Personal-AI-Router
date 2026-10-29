// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
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
		if len(parts) < 3 || !llamaCPPRepoPattern.MatchString(parts[0]+"/"+parts[1]) {
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
					req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint+"/api/v1/models/"+repo+"/repo?"+query.Encode(), nil)
					if err != nil {
						http.Error(w, "invalid ModelScope download request", http.StatusBadGateway)
						return
					}
					for _, header := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
						if value := r.Header.Get(header); value != "" {
							req.Header.Set(header, value)
						}
					}
					resp, err := client.Do(req)
					if err != nil {
						http.Error(w, "ModelScope download request failed", http.StatusBadGateway)
						return
					}
					defer resp.Body.Close()
					for _, header := range []string{"Content-Length", "Content-Type", "ETag", "Accept-Ranges", "Content-Range", "Last-Modified"} {
						if value := resp.Header.Get(header); value != "" {
							w.Header().Set(header, value)
						}
					}
					w.WriteHeader(resp.StatusCode)
					if _, err := io.Copy(w, resp.Body); err != nil {
						panic(http.ErrAbortHandler)
					}
					return
				}
			}
		}
		http.NotFound(w, r)
	})
}
