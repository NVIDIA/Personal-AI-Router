// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func modelScopeDownloadFixture(t *testing.T, content string, download http.Handler) (http.Handler, string) {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"Success": true,
				"Data": map[string]any{"Files": []modelScopeFile{{
					Path: "model-Q4_K_M.gguf", Type: "blob", Revision: modelScopeTestRevision,
					SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)),
				}}},
			}))
			return
		}
		assert.Equal(t, "identity", r.Header.Get("Accept-Encoding"))
		download.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	handler := newModelScopeHandler(upstream.Client(), upstream.URL)
	commit := modelScopeTestCommit(t, handler)
	return handler, "/owner/repo/resolve/" + commit + "/model-Q4_K_M.gguf"
}

func TestModelScopeDownload_CompletesVerifiedFullFiles(t *testing.T) {
	test := func(name, content, contentRange string) {
		t.Run(name, func(t *testing.T) {
			handler, path := modelScopeDownloadFixture(t, content, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", contentRange)
				// Exercise unknown-length upstream bodies as well as fixed lengths.
				assert.NoError(t, http.NewResponseController(w).Flush())
				_, err := io.WriteString(w, content)
				assert.NoError(t, err)
			}))
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			response, err := server.Client().Get(server.URL + path)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, response.Body.Close()) })

			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, content, string(body))
			assert.Equal(t, int64(len(content)), response.ContentLength)
			assert.Equal(t, "none", response.Header.Get("Accept-Ranges"))
			assert.Empty(t, response.Header.Get("Content-Range"))
		})
	}
	test("empty file", "", "")
	test("single byte", "a", "")
	test("complete file", "0123456789", "")
	test("complete file with Content-Range", "0123456789", "bytes 0-9/10")
}

func TestModelScopeDownload_AbortsIncompleteAndCorruptBodies(t *testing.T) {
	test := func(name, body string) {
		t.Run(name, func(t *testing.T) {
			handler, path := modelScopeDownloadFixture(t, "0123456789", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", "bytes 0-9/10")
				assert.NoError(t, http.NewResponseController(w).Flush())
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()

			require.PanicsWithValue(t, http.ErrAbortHandler, func() { handler.ServeHTTP(response, request) })
			assert.Less(t, response.Body.Len(), 10, "invalid transfer must not deliver a complete file")
		})
	}
	test("truncated file", "012345678")
	test("oversized file", "01234567890")
	test("checksum mismatch", "012345678X")
}

func TestModelScopeDownload_RejectsMismatchedResponseHeaders(t *testing.T) {
	test := func(name, requestedRange string, status int, headers http.Header) {
		t.Run(name, func(t *testing.T) {
			handler, path := modelScopeDownloadFixture(t, "0123456789", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for key, values := range headers {
					w.Header()[key] = values
				}
				w.WriteHeader(status)
			}))
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Range", requestedRange)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, http.StatusBadGateway, response.Code)
		})
	}
	test("short Content-Length", "", http.StatusOK, http.Header{"Content-Length": {"9"}})
	test("large Content-Length", "", http.StatusOK, http.Header{"Content-Length": {"11"}})
	test("unexpected encoding", "", http.StatusOK, http.Header{"Content-Encoding": {"gzip"}})
	test("range without request", "", http.StatusPartialContent, http.Header{"Content-Range": {"bytes 0-9/10"}})
	test("range despite full-file request upstream", "bytes=5-9", http.StatusPartialContent, http.Header{"Content-Range": {"bytes 5-9/10"}})
}

func TestModelScopeDownload_RetriesCorruptFilesFromBeginning(t *testing.T) {
	content := strings.Repeat("x", 100000)
	var attempts atomic.Int32
	handler, path := modelScopeDownloadFixture(t, content, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Range"))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Accept-Ranges", "bytes")
		body := content
		if attempts.Add(1) == 1 {
			body = strings.Repeat("y", len(content))
		}
		_, err := io.WriteString(w, body)
		assert.NoError(t, err)
	}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	response, err := server.Client().Get(server.URL + path)
	require.NoError(t, err)
	partial, err := io.ReadAll(response.Body)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.NoError(t, response.Body.Close())
	require.NotEmpty(t, partial)
	require.Less(t, len(partial), len(content))
	assert.Equal(t, "none", response.Header.Get("Accept-Ranges"))

	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	require.NoError(t, err)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(partial)))
	response, err = server.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, response.Body.Close()) })
	complete, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, content, string(complete))
	assert.Equal(t, "none", response.Header.Get("Accept-Ranges"))
}

type modelScopeDownloadTransport func(*http.Request) (*http.Response, error)

func (f modelScopeDownloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type modelScopeDownloadBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *modelScopeDownloadBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *modelScopeDownloadBody) Close() error { b.closed = true; return nil }

func modelScopeDownloadResponseFixture(t *testing.T, download *http.Response) (http.Handler, string) {
	t.Helper()
	metadata := fmt.Sprintf(`{"Success":true,"Data":{"Files":[{"Path":"model-Q4_K_M.gguf","Type":"blob","Revision":%q,"Sha256":%q,"Size":10}]}}`, modelScopeTestRevision, modelScopeTestSHA256)
	client := &http.Client{Transport: modelScopeDownloadTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: int64(len(metadata)), Body: io.NopCloser(strings.NewReader(metadata))}, nil
		}
		assert.Equal(t, http.MethodGet, r.Method)
		return download, nil
	})}
	handler := newModelScopeHandler(client, "https://modelscope.invalid")
	commit := modelScopeTestCommit(t, handler)
	return handler, "/owner/repo/resolve/" + commit + "/model-Q4_K_M.gguf"
}

func TestModelScopeDownload_BoundsUpstreamReadsBySnapshotSize(t *testing.T) {
	body := &modelScopeDownloadBody{Reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	handler, path := modelScopeDownloadResponseFixture(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: -1, Body: body,
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)

	require.PanicsWithValue(t, http.ErrAbortHandler, func() { handler.ServeHTTP(response, request) })

	assert.Equal(t, 11, body.read, "read only the expected bytes and one byte to detect overflow")
	assert.Equal(t, 9, response.Body.Len(), "withhold the final byte until validation succeeds")
	assert.True(t, body.closed)
}

func TestModelScopeDownload_DoesNotReadHeadOrErrorBodies(t *testing.T) {
	test := func(name, method string, upstreamStatus int, requestHeaders, responseHeaders http.Header) {
		t.Run(name, func(t *testing.T) {
			body := &modelScopeDownloadBody{Reader: strings.NewReader(strings.Repeat("x", 1<<20))}
			handler, path := modelScopeDownloadResponseFixture(t, &http.Response{
				StatusCode: upstreamStatus, Header: responseHeaders, ContentLength: 10, Body: body,
			})
			request := httptest.NewRequest(method, path, nil)
			request.Header = requestHeaders
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, upstreamStatus, response.Code)
			assert.Zero(t, body.read)
			assert.True(t, body.closed)
		})
	}
	test("HEAD", http.MethodHead, http.StatusOK, http.Header{}, http.Header{})
	test("HEAD with Content-Range", http.MethodHead, http.StatusOK, http.Header{}, http.Header{"Content-Range": {"bytes 0-9/10"}})
	test("not modified", http.MethodGet, http.StatusNotModified, http.Header{"If-None-Match": {"model-etag"}}, http.Header{"Etag": {"model-etag"}})
	test("invalid range", http.MethodGet, http.StatusRequestedRangeNotSatisfiable, http.Header{"Range": {"bytes=20-"}}, http.Header{"Content-Range": {"bytes */10"}})
	test("provider failure", http.MethodGet, http.StatusServiceUnavailable, http.Header{}, http.Header{})
}

func TestModelScopeDownload_CancelsUpstreamWhenClientDisconnects(t *testing.T) {
	canceled := make(chan struct{})
	handler, path := modelScopeDownloadFixture(t, strings.Repeat("x", 100000), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, strings.Repeat("x", 64000))
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
		close(canceled)
	}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, response.Body.Close()) })

	cancel()

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream download continued after client cancellation")
	}
}
