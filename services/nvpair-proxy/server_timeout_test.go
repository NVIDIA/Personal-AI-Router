// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"testing"

	"nvpair-shared/clustertrust"
)

func TestHTTPServersConfigureIdleTimeouts(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.soleFacade().serveHTTP(context.Background(), ln)
	defer p.shutdown(context.Background())

	if p.soleFacade().plainSrv == nil || p.soleFacade().tlsSrv == nil {
		t.Fatal("servers not recorded")
	}
	for name, srv := range map[string]struct {
		readHeader, idle interface{}
	}{
		"plain": {p.soleFacade().plainSrv.ReadHeaderTimeout, p.soleFacade().plainSrv.IdleTimeout},
		"tls":   {p.soleFacade().tlsSrv.ReadHeaderTimeout, p.soleFacade().tlsSrv.IdleTimeout},
	} {
		if srv.readHeader != proxyReadHeaderTimeout {
			t.Errorf("%s ReadHeaderTimeout = %v, want %v", name, srv.readHeader, proxyReadHeaderTimeout)
		}
		if srv.idle != proxyServerIdleTimeout {
			t.Errorf("%s IdleTimeout = %v, want %v", name, srv.idle, proxyServerIdleTimeout)
		}
	}
	if proxyServerIdleTimeout != proxyIdleConnTimeout {
		t.Fatalf("server IdleTimeout %v != client IdleConnTimeout %v", proxyServerIdleTimeout, proxyIdleConnTimeout)
	}
}
