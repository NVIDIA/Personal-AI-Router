// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCORSInvalidInventoryPreservesApprovedPolicy(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		test := func(name string, denied, malformedFirst bool) {
			t.Run(name, func(t *testing.T) {
				broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Access-Control-Allow-Origin", "http://app.test")
					w.Header().Set("Vary", "Origin")
					{
						_, err := io.WriteString(w, "{malformed")
						assert.NoError(t, err)
					}
				}))
				defer broken.Close()
				status, origin := http.StatusOK, "http://app.test"
				if denied {
					status, origin = http.StatusForbidden, ""
				}
				other := corsEngine(t, tc, origin, status)
				defer other.Close()
				targets := []*httptest.Server{other, broken}
				if malformedFirst {
					targets = []*httptest.Server{broken, other}
				}
				p := proxyForCORSTargets(t, tc, targets...)
				rec := httptest.NewRecorder()
				p.handlePlain(rec, corsRequest(http.MethodGet, tc.modelListPath, "http://app.test"))
				wantStatus, wantOrigin := http.StatusBadGateway, "http://app.test"
				if denied {
					wantStatus, wantOrigin = http.StatusForbidden, ""
				}
				require.True(t, rec.Code == wantStatus, "status (%v, %v)", wantStatus, wantOrigin)
				require.True(t, rec.Header().Get("Access-Control-Allow-Origin") == wantOrigin, "status (%v, %v)", wantStatus, wantOrigin)
				require.NotContains(t, rec.Body.String(), "private-model", "returned partial model inventory")
				require.False(t, !denied && !strings.Contains(rec.Body.String(), "model inventory unavailable"), "missing readable error")
			})
		}
		test("approved invalid inventory first", false, true)
		test("approved invalid inventory last", false, false)
		test("denial after invalid inventory", true, true)
		test("denial before invalid inventory", true, false)
	})
}

func TestModelListStripsCredentialsWithoutOrigin(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.False(t, r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "", "caller credentials forwarded to model-list candidate")
			{
				_, err := io.WriteString(w, corsModels(tc))
				assert.NoError(t, err)
			}
		}))
		defer upstream.Close()
		p := proxyForCORSTargets(t, tc, upstream, upstream)
		request := corsRequest(http.MethodGet, tc.modelListPath, "")
		request.Header.Set("Authorization", "Bearer private")
		request.Header.Set("Cookie", "session=private")
		rec := httptest.NewRecorder()
		p.handlePlain(rec, request)
		require.True(t, rec.Code == http.StatusOK, "status")
	})
}
