// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modelScopeTestRevision = "0123456789abcdef0123456789abcdef01234567"
const modelScopeTestSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func modelScopeTestCommit(t *testing.T, handler http.Handler) string {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/models/owner/repo/refs", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var result struct {
		Branches []struct{ Name, TargetCommit string }
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Branches, 1)
	assert.Equal(t, "main", result.Branches[0].Name)
	require.Len(t, result.Branches[0].TargetCommit, 40)
	return result.Branches[0].TargetCommit
}

func TestModelScopeMetadata_PreservesSnapshotAfterBranchChanges(t *testing.T) {
	var changed atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/models/owner/repo/repo/files", r.URL.Path)
		assert.Equal(t, "master", r.URL.Query().Get("Revision"))
		assert.Equal(t, "true", r.URL.Query().Get("Recursive"))
		file := modelScopeFile{Path: "nested/model-Q4_K_M.gguf", Type: "blob", Revision: modelScopeTestRevision, SHA256: modelScopeTestSHA256, Size: 100}
		if changed.Load() {
			file.SHA256 = strings.Repeat("a", 64)
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"Success": true, "Data": map[string]any{"Files": []modelScopeFile{file}}}))
	}))
	t.Cleanup(upstream.Close)
	handler := newModelScopeHandler(upstream.Client(), upstream.URL)
	commit := modelScopeTestCommit(t, handler)
	changed.Store(true)
	assert.NotEqual(t, commit, modelScopeTestCommit(t, handler))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/models/owner/repo/tree/"+commit+"?recursive=true", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var files []struct {
		Type, Path, OID string
		Size            int64
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &files))
	require.Len(t, files, 1)
	assert.Equal(t, "file", files[0].Type)
	assert.Equal(t, "nested/model-Q4_K_M.gguf", files[0].Path)
	assert.Equal(t, modelScopeTestSHA256, files[0].OID)
	assert.Equal(t, int64(100), files[0].Size)
}

func TestModelScopeDownload_PreservesRangesWithoutForwardingCredentials(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		assert.Equal(t, "bytes=10-19", r.Header.Get("Range"))
		w.Header().Set("Content-Range", "bytes 10-19/100")
		w.Header().Set("ETag", "model-etag")
		w.WriteHeader(http.StatusPartialContent)
		_, err := fmt.Fprint(w, "0123456789")
		assert.NoError(t, err)
	}))
	t.Cleanup(cdn.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		if strings.HasSuffix(r.URL.Path, "/files") {
			_, err := fmt.Fprintf(w, `{"Success":true,"Data":{"Files":[{"Path":"nested/model-Q4_K_M.gguf","Type":"blob","Revision":%q,"Sha256":%q,"Size":100}]}}`, modelScopeTestRevision, modelScopeTestSHA256)
			assert.NoError(t, err)
			return
		}
		assert.Equal(t, modelScopeTestRevision, r.URL.Query().Get("Revision"))
		assert.Equal(t, "nested/model-Q4_K_M.gguf", r.URL.Query().Get("FilePath"))
		http.Redirect(w, r, cdn.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)
	handler := newModelScopeHandler(upstream.Client(), upstream.URL)
	commit := modelScopeTestCommit(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/owner/repo/resolve/"+commit+"/nested/model-Q4_K_M.gguf", nil)
	request.Header.Set("Range", "bytes=10-19")
	request.Header.Set("Authorization", "Bearer hf-test-secret")
	request.Header.Set("Cookie", "session=test-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	assert.Equal(t, http.StatusPartialContent, response.Code)
	assert.Equal(t, "bytes 10-19/100", response.Header().Get("Content-Range"))
	assert.Equal(t, "model-etag", response.Header().Get("ETag"))
	assert.Equal(t, "0123456789", response.Body.String())
}

func TestModelScopeMetadata_RejectsUnsafeFilePaths(t *testing.T) {
	for _, path := range []string{"../model.gguf", "nested/../../model.gguf", `C:\model.gguf`, "/model.gguf"} {
		t.Run(path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"Success": true, "Data": map[string]any{"Files": []modelScopeFile{{Path: path, Type: "blob", Revision: modelScopeTestRevision, SHA256: modelScopeTestSHA256}}}}))
			}))
			t.Cleanup(upstream.Close)
			response := httptest.NewRecorder()
			newModelScopeHandler(upstream.Client(), upstream.URL).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/models/owner/repo/refs", nil))
			assert.Equal(t, http.StatusBadGateway, response.Code)
		})
	}
}

func TestModelScopeMetadata_RejectsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	response := httptest.NewRecorder()
	newModelScopeHandler(upstream.Client(), upstream.URL).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/models/owner/repo/refs", nil))
	assert.Equal(t, http.StatusBadGateway, response.Code)
}

func TestModelScopeMetadata_UsesRepositoryCommitWhenFileRevisionIsAbsent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := fmt.Fprintf(w, `{"Success":true,"Data":{"LatestCommitter":{"ShortId":"f659fb25"},"Files":[{"Path":"model-Q4_K_M.gguf","Type":"blob","Sha256":%q,"Size":100}]}}`, modelScopeTestSHA256)
		assert.NoError(t, err)
	}))
	t.Cleanup(upstream.Close)
	files, err := fetchModelScopeFiles(context.Background(), upstream.Client(), upstream.URL, "owner/repo", "master")
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "f659fb25", files[0].Revision)
}
