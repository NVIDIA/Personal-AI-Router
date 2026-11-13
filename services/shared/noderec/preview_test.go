// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"context"
	"encoding/json"
	"net"
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

// Public hardware can be useful for display even when the responder invents its
// identity. Preserve hardware and sample age, but remove UUID and registry claims
// before callers can associate the preview with a discovery record.
func TestReadHardwarePreview_PreservesHardwareWithoutIdentityClaims(t *testing.T) {
	principal := "claimed-host"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/node-info", r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(NodeInfo{
			HostUUID: "claimed-host", ClusterUUID: &principal,
			Peer: &PeerMetadata{Name: "claimed-name"},
			GPUs: []GPUInfo{{Name: "preview GPU", UtilizationPercent: 61}},
			CPU:  &CPUInfo{Name: "preview CPU"}, Memory: &MemoryInfo{TotalBytes: 16384},
			TelemetryValid: true, MSSince: 137,
		}))
	}))
	t.Cleanup(server.Close)
	host, port := previewServerAddress(t, server)

	info, ok := ReadHardwarePreview(context.Background(), host, port)

	require.True(t, ok)
	assert.Empty(t, info.HostUUID)
	assert.Nil(t, info.ClusterUUID)
	assert.Nil(t, info.Peer)

	assert.Equal(t, []GPUInfo{{Name: "preview GPU", UtilizationPercent: 61}}, info.GPUs)
	assert.Equal(t, &CPUInfo{Name: "preview CPU"}, info.CPU)
	assert.Equal(t, &MemoryInfo{TotalBytes: 16384}, info.Memory)

	// Request time may make the sample older, but reading it must never reset
	// the age reported by the hardware collector.
	assert.True(t, info.TelemetryValid)
	assert.GreaterOrEqual(t, info.MSSince, int64(137))
}

func TestReadHardwarePreview_RejectsInvalidResponses(t *testing.T) {
	test := func(name, body string, status int) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, err := w.Write([]byte(body))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			host, port := previewServerAddress(t, server)

			info, ok := ReadHardwarePreview(context.Background(), host, port)

			assert.False(t, ok)
			assert.Equal(t, NodeInfo{}, info)
		})
	}

	test("malformed JSON", `{"GPUs":`, http.StatusOK)
	test("null response", `null`, http.StatusOK)
	test("unavailable endpoint", `{}`, http.StatusServiceUnavailable)
}

// The response limit includes the complete JSON body. Accept exactly 64 KiB,
// but reject even one byte more without returning partially decoded hardware.
func TestReadHardwarePreview_EnforcesResponseSizeLimit(t *testing.T) {
	test := func(name string, bodyBytes int, wantOK bool, wantGPUs []GPUInfo) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			const prefix = `{"GPUs":[{"name":"bounded GPU"}],"padding":"`
			const suffix = `"}`
			body := prefix + strings.Repeat("x", bodyBytes-len(prefix)-len(suffix)) + suffix
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write([]byte(body))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			host, port := previewServerAddress(t, server)

			info, ok := ReadHardwarePreview(context.Background(), host, port)

			assert.Equal(t, wantOK, ok)
			assert.Equal(t, wantGPUs, info.GPUs)
		})
	}

	test("exactly 64 KiB", 65536, true, []GPUInfo{{Name: "bounded GPU"}})
	test("one byte above 64 KiB", 65537, false, nil)
}

// Scanner applies one deadline to a complete address sweep. Canceling that
// context must also interrupt a preview whose headers arrived but whose body
// is still pending, rather than waiting for the client's longer timeout.
func TestReadHardwarePreview_CancelsPendingBodyReadWithCallerContext(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, http.NewResponseController(w).Flush())
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	host, port := previewServerAddress(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type previewResult struct {
		info NodeInfo
		ok   bool
	}
	done := make(chan previewResult, 1)
	go func() {
		info, ok := ReadHardwarePreview(ctx, host, port)
		done <- previewResult{info: info, ok: ok}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("preview request did not reach the test server")
	}

	cancel()

	select {
	case result := <-done:
		assert.False(t, result.ok)
		assert.Equal(t, NodeInfo{}, result.info)
	case <-time.After(time.Second):
		t.Fatal("preview body read did not stop after cancellation")
	}
}

// Discovery supplies the host to read; an untrusted responder must not choose
// another destination through a redirect. Check that the read fails and that
// the redirect target receives no request.
func TestReadHardwarePreview_DoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(server.Close)
	host, port := previewServerAddress(t, server)

	_, ok := ReadHardwarePreview(context.Background(), host, port)

	assert.False(t, ok)

	assert.False(t, redirected.Load())
}

func previewServerAddress(t *testing.T, server *httptest.Server) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}
