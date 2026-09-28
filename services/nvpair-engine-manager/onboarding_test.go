// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"net"
	"nvpair-shared/discovery"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOnboardingNativePlatformDoesNotUseControllerArchitecture(t *testing.T) {
	for _, test := range []struct{ input, want string }{{"aarch64", "arm64"}, {"arm64", "arm64"}, {"x86_64", "amd64"}, {"amd64", "amd64"}} {
		os, arch, err := onboardingPlatform("Linux", test.input)
		if err != nil || os != "linux" || arch != test.want {
			t.Fatalf("platform=%s/%s err=%v", os, arch, err)
		}
	}
	for _, values := range [][2]string{{"Windows", "arm64"}, {"Darwin", "amd64"}, {"Linux", "x86_64"}} {
		osName, arch, err := onboardingPlatform(values[0], values[1])
		if err != nil || arch == "" || osName == "" {
			t.Fatalf("supported target %s/%s rejected: %v", values[0], values[1], err)
		}
	}
	if _, _, err := onboardingPlatform("Linux", "riscv64"); err == nil {
		t.Fatal("unsupported target guessed")
	}
	if _, _, err := onboardingPlatform("FreeBSD", "amd64"); err == nil {
		t.Fatal("unsupported operating system guessed")
	}
}

func TestOnboardingScopeSearchDispatchCancellationAndOverflow(t *testing.T) {
	var output bytes.Buffer
	m := NewManager(NewCodec(&output), newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil)
	scopeID := strings.Repeat("a", 32)
	m.onboarding.scopes = func(context.Context) ([]onboardingDiscoveryScope, error) {
		return []onboardingDiscoveryScope{{ScopeID: scopeID, Interface: "fixture", LocalAddress: "192.0.2.1", CIDR: "192.0.2.0/24", Eligible: true}}, nil
	}
	id := json.RawMessage(`10`)
	m.handleOnboarding(context.Background(), &Message{ID: &id, Method: "engine:onboarding-scopes", Params: json.RawMessage(`{}`)})
	if !bytes.Contains(output.Bytes(), []byte(scopeID)) {
		t.Fatal("real scopes dispatch did not return helper facts")
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	m.onboarding.search = func(ctx context.Context, id string, seeds []onboardingDiscoveredDevice) ([]onboardingDiscoveredDevice, error) {
		if id != scopeID {
			return nil, errors.New("wrong scope")
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() { _, err := m.onboarding.searchCandidates(context.Background(), scopeID, false); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("search did not start")
	}
	if _, err := m.onboarding.searchCandidates(context.Background(), scopeID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled search became successful inventory")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel acknowledgement did not settle original request")
	}
	m.onboarding.search = func(context.Context, string, []onboardingDiscoveredDevice) ([]onboardingDiscoveredDevice, error) {
		devices := []onboardingDiscoveredDevice{}
		for i := 0; i < 33; i++ {
			devices = append(devices, onboardingDiscoveredDevice{Address: fmt.Sprintf("192.0.2.%d", i+1), Port: 22})
		}
		return devices, nil
	}
	if _, err := m.onboarding.searchCandidates(context.Background(), scopeID, false); err == nil || len(m.onboarding.targets) != 0 {
		t.Fatal("overflow silently admitted partial discovery")
	}
}
func TestOnboardingLocalImportDispatchVerifiesNativePackage(t *testing.T) {
	file, artifact := writeOnboardingFixture(t, onboardingFixtureEntries(183))
	var output bytes.Buffer
	m := NewManager(NewCodec(&output), newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil)
	id := json.RawMessage(`11`)
	params, _ := json.Marshal(map[string]string{"file": file})
	m.handleOnboarding(context.Background(), &Message{ID: &id, Method: "engine:onboarding-import-artifact", Params: params})
	var result struct {
		Result onboardingArtifact `json:"result"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Result.SHA256 != artifact.SHA256 || result.Result.Arch != "arm64" || result.Result.Provenance != "engineering" {
		t.Fatalf("import did not verify and expose truthful native metadata: %s", output.Bytes())
	}
	if err := os.WriteFile(file, []byte("changed after import"), 0600); err != nil {
		t.Fatal(err)
	}
	source := m.onboarding.imports[result.Result.ArtifactID]
	if _, _, err := verifyOnboardingArchive(source.File, result.Result); err != nil {
		t.Fatal("reviewed snapshot followed changed source file")
	}
	if runtime.GOOS == "windows" {
		for _, path := range []string{`\\server\share\package.tar.gz`, `\\?\C:\package.tar.gz`, `\\.\pipe\package`, `C:\package.tar.gz:stream`} {
			if onboardingLocalArtifactPath(path) {
				t.Fatal("network/device path accepted before file I/O")
			}
		}
	}
}
func TestOnboardingCurrentTrustRejectsRevocationAfterReview(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	fingerprint := ssh.FingerprintSHA256(key)
	for _, test := range []struct {
		name    string
		err     error
		blocked bool
	}{
		{"known", nil, false}, {"unknown", &knownhosts.KeyError{}, false},
		{"changed", &knownhosts.KeyError{Want: []knownhosts.KnownKey{{Key: key}}}, true},
		{"revoked", &knownhosts.RevokedError{}, true}, {"certificate", errors.New("certificate validation denied"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := func(string, net.Addr, ssh.PublicKey) error { return test.err }
			err := verifyOnboardingCurrentTrust(fingerprint, checker, "192.0.2.1:22", &net.TCPAddr{}, key)
			if (err != nil) != test.blocked {
				t.Fatal("fresh trust-store denial did not fence reviewed fingerprint")
			}
		})
	}
}
func TestOnboardingTrustRequiresExactKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if verifyOnboardingFingerprint(ssh.FingerprintSHA256(key), key) != nil {
		t.Fatal("exact host key rejected")
	}
	if verifyOnboardingFingerprint("", key) == nil || verifyOnboardingFingerprint("SHA256:wrong", key) == nil {
		t.Fatal("unverified host accepted")
	}
}
func TestOnboardingDecodeNeverEchoesTransientSecret(t *testing.T) {
	const secret = "private-test-value"
	var request onboardingBatchAccess
	err := onboardingDecode([]byte(`{"password":"`+secret+`","shell":"not-allowed"}`), &request)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("invalid access request leaked input")
	}
}
func TestOnboardingDiscoveryProducesBareCandidateNotMember(t *testing.T) {
	m := NewManager(nil, newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil)
	s := m.onboarding
	s.discover = func(context.Context) []discovery.Node {
		return []discovery.Node{{Host: "spark-test.local.", Port: 22, Addresses: []string{"192.0.2.10"}}}
	}
	raw, err := s.candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(raw)
	if strings.Contains(string(data), "nodeUuid") || strings.Contains(string(data), "nodeId") {
		t.Fatal("discovery fabricated a PAIR member")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.targets) != 1 {
		t.Fatal("candidate absent")
	}
	for _, target := range s.targets {
		if !onboardingID.MatchString(target.candidate.CandidateID) || target.candidate.AccessAvailable || target.candidate.HostKeyTrusted {
			t.Fatal("unverified candidate gained access/trust")
		}
	}
}
func TestOnboardingAddTargetDeduplicatesEndpoint(t *testing.T) {
	m := NewManager(nil, newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil)
	s := m.onboarding
	a, err := s.addTarget(onboardingAddTargetRequest{Address: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.addTarget(onboardingAddTargetRequest{Address: "192.0.2.1", Port: 22})
	if err != nil || a.CandidateID != b.CandidateID {
		t.Fatal("duplicate endpoint acquired new candidate")
	}
	if _, err = s.addTarget(onboardingAddTargetRequest{Address: "192.0.2.1; unsafe"}); err == nil {
		t.Fatal("invalid address accepted")
	}
}
func TestOnboardingActualManagerDispatchIsCallableWithoutExecutionStubSuccess(t *testing.T) {
	var output bytes.Buffer
	firstID := json.RawMessage(`1`)
	secondID := json.RawMessage(`2`)
	m := NewManager(NewCodec(&output), newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil)
	m.handleOnboarding(context.Background(), &Message{ID: &firstID, Method: "engine:onboarding-add-target", Params: json.RawMessage(`{"address":"192.0.2.11","port":22}`)})
	var result struct {
		Result onboardingCandidate `json:"result"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Result.CandidateID == "" {
		t.Fatalf("real dispatch=%s", output.Bytes())
	}
	output.Reset()
	m.handleOnboarding(context.Background(), &Message{ID: &secondID, Method: "engine:onboarding-approve", Params: json.RawMessage(`{"reviewId":"not-ready"}`)})
	if !bytes.Contains(output.Bytes(), []byte(`"error"`)) {
		t.Fatal("unfinished approve pretended to succeed")
	}
}

func onboardingRecoveryAccessFixture(t *testing.T) (*onboardingControllerFixture, *onboardingRun, onboardingBatchAccess, *int) {
	t.Helper()
	f := newOnboardingControllerFixture(t, 2)
	run := &onboardingRun{Public: onboardingOperation{OperationID: newOpID(), ReviewID: newOpID(), Revision: 5, State: "failed", Targets: []onboardingTargetState{}}, ControllerNodeID: onboardingFixtureController, Plans: map[string]onboardingPlan{}}
	for i, id := range f.ids {
		p := f.s.targets[id]
		lifetime := "session"
		if i == 1 {
			lifetime = "persistent"
		}
		run.Plans[id] = onboardingPlan{Candidate: p.candidate, Username: "approved-user", AuthKind: "password", Review: onboardingReviewTarget{CandidateID: id, HostKeySHA256: p.candidate.HostKeySHA256, StartupLifetime: lifetime, NeedsLinger: i == 1}, LingerChanged: i == 1}
		run.Public.Targets = append(run.Public.Targets, onboardingTargetState{CandidateID: id, Stage: "verification-failed", CanRetry: true, CanCancel: true})
		p.access = onboardingAccess{}
		p.candidate.AccessAvailable = false
	}
	f.s.operations[run.Public.OperationID] = run
	calls := new(int)
	f.s.observeKey = func(_ context.Context, c onboardingCandidate, _ string) (string, bool, bool, error) {
		*calls++
		return c.HostKeySHA256, false, false, nil
	}
	request := onboardingBatchAccess{OperationID: run.Public.OperationID, CandidateIDs: f.ids, Username: "approved-user", Auth: "password", Password: "volatile-recovery-secret"}
	return f, run, request, calls
}
func TestOnboardingRecoveryAccessPreservesEachPlanAndHasNoEffects(t *testing.T) {
	f, run, request, calls := onboardingRecoveryAccessFixture(t)
	before := onboardingMarshal(run)
	if _, err := f.s.bindAccess(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || !bytes.Equal(before, onboardingMarshal(run)) || len(f.s.reviews) != 0 || f.s.active != "" {
		t.Fatal("access changed operation or created review/execution")
	}
	for _, id := range f.ids {
		p := f.s.targets[id]
		if !p.candidate.AccessAvailable || p.access.password != request.Password || p.lifetime != run.Plans[id].Review.StartupLifetime || p.candidate.HostKeySHA256 != run.Plans[id].Review.HostKeySHA256 || p.candidate.HostKeyTrusted {
			t.Fatal("recovery strengthened or replaced approved intent")
		}
	}
	if f.invites != 0 || f.responses != 0 {
		t.Fatal("reauthorization invoked pairing")
	}
	for _, d := range f.devices {
		if d.transfers != 0 || d.started {
			t.Fatal("reauthorization invoked install/start")
		}
	}
}
func TestOnboardingRecoveryAccessRejectsChangedIntentBeforeHostObservation(t *testing.T) {
	for _, kind := range []string{"extra-target", "account", "lifetime", "paired-target", "missing-operation", "busy"} {
		t.Run(kind, func(t *testing.T) {
			f, run, request, calls := onboardingRecoveryAccessFixture(t)
			switch kind {
			case "extra-target":
				c, _ := f.s.addTarget(onboardingAddTargetRequest{Address: "192.0.2.99"})
				request.CandidateIDs = []string{f.ids[0], c.CandidateID}
			case "account":
				request.Username = "different-user"
			case "lifetime":
				request.StartupLifetime = "persistent"
			case "paired-target":
				run.Public.Targets[0].Stage = "paired"
			case "missing-operation":
				request.OperationID = newOpID()
			case "busy":
				f.s.active = run.Public.OperationID
			}
			if _, err := f.s.bindAccess(context.Background(), request); err == nil || *calls != 0 {
				t.Fatal("changed intent reached host observation")
			}
			for _, id := range f.ids {
				if f.s.targets[id].candidate.AccessAvailable {
					t.Fatal("failed batch staged partial access")
				}
			}
		})
	}
}
func TestOnboardingRecoveryAccessRejectsChangedTrustAndController(t *testing.T) {
	for _, kind := range []string{"changed-key", "revoked", "unavailable", "controller", "late-operation"} {
		t.Run(kind, func(t *testing.T) {
			f, run, request, _ := onboardingRecoveryAccessFixture(t)
			original := f.s.observeKey
			f.s.observeKey = func(ctx context.Context, c onboardingCandidate, user string) (string, bool, bool, error) {
				switch kind {
				case "changed-key":
					return "SHA256:different", false, false, nil
				case "revoked":
					return c.HostKeySHA256, false, true, nil
				case "unavailable":
					return "", false, true, errors.New("trust unavailable")
				case "late-operation":
					f.s.mu.Lock()
					run.Public.Revision++
					f.s.mu.Unlock()
				}
				return original(ctx, c, user)
			}
			if kind == "controller" {
				f.s.testCluster = func(context.Context, string, any) (json.RawMessage, error) {
					return []byte(`{"nodeUuid":"replacement-controller"}`), nil
				}
			}
			before := run.Plans[f.ids[0]].Review.HostKeySHA256
			if _, err := f.s.bindAccess(context.Background(), request); err == nil {
				t.Fatal("changed recovery boundary was accepted")
			}
			for _, id := range f.ids {
				if f.s.targets[id].candidate.AccessAvailable || f.s.targets[id].access.password != "" {
					t.Fatal("failed recovery persisted credentials")
				}
			}
			if run.Plans[f.ids[0]].Review.HostKeySHA256 != before {
				t.Fatal("original pin overwritten")
			}
		})
	}
}
func TestOnboardingRecoveryAccessSubsetAndOrdinaryAccessCompatibility(t *testing.T) {
	f, _, request, calls := onboardingRecoveryAccessFixture(t)
	request.CandidateIDs = f.ids[1:]
	if _, err := f.s.bindAccess(context.Background(), request); err != nil || *calls != 1 {
		t.Fatal("exact unfinished subset was rejected")
	}
	if f.s.targets[f.ids[0]].candidate.AccessAvailable || f.s.targets[f.ids[1]].lifetime != "persistent" {
		t.Fatal("subset modified unselected target")
	}
	request.OperationID = ""
	request.CandidateIDs = f.ids[:1]
	if _, err := f.s.bindAccess(context.Background(), request); err != nil || f.s.targets[f.ids[0]].lifetime != "persistent" {
		t.Fatal("ordinary access default changed")
	}
}
