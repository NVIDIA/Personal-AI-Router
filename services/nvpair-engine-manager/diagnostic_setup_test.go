// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func setupReviewFixture(t *testing.T) (*diagnosticService, diagnosticSetupRequest, string) {
	t.Helper()
	d := diagnosticTestService(t)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "principal-a", "principal-b")
	d.m.mesh = clustertrust.Open(dir)
	r := diagnosticSetupRequest{Members: []diagnosticSetupMember{{"host-a", "principal-a", true, true}, {"host-b", "principal-b", true, true}}}
	binding, _ := json.Marshal([]string{"host-a", "principal-a", "host-b", "principal-b"})
	hash := sha256.Sum256(binding)
	r.GroupID = fmt.Sprintf("pair-recipe/nccl-%x", hash[:16])
	return d, r, dir
}

func TestDiagnosticSetupReviewPinsSourcesAndDoesNotInstallOrGrantReadiness(t *testing.T) {
	d, r, _ := setupReviewFixture(t)
	before, _ := os.ReadDir(d.m.exec.baseDir)
	review, err := d.setupReview(r)
	if err != nil {
		t.Fatal(err)
	}
	if review.Executable || review.EffectsApplied || review.State != "blocked" || review.Package.TransactionResolved || review.Package.Version != nil || review.RequiredDiskBytes != nil {
		t.Fatalf("review manufactured readiness: %+v", review)
	}
	if review.Target.Architecture != "arm64" || review.Target.SM != 121 || review.BuildJobs != 2 || review.ConcurrentNodes != 1 {
		t.Fatal("incorrect platform or resource contract")
	}
	if len(review.Sources) != 3 || len(review.Prerequisites) != 6 || len(review.Steps) != 4 || len(review.Adaptations) != 3 {
		t.Fatal("incomplete review")
	}
	for _, source := range review.Sources {
		if len(source.Revision) != 40 || !strings.Contains(source.URL, source.Revision) {
			t.Fatal("unpinned source")
		}
	}
	if review.Steps[0].Privilege != "administrator" || !strings.Contains(review.Steps[0].Rollback, "shared packages") {
		t.Fatal("package authority or rollback missing")
	}
	for _, check := range review.Prerequisites {
		if check.State != "unknown" && check.State != "blocked" {
			t.Fatal("discovery promoted to prerequisite proof")
		}
	}
	after, _ := os.ReadDir(d.m.exec.baseDir)
	if !reflect.DeepEqual(before, after) || len(d.operations) != 0 || d.reservation != nil {
		t.Fatal("review created execution or install state")
	}
	again, err := d.setupReview(r)
	if err != nil || again.ReviewID != review.ReviewID {
		t.Fatal("same facts changed review identity")
	}
	r.Members[1].GB10Observed = false
	changed, err := d.setupReview(r)
	if err != nil || changed.ReviewID == review.ReviewID || changed.Executable {
		t.Fatal("observation drift not bound to review")
	}
	// This actual Go wire result is used as the desktop parser's fixture.
	data, _ := json.Marshal(review)
	t.Log("SETUP_REVIEW_FIXTURE=" + string(data))
}

func TestDiagnosticSetupReviewRejectsMembershipDriftAndUntrustedFields(t *testing.T) {
	d, r, dir := setupReviewFixture(t)
	for _, name := range []string{"one-member", "duplicate-host", "duplicate-principal", "wrong-pair", "invalid-principal"} {
		t.Run(name, func(t *testing.T) {
			bad := r
			bad.Members = append([]diagnosticSetupMember(nil), r.Members...)
			switch name {
			case "one-member":
				bad.Members = bad.Members[:1]
			case "duplicate-host":
				bad.Members[1].NodeID = bad.Members[0].NodeID
			case "duplicate-principal":
				bad.Members[1].Principal = bad.Members[0].Principal
			case "wrong-pair":
				bad.GroupID += "f"
			case "invalid-principal":
				bad.Members[0].Principal = "bad\nprincipal"
			}
			if _, err := d.setupReview(bad); err == nil {
				t.Fatal("invalid review admitted")
			}
		})
	}
	for _, raw := range []string{`{"groupId":"x","members":[],"argv":["command"]}`, `{"groupId":"x","members":[],"sudoPassword":"not-a-secret"}`, `{"groupId":"x","members":[],"approved":true}`, `{} {}`} {
		var parsed diagnosticSetupRequest
		if strictDiagnosticJSON([]byte(raw), &parsed) == nil {
			t.Fatal("unexpected review fields accepted")
		}
	}
	clustertrusttest.RemovePeerPin(t, dir, "principal-b")
	if _, err := d.setupReview(r); err == nil {
		t.Fatal("revoked membership accepted")
	}
}
