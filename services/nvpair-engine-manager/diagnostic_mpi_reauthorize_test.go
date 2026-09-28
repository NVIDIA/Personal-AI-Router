// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/clustertrust"
)

type mpiReviewMemoryTransport func(*http.Request) (*http.Response, error)

func (f mpiReviewMemoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mpiReauthorizedFixture(t *testing.T) (*diagnosticService, *diagnosticRuntimeRun, *int, *int) {
	t.Helper()
	d, run, adoption := managedFixture(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	hostKey := onboardingHostPublicKey{Algorithm: key.Type(), Blob: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}
	for i := range run.Binding.Targets {
		target := &run.Binding.Targets[i]
		target.Candidate.HostKeySHA256 = hostKey.Fingerprint
		run.Binding.Review.Targets[i].Candidate.HostKeySHA256 = hostKey.Fingerprint
		run.Public.Targets[i].Candidate.HostKeySHA256 = hostKey.Fingerprint
		d.m.onboarding.targets[target.Candidate.CandidateID].candidate.HostKeySHA256 = hostKey.Fingerprint
	}
	if _, err := d.adoptRuntime(context.Background(), adoption); err != nil {
		t.Fatal(err)
	}
	// Round-trip the actual durable operation schema: access generations must
	// disappear, while the approved participant and artifact bindings survive.
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var restored diagnosticRuntimeRun
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Binding.Generations != nil {
		t.Fatal("volatile generation persisted")
	}
	run = &restored
	d.runtimeRuns[run.Public.OperationID] = run
	for _, target := range run.Binding.Targets {
		current := d.m.onboarding.targets[target.Candidate.CandidateID]
		current.accessGeneration = newOpID()
		current.expiresAt = time.Now().Add(time.Minute)
	}
	dials, posts := new(int), new(int)
	d.m.onboarding.dial = func(_ context.Context, c onboardingCandidate, a onboardingAccess) (*onboardingSSH, error) {
		*dials++
		if !c.HostKeyTrusted || c.HostKeySHA256 != hostKey.Fingerprint || a.user != "fixture" {
			t.Fatal("unbound account or host dial")
		}
		return &onboardingSSH{hostPublicKey: hostKey}, nil
	}
	pool := clustertrust.NewPeerClientPool(d.m.mesh, 0)
	d.m.remoteHTTP, d.m.readyHTTP = pool, pool
	client, ok := pool.Client(run.Binding.Targets[0].Principal)
	if !ok {
		t.Fatal("fixture peer not pinned")
	}
	client.Transport = mpiReviewMemoryTransport(func(r *http.Request) (*http.Response, error) {
		*posts++
		if r.Method != http.MethodPost || r.URL.Path != diagnosticControlPath {
			t.Fatal("unexpected request")
		}
		var request diagnosticControlRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "mpi-review" || request.MPIReview == nil {
			t.Fatal("not a new review")
		}
		control := request.MPIReview
		if control.Record.OperationID != run.Public.OperationID || len(control.Requests) != 2 {
			t.Fatal("registered identity changed")
		}
		for i, target := range control.Record.Targets {
			if !sameManagedJSON(target.Registration, control.Requests[i].Target.Registration) {
				t.Fatal("artifact substitution")
			}
		}
		review := diagnosticMPIReview{ReviewID: strings.Repeat("d", 32), OperationID: strings.Repeat("e", 32), GroupID: "pair-smoke-" + strings.Repeat("e", 32), BuildOperationID: run.Public.OperationID, OwnerNodeID: control.Record.Targets[0].NodeID, Network: "management", Transport: "socket", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
		for _, target := range control.Record.Targets {
			review.Targets = append(review.Targets, diagnosticMPIReviewTarget{NodeID: target.NodeID, SSHAddress: target.Address, Address: target.Address, Interface: "eth0"})
		}
		body, _ := json.Marshal(review)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	// The chooser's one-address path returns without a network probe.
	peer, _ := d.m.peers.lookup(run.Binding.Targets[0].NodeID)
	if len(peer.addresses) != 1 {
		t.Fatal("fixture would need address probing")
	}
	return d, run, dials, posts
}

func TestDiagnosticMPINewReviewAfterRestartBindsReauthorizedAccess(t *testing.T) {
	d, run, dials, posts := mpiReauthorizedFixture(t)
	before, _ := json.Marshal(run)
	registryBefore, err := os.ReadFile(d.managedPath(run.Public.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if d.participantBindingCurrent(run.Binding.diagnosticParticipantBinding) == nil {
		t.Fatal("old approval unexpectedly current")
	}
	review, err := d.reviewMPI(context.Background(), diagnosticMPIRequest{BuildOperationID: run.Public.OperationID, Network: "management", DedicatedTestWindow: true})
	if err != nil || review.BuildOperationID != run.Public.OperationID || *dials != 2 || *posts != 1 {
		t.Fatalf("fresh review failed after reauthorization: %v dials=%d posts=%d", err, *dials, *posts)
	}
	after, _ := json.Marshal(run)
	registryAfter, err := os.ReadFile(d.managedPath(run.Public.OperationID))
	if err != nil || !bytes.Equal(before, after) || !bytes.Equal(registryBefore, registryAfter) || run.Binding.Generations != nil {
		t.Fatal("fresh review rewrote original binding or registry")
	}
	if d.participantBindingCurrent(run.Binding.diagnosticParticipantBinding) == nil {
		t.Fatal("fresh review revived old approval")
	}
}

func TestDiagnosticMPINewReviewDoesNotRewriteOldGenerationMap(t *testing.T) {
	d, run, _, _ := mpiReauthorizedFixture(t)
	old := map[string]string{}
	for _, target := range run.Binding.Targets {
		old[target.NodeID] = newOpID()
	}
	run.Binding.Generations = old
	before := map[string]string{}
	for node, generation := range old {
		before[node] = generation
	}
	if _, err := d.reviewMPI(context.Background(), diagnosticMPIRequest{BuildOperationID: run.Public.OperationID, Network: "management", DedicatedTestWindow: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, old) || !reflect.DeepEqual(before, run.Binding.Generations) || d.participantBindingCurrent(run.Binding.diagnosticParticipantBinding) == nil {
		t.Fatal("fresh review renewed the old approval")
	}
}

func TestDiagnosticMPINewReviewRejectsChangedAccessBeforeHandshake(t *testing.T) {
	for _, change := range []string{"missing", "expired", "user", "port", "candidate", "host-key", "empty-generation", "node-address", "cluster-pin"} {
		t.Run(change, func(t *testing.T) {
			d, run, dials, posts := mpiReauthorizedFixture(t)
			target := run.Binding.Targets[0]
			current := d.m.onboarding.targets[target.Candidate.CandidateID]
			switch change {
			case "missing":
				delete(d.m.onboarding.targets, target.Candidate.CandidateID)
			case "expired":
				current.expiresAt = time.Now().Add(-time.Second)
			case "user":
				current.access.user = "different-account"
			case "port":
				current.candidate.Port++
			case "candidate":
				current.candidate.CandidateID = newOpID()
			case "host-key":
				current.candidate.HostKeySHA256 = "SHA256:changed"
			case "empty-generation":
				current.accessGeneration = ""
			case "node-address":
				current.candidate.Address = "192.0.2.99"
			case "cluster-pin":
				run.Binding.Pins[target.Principal] = strings.Repeat("0", 64)
			}
			before, _ := json.Marshal(run)
			registryBefore, _ := os.ReadFile(d.managedPath(run.Public.OperationID))
			_, err := d.reviewMPI(context.Background(), diagnosticMPIRequest{BuildOperationID: run.Public.OperationID, Network: "management", DedicatedTestWindow: true})
			after, _ := json.Marshal(run)
			registryAfter, _ := os.ReadFile(d.managedPath(run.Public.OperationID))
			if err == nil || *dials != 0 || *posts != 0 || !bytes.Equal(before, after) || !bytes.Equal(registryBefore, registryAfter) {
				t.Fatalf("%s reached new review or changed retained state: %v dials=%d posts=%d", change, err, *dials, *posts)
			}
		})
	}
}

func TestDiagnosticMPINewReviewRejectsAccessChangeDuringKeyRead(t *testing.T) {
	for _, change := range []string{"generation", "expiry"} {
		t.Run(change, func(t *testing.T) {
			d, run, dials, posts := mpiReauthorizedFixture(t)
			original := d.m.onboarding.dial
			d.m.onboarding.dial = func(ctx context.Context, c onboardingCandidate, a onboardingAccess) (*onboardingSSH, error) {
				client, err := original(ctx, c, a)
				current := d.m.onboarding.targets[c.CandidateID]
				if change == "generation" {
					current.accessGeneration = newOpID()
				} else {
					current.expiresAt = time.Now().Add(-time.Second)
				}
				return client, err
			}
			if _, err := d.reviewMPI(context.Background(), diagnosticMPIRequest{BuildOperationID: run.Public.OperationID, Network: "management", DedicatedTestWindow: true}); err == nil || *dials != 1 || *posts != 0 {
				t.Fatalf("%s during key read was accepted: %v", change, err)
			}
		})
	}
}

func TestDiagnosticMPINewReviewRechecksAccessAndRecordAfterCoordinatorReply(t *testing.T) {
	for _, change := range []string{"generation", "expiry", "user", "port", "registry", "harmless-status-revision"} {
		t.Run(change, func(t *testing.T) {
			d, run, dials, posts := mpiReauthorizedFixture(t)
			client, _ := d.m.remoteHTTP.Client(run.Binding.Targets[0].Principal)
			original := client.Transport
			client.Transport = mpiReviewMemoryTransport(func(r *http.Request) (*http.Response, error) {
				response, err := original.RoundTrip(r)
				current := d.m.onboarding.targets[run.Binding.Targets[0].Candidate.CandidateID]
				switch change {
				case "generation":
					current.accessGeneration = newOpID()
				case "expiry":
					current.expiresAt = time.Now().Add(-time.Second)
				case "user":
					current.access.user = "different-account"
				case "port":
					current.candidate.Port++
				case "registry":
					if err := os.WriteFile(d.managedPath(run.Public.OperationID), []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				case "harmless-status-revision":
					run.Public.Revision++
				}
				return response, err
			})
			_, err := d.reviewMPI(context.Background(), diagnosticMPIRequest{BuildOperationID: run.Public.OperationID, Network: "management", DedicatedTestWindow: true})
			if (err == nil) != (change == "harmless-status-revision") || *dials != 2 || *posts != 1 {
				t.Fatalf("wrong final %s disposition: %v dials=%d posts=%d", change, err, *dials, *posts)
			}
			if len(d.operations) != 0 || len(d.cancels) != 0 || d.reservation != nil || run.Binding.Generations != nil {
				t.Fatal("review created execution or renewed retained approval")
			}
		})
	}
}
