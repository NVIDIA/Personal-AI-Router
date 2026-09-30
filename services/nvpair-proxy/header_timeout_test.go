// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestResolveResponseHeaderTimeout(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  string
		want time.Duration
	}{
		{"default", "", "", 120 * time.Second},
		{"env", "", "5m", 5 * time.Minute},
		{"flag beats env", "30s", "5m", 30 * time.Second},
		{"invalid env falls back", "", "bogus", 120 * time.Second},
		{"zero env falls back", "", "0", 120 * time.Second},
		{"negative flag falls back", "-1s", "", 120 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(responseHeaderTimeoutEnv, tc.env)
			if got := resolveResponseHeaderTimeout(tc.flag); got != tc.want {
				t.Fatalf("resolveResponseHeaderTimeout(%q) = %s, want %s", tc.flag, got, tc.want)
			}
		})
	}
}

// The configured timeout must reach the upstream transports built after
// startup.
func TestProxyTransportUsesConfiguredHeaderTimeout(t *testing.T) {
	old := proxyResponseTimeout
	defer func() { proxyResponseTimeout = old }()
	proxyResponseTimeout = 10 * time.Minute
	tr := newProxyTransport(nil)
	if tr.ResponseHeaderTimeout != 10*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %s, want 10m", tr.ResponseHeaderTimeout)
	}
}
