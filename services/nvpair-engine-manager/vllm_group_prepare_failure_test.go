// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
)

func TestVLLMPrepareFailureBoundPeerResponseRetainsPredicate(t *testing.T) {
	for _, cause := range []error{rankStartError("admission", "prepare_owner_busy", nil, 0), errors.New("private-fixture-message")} {
		p, request, certificate, _ := vllmGroupPeerFixture(t, false)
		p.native = func(context.Context, vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
			return vllmGroupPeerResult{}, cause
		}
		raw, _ := json.Marshal(request)
		r := httptest.NewRequest(http.MethodPost, vllmGroupPeerPath, bytes.NewReader(raw))
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
		w := httptest.NewRecorder()
		p.serveHTTP(w, r) // in-memory handler, no network listener or native process
		var result vllmGroupPeerResult
		if w.Code != http.StatusOK || strictDiagnosticJSON(w.Body.Bytes(), &result) != nil || validateVLLMGroupPeerResult(request, result) != nil {
			t.Fatal("preparation failure collapsed to an unbound HTTP error")
		}
		if result.Action != "prepare" || result.State != "failed" || result.Code != "prepare_failed" || result.EffectsApplied || result.CleanupConfirmed || result.StartFailure == nil {
			t.Fatal("failed preparation claimed successful preparation, effects or cleanup")
		}
		var typed *vllmRankStartError
		want := "unclassified"
		if errors.As(cause, &typed) {
			want = typed.Failure.Code
		}
		if result.StartFailure.Stage != "admission" || result.StartFailure.Code != want {
			t.Fatal("safe predicate identity lost")
		}
		if bytes.Contains(w.Body.Bytes(), []byte("private-fixture")) {
			t.Fatal("raw error escaped the closed diagnostic")
		}
	}
}

func TestVLLMPrepareFailureRejectsEffectAndCleanupClaims(t *testing.T) {
	request := verticalRequest("prepare")
	result := failedVLLMGroupPrepare(request, rankStartError("admission", "prepare_owner_busy", nil, 0))
	for _, change := range []func(*vllmGroupPeerResult){func(r *vllmGroupPeerResult) { r.EffectsApplied = true }, func(r *vllmGroupPeerResult) { r.CleanupConfirmed = true }, func(r *vllmGroupPeerResult) { r.State = "prepared" }, func(r *vllmGroupPeerResult) { r.Code = "start_failed" }, func(r *vllmGroupPeerResult) { r.Action = "start" }} {
		copy := result
		change(&copy)
		if validateVLLMGroupPeerResult(request, copy) == nil {
			t.Fatal("malformed failed preparation accepted")
		}
	}
}

func TestVLLMPrepareFailureMalformedNativeReceiptIsExplicit(t *testing.T) {
	p, request, _, _ := vllmGroupPeerFixture(t, false)
	p.native = func(_ context.Context, r vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
		result := verticalResult(r, "prepared")
		result.RunID = strings.Repeat("f", 32)
		return result, nil
	}
	result, err := p.local(context.Background(), "fixture-peer", request)
	if err != nil || result.Code != "prepare_failed" || result.StartFailure == nil || result.StartFailure.Code != "binding_mismatch" || result.EffectsApplied || result.CleanupConfirmed {
		t.Fatal("malformed native preparation reply was not retained as a bound failure")
	}
}

func TestVLLMPrepareFailurePreservesCancellationAndFirstTypedCause(t *testing.T) {
	request := verticalRequest("prepare")
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		result := failedVLLMGroupPrepare(request, cause)
		raw, _ := json.Marshal(result)
		var decoded vllmGroupPeerResult
		if strictDiagnosticJSON(raw, &decoded) != nil || !errors.Is(vllmGroupActionOutcome(request, decoded), cause) {
			t.Fatal("redacted preparation cancellation identity lost")
		}
	}
	first := rankStartError("admission", "prepare_owner_busy", nil, 0)
	result := failedVLLMGroupPrepare(request, errors.Join(first, context.Canceled))
	if result.StartFailure == nil || !reflect.DeepEqual(*result.StartFailure, first.Failure) {
		t.Fatal("later cancellation overwrote first typed predicate")
	}
}

func TestVLLMPrepareFailureSurvivesCleanupAndReconcileWithoutStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starts := 0
		cleanupAvailable := false
		expected := rankStartError("admission", "prepare_owner_busy", nil, 0)
		g := vllmGroupTestOwner(t, func(_ context.Context, b vllmGroupBinding, action string) error {
			if action == "start" {
				starts++
			}
			if action == "prepare" && b.Rank == 1 {
				return vllmGroupActionOutcome(vllmGroupRequest(b, action), failedVLLMGroupPrepare(vllmGroupRequest(b, action), expected))
			}
			if action == "stop" && b.Rank == 1 && !cleanupAvailable {
				return errors.New("fixture cleanup unavailable")
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, context.Background(), 2)
		synctest.Wait()
		got := g.status()
		if starts != 0 || got.State != "cleanup-required" || got.Ranks[1].Started || got.Ranks[1].StartFailure == nil || !reflect.DeepEqual(*got.Ranks[1].StartFailure, expected.Failure) {
			t.Fatal("prepare failure lost its predicate or admitted Start")
		}
		cleanupAvailable = true
		if err := g.reconcile(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		got = g.status()
		if !got.CleanupConfirmed || got.State != "failed" || got.Ranks[1].StartFailure == nil || !reflect.DeepEqual(*got.Ranks[1].StartFailure, expected.Failure) {
			t.Fatal("cleanup reconciliation erased original preparation failure")
		}
	})
}

func TestVLLMPrepareFailureNormalStopKeepsCancellationWithoutFalseFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
			if action == "prepare" && b.Rank == 1 {
				<-ctx.Done()
				r := vllmGroupRequest(b, action)
				return vllmGroupActionOutcome(r, failedVLLMGroupPrepare(r, ctx.Err()))
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, context.Background(), 2)
		synctest.Wait()
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		got := g.status()
		if got.State != "stopped" || got.Failure != "" || !got.CleanupConfirmed || got.Ranks[1].StartFailure == nil || got.Ranks[1].StartFailure.Code != "cancelled" {
			t.Fatal("normal Stop became an operational failure or lost cancellation detail")
		}
	})
}
