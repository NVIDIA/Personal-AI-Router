// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"
)

func TestParseManualInput(t *testing.T) {
	cases := []struct {
		in      string
		address string
		baseURL string
		wantErr bool
	}{
		{in: "192.168.1.50", address: "192.168.1.50"},
		{in: "dgx", address: "dgx"},
		{in: "http://localhost:8888/v1", baseURL: "http://localhost:8888/v1"},
		{in: "http://dgx:8000", baseURL: "http://dgx:8000"},
		{in: "https://localhost:8888/v1", wantErr: true}, // http only, say so
		{in: "  ", wantErr: true},
	}
	for _, tc := range cases {
		addr, base, err := parseManualInput(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseManualInput(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil || addr != tc.address || base != tc.baseURL {
			t.Errorf("parseManualInput(%q) = %q %q %v, want %q %q",
				tc.in, addr, base, err, tc.address, tc.baseURL)
		}
	}
}
