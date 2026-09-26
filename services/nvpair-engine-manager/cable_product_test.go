// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// Transport and process seams are in-memory. The real review, shared certificate
// generations, approval, coordinator, cancellation and journal code all execute.
// No Manager.Run, TestMain, native command, helper, listener or socket is used.
type cableProductFixture struct {
	t              *testing.T
	s              *cableProductService
	dir            string
	ids            map[string]string
	mu             sync.Mutex
	events         []string
	changed        chan struct{}
	onArm          func(string)
	onStart        func(string) error
	failLaunch     string
	holdResult     bool
	eofResult      bool
	unknownCleanup bool
	oneWay         bool
	armLease       int64
}

func newCableProductFixture(t *testing.T) *cableProductFixture {
	t.Helper()
	m, _, dir := cableTestManager(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errors.New("fixture permits only passive GET")
		}
		return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
	})
	cableTestRemoteRead(t, m, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != controlCablePortsPath {
			return nil, errors.New("unexpected fixture paired request")
		}
		return cableTestResponse(t, cableTestPeerSnapshot(t)), nil
	})
	f := &cableProductFixture{t: t, s: m.cables, dir: dir, ids: map[string]string{}, changed: make(chan struct{}, 32), armLease: 30000}
	m.cableLocal.controlPort = 14323
	for node, address := range map[string]string{"host-owner": "127.0.0.1", "host-peer": "192.0.2.7"} {
		candidate, err := m.onboarding.addTarget(onboardingAddTargetRequest{Address: address, Port: 22, Label: node})
		if err != nil {
			t.Fatal(err)
		}
		stored := m.onboarding.targets[candidate.CandidateID]
		stored.candidate.AccessID, stored.candidate.AccessLabel = "synthetic-access-"+node, "synthetic-user (password)"
		stored.candidate.AccessAvailable, stored.candidate.HostKeyTrusted = true, true
		stored.candidate.HostKeySHA256 = "SHA256:" + strings.Repeat("a", 43)
		if node == "host-peer" {
			stored.candidate.HostKeySHA256 = "SHA256:" + strings.Repeat("c", 43)
		}
		stored.accessGeneration, stored.lifetime, stored.expiresAt = "synthetic-generation-1", "session", time.Now().Add(time.Minute)
		stored.access = onboardingAccess{user: "synthetic-user", password: "synthetic-access-input", elevationPassword: "synthetic-admin-input"}
		f.ids[node] = candidate.CandidateID
	}
	m.onboarding.dial = func(ctx context.Context, candidate onboardingCandidate, access onboardingAccess) (*onboardingSSH, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !candidate.HostKeyTrusted || access.user != "synthetic-user" || access.password != "synthetic-access-input" {
			return nil, errors.New("fixture access is not approved")
		}
		if candidate.Address != "127.0.0.1" && candidate.Address != "192.0.2.7" {
			return nil, errors.New("fixture target is outside selection")
		}
		return &onboardingSSH{testRun: func(ctx context.Context, command string, input io.Reader) ([]byte, error) {
			if command != onboardingPython(onboardingInspectScript) || input != nil {
				return nil, errors.New("fixture rejected non-inspection command")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return json.Marshal(onboardingPlatformInfo{Hostname: "synthetic-host", OS: "Linux", Arch: "aarch64", Home: "/home/synthetic-user", UID: 1000, ExistingPAIR: true, UserRuntime: true, FreeBytes: 1 << 30})
		}}, nil
	}
	f.s.workerBinding = func(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
		return cableWorkerBinding{NodeID: target.NodeID, Principal: target.Principal, UID: 1000, WorkerPath: "/opt/pair/nvpair-engine-manager", WorkerSHA256: strings.Repeat("b", 64), WorkerBytes: 128, ProfileDir: "/home/synthetic-user/.config/pair", CertificateSHA256: strings.Repeat("c", 64), Protocol: cableWorkerProtocol, RequesterAddress: "192.0.2.1"}, ctx.Err()
	}
	f.s.inspect = func(ctx context.Context, _ *onboardingSSH, plan cableLaunchPlan, _ *clustertrust.Mesh) (cableLaunchPlan, error) {
		if plan.Runtime == nil || plan.Info.UID != 1000 || !plan.Info.ExistingPAIR {
			return plan, errors.New("fixture expected a running native worker and current account")
		}
		plan.WorkerSHA256, plan.WorkerBytes, plan.CertificateSHA256 = plan.Runtime.WorkerSHA256, plan.Runtime.WorkerBytes, plan.Runtime.CertificateSHA256
		return plan, ctx.Err()
	}
	f.s.knownWorker = func(ctx context.Context, _ cableLaunchPlan) (string, error) {
		return "synthetic-controller-reference", ctx.Err()
	}
	f.s.launch = f.launch
	t.Cleanup(f.s.shutdown)
	return f
}

func (f *cableProductFixture) event(value string) {
	f.mu.Lock()
	f.events = append(f.events, value)
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}
func (f *cableProductFixture) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, event := range f.events {
		if strings.HasPrefix(event, prefix) {
			n++
		}
	}
	return n
}
func (f *cableProductFixture) awaitCount(prefix string, n int) {
	f.t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for f.count(prefix) < n {
		select {
		case <-f.changed:
		case <-timer.C:
			f.t.Fatalf("timed out waiting for %s count %d; got %d", prefix, n, f.count(prefix))
		}
	}
}

func (f *cableProductFixture) launch(ctx context.Context, _ *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if admin != "synthetic-admin-input" || request.Review.OwnerNodeID != plan.NodeID || request.Review.Permission != nil || request.Review.Available {
		return nil, errors.New("fixture launch does not match explicit bounded approval")
	}
	f.event("launch:" + plan.NodeID)
	if plan.NodeID == f.failLaunch {
		return nil, errors.New("synthetic launch acknowledgement lost")
	}
	commands := make(chan string, 4)
	reads := 0
	worker := &cableWorker{}
	worker.send = func(command cableProbeCommand) error {
		if command.RunID != request.RunID {
			return errors.New("wrong fixture run")
		}
		if command.Command == "start" {
			if f.count("armed:") != len(request.Review.Targets) {
				f.t.Error("a start was sent before every participant armed")
			}
			f.event("start-attempt:" + plan.NodeID)
			if f.onStart != nil {
				if err := f.onStart(plan.NodeID); err != nil {
					return err
				}
			}
			f.event("started:" + plan.NodeID)
		} else if command.Command == "cancel" {
			f.event("cancel:" + plan.NodeID)
		} else {
			return errors.New("unknown fixture command")
		}
		commands <- command.Command
		return nil
	}
	worker.read = func(ctx context.Context) (cableWorkerMessage, error) {
		reads++
		message := cableWorkerMessage{RunID: request.RunID, ReviewID: request.Review.ReviewID, cableProbeResult: cableProbeResult{Directness: "unverified", Observations: []cableProbeObservation{}}}
		if reads == 1 {
			if f.onArm != nil {
				f.onArm(plan.NodeID)
			}
			message.State, message.RemainingMs = "armed", f.armLease
			f.event("armed:" + plan.NodeID)
			return message, nil
		}
		var command string
		select {
		case command = <-commands:
		case <-ctx.Done():
			return message, ctx.Err()
		}
		if f.eofResult && (reads == 2 || f.unknownCleanup) {
			return message, io.EOF
		}
		if command == "start" && f.holdResult && reads == 2 {
			<-ctx.Done()
			return message, ctx.Err()
		}
		message.State, message.CleanupConfirmed = "completed", !f.unknownCleanup
		if command == "cancel" {
			message.State = "cancelled"
		}
		if message.State == "completed" && (!f.oneWay || plan.NodeID == "host-owner") {
			var local, peer cableprobe.PortRef
			for _, target := range request.Review.Targets {
				port := cableprobe.PortRef{NodeID: target.NodeID, SwitchID: target.Ports[0].SwitchID, PortName: target.Ports[0].PortName}
				if target.NodeID == plan.NodeID {
					local = port
				} else {
					peer = port
				}
			}
			message.Observations = []cableProbeObservation{{Local: local, Peer: peer, AgeMs: 25, Sequence: 1}}
			message.Sent, message.Received = 1, 1
		}
		f.event("terminal:" + plan.NodeID)
		return message, nil
	}
	worker.closeInput = func() error { f.event("input-closed:" + plan.NodeID); return nil }
	worker.wait = func(ctx context.Context) error { f.event("joined:" + plan.NodeID); return ctx.Err() }
	worker.close = func() { f.event("closed:" + plan.NodeID) }
	return worker, nil
}

func (f *cableProductFixture) review() cableprobe.Review {
	f.t.Helper()
	review, err := f.s.review(context.Background(), cableProductReviewRequest{ReviewRequest: cableTestSelection()})
	if err != nil {
		f.t.Fatal(err)
	}
	return review
}
func (f *cableProductFixture) ready() cableprobe.Review {
	f.t.Helper()
	review := f.review()
	if !review.Available {
		f.t.Fatalf("synthetic current prerequisites were unavailable: %+v", review.Permission)
	}
	return review
}
func (f *cableProductFixture) start(review cableprobe.Review) cableprobe.Run {
	f.t.Helper()
	run, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}
func (f *cableProductFixture) done(run cableprobe.Run) cableprobe.Run {
	f.t.Helper()
	f.s.mu.Lock()
	retained := f.s.runs[run.RunID]
	f.s.mu.Unlock()
	if retained == nil {
		f.t.Fatal("approved operation was not retained")
	}
	select {
	case <-retained.done:
	case <-time.After(3 * time.Second):
		f.t.Fatal("finite fixture operation did not finish")
	}
	got, err := f.s.status(cableprobe.StatusRequest{RunID: run.RunID})
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}
func (f *cableProductFixture) mutate(node string, change func(*onboardingPrivateTarget)) {
	f.s.m.onboarding.mu.Lock()
	defer f.s.m.onboarding.mu.Unlock()
	change(f.s.m.onboarding.targets[f.ids[node]])
}

func TestCableProductPrerequisitesAndExplicitApproval(t *testing.T) {
	for _, mode := range []string{"ready", "missing-access", "missing-admin", "missing-worker", "missing-host-trust", "expired-access"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			switch mode {
			case "missing-access":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.candidate.AccessAvailable = false })
			case "missing-admin":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.access.elevationPassword = "" })
			case "missing-worker":
				f.s.workerBinding = func(context.Context, cableprobe.Target) (cableWorkerBinding, error) {
					return cableWorkerBinding{}, errors.New("synthetic unsupported worker")
				}
			case "missing-host-trust":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.candidate.HostKeyTrusted = false })
			case "expired-access":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.expiresAt = time.Now().Add(-time.Second) })
			}
			review := f.review()
			if review.Available != (mode == "ready") || review.Permission == nil || len(review.Permission.Targets) != 2 {
				t.Fatalf("incorrect prerequisite result for %s", mode)
			}
			if _, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID}); err == nil {
				t.Fatal("declined administrator approval accepted")
			}
			if mode != "ready" {
				if _, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}); err == nil {
					t.Fatal("missing prerequisite reached start")
				}
			}
			if f.count("launch:") != 0 {
				t.Fatal("read-only or declined review launched a worker")
			}
		})
	}
}

func TestCableProductReciprocalAndIncompleteTerminalResults(t *testing.T) {
	for _, oneWay := range []bool{false, true} {
		t.Run(map[bool]string{false: "reciprocal", true: "one-way"}[oneWay], func(t *testing.T) {
			f := newCableProductFixture(t)
			f.oneWay = oneWay
			run := f.done(f.start(f.ready()))
			want := "reciprocal-observations"
			if oneWay {
				want = "incomplete"
			}
			if run.State != "completed" || run.Result != want || !run.CleanupConfirmed || run.FreshnessRemainingMs != 0 || run.RemainingMs != 0 || run.Directness != "unverified" {
				t.Fatalf("incorrect terminal result: %+v", run)
			}
			for _, edge := range run.Edges {
				if edge.Fresh {
					t.Fatal("terminal result retained live edge")
				}
			}
			if f.count("armed:") != 2 || f.count("started:") != 2 || f.count("closed:") != 2 || f.count("joined:") != 2 || f.count("input-closed:") != 2 {
				t.Fatal("worker lifecycle did not balance")
			}
			for _, id := range f.ids {
				if f.s.m.onboarding.targets[id].candidate.AccessAvailable || f.s.m.onboarding.targets[id].access.elevationPassword != "" {
					t.Fatal("temporary access retained after terminal cleanup")
				}
			}
			body, err := os.ReadFile(f.s.file(run.RunID))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "synthetic-admin-input") || strings.Contains(string(body), "synthetic-access-input") {
				t.Fatal("volatile access leaked into retained operation")
			}
		})
	}
}

func TestCableProductConcurrentApprovalIsOneRun(t *testing.T) {
	f := newCableProductFixture(t)
	f.holdResult = true
	review := f.ready()
	var wait sync.WaitGroup
	results := make(chan cableprobe.Run, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			run, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true})
			results <- run
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first cableprobe.Run
	for run := range results {
		if first.RunID == "" {
			first = run
		}
		if first.RunID != run.RunID {
			t.Fatal("same review created multiple operations")
		}
	}
	f.awaitCount("started:", 2)
	if f.count("launch:") != 2 {
		t.Fatal("concurrent approval relaunched participants")
	}
	if _, err := f.s.cancelRun(first.RunID); err != nil {
		t.Fatal(err)
	}
	if run := f.done(first); run.State != "cancelled" || !run.CleanupConfirmed {
		t.Fatalf("cancel did not finish the same run: %+v", run)
	}
}

func TestCableProductArmAndStartFailures(t *testing.T) {
	for _, mode := range []string{"early-arm-expiry", "launch-failure", "later-start-failure", "trust-after-arm", "trust-between-starts", "access-after-arm", "key-after-arm", "access-between-starts"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			switch mode {
			case "early-arm-expiry":
				f.armLease = 1
				f.onArm = func(node string) {
					if node == "host-peer" {
						time.Sleep(20 * time.Millisecond)
					}
				}
			case "launch-failure":
				f.failLaunch = "host-peer"
			case "later-start-failure":
				f.onStart = func(node string) error {
					if node == "host-peer" {
						return errors.New("synthetic later start failure")
					}
					return nil
				}
			case "trust-after-arm":
				f.onArm = func(node string) {
					if node == "host-peer" {
						clustertrusttest.WritePeerPin(t, f.dir, "principal-peer")
					}
				}
			case "trust-between-starts":
				f.onStart = func(node string) error {
					if node == "host-owner" {
						clustertrusttest.WritePeerPin(t, f.dir, "principal-peer")
					}
					return nil
				}
			case "access-after-arm", "key-after-arm":
				f.onArm = func(node string) {
					if node == "host-peer" {
						f.mutate("host-peer", func(p *onboardingPrivateTarget) {
							if mode == "key-after-arm" {
								p.changedKey = true
							} else {
								p.accessGeneration = "synthetic-generation-2"
							}
						})
					}
				}
			case "access-between-starts":
				f.onStart = func(node string) error {
					if node == "host-owner" {
						f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.accessGeneration = "synthetic-generation-2" })
					}
					return nil
				}
			}
			run := f.done(f.start(f.ready()))
			wantStarts := 0
			if mode == "later-start-failure" || mode == "trust-between-starts" || mode == "access-between-starts" {
				wantStarts = 1
			}
			if f.count("started:") != wantStarts || run.Result == "reciprocal-observations" || run.FreshnessRemainingMs != 0 || run.State != "failed" {
				t.Fatalf("failure crossed the arm/start barrier: mode=%s started=%d result=%+v", mode, f.count("started:"), run)
			}
			if mode == "launch-failure" {
				if run.CleanupConfirmed || !f.s.held() {
					t.Fatal("lost launch acknowledgement inferred cleanup")
				}
			} else if !run.CleanupConfirmed {
				t.Fatal("acknowledged fixture cleanup was lost")
			}
			if mode == "access-after-arm" || mode == "access-between-starts" {
				f.mutate("host-peer", func(p *onboardingPrivateTarget) {
					if p.accessGeneration != "synthetic-generation-2" || !p.candidate.AccessAvailable || p.access.elevationPassword == "" {
						t.Error("cleanup consumed access belonging to a newer generation")
					}
				})
			}
		})
	}
}

func TestCableProductTrustAndAccessChangesBeforeStart(t *testing.T) {
	for _, mode := range []string{"peer-certificate", "owner-certificate", "access-generation", "ssh-fingerprint", "changed-key", "expired-access", "review-expired", "review-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			review := f.ready()
			reason := "access-changed"
			switch mode {
			case "peer-certificate":
				reason = "trust-changed"
				clustertrusttest.WritePeerPin(t, f.dir, "principal-peer")
			case "owner-certificate":
				reason = "trust-changed"
				clustertrusttest.WriteKeypair(t, f.dir, "principal-owner")
			case "access-generation":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.accessGeneration = "synthetic-generation-2" })
			case "ssh-fingerprint":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.candidate.HostKeySHA256 = "SHA256:" + strings.Repeat("b", 43) })
			case "changed-key":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.changedKey = true })
			case "expired-access":
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.expiresAt = time.Now().Add(-time.Second) })
			case "review-expired":
				reason = "review-expired"
				f.s.mu.Lock()
				binding := f.s.reviews[review.ReviewID]
				binding.expires = time.Now().Add(-time.Second)
				f.s.reviews[review.ReviewID] = binding
				f.s.mu.Unlock()
			case "review-unavailable":
				reason = "review-unavailable"
				f.s.mu.Lock()
				binding := f.s.reviews[review.ReviewID]
				binding.public.Available = false
				binding.public.Reason = "synthetic-private-review-detail"
				f.s.reviews[review.ReviewID] = binding
				f.s.mu.Unlock()
			}
			request := cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}
			_, err := f.s.start(context.Background(), request)
			assertCableStartRefused(t, f, review, err, reason)
			var refusal *cableStartNotStartedError
			if _, err := f.s.start(context.Background(), request); err == nil || errors.As(err, &refusal) {
				t.Fatal("retired review became another definitive refusal or accepted run")
			}
		})
	}
}

func assertCableStartRefused(t *testing.T, f *cableProductFixture, review cableprobe.Review, err error, reason string) {
	t.Helper()
	var refusal *cableStartNotStartedError
	if !errors.As(err, &refusal) || refusal.result != (cableprobe.StartNotStarted{Disposition: "not-started", ReviewID: review.ReviewID, OwnerNodeID: review.OwnerNodeID, Reason: reason}) {
		t.Fatalf("expected exact fixed refusal %s, got %v", reason, err)
	}
	f.s.mu.Lock()
	_, retained := f.s.reviews[review.ReviewID]
	runs := len(f.s.runs)
	f.s.mu.Unlock()
	files, globErr := filepath.Glob(f.s.file("*"))
	if retained || runs != 0 || f.count("launch:") != 0 || globErr != nil || len(files) != 0 || f.s.held() {
		t.Fatal("definitive refusal retained a review, run, journal, worker or recovery hold")
	}
}

func TestCableProductApprovalUsesExactReviewedTargetsWithoutHTTP(t *testing.T) {
	f := newCableProductFixture(t)
	f.holdResult = true
	review := f.ready()
	expected := cableProbeTargetFacts(cloneCableRun(cableprobe.Run{Targets: review.Targets}).Targets)
	forbidden := cableTestTransport(func(*http.Request) (*http.Response, error) {
		panic("approval reread inventory after its successful review")
	})
	f.s.m.cableLocal.http.Transport = forbidden
	cableTestRemoteRead(t, f.s.m, forbidden)
	launch := f.s.launch
	f.s.launch = func(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
		if !reflect.DeepEqual(cableProbeTargetFacts(request.Review.Targets), expected) {
			return nil, errors.New("worker request changed the approved target manifest")
		}
		return launch(ctx, client, plan, request, admin)
	}
	run := f.start(review)
	if !reflect.DeepEqual(cableProbeTargetFacts(run.Targets), expected) {
		t.Fatal("accepted run changed the approved target manifest")
	}
	f.awaitCount("started:", 2)
	if _, err := f.s.cancelRun(run.RunID); err != nil {
		t.Fatal(err)
	}
	terminal := f.done(run)
	// Completion releases account access. The accepted review must still resolve
	// to its same run, even if its review lease subsequently expires.
	f.s.mu.Lock()
	binding := f.s.reviews[review.ReviewID]
	binding.expires = time.Now().Add(-time.Second)
	f.s.reviews[review.ReviewID] = binding
	f.s.mu.Unlock()
	again, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true})
	if err != nil || again.RunID != terminal.RunID || again.State != terminal.State || f.count("launch:") != 2 {
		t.Fatalf("accepted review was replaced or refused: %+v %v", again, err)
	}
}

func TestCableProductUncertainFailuresNeverBecomeDefinitiveRefusals(t *testing.T) {
	for _, mode := range []string{"missing-review", "declined", "cancelled", "retention", "recovery-hold", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			review := f.ready()
			request := cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "missing-review":
				request.ReviewID = "unknown-review"
			case "declined":
				request.ApproveAdmin = false
			case "cancelled":
				cancel()
			case "retention":
				if err := os.WriteFile(filepath.Dir(f.s.file("unused")), []byte("block journal directory"), 0600); err != nil {
					t.Fatal(err)
				}
			case "recovery-hold":
				f.s.recoveryFailed = true
			case "shutdown":
				f.s.shuttingDown = true
			}
			run, err := f.s.start(ctx, request)
			var refusal *cableStartNotStartedError
			if err == nil || errors.As(err, &refusal) || run.RunID != "" || f.count("launch:") != 0 {
				t.Fatalf("generic failure fabricated definitive refusal or launch: %+v %v", run, err)
			}
			f.s.mu.Lock()
			_, retained := f.s.reviews[review.ReviewID]
			runs := len(f.s.runs)
			f.s.mu.Unlock()
			if !retained || runs != 0 {
				t.Fatal("generic failure retired the known review or created a run")
			}
		})
	}
}

func TestCableProductStartWireKeepsRefusalRunAndErrorDistinct(t *testing.T) {
	for _, mode := range []string{"not-started", "accepted", "unknown", "status"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			review := f.ready()
			request := cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}
			if mode == "not-started" {
				f.s.mu.Lock()
				binding := f.s.reviews[review.ReviewID]
				binding.expires = time.Now().Add(-time.Second)
				f.s.reviews[review.ReviewID] = binding
				f.s.mu.Unlock()
			} else if mode == "unknown" {
				request.ReviewID = "unknown-review"
			}
			method := "engine:cable-start"
			params, _ := json.Marshal(request)
			if mode == "status" {
				method = "engine:cable-status"
				params, _ = json.Marshal(cableprobe.StatusRequest{ReviewID: review.ReviewID})
			}
			var output bytes.Buffer
			f.s.m.codec = NewCodec(&output)
			id := json.RawMessage("902")
			f.s.m.runCableProduct(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
			var wire Message
			decoder := json.NewDecoder(&output)
			if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || wire.ID == nil || string(*wire.ID) != string(id) {
				t.Fatal("cable handler did not emit one matching JSON-RPC frame")
			}
			if mode == "unknown" || mode == "status" {
				if wire.Error == nil || wire.Error.Code != -32000 || len(wire.Result) != 0 {
					t.Fatalf("generic or status error became a normal refusal: %+v", wire)
				}
				return
			}
			if wire.Error != nil {
				t.Fatalf("normal start result became a JSON-RPC error: %+v", wire.Error)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(wire.Result, &fields) != nil {
				t.Fatal("invalid normal start result")
			}
			if mode == "not-started" {
				var refusal cableprobe.StartNotStarted
				if json.Unmarshal(wire.Result, &refusal) != nil || len(fields) != 4 || refusal != (cableprobe.StartNotStarted{Disposition: "not-started", ReviewID: review.ReviewID, OwnerNodeID: review.OwnerNodeID, Reason: "review-expired"}) {
					t.Fatalf("definitive refusal wire contract changed: %s", wire.Result)
				}
				assertCableStartRefused(t, f, review, &cableStartNotStartedError{result: refusal}, "review-expired")
			} else {
				var run cableprobe.Run
				if json.Unmarshal(wire.Result, &run) != nil || run.RunID == "" || run.ReviewID != review.ReviewID || fields["disposition"] != nil {
					t.Fatalf("accepted run shape changed: %s", wire.Result)
				}
				f.done(run)
			}
		})
	}
}

func TestCableProductCancellationEOFAndRecovery(t *testing.T) {
	for _, mode := range []string{"pre-cancel", "cancel-running", "eof-clean", "eof-unknown", "terminal-unknown", "restart"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			f.holdResult = mode == "cancel-running" || mode == "restart"
			f.eofResult = mode == "eof-clean" || mode == "eof-unknown"
			f.unknownCleanup = mode == "eof-unknown" || mode == "terminal-unknown"
			review := f.ready()
			if mode == "pre-cancel" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				f.s.ctx = ctx
			}
			run := f.start(review)
			if f.holdResult {
				f.awaitCount("started:", 2)
				if current := f.review(); current.Available {
					t.Fatal("active operation allowed replacement review")
				}
				if mode == "restart" {
					reloaded := newCableProductService(f.s.m)
					retained, err := reloaded.status(cableprobe.StatusRequest{ReviewID: review.ReviewID})
					if err != nil || retained.RunID != run.RunID || retained.State != "failed" || retained.Result != "incomplete" || retained.CleanupConfirmed || retained.FreshnessRemainingMs != 0 || !reloaded.held() {
						t.Fatalf("restart invented cleanup or live success: %+v %v", retained, err)
					}
					if _, err = reloaded.cancelRun(run.RunID); err != nil {
						t.Fatal(err)
					}
					if !reloaded.held() {
						t.Fatal("cancel on reloaded operation invented cleanup")
					}
				}
				if _, err := f.s.cancelRun(run.RunID); err != nil {
					t.Fatal(err)
				}
			}
			terminal := f.done(run)
			if terminal.Result == "reciprocal-observations" || terminal.FreshnessRemainingMs != 0 {
				t.Fatal("cancel/EOF/uncertain cleanup became reciprocal pass")
			}
			if mode == "pre-cancel" && f.count("launch:") != 0 {
				t.Fatal("pre-cancelled coordinator launched a worker")
			}
			if f.unknownCleanup {
				if terminal.CleanupConfirmed || !f.s.held() {
					t.Fatal("unknown cleanup released replacement gate")
				}
			} else if !terminal.CleanupConfirmed {
				t.Fatal("known fixture cleanup was not confirmed")
			}
		})
	}
}

func TestCableProductAcceptedFingerprintIsTransientAndExact(t *testing.T) {
	f := newCableProductFixture(t)
	f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.candidate.HostKeyTrusted = false })
	request := cableProductReviewRequest{ReviewRequest: cableTestSelection()}
	request.AcceptedHostKeys = append(request.AcceptedHostKeys, struct {
		CandidateID string `json:"candidateId"`
		SHA256      string `json:"sha256"`
	}{f.ids["host-peer"], "SHA256:" + strings.Repeat("b", 43)})
	review, err := f.s.review(context.Background(), request)
	if err != nil || review.Available {
		t.Fatal("mismatched fingerprint approval accepted")
	}
	request.AcceptedHostKeys[0].SHA256 = f.s.m.onboarding.targets[f.ids["host-peer"]].candidate.HostKeySHA256
	review, err = f.s.review(context.Background(), request)
	if err != nil || !review.Available {
		t.Fatalf("exact transient fingerprint not admitted: %v", err)
	}
	if f.s.m.onboarding.targets[f.ids["host-peer"]].candidate.HostKeyTrusted {
		t.Fatal("transient consent became existing trust")
	}
	f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.accessGeneration = "synthetic-generation-2" })
	if _, err = f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}); err == nil {
		t.Fatal("access replacement retained old consent approval")
	}
	if f.count("launch:") != 0 {
		t.Fatal("stale consent launched workers")
	}
}

func TestCableProductRunningPolicyRevocationCancels(t *testing.T) {
	for _, kind := range []string{"access", "certificate"} {
		t.Run(kind, func(t *testing.T) {
			f := newCableProductFixture(t)
			f.holdResult = true
			review := f.review()
			run, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true})
			if err != nil {
				t.Fatal(err)
			}
			f.awaitCount("started:", 2)
			if kind == "access" {
				f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.accessGeneration = "replacement-generation" })
			} else {
				clustertrusttest.WritePeerPin(t, f.dir, "principal-peer")
			}
			f.awaitCount("cancel:", 2)
			terminal := f.done(run)
			if terminal.State != "failed" || terminal.Result == "reciprocal-observations" || !terminal.CleanupConfirmed || terminal.FreshnessRemainingMs != 0 {
				t.Fatalf("revoked run: %+v", terminal)
			}
		})
	}
}

func TestCableProductContradictoryOneWayObservationIsAmbiguous(t *testing.T) {
	targets := probeTestTargets(2, 1)
	peer := targets[1].Ports[0]
	peer.PortName = "p1"
	peer.Interfaces = append([]cableprobe.Interface(nil), peer.Interfaces...)
	peer.Interfaces[0].Index++
	peer.Interfaces[0].Name = "peer1"
	peer.Interfaces[0].MAC = "02:00:00:00:00:fe"
	targets[1].Ports = append(targets[1].Ports, peer)
	ref := func(node, port int) cableprobe.PortRef {
		p := targets[node].Ports[port]
		return cableprobe.PortRef{NodeID: targets[node].NodeID, SwitchID: p.SwitchID, PortName: p.PortName}
	}
	messages := []cableWorkerMessage{{cableProbeResult: cableProbeResult{State: "completed", Directness: "unverified", CleanupConfirmed: true}}, {cableProbeResult: cableProbeResult{State: "completed", Directness: "unverified", CleanupConfirmed: true}}}
	for p := 0; p < 2; p++ {
		messages[0].Observations = append(messages[0].Observations, cableProbeObservation{Local: ref(0, p), Peer: ref(1, p), Sequence: 1})
		messages[1].Observations = append(messages[1].Observations, cableProbeObservation{Local: ref(1, p), Peer: ref(0, p), Sequence: 1})
	}
	if _, verdict := reciprocalCableResult(targets, messages); verdict != "reciprocal-observations" {
		t.Fatalf("valid pair fixture: %s", verdict)
	}
	messages[0].Observations = append(messages[0].Observations, cableProbeObservation{Local: ref(0, 0), Peer: ref(1, 1), Sequence: 2})
	if _, verdict := reciprocalCableResult(targets, messages); verdict != "ambiguous" {
		t.Fatalf("contradictory partner omitted: %s", verdict)
	}
}

func TestCableProductMalformedRetainedStateHoldsRecovery(t *testing.T) {
	f := newCableProductFixture(t)
	review := f.review()
	run, err := f.s.start(context.Background(), cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	terminal := f.done(run)
	bad := cableProductRun{Public: terminal, OwnerPrincipal: "principal-owner"}
	bad.Public.State = "invented-terminal"
	if err := f.s.save(&bad); err != nil {
		t.Fatal(err)
	}
	reloaded := newCableProductService(f.s.m)
	if !reloaded.recoveryFailed || !reloaded.held() {
		t.Fatal("invalid journal silently removed the recovery gate")
	}
}
