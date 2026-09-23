// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSInvalidInventoryPreservesApprovedPolicy(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		test := func(name string, denied, malformedFirst bool) {
			t.Run(name, func(t *testing.T) {
				broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Access-Control-Allow-Origin", "http://app.test")
					w.Header().Set("Vary", "Origin")
					if _, err := io.WriteString(w, "{malformed"); err != nil {
						t.Error(err)
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
				if rec.Code != wantStatus || rec.Header().Get("Access-Control-Allow-Origin") != wantOrigin {
					t.Fatalf("status=%d headers=%v, want %d origin %q", rec.Code, rec.Header(), wantStatus, wantOrigin)
				}
				if strings.Contains(rec.Body.String(), "private-model") {
					t.Fatal("returned partial model inventory")
				}
				if !denied && !strings.Contains(rec.Body.String(), "model inventory unavailable") {
					t.Fatalf("missing readable error: %s", rec.Body.String())
				}
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
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("caller credentials forwarded to model-list candidate")
			}
			if _, err := io.WriteString(w, corsModels(tc)); err != nil {
				t.Error(err)
			}
		}))
		defer upstream.Close()
		p := proxyForCORSTargets(t, tc, upstream, upstream)
		request := corsRequest(http.MethodGet, tc.modelListPath, "")
		request.Header.Set("Authorization", "Bearer private")
		request.Header.Set("Cookie", "session=private")
		rec := httptest.NewRecorder()
		p.handlePlain(rec, request)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}
