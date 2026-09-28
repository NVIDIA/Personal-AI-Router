// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestVLLMGroupRestoredOwnerAttachesWithoutChangingRetainedState(t *testing.T) {
	for _, clean := range []bool{true, false} {
		for _, create := range []bool{true, false} {
			name := "held"
			if clean {
				name = "clean"
			}
			if create {
				name += "/review"
			} else {
				name += "/status-or-cleanup"
			}
			t.Run(name, func(t *testing.T) {
				f := vllmResourceFixture(t)
				plan := vllmGroupTestPlan(2)
				digest, err := vllmGroupPlanDigest(plan)
				if err != nil {
					t.Fatal(err)
				}
				run := vllmGroupRun{RunID: "0123456789abcdef0123456789abcdef", Generation: 1, Plan: plan, PlanDigest: digest, State: "failed", CleanupConfirmed: clean}
				for _, member := range plan.Members {
					run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: member.NodeID, Attempted: true, Started: true, CleanupConfirmed: clean})
				}
				if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.st.installDir, "serving-group.json")
				if err := writeVLLMJSON(f.st.installDir, path, run); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				f.st.opMu.Lock()
				g, err := newVLLMServingGroup(f.st, path)
				f.st.opMu.Unlock()
				if err != nil || groupStatus(g).ActivationEnabled {
					t.Fatalf("restored fixture: %v", err)
				}
				retained := g.status()
				f.e.groupPeer = &vllmGroupPeer{native: func(context.Context, vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
					t.Fatal("attachment invoked native work")
					return vllmGroupPeerResult{}, nil
				}}
				for range 2 {
					owner, err := f.e.groupOwner(create)
					if err != nil || owner != g || !groupStatus(g).ActivationEnabled {
						t.Fatalf("restored owner did not attach idempotently: %v", err)
					}
				}
				review, err := g.reviewPlan(plan)
				if clean && (err != nil || !review.ActivationEnabled) {
					t.Fatalf("clean restored owner cannot review: %v", err)
				}
				if !clean {
					if err == nil {
						t.Fatal("attachment admitted review over unresolved cleanup")
					}
					if _, err := g.start(context.Background(), "old-review"); err == nil {
						t.Fatal("attachment admitted a replacement generation")
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(retained, g.status()) || g.reserved() != !clean {
					t.Fatal("attachment changed the retained journal, run or cleanup hold")
				}
			})
		}
	}
}

func TestVLLMGroupOwnerRestoresRankHoldAfterManagerRestart(t *testing.T) {
	f := vllmResourceFixture(t)
	plan := vllmGroupTestPlan(2)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{
		RunID:            strings.Repeat("a", 32),
		Generation:       13,
		Plan:             plan,
		PlanDigest:       digest,
		State:            "cleanup-required",
		CleanupConfirmed: false,
	}
	for _, member := range plan.Members {
		run.Ranks = append(run.Ranks, vllmGroupRank{
			NodeID: member.NodeID, Attempted: true, Started: true, CleanupConfirmed: false,
		})
	}
	if err := os.MkdirAll(f.st.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-group.json"), run); err != nil {
		t.Fatal(err)
	}
	systemPlan := vllmRankSystemPlan{
		Owner: vllmSystemRankOwner, RunID: run.RunID, Generation: run.Generation,
		Rank: 1, PlanDigest: digest, NodeID: plan.Members[1].NodeID, UID: os.Getuid(),
	}
	receipt := vllmRankReceipt{
		RunID: run.RunID, Generation: run.Generation, PlanDigest: digest, Rank: 1,
		State: "stopping", CleanupConfirmed: false, SystemPlan: &systemPlan,
	}
	rankPath := filepath.Join(f.st.installDir, "serving-rank.json")
	if err := writeVLLMJSON(f.st.installDir, rankPath, receipt); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(rankPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.e.groupOwner(true); err != nil {
		t.Fatal(err)
	}
	f.st.mu.Lock()
	rank := f.st.vllmRank
	f.st.mu.Unlock()
	if rank == nil || !rank.reserved() || rejectVLLMGroupOwnerMutation(f.st, "ordinary start") == nil {
		t.Fatal("manager restart lost the retained rank cleanup hold")
	}
	rank.mu.Lock()
	gotPlan := cloneVLLMGroupPlan(rank.binding.Plan)
	rank.mu.Unlock()
	if !reflect.DeepEqual(gotPlan, plan) {
		t.Fatal("restored rank did not recover its exact retained group plan")
	}
	after, err := os.ReadFile(rankPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("restoring rank custody rewrote the retained receipt")
	}
}

func TestVLLMGroupOwnerLookupDoesNotWaitForBusyLifecycle(t *testing.T) {
	f := vllmResourceFixture(t)
	g, err := f.e.groupOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	f.st.opMu.Lock()
	defer f.st.opMu.Unlock()
	owner, err := f.e.groupOwner(false)
	if err != nil || owner != g {
		t.Fatalf("busy lifecycle hid its existing owner: %v", err)
	}
}

func TestVLLMGroupAttachedStatusDoesNotOccupyLifecycle(t *testing.T) {
	f := vllmResourceFixture(t)
	f.e.groupPeer = &vllmGroupPeer{native: func(context.Context, vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
		t.Fatal("owner lookup invoked native work")
		return vllmGroupPeerResult{}, nil
	}}
	g, err := f.e.groupOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	// Pause the actual status read at g.mu. Mutex waits are not durable in
	// synctest, so observe its blocked stack instead of assuming a sleep ordered it.
	g.mu.Lock()
	done := make(chan struct{})
	var owner *vllmServingGroup
	var lookupErr error
	go func() {
		defer close(done)
		owner, lookupErr = f.e.groupOwner(false)
	}()
	blocked := false
	stack := make([]byte, 64<<10)
	deadline := time.Now().Add(2 * time.Second)
	for !blocked && time.Now().Before(deadline) {
		for _, frame := range bytes.Split(stack[:runtime.Stack(stack, true)], []byte("\n\n")) {
			if bytes.Contains(frame, []byte("(*Executor).groupOwner(")) && bytes.Contains(frame, []byte("(*Mutex).lockSlow(")) {
				blocked = true
				break
			}
		}
		runtime.Gosched()
	}
	available := f.st.opMu.TryLock()
	if available {
		f.st.opMu.Unlock()
	}
	g.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("status lookup did not finish after releasing its read lock")
	}
	if !blocked || lookupErr != nil || owner != g {
		t.Fatalf("status ordering was not established: blocked=%v error=%v", blocked, lookupErr)
	}
	if !available {
		t.Fatal("attached status occupied opMu and would reject concurrent Prepare")
	}
}

func groupControlTestCall(t *testing.T, m *Manager, method string, params any) Message {
	t.Helper()
	var out bytes.Buffer
	m.codec = NewCodec(&out)
	id := json.RawMessage(`1`)
	raw, _ := json.Marshal(params)
	m.handleVLLMGroup(context.Background(), &Message{ID: &id, Method: method, Params: raw})
	var reply Message
	if json.Unmarshal(out.Bytes(), &reply) != nil {
		t.Fatal("missing group reply")
	}
	return reply
}

func TestVLLMGroupPublicReviewRemainsProposalAndNativeStartRefuses(t *testing.T) {
	p, request, _, _ := vllmGroupPeerFixture(t, true)
	f := vllmResourceFixture(t)
	p.m.exec = f.e
	f.e.groupPeer = p
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	r := groupControlTestCall(t, p.m, "engine:vllm-group-status", struct{}{})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	r = groupControlTestCall(t, p.m, "engine:vllm-group-review", map[string]any{"plan": request.Plan})
	if r.Error == nil {
		t.Fatal("public caller-supplied hashes were accepted")
	}
	r = groupControlTestCall(t, p.m, "engine:vllm-group-review", map[string]any{"selection": vllmGroupSelection{NodeIDs: []string{"host-a", "host-b"}, Model: request.Plan.Model}})
	if r.Error == nil {
		t.Fatal("review invented missing local model/runtime facts")
	}
	g, err := p.m.exec.groupOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	review, err := g.reviewPlan(request.Plan)
	if err != nil || review.ActivationEnabled || review.ReviewID == "" {
		t.Fatal("review claimed native readiness")
	}
	r = groupControlTestCall(t, p.m, "engine:vllm-group-start", map[string]string{"reviewId": review.ReviewID})
	if r.Error == nil || f.st.vllmGroup.reserved() {
		t.Fatal("native start bypassed hold")
	}
	if _, err := os.Stat(f.st.vllmGroup.path); !os.IsNotExist(err) {
		t.Fatal("native refusal created ownership effects")
	}
	r = groupControlTestCall(t, p.m, "engine:vllm-group-review", map[string]any{"plan": request.Plan, "url": "https://example.invalid"})
	if r.Error == nil {
		t.Fatal("caller endpoint accepted")
	}
}

func TestVLLMGroupCompletedPublicStopDoesNotStopLaterStandaloneIntent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := vllmResourceFixture(t)
		g, err := newVLLMServingGroup(f.st, filepath.Join(t.TempDir(), "group.json"))
		if err != nil {
			t.Fatal(err)
		}
		g.call = func(context.Context, vllmGroupBinding, string) error { return nil }
		run := vllmGroupTestStart(t, g, context.Background(), 2)
		synctest.Wait()
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if err := f.e.setDesiredEnabled("vllm", true); err != nil {
			t.Fatal(err)
		}
		f.st.mu.Lock()
		f.st.running = true
		f.st.mu.Unlock()
		m := &Manager{exec: f.e}
		r := groupControlTestCall(t, m, "engine:vllm-group-stop", map[string]any{"runId": run.RunID, "generation": run.Generation})
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		on, known, err := f.e.desired.get("vllm")
		if err != nil || !known || !on || !f.st.running || !g.status().CleanupConfirmed {
			t.Fatal("consumed group stop changed standalone intent")
		}
	})
}
