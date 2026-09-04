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
	p := testProxy(NewDiscovery(), 8080)
	p.mesh = clustertrust.Open(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.serveHTTP(context.Background(), ln)
	defer p.shutdown(context.Background())

	if p.plainSrv == nil || p.tlsSrv == nil {
		t.Fatal("servers not recorded")
	}
	for name, srv := range map[string]struct {
		readHeader, idle interface{}
	}{
		"plain": {p.plainSrv.ReadHeaderTimeout, p.plainSrv.IdleTimeout},
		"tls":   {p.tlsSrv.ReadHeaderTimeout, p.tlsSrv.IdleTimeout},
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
