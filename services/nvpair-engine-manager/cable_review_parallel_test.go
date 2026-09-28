// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrusttest"
)

func TestCableReviewParallelThreeParticipants(t *testing.T) {
	for _, mode := range []string{"fresh", "failure-order"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var started, finished atomic.Int32
				delays := []time.Duration{700 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond}
				read := func(index int, request *http.Request) (*http.Response, error) {
					started.Add(1)
					defer finished.Add(1)
					observed := time.Now().Add(-700 * time.Millisecond)
					time.Sleep(delays[index])
					if index == 1 {
						return cableTestResponse(t, cableTestInfo(t, observed, 1)), nil
					}
					facts := cableTestPeerSnapshot(t)
					if index == 2 {
						facts.NodeID, facts.Principal = "host-third", "principal-third"
						facts.Ports[0].SwitchID = "0011223344556699"
					}
					if mode == "failure-order" {
						if index == 2 {
							return nil, errors.New("synthetic-private-first-completion")
						}
						facts.Principal = "foreign"
					}
					return cableTestResponse(t, facts), nil
				}
				m, _, dir := cableTestManager(t, func(r *http.Request) (*http.Response, error) { return read(1, r) })
				cableTestRemoteRead(t, m, func(r *http.Request) (*http.Response, error) { return read(0, r) })
				clustertrusttest.WritePeerPin(t, dir, "principal-third")
				m.mesh.Refresh()
				m.peers.peers["host-third"] = ecPeer{nodeID: "host-third", clusterUUID: "principal-third", addresses: []string{"192.0.2.8"}, port: 14323}
				client, ok := m.remoteHTTP.Client("principal-third")
				if !ok {
					t.Fatal("synthetic third peer pin unavailable")
				}
				client.Transport = cableTestTransport(func(r *http.Request) (*http.Response, error) { return read(2, r) })
				selection := cableTestSelection()
				selection.NodeIDs = []string{"host-peer", "host-owner", "host-third"}
				selection.Ports = []cableprobe.PortRef{selection.Ports[1], selection.Ports[0], {NodeID: "host-third", SwitchID: "0011223344556699", PortName: "p1"}}
				local, peer := cableTestSnapshot(t, time.Now(), 1), cableTestPeerSnapshot(t)
				third := cableTestPeerSnapshot(t)
				third.Ports[0].SwitchID = "0011223344556699"
				want := []cableprobe.Target{
					{NodeID: "host-peer", Principal: "principal-peer", Ports: peer.Ports},
					{NodeID: "host-owner", Principal: "principal-owner", Ports: local.Ports},
					{NodeID: "host-third", Principal: "principal-third", Ports: third.Ports},
				}
				began := time.Now()
				review, statuses, err := m.reviewCablesDetailed(context.Background(), selection)
				if err != nil {
					t.Fatal(err)
				}
				if started.Load() != 3 || finished.Load() != 3 || review.Available || review.ConsumedRunID != "" {
					t.Fatal("bounded read-only gather did not finish every selected read")
				}
				if mode == "fresh" {
					// Serial gathering takes 1600ms and expires only local index 1:
					// 700ms start + (2000 - 1100ms returned age) = 1600ms.
					if !reflect.DeepEqual(cableProbeTargetFacts(review.Targets), cableProbeTargetFacts(want)) ||
						!reflect.DeepEqual(statuses, []cableFactsReadStatus{"", "", ""}) {
						t.Fatalf("serial participant delay expired ordered facts: elapsed=%s statuses=%v", time.Since(began), statuses)
					}
				} else {
					difference := firstCableFactsDifference(want, review.Targets, statuses)
					if difference == nil || *difference.TargetIndex != 0 || difference.ReadStatus != "invalid" {
						t.Fatalf("completion order replaced the selected-order failure: %+v", difference)
					}
				}
				if time.Since(began) != 700*time.Millisecond {
					t.Fatalf("independent read delays accumulated: %s", time.Since(began))
				}
				for _, target := range review.Targets {
					if strings.Contains(target.Reason, "synthetic-private") {
						t.Fatal("raw transport error escaped")
					}
				}
			})
		})
	}
}

func TestCableReviewParallelCancellationJoinsReads(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var started, finished atomic.Int32
				read := func(r *http.Request) (*http.Response, error) {
					started.Add(1)
					defer finished.Add(1)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				m, _, _ := cableTestManager(t, read)
				cableTestRemoteRead(t, m, read)
				ctx, cancel := context.WithCancel(context.Background())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
				} else {
					go func() { time.Sleep(250 * time.Millisecond); cancel() }()
				}
				defer cancel()
				began := time.Now()
				review, statuses, err := m.reviewCablesDetailed(ctx, cableTestSelection())
				if err != nil || ctx.Err() == nil || time.Since(began) != 250*time.Millisecond || started.Load() != 2 || finished.Load() != 2 {
					t.Fatalf("canceled gather did not join its bounded reads: started=%d finished=%d elapsed=%s error=%v", started.Load(), finished.Load(), time.Since(began), err)
				}
				if review.Available || !reflect.DeepEqual(statuses, []cableFactsReadStatus{"unavailable", "unavailable"}) {
					t.Fatal("canceled read became admission or lost its failure")
				}
				for _, target := range review.Targets {
					if len(target.Ports) != 0 || target.Principal != "" {
						t.Fatal("canceled facts retained participant authority")
					}
				}
			})
		})
	}
}
