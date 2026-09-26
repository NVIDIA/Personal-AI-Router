// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrusttest"
)

func TestCableRetainedFreshBackendDiscoversAndLoadsOriginal(t *testing.T) {
	x := newCleanupProductFixture(t)
	loaded := newCableProductService(x.f.s.m)
	defer loaded.shutdown()
	if len(loaded.reviews) != 0 || len(loaded.cleanupReviews) != 0 {
		t.Fatal("fresh owner manufactured review state")
	}
	catalog, err := loaded.retainedRuns(context.Background())
	if err != nil || !catalog.Held || catalog.Limited || len(catalog.Runs) != 1 {
		t.Fatalf("retained run was undiscoverable: %+v %v", catalog, err)
	}
	row := catalog.Runs[0]
	if row.RunID != x.run.RunID || row.ReviewID != x.run.ReviewID || row.OwnerNodeID != x.run.OwnerNodeID || row.Revision != x.run.Revision || row.State != "failed" || row.CleanupConfirmed {
		t.Fatal("summary changed original identity or cleanup")
	}
	var nodes []string
	var ports []cableprobe.PortRef
	for _, target := range x.run.Targets {
		nodes = append(nodes, target.NodeID)
		for _, port := range target.Ports {
			ports = append(ports, cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName})
		}
	}
	if !reflect.DeepEqual(row.NodeIDs, nodes) || !reflect.DeepEqual(row.Ports, ports) {
		t.Fatal("summary borrowed a new selection or current NIC facts")
	}
	run, err := loaded.status(cableprobe.StatusRequest{RunID: row.RunID})
	if err != nil || !reflect.DeepEqual(run, x.run) {
		t.Fatalf("explicit original load changed run: %v", err)
	}
	if x.launches.Load() != 0 || x.finishes.Load() != 0 || len(loaded.reviews) != 0 || len(loaded.cleanupReviews) != 0 || len(loaded.cleanupRecords) != 0 {
		t.Fatal("read-only resume created review, approval or inspection work")
	}
	data, err := os.ReadFile(loaded.file(run.RunID))
	if err != nil || !bytes.Equal(data, x.original) {
		t.Fatal("discovery/load rewrote original envelope")
	}
	encoded, _ := json.Marshal(catalog)
	for _, forbidden := range []string{"synthetic-admin-input", "synthetic-access-input", "workerPath", "candidateId", "principal-owner"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatal("summary exposed private plan/access data")
		}
	}
}

func TestCableRetainedSelectionOwnershipAndVisibleForeignHold(t *testing.T) {
	for _, mode := range []string{"saved-principal", "saved-host", "current-principal", "current-host"} {
		t.Run(mode, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			s := x.f.s
			initial, err := s.retainedRuns(context.Background())
			if err != nil || len(initial.Runs) != 1 {
				t.Fatal("original owner could not discover its run")
			}
			switch mode {
			case "saved-principal":
				s.runs[x.run.RunID].OwnerPrincipal = "other-principal"
			case "saved-host":
				s.runs[x.run.RunID].Public.OwnerNodeID = "other-host"
			case "current-principal":
				clustertrusttest.Join(t, x.f.dir, "synthetic-cluster", "other-principal", "principal-peer")
			case "current-host":
				s.m.cableLocal.nodeID = "other-host"
			}
			catalog, err := s.retainedRuns(context.Background())
			if err != nil || !catalog.Held || !catalog.Limited || catalog.Reason == "" || len(catalog.Runs) != 0 {
				t.Fatalf("foreign hold appeared empty/free: %+v %v", catalog, err)
			}
			for _, query := range []cableprobe.StatusRequest{{RunID: x.run.RunID}, {ReviewID: x.run.ReviewID}} {
				if _, err := s.status(query); err == nil {
					t.Fatal("known ID bypassed current original owner checks")
				}
			}
			data, err := os.ReadFile(s.file(x.run.RunID))
			if err != nil || !bytes.Equal(data, x.original) {
				t.Fatal("owner refusal rewrote original")
			}
		})
	}
}

func TestCableRetainedRestartKeepsOriginalBytesAndCorruptHoldVisible(t *testing.T) {
	for _, mode := range []string{"preparing", "running", "cancelling", "corrupt-neighbor"} {
		t.Run(mode, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			s := x.f.s
			if mode == "corrupt-neighbor" {
				if err := os.WriteFile(s.file(strings.Repeat("f", 32)), []byte("invalid fixture JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				r := *s.runs[x.run.RunID]
				r.Public = cloneCableRun(r.Public)
				r.Public.State = mode
				r.Public.FinishedAt = 0
				if err := s.save(&r); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(s.file(x.run.RunID))
			if err != nil {
				t.Fatal(err)
			}
			loaded := newCableProductService(s.m)
			defer loaded.shutdown()
			catalog, err := loaded.retainedRuns(context.Background())
			if err != nil || !catalog.Held || len(catalog.Runs) != 1 || catalog.Limited != (mode == "corrupt-neighbor") {
				t.Fatalf("restart history disappeared: %+v %v", catalog, err)
			}
			run, err := loaded.status(cableprobe.StatusRequest{RunID: catalog.Runs[0].RunID})
			if err != nil || run.RunID != x.run.RunID || run.ReviewID != x.run.ReviewID || run.State != "failed" || run.CleanupConfirmed {
				t.Fatal("restart view manufactured completion or new identity")
			}
			if mode != "corrupt-neighbor" && run.Revision != x.run.Revision+1 {
				t.Fatal("existing interrupted-run projection changed")
			}
			after, err := os.ReadFile(s.file(x.run.RunID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("startup/discovery/load changed the original record bytes")
			}
			if len(loaded.reviews) != 0 || len(loaded.cleanupReviews) != 0 || x.launches.Load() != 0 {
				t.Fatal("resume automatically requested new work")
			}
		})
	}
}

func TestCableRetainedDeterministicBoundedAttentionList(t *testing.T) {
	x := newCleanupProductFixture(t)
	s := x.f.s
	base := s.runs[x.run.RunID]
	s.runs = map[string]*cableProductRun{}
	for i, state := range []string{"failed", "cancelled", "running", "preparing", "cancelling", "completed"} {
		copy := *base
		copy.Public = cloneCableRun(base.Public)
		copy.Public.RunID = strings.Repeat(string(rune('a'+i)), 32)
		copy.Public.State = state
		copy.Public.StartedAt = 1000 + int64(i/2)
		copy.Public.CleanupConfirmed = state == "completed"
		s.runs[copy.Public.RunID] = &copy
	}
	catalog, err := s.retainedRuns(context.Background())
	if err != nil || !catalog.Held || catalog.Limited || len(catalog.Runs) != 5 {
		t.Fatalf("attention selection changed: %+v %v", catalog, err)
	}
	for i := 1; i < len(catalog.Runs); i++ {
		before, after := catalog.Runs[i-1], catalog.Runs[i]
		if before.StartedAt < after.StartedAt || (before.StartedAt == after.StartedAt && before.RunID > after.RunID) {
			t.Fatal("catalog order is not deterministic")
		}
	}
	s.recoveryFailed = true
	catalog, err = s.retainedRuns(context.Background())
	if err != nil || !catalog.Held || !catalog.Limited || catalog.Reason == "" || len(catalog.Runs) != 5 {
		t.Fatal("incomplete history was hidden or valid owner rows discarded")
	}
	s.recoveryFailed = false
	for i := 0; i < 129; i++ {
		copy := *base
		copy.Public = cloneCableRun(base.Public)
		copy.Public.RunID = strings.Repeat("a", 30) + string(rune(0x100+i))
		s.runs[copy.Public.RunID] = &copy
	}
	if _, err = s.retainedRuns(context.Background()); err == nil {
		t.Fatal("oversized history became an unbounded response")
	}
}

func TestCableRetainedReleasedHoldIsNotResumed(t *testing.T) {
	x := newCleanupProductFixture(t)
	x.authorize()
	review := x.review(t)
	if _, err := x.f.s.verifyCleanupAdmission(context.Background(), x.run.RunID, review.ReviewID, true); err != nil {
		t.Fatal(err)
	}
	x.done(t)
	catalog, err := x.f.s.retainedRuns(context.Background())
	if err != nil || catalog.Held || catalog.Limited || len(catalog.Runs) != 0 {
		t.Fatalf("released hold remained in attention list: %+v %v", catalog, err)
	}
	run, err := x.f.s.status(cableprobe.StatusRequest{RunID: x.run.RunID})
	if err != nil || run.CleanupConfirmed || run.CleanupRecovery == nil || !run.CleanupRecovery.HoldReleased {
		t.Fatal("historical original and separate release changed")
	}
}

func TestCableRetainedRPCIsReadOnlyAndRejectsActionParameters(t *testing.T) {
	for _, params := range []string{`{}`, `{"runId":"injected"}`, `{"approveAdmin":true}`, `{"nodeIds":["host-peer"]}`} {
		t.Run(params, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			var output bytes.Buffer
			x.f.s.m.codec = NewCodec(&output)
			id := json.RawMessage(`1`)
			x.f.s.m.runCableRetained(context.Background(), &Message{ID: &id, Method: "engine:cable-retained-runs", Params: json.RawMessage(params)})
			var response struct {
				Result *cableprobe.RetainedRuns `json:"result"`
				Error  *RPCError                `json:"error"`
			}
			if json.Unmarshal(output.Bytes(), &response) != nil {
				t.Fatal("malformed normal RPC reply")
			}
			if params == `{}` {
				if response.Error != nil || response.Result == nil || len(response.Result.Runs) != 1 || response.Result.Runs[0].RunID != x.run.RunID {
					t.Fatal("ordinary RPC could not discover retained run")
				}
			} else if response.Error == nil || response.Result != nil {
				t.Fatal("discovery admitted action/selection input")
			}
			if x.launches.Load() != 0 || x.finishes.Load() != 0 || len(x.f.s.cleanupReviews) != 0 {
				t.Fatal("discovery performed a reviewed action")
			}
		})
	}
}
