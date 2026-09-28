// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCableFactsFailureClassifications(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{"unsupported", 404, nil, "does not expose"}, {"unsupported-method", 405, nil, "does not expose"}, {"denied", 403, nil, "rejected paired access"},
		{"deadline", 0, context.DeadlineExceeded, "exceeded its deadline"}, {"connection", 0, errors.New("synthetic private transport text"), "connection or authentication"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: cableTestTransport(func(*http.Request) (*http.Response, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader("synthetic untrusted response body"))}, nil
			})}
			var value map[string]any
			err := readCableJSON(context.Background(), client, "https://synthetic.invalid/facts", &value)
			if err == nil || !strings.Contains(cableFactsMessage(err), test.want) || strings.Contains(err.Error(), "synthetic") {
				t.Fatalf("unsafe or conflated failure: %v", err)
			}
		})
	}
}

func TestCableAccessPurposeReusesVolatileOwner(t *testing.T) {
	f := newCableProductFixture(t)
	owner := f.s.m.onboarding
	observed := 0
	owner.observeKey = func(_ context.Context, c onboardingCandidate, _ string) (string, bool, bool, error) {
		observed++
		return c.HostKeySHA256, true, false, nil
	}
	id := f.ids["host-peer"]
	owner.targets[id].lifetime = "persistent"
	request := onboardingBatchAccess{Purpose: "cable", CandidateIDs: []string{id}, Username: "synthetic-user", Auth: "password", Password: "synthetic-account-input", ElevationPassword: "synthetic-admin-input"}
	if _, err := owner.bindAccess(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if observed != 1 || owner.targets[id].lifetime != "session" || owner.targets[id].access.elevationPassword != "synthetic-admin-input" || len(owner.operations) != 0 {
		t.Fatal("cable access changed setup state or lost its explicit administrator input")
	}
	for _, mutate := range []func(*onboardingBatchAccess){func(r *onboardingBatchAccess) { r.StartupLifetime = "persistent" }, func(r *onboardingBatchAccess) { r.OperationID = strings.Repeat("a", 32) }, func(r *onboardingBatchAccess) { r.Purpose = "arbitrary" }} {
		bad := request
		mutate(&bad)
		if _, err := owner.bindAccess(context.Background(), bad); err == nil {
			t.Fatal("cable access changed an unrelated operation/lifetime")
		}
	}
	if observed != 1 {
		t.Fatal("invalid purpose reached host access inspection")
	}
}

func TestCableWorkerConstructorKeepsOtherServiceStoresClosed(t *testing.T) {
	m := newManagerTransport(nil, nil, nil)
	defer m.remoteHTTP.CloseIdle()
	defer m.readyHTTP.CloseIdle()
	if m.exec != nil || m.onboarding != nil || m.cables != nil || m.peers == nil {
		t.Fatal("one-shot transport initialized unrelated service/recovery owners")
	}
}

func TestCableWorkerReferenceRequiresControllerApprovedBytes(t *testing.T) {
	f := newCableProductFixture(t)
	file, artifact := writeOnboardingFixture(t, onboardingFixtureEntries(183))
	manifest, err := onboardingArchiveManifest(file)
	if err != nil {
		t.Fatal(err)
	}
	plan := cableLaunchPlan{Info: onboardingPlatformInfo{OS: "Linux", Arch: "aarch64"}}
	for _, entry := range manifest.Files {
		if entry.FileName == "nvpair-engine-manager" {
			plan.WorkerSHA256 = entry.SHA256
			plan.WorkerBytes = entry.Size
		}
	}
	if _, err = f.s.matchKnownWorker(context.Background(), plan); err == nil {
		t.Fatal("remote-reported bytes became a trusted worker without a controller reference")
	}
	f.s.m.onboarding.imports[artifact.ArtifactID] = onboardingArtifactSource{onboardingArtifact: artifact, File: file}
	origin, err := f.s.matchKnownWorker(context.Background(), plan)
	if err != nil || origin != "verified-package:"+strings.ToLower(artifact.SHA256) {
		t.Fatalf("admitted artifact not resolved: %s %v", origin, err)
	}
	bad := plan
	bad.WorkerSHA256 = strings.Repeat("f", 64)
	if _, err = f.s.matchKnownWorker(context.Background(), bad); err == nil {
		t.Fatal("different worker bytes borrowed an admitted package origin")
	}
	changed, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = changed.Write([]byte("changed"))
	_ = changed.Close()
	if _, err = f.s.matchKnownWorker(context.Background(), plan); err == nil {
		t.Fatal("changed package retained its approved digest")
	}
}

func TestCableDistinctPAIRAdmissionsAllowSharedHostTraits(t *testing.T) {
	f := newCableProductFixture(t)
	ownerKey := f.s.m.onboarding.targets[f.ids["host-owner"]].candidate.HostKeySHA256
	f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.candidate.HostKeySHA256 = ownerKey })
	review := f.ready()
	run := f.start(review)
	result := f.done(run)
	if result.State != "completed" || result.Result != "reciprocal-observations" {
		t.Fatalf("shared UID/path/hash/SSH-key heuristics blocked distinct bound PAIR identities: %+v", result)
	}
}
