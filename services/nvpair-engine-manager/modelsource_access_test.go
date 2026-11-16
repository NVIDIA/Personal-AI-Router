// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type modelScopeAccessTransport func(*http.Request) (*http.Response, error)

func (f modelScopeAccessTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func modelScopeAccessExecutor(t *testing.T) (*Executor, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	executor := &Executor{client: &http.Client{Transport: modelScopeAccessTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		assert.Equal(t, "https", r.URL.Scheme)
		assert.Equal(t, "modelscope.cn", r.URL.Host)
		assert.Equal(t, "/api/v1/models/owner/repo/repo/files", r.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"Success":true,"Data":{"Files":[]}}`)),
			Request:    r,
		}, nil
	})}}
	t.Cleanup(executor.StopAll)
	return executor, &requests
}

func TestModelScopeEndpoint_RejectsRequestsWithoutListenerHostAndToken(t *testing.T) {
	executor, outboundRequests := modelScopeAccessExecutor(t)
	endpoint, err := executor.modelScopeEndpoint()
	require.NoError(t, err)
	address, err := url.Parse(endpoint)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	metadataPath := "/api/models/owner/repo/refs"
	for _, test := range []struct {
		name string
		host string
		path string
	}{
		{name: "missing token", host: address.Host, path: metadataPath},
		{name: "wrong token", host: address.Host, path: "/incorrect" + metadataPath},
		{name: "token must end at a path boundary", host: address.Host, path: address.Path + "extra" + metadataPath},
		{name: "hostname must match numeric listener", host: "localhost:" + address.Port(), path: address.Path + metadataPath},
		{name: "foreign host", host: "modelscope.cn", path: address.Path + metadataPath},
		{name: "listener port is required", host: address.Hostname(), path: address.Path + metadataPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, address.Scheme+"://"+address.Host+test.path, nil)
			require.NoError(t, err)
			request.Host = test.host
			response, err := client.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			assert.Equal(t, http.StatusNotFound, response.StatusCode)
			assert.Zero(t, outboundRequests.Load(), "rejected requests must not reach ModelScope")
		})
	}

	response, err := client.Get(endpoint + metadataPath)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var metadata struct {
		Branches []struct{ Name, TargetCommit string }
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&metadata))
	require.Len(t, metadata.Branches, 1)
	assert.Equal(t, "main", metadata.Branches[0].Name)
	assert.Len(t, metadata.Branches[0].TargetCommit, 40)
	assert.Equal(t, int32(1), outboundRequests.Load())
}

func TestModelScopeEndpoint_KeepsTokensSpecificToEachExecutor(t *testing.T) {
	first, firstRequests := modelScopeAccessExecutor(t)
	firstEndpoint, err := first.modelScopeEndpoint()
	require.NoError(t, err)
	firstAddress, err := url.Parse(firstEndpoint)
	require.NoError(t, err)
	second, secondRequests := modelScopeAccessExecutor(t)
	secondEndpoint, err := second.modelScopeEndpoint()
	require.NoError(t, err)
	secondAddress, err := url.Parse(secondEndpoint)
	require.NoError(t, err)
	require.NotEqual(t, firstAddress.Path, secondAddress.Path)

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(secondAddress.Scheme + "://" + secondAddress.Host + firstAddress.Path + "/api/models/owner/repo/refs")
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	assert.Zero(t, firstRequests.Load())
	assert.Zero(t, secondRequests.Load())
}

func TestModelScopeEndpoint_RemainsStableUntilExecutorShutdown(t *testing.T) {
	executor, outboundRequests := modelScopeAccessExecutor(t)
	endpoint, err := executor.modelScopeEndpoint()
	require.NoError(t, err)
	again, err := executor.modelScopeEndpoint()
	require.NoError(t, err)
	assert.Equal(t, endpoint, again)

	executor.StopAll()
	_, err = executor.modelScopeEndpoint()
	require.ErrorContains(t, err, "shutting down")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(endpoint + "/api/models/owner/repo/refs")
	if response != nil {
		assert.NoError(t, response.Body.Close())
	}
	require.Error(t, err, "shutdown must close the adapter listener")
	assert.Zero(t, outboundRequests.Load())
}

func TestModelScopeEndpoint_RedactsPrivateEndpointAndStandaloneToken(t *testing.T) {
	const token = "68d237f5b99142fd6337f8226684f55ce24b9b240fbac2a2f311302028379254"
	const endpoint = "http://127.0.0.1:12345/" + token
	assert.Equal(t, "download [redacted]/api/models/owner/repo/refs", redactModelScopeEndpoint("download "+endpoint+"/api/models/owner/repo/refs", endpoint))
	assert.Equal(t, "path /[redacted]/owner/repo", redactModelScopeEndpoint("path /"+token+"/owner/repo", endpoint))
	assert.Equal(t, "token [redacted]", redactModelScopeEndpoint("token "+token, endpoint))
	assert.Equal(t, "ordinary engine output", redactModelScopeEndpoint("ordinary engine output", endpoint))
	assert.Equal(t, "ordinary engine output", redactModelScopeEndpoint("ordinary engine output", ""))
}
