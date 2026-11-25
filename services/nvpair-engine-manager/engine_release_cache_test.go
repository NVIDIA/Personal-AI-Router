// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func releaseAPIResponse(status int, headers http.Header, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestGitHubRelease_PausesOtherRepositoriesUntilTheRateLimitResets(t *testing.T) {
	check := func(name string, status int, headers http.Header, body string, wait time.Duration) {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
				reset := time.Now().Add(wait)
				if headers.Get("X-RateLimit-Remaining") == "0" {
					headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				}
				calls := 0
				e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return releaseAPIResponse(status, headers, body)
					}
					return releaseAPIResponse(http.StatusOK, nil, `{"tag_name":"v1"}`)
				})

				_, err := e.githubRelease(t.Context(), "ggml-org/llama.cpp", "tags/b100")
				require.ErrorContains(t, err, reset.UTC().Format(time.RFC1123))
				time.Sleep(wait - time.Second)
				_, err = e.githubRelease(t.Context(), "ollama/ollama", "latest")
				require.ErrorContains(t, err, reset.UTC().Format(time.RFC1123))
				require.Equal(t, 1, calls)

				time.Sleep(time.Second)
				release, err := e.githubRelease(t.Context(), "ollama/ollama", "latest")
				require.NoError(t, err)
				assert.Equal(t, "v1", release.Tag)
				assert.Equal(t, 2, calls)
			})
		})
	}
	check("exhausted hourly quota", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, "", 5*time.Minute)
	check("explicit retry delay", http.StatusTooManyRequests, http.Header{"Retry-After": {"90"}}, "", 90*time.Second)
	check("secondary limit without a retry header", http.StatusForbidden, nil, `{"message":"You have exceeded a secondary rate limit."}`, time.Minute)
}

func TestGitHubRelease_LeavesOtherRepositoriesAvailableAfterAnAccessDenial(t *testing.T) {
	e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "ggml-org/llama.cpp") {
			return releaseAPIResponse(http.StatusForbidden, nil, `{"message":"Access denied"}`)
		}
		return releaseAPIResponse(http.StatusOK, nil, `{"tag_name":"v1"}`)
	})

	_, err := e.githubRelease(t.Context(), "ggml-org/llama.cpp", "tags/b100")
	require.ErrorContains(t, err, "HTTP 403")
	assert.NotContains(t, err.Error(), "rate limit")
	release, err := e.githubRelease(t.Context(), "ollama/ollama", "latest")
	require.NoError(t, err)
	assert.Equal(t, "v1", release.Tag)
}

func TestGitHubRelease_FetchesANewTagImmediately(t *testing.T) {
	e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		tag := strings.TrimPrefix(req.URL.Path, "/repos/ggml-org/llama.cpp/releases/tags/")
		return releaseAPIResponse(http.StatusOK, nil, `{"tag_name":"`+tag+`"}`)
	})

	_, err := e.githubRelease(t.Context(), "ggml-org/llama.cpp", "tags/b100")
	require.NoError(t, err)
	release, err := e.githubRelease(t.Context(), "ggml-org/llama.cpp", "tags/b101")
	require.NoError(t, err)
	assert.Equal(t, "b101", release.Tag)
}

func TestGitHubRelease_RefreshesLatestAfterItsCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
		body := `{"tag_name":"v1"}`
		e.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return releaseAPIResponse(http.StatusOK, nil, body)
		})
		release, err := e.githubRelease(t.Context(), "ollama/ollama", "latest")
		require.NoError(t, err)
		require.Equal(t, "v1", release.Tag)

		body = `{"tag_name":"v2"}`
		time.Sleep(15 * time.Minute)
		release, err = e.githubRelease(t.Context(), "ollama/ollama", "latest")
		require.NoError(t, err)
		assert.Equal(t, "v2", release.Tag)
	})
}

func TestGitHubRelease_CancelingAWaiterKeepsTheSharedRequestRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
		var calls atomic.Int32
		resume := make(chan struct{})
		e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-resume:
				return releaseAPIResponse(http.StatusOK, nil, `{"tag_name":"b100"}`)
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		request := func(ctx context.Context) <-chan error {
			done := make(chan error, 1)
			go func() {
				_, err := e.githubRelease(ctx, "ggml-org/llama.cpp", "tags/b100")
				done <- err
			}()
			return done
		}
		original := request(t.Context())
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		waiter := request(ctx)
		synctest.Wait()

		cancel()
		assert.ErrorIs(t, <-waiter, context.Canceled)
		close(resume)
		assert.NoError(t, <-original)
		assert.Equal(t, int32(1), calls.Load())
	})
}
