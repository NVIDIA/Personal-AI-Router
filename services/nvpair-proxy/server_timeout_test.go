// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

func TestHTTPServersConfigureIdleTimeouts(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	p.soleFacade().serveHTTP(context.Background(), ln)
	defer p.shutdown(context.Background())

	require.NotNil(t, p.soleFacade().plainSrv, "servers not recorded")
	require.NotNil(t, p.soleFacade().tlsSrv, "servers not recorded")
	for name, srv := range map[string]struct {
		readHeader, idle interface{}
	}{
		"plain": {p.soleFacade().plainSrv.ReadHeaderTimeout, p.soleFacade().plainSrv.IdleTimeout},
		"tls":   {p.soleFacade().tlsSrv.ReadHeaderTimeout, p.soleFacade().tlsSrv.IdleTimeout},
	} {
		assert.Equal(t, proxyReadHeaderTimeout, srv.readHeader, " (%v, %v)", name, proxyReadHeaderTimeout)
		assert.Equal(t, proxyServerIdleTimeout, srv.idle, " (%v, %v)", name, proxyServerIdleTimeout)
	}
	require.Equal(t, proxyIdleConnTimeout, proxyServerIdleTimeout, "server IdleTimeout (%v, %v)", proxyServerIdleTimeout, proxyIdleConnTimeout)
}
