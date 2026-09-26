// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrusttest"
)

func TestCableTopologyReceiptFailureIsUnavailable(t *testing.T) {
	f := newCableProductFixture(t)
	var blockSaves atomic.Bool
	f.s.testSave = func(*cableProductRun) error {
		if blockSaves.Load() {
			return errors.New("synthetic receipt write failure")
		}
		return nil
	}
	f.s.launch = func(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
		worker, err := f.launch(ctx, client, plan, request, admin)
		if err != nil || plan.NodeID != "host-owner" {
			return worker, err
		}
		wait := worker.wait
		worker.wait = func(ctx context.Context) error {
			if err := wait(ctx); err != nil {
				return err
			}
			// Fail only the writes after clean observation; the prior receipt
			// remains untouched.
			blockSaves.Store(true)
			return nil
		}
		return worker, nil
	}
	run := f.done(f.start(f.ready()))
	if run.State != "failed" || !run.CleanupConfirmed || run.Result != "reciprocal-observations" || run.Topology == nil || run.Topology.Status != "unavailable" || run.Diagnostics == nil || run.Diagnostics.Failure == nil || run.Diagnostics.Failure.Code != "receipt-write-failed" {
		t.Fatalf("receipt failure retained a cable verdict: %+v", run)
	}
	if !f.s.held() {
		t.Fatal("receipt failure did not hold recovery")
	}
}

// Existing fake descriptors and in-memory transports only; no native adapter,
// listener, process, or network configuration is used by these tests.
func topologyTestNoHTTPManager(t *testing.T) (*Manager, string) {
	t.Helper()
	forbidden := cableTestTransport(func(*http.Request) (*http.Response, error) {
		panic("topology probe attempted an inventory HTTP read")
	})
	m, _, dir := cableTestManager(t, forbidden)
	cableTestRemoteRead(t, m, forbidden)
	return m, dir
}

func TestCableTopologyProbeUsesLocalNativeFactsWithoutInventory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := topologyTestNoHTTPManager(t)
		request, fake := onceTestRequest(t), newProbeTestIO()
		request.Review.Targets[1].Ports[0].Interfaces[0].MAC = "02:00:00:00:00:02"
		local, peer := request.Review.Targets[0], request.Review.Targets[1]
		localPort := cableprobe.PortRef{NodeID: local.NodeID, SwitchID: local.Ports[0].SwitchID, PortName: local.Ports[0].PortName}
		probeIO, validations := fake.io(), 0
		probeIO.validate = func(alias cableprobe.Interface, port cableprobe.PortRef) error {
			validations++
			if alias != local.Ports[0].Interfaces[0] || port != localPort {
				return errors.New("validation escaped the selected local alias")
			}
			return nil
		}
		seen := false
		fake.onRead = func(wait time.Duration) (cableProbePacket, error) {
			time.Sleep(wait)
			if seen {
				return cableProbePacket{}, errCableProbeIdle
			}
			seen = true
			frame, err := cableprobe.EncodeFrame(peer.Ports[0].Interfaces[0].MAC, request.Marker, 1)
			if err != nil {
				t.Fatal(err)
			}
			return cableProbePacket{Data: frame, Index: local.Ports[0].Interfaces[0].Index, Kind: 2, ReceivedAt: time.Now()}, nil
		}
		session, err := m.prepareCableProbeOnce(context.Background(), request, probeIO)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		result := session.Run(context.Background())
		if result.State != "completed" || !result.CleanupConfirmed || result.Sent != 20 || time.Since(started) != 20*time.Second {
			t.Fatalf("local-only finite observation: %+v elapsed=%s", result, time.Since(started))
		}
		if validations < 3 || len(result.Observations) != 1 || result.Observations[0].Local != localPort || result.Observations[0].Peer.NodeID != peer.NodeID {
			t.Fatalf("selected native validation or approved peer mapping was lost: validations=%d result=%+v", validations, result)
		}
		if _, owned, _ := fake.counts(); owned != 0 {
			t.Fatal("completed observation retained owned descriptors")
		}
	})
}

func TestCableTopologyLocalChangeOrCertificateStopsTraffic(t *testing.T) {
	for _, mode := range []string{"alias-before-prepare", "alias-before-start", "peer-certificate-before-start", "owner-certificate-during-validation"} {
		t.Run(mode, func(t *testing.T) {
			m, dir := topologyTestNoHTTPManager(t)
			request, fake := onceTestRequest(t), newProbeTestIO()
			probeIO := fake.io()
			changed, rotated := mode == "alias-before-prepare", false
			probeIO.validate = func(cableprobe.Interface, cableprobe.PortRef) error {
				if changed {
					return errors.New("selected alias changed")
				}
				if mode == "owner-certificate-during-validation" && !rotated {
					rotated = true
					clustertrusttest.WriteKeypair(t, dir, "principal-owner")
				}
				return nil
			}
			session, err := m.prepareCableProbeOnce(context.Background(), request, probeIO)
			if mode == "alias-before-prepare" || mode == "owner-certificate-during-validation" {
				if err == nil || session != nil || fake.lockCalls != 0 {
					t.Fatal("invalid local admission reached reservation")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if mode == "alias-before-start" {
					changed = true
				} else {
					clustertrusttest.WritePeerPin(t, dir, "principal-peer")
				}
				result := session.Run(context.Background())
				if result.State != "failed" || !result.CleanupConfirmed || len(result.Observations) != 0 {
					t.Fatalf("changed authority or local alias was accepted: %+v", result)
				}
			}
			if _, owned, _ := fake.counts(); owned != 0 || fake.writeCalls != 0 {
				t.Fatal("rejected preparation/start sent traffic or retained descriptors")
			}
		})
	}
}

func TestCableTopologyWorkerReservesObservationBeforeReady(t *testing.T) {
	for _, delay := range []time.Duration{10 * time.Second, 11 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				input, parentInput := io.Pipe()
				parentOutput, output := io.Pipe()
				defer input.Close()
				defer parentInput.Close()
				defer parentOutput.Close()
				defer output.Close()
				fake, finished := newProbeTestIO(), make(chan error, 1)
				began := time.Now()
				go func() {
					finished <- runCableProbeWorker(context.Background(), input, output, func(ctx context.Context, _ cableProbeOnceRequest) (*cableProbeSession, error) {
						// Deliberately overrun the normal local preparation budget to
						// exercise the worker's independent final READY boundary.
						time.Sleep(delay)
						return prepareCableProbe(ctx, true, "local", probeTestMarker, probeTestTargets(1, 1), fake.io(), probeTestCurrent)
					})
				}()
				if err := json.NewEncoder(parentInput).Encode(onceTestRequest(t)); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(parentOutput)
				line, err := reader.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var first struct {
					State, Code, ResourceOutcome string
					RemainingMs                  int64
				}
				if json.Unmarshal(line, &first) != nil || fake.writeCalls != 0 {
					t.Fatal("invalid readiness response or traffic before start")
				}
				if delay == 11*time.Second {
					if first.State != "prearm-failed" || first.Code != "prepare-admission-expired" || first.ResourceOutcome != "rollback-confirmed" {
						t.Fatalf("insufficient full-window budget advertised READY: %s", line)
					}
				} else {
					if first.State != "armed" || first.RemainingMs != 1000 {
						t.Fatalf("READY did not reserve 20s observation plus 4s final result: %s", line)
					}
					time.Sleep(900 * time.Millisecond)
					started := time.Now()
					if err := json.NewEncoder(parentInput).Encode(cableProbeCommand{RunID: "synthetic-run", Command: "start"}); err != nil {
						t.Fatal(err)
					}
					line, err = reader.ReadBytes('\n')
					var terminal cableProbeResult
					if err != nil || json.Unmarshal(line, &terminal) != nil || terminal.State != "completed" || terminal.Sent != 20 || !terminal.CleanupConfirmed || time.Since(started) != 20*time.Second {
						t.Fatalf("late admitted start lost its full observation: %s %v", line, err)
					}
				}
				if err := <-finished; err != nil || time.Since(began) > 35*time.Second {
					t.Fatalf("worker exceeded its unchanged process budget: %v elapsed=%s", err, time.Since(began))
				}
				if _, owned, _ := fake.counts(); owned != 0 {
					t.Fatal("worker budget boundary retained descriptors")
				}
			})
		})
	}
}

func TestCableTopologyClassifiesObservedLayout(t *testing.T) {
	for _, test := range []struct {
		name, layout, status string
	}{
		{"direct", "direct", "matched"}, {"ring", "ring", "matched"},
		{"missing", "ring", "missing"}, {"duplicate-partner", "ring", "unexpected"},
		{"ambiguous", "ring", "unexpected"}, {"failed", "ring", "unavailable"},
		{"unclean", "ring", "unavailable"}, {"unsupported-selection", "ring", "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := cableprobe.Run{State: "completed", Result: "reciprocal-observations", CleanupConfirmed: true}
			nodes, ports := []string{"a", "b", "c"}, []string{"p0", "p1"}
			if test.name == "direct" {
				nodes, ports = nodes[:2], ports[:1]
			}
			for _, node := range nodes {
				target := cableprobe.Target{NodeID: node, Principal: "principal-" + node}
				for _, port := range ports {
					target.Ports = append(target.Ports, cableprobe.Port{SwitchID: "switch-" + node, PortName: port})
				}
				run.Targets = append(run.Targets, target)
			}
			ref := func(node, port string) cableprobe.PortRef {
				return cableprobe.PortRef{NodeID: node, SwitchID: "switch-" + node, PortName: port}
			}
			run.Edges = []cableprobe.Edge{{Left: ref("a", "p0"), Right: ref("b", "p0")}}
			if test.name != "direct" {
				run.Edges = []cableprobe.Edge{{Left: ref("a", "p0"), Right: ref("b", "p1")}, {Left: ref("a", "p1"), Right: ref("c", "p0")}, {Left: ref("b", "p0"), Right: ref("c", "p1")}}
			}
			switch test.name {
			case "missing":
				run.Edges, run.Result = run.Edges[:2], "incomplete"
			case "duplicate-partner":
				run.Edges[2] = cableprobe.Edge{Left: ref("a", "p1"), Right: ref("b", "p0")}
			case "ambiguous":
				run.Result = "ambiguous"
			case "failed":
				run.State = "failed"
			case "unclean":
				run.CleanupConfirmed = false
			case "unsupported-selection":
				run.Targets[0].Ports = run.Targets[0].Ports[:1]
			}
			if result := cableTopologyResult(run); result == nil || result.Layout != test.layout || result.Status != test.status {
				t.Fatalf("topology=%+v, want %s/%s", result, test.layout, test.status)
			}
		})
	}
}
