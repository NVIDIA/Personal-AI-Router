// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// CORS follows HTTP responses even for a profile absent from the engine table.
// No launch-option knowledge is supplied to the proxy.
func TestCORSPolicyIsIndependentOfEngineIdentity(t *testing.T) {
	tc := lmstudioCase(t)
	tc.profile.Name = "future-engine"
	test := func(name, allowed string) {
		t.Run(name, func(t *testing.T) {
			upstream := corsEngine(t, tc, allowed, http.StatusOK)
			defer upstream.Close()
			proxy := proxyForCORSTargets(t, tc, upstream)
			for _, method := range []string{http.MethodOptions, http.MethodGet} {
				rec := httptest.NewRecorder()
				proxy.handlePlain(rec, corsRequest(method, "/future-api", "https://app.test"))
				wantStatus := http.StatusOK
				if method == http.MethodOptions {
					wantStatus = http.StatusNoContent
				}
				require.True(t, rec.Code == wantStatus, " (%v)", method)
				require.True(t, rec.Header().Get("Access-Control-Allow-Origin") == allowed, " (%v)", method)
			}
		})
	}
	test("engine grants browser access", "https://app.test")
	test("engine grants no CORS permission", "")
}
