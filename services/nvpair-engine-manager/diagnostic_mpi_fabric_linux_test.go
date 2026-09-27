// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

// Every participant prepares through the real admission path; only the peer's
// HTTP reply and the native adapter are substituted, so no rank can run.
func TestDiagnosticMPILaunchRevalidatesTheFabricAfterEveryParticipantPrepares(t *testing.T) {
	for _, changed := range []bool{true, false} {
		t.Run("changed="+strconv.FormatBool(changed), func(t *testing.T) {
			d := diagnosticTestService(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			d.ctx = ctx
			dir := t.TempDir()
			clustertrusttest.Join(t, dir, "cluster", "node-a", "node-b")
			d.m.mesh = clustertrust.Open(dir)
			s, r := diagnosticMPIDirectFabricFixture(t)
			d.m.exec.fabric, d.m.cableLocal = s, newCableLocalFacts("node-a", 0)
			check := diagnosticMPIFabricCheckFor(t, r, "node-a")
			p := bootstrapProfileFixture(t)
			p.Fabric = check.Fabric
			members := []map[string]any{}
			for i := range p.Members {
				pin, ok := d.m.mesh.PinSHA256(p.Members[i].Principal)
				if !ok {
					t.Fatal("fixture participant pin missing")
				}
				p.Members[i].ClusterPinSHA256, p.Members[i].Fabric = pin, check.Members[i]
				members = append(members, map[string]any{"nodeId": p.Members[i].NodeID, "fabric": check.Members[i]})
			}
			now := time.Now()
			body := map[string]any{"schemaVersion": 1, "recipeId": diagnosticMPIQuickRecipe, "operationId": p.Bootstrap.OperationID, "groupId": p.GroupID, "ownerNodeId": p.OwnerNodeID, "profileDigest": profileDigest(p),
				"createdAt": now.Add(-time.Second).UnixMilli(), "expiresAt": now.Add(time.Minute).UnixMilli(), "fabric": p.Fabric, "members": members}
			canonical, err := mpiCanonical(body)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(canonical)
			body["planDigest"] = hex.EncodeToString(sum[:])
			raw, err := mpiCanonical(body)
			if err != nil {
				t.Fatal(err)
			}
			d.m.peers.set([]noderec.DirectoryNode{{HostUUID: "node-b", ClusterUUID: "node-b", IP: p.Members[1].Host, Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: 14323}}}})
			pool := clustertrust.NewPeerClientPool(d.m.mesh, 0)
			d.m.remoteHTTP, d.m.readyHTTP = pool, pool
			client, ok := pool.Client("node-b")
			if !ok {
				t.Fatal("fixture peer is not pinned")
			}
			client.Transport = mpiReviewMemoryTransport(func(req *http.Request) (*http.Response, error) {
				var request diagnosticControlRequest
				if json.NewDecoder(req.Body).Decode(&request) != nil || request.Bootstrap == nil {
					t.Fatal("the peer received more than its bootstrap control")
				}
				reply, _ := json.Marshal(diagnosticParticipantResult{NodeID: "node-b", Prepared: request.Method == "bootstrap-prepare", CleanupConfirmed: request.Method == "bootstrap-cancel"})
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(reply)), Header: make(http.Header)}, nil
			})
			d.preflight = func(context.Context, diagnosticProfile, diagnosticMember, string) error { return nil }
			var mu sync.Mutex
			actions := []string{}
			d.bootstrapTestRun = func(_ context.Context, input io.Reader) ([]byte, error) {
				wire, err := io.ReadAll(input)
				if err != nil {
					return nil, err
				}
				header, _, _ := bytes.Cut(wire, []byte{'\n'})
				var request struct {
					Action string              `json:"action"`
					Plan   diagnosticMPIHeader `json:"plan"`
				}
				if err := json.Unmarshal(header, &request); err != nil {
					return nil, err
				}
				mu.Lock()
				actions = append(actions, request.Action)
				mu.Unlock()
				zero := 0
				state := "cancelled"
				if request.Action == "start-coordinator" {
					state = "completed"
				}
				return json.Marshal(diagnosticMPIResult{SchemaVersion: 1, Action: request.Action, NodeID: "node-a", OperationID: request.Plan.OperationID, PlanDigest: request.Plan.PlanDigest, ProfileDigest: request.Plan.ProfileDigest, State: state, ExitCode: &zero,
					CleanupConfirmed: true, UnitOwned: true, UnitProcessesGone: true, UnitMetadataRemoved: true, RankCleanupConfirmed: true, AuthorizationRemoved: true, AgentGone: true, PublicArtifactsRemoved: true})
			}
			if changed {
				s.mu.Lock()
				delete(s.qualified, r.Public.OperationID)
				s.mu.Unlock()
			}
			op, err := d.startBootstrap(p, raw, []byte("test-only volatile input"))
			if err != nil {
				t.Fatal(err)
			}
			settled := waitDiagnostic(t, d, op.OperationID)
			mu.Lock()
			launched := slices.Contains(actions, "start-coordinator")
			mu.Unlock()
			if changed && (launched || settled.State != "failed" || !settled.CleanupConfirmed) {
				t.Fatalf("a changed fabric launched ranks or lost cleanup: %+v actions=%v", settled, actions)
			}
			if !changed && !launched {
				t.Fatalf("an unchanged fabric never reached the launch: %+v actions=%v", settled, actions)
			}
		})
	}
}
