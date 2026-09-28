// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrusttest"
)

func onceTestRequest(t *testing.T) cableProbeOnceRequest {
	local, peer := cableTestSnapshot(t, time.Now(), 1), cableTestPeerSnapshot(t)
	r := cableProbeOnceRequest{Protocol: cableWorkerProtocol, RunID: "synthetic-run", Marker: probeTestMarker, Review: cableprobe.Review{
		ReviewID: "synthetic-review", OwnerNodeID: "host-owner", RemainingMs: 30000, Available: false,
		Targets: []cableprobe.Target{
			{NodeID: local.NodeID, Principal: local.Principal, Ports: local.Ports, RawPrivilege: "unknown"},
			{NodeID: peer.NodeID, Principal: peer.Principal, Ports: peer.Ports, RawPrivilege: "unknown"},
		},
	}}
	r.Peers = append(r.Peers, struct {
		NodeID      string `json:"nodeId"`
		Principal   string `json:"principal"`
		Address     string `json:"address"`
		ControlPort int    `json:"controlPort"`
	}{"host-peer", "principal-peer", "192.0.2.7", 14323})
	return r
}

func TestCableProbeOnceRequestBoundary(t *testing.T) {
	for _, name := range []string{"valid", "owner", "run-id", "expiry", "too-long", "consumed", "false-ready", "address", "principal", "extra-peer", "marker"} {
		t.Run(name, func(t *testing.T) {
			m, _, _ := cableTestManager(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("binding must not read a service")
				return nil, errors.New("unexpected read")
			})
			r := onceTestRequest(t)
			switch name {
			case "owner":
				r.Review.OwnerNodeID = "other"
			case "run-id":
				r.RunID = ""
			case "expiry":
				r.Review.RemainingMs = 0
			case "too-long":
				r.Review.RemainingMs = 30001
			case "consumed":
				r.Review.ConsumedRunID = "another-run"
			case "false-ready":
				r.Review.Available = true
			case "address":
				r.Peers[0].Address = "arbitrary.example"
			case "principal":
				r.Peers[0].Principal = "other-principal"
			case "extra-peer":
				r.Peers = append(r.Peers, r.Peers[0])
			case "marker":
				r.Marker = "review-not-a-frame-marker"
			}
			_, err := bindCableProbeRequest(m, r)
			if (err == nil) != (name == "valid") {
				t.Fatalf("boundary outcome: %v", err)
			}
		})
	}
	for _, input := range []string{`{"argv":["anything"]}`, `{"packet":"anything"}`, `{"approved":true}`, `{}` + `{}`, strings.Repeat("x", 32769)} {
		if decodeCableProbeLine([]byte(input), new(cableProbeOnceRequest)) == nil {
			t.Fatal("worker accepted an override or unbounded object")
		}
	}
}

func TestCableProbeOnceCurrentAdmissionAndPermissionFailure(t *testing.T) {
	for _, name := range []string{"valid", "changed-ports", "permission", "certificate-change"} {
		t.Run(name, func(t *testing.T) {
			m, _, dir := cableTestManager(t, func(*http.Request) (*http.Response, error) {
				return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
			})
			cableTestRemoteRead(t, m, func(*http.Request) (*http.Response, error) {
				t.Fatal("worker admission must not read cached peer inventory")
				return nil, errors.New("unexpected inventory read")
			})
			r, fake := onceTestRequest(t), newProbeTestIO()
			if name == "changed-ports" {
				r.Review.Targets[0].Ports[0].Interfaces[0].MAC = "02:00:00:00:00:fe"
			}
			if name == "permission" {
				fake.failOpen = 1
			}
			ops := fake.io()
			ops.validate = func(alias cableprobe.Interface, _ cableprobe.PortRef) error {
				if name == "certificate-change" {
					clustertrusttest.WritePeerPin(t, dir, "principal-peer")
				}
				if alias.MAC == "02:00:00:00:00:fe" {
					return errors.New("native alias changed")
				}
				return nil
			}
			session, err := m.prepareCableProbeOnce(context.Background(), r, ops)
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if fake.writeCalls != 0 {
					t.Fatal("prepare sent frames")
				}
				clustertrusttest.WritePeerPin(t, dir, "principal-peer")
				result := session.Run(context.Background())
				if result.State != "failed" || len(result.Observations) != 0 || !result.CleanupConfirmed || fake.writeCalls != 0 {
					t.Fatalf("start after certificate change: %+v", result)
				}
			} else if err == nil {
				session.Close()
				t.Fatal("invalid admission succeeded")
			}
			_, owned, _ := fake.counts()
			if owned != 0 {
				t.Fatal("failed/finished admission retained descriptors")
			}
			if name == "changed-ports" || name == "certificate-change" {
				if fake.lockCalls != 0 {
					t.Fatal("invalid current facts reached raw reservation")
				}
			}
		})
	}
}

func TestCableProbePreparedAdmissionExpiresBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		fake := newProbeTestIO()
		session, err := prepareCableProbe(ctx, true, "local", probeTestMarker, probeTestTargets(1, 1), fake.io(), func(context.Context) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		_, owned, _ := fake.counts()
		if owned != 0 {
			t.Fatal("expired prepared admission retained raw descriptors before any start")
		}
		result := session.Run(context.Background())
		if result.State != "failed" || !result.CleanupConfirmed || fake.writeCalls != 0 {
			t.Fatalf("expired prepared session: %+v", result)
		}
	})
}

func TestCableProbeWorkerArmStartAndOwnerLoss(t *testing.T) {
	for _, mode := range []string{"complete", "disconnect", "cancel", "cancel-before-start", "wrong-start", "no-start"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				input, parentInput := io.Pipe()
				parentOutput, output := io.Pipe()
				defer input.Close()
				defer parentInput.Close()
				defer parentOutput.Close()
				defer output.Close()
				fake := newProbeTestIO()
				finished := make(chan error, 1)
				go func() {
					finished <- runCableProbeWorker(context.Background(), input, output, func(ctx context.Context, _ cableProbeOnceRequest) (*cableProbeSession, error) {
						return prepareCableProbe(ctx, true, "local", probeTestMarker, probeTestTargets(1, 1), fake.io(), func(context.Context) error { return nil })
					})
				}()
				if err := json.NewEncoder(parentInput).Encode(onceTestRequest(t)); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(parentOutput)
				armed, err := reader.ReadBytes('\n')
				if err != nil || !bytes.Contains(armed, []byte(`"state":"armed"`)) || fake.writeCalls != 0 {
					t.Fatalf("unbounded arm: %s %v", armed, err)
				}
				var arm struct {
					RemainingMs int64 `json:"remainingMs"`
				}
				if json.Unmarshal(armed, &arm) != nil || arm.RemainingMs <= 0 || arm.RemainingMs > 5000 {
					t.Fatal("armed lease exceeds actual start/cleanup deadline")
				}
				if mode == "no-start" {
					if err := <-finished; err == nil {
						t.Fatal("missing start did not time out")
					}
				} else {
					id := "synthetic-run"
					if mode == "wrong-start" {
						id = "other"
					}
					command := "start"
					if mode == "cancel-before-start" {
						command = "cancel"
					}
					if err := json.NewEncoder(parentInput).Encode(cableProbeCommand{RunID: id, Command: command}); err != nil {
						t.Fatal(err)
					}
					if mode == "wrong-start" {
						if err := <-finished; err == nil {
							t.Fatal("foreign start accepted")
						}
					} else {
						if mode == "disconnect" {
							parentInput.Close()
						}
						if mode == "cancel" {
							if err := json.NewEncoder(parentInput).Encode(cableProbeCommand{RunID: id, Command: "cancel"}); err != nil {
								t.Fatal(err)
							}
						}
						line, err := reader.ReadBytes('\n')
						if err != nil {
							t.Fatal(err)
						}
						var result cableProbeResult
						if json.Unmarshal(line, &result) != nil || !result.CleanupConfirmed || result.Directness != "unverified" {
							t.Fatalf("invalid terminal result: %s", line)
						}
						if mode == "complete" && (result.State != "completed" || result.Sent != 20) {
							t.Fatalf("finite run: %+v", result)
						}
						if mode != "complete" && result.State != "cancelled" {
							t.Fatalf("owner loss did not cancel: %+v", result)
						}
						if mode == "cancel-before-start" && result.Sent != 0 {
							t.Fatal("pre-start cancellation transmitted frames")
						}
						if err := <-finished; err != nil {
							t.Fatal(err)
						}
					}
				}
				_, owned, _ := fake.counts()
				if owned != 0 {
					t.Fatal("worker retained descriptors after terminal path")
				}
			})
		})
	}
}

func TestCableProbeWorkerUndrainedOutputReleasesDescriptors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parentOutput, output := io.Pipe()
		defer parentOutput.Close()
		defer output.Close()
		request, err := json.Marshal(onceTestRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		fake := newProbeTestIO()
		err = runCableProbeWorker(context.Background(), strings.NewReader(string(request)+"\n"), output, func(ctx context.Context, _ cableProbeOnceRequest) (*cableProbeSession, error) {
			return prepareCableProbe(ctx, true, "local", probeTestMarker, probeTestTargets(1, 1), fake.io(), func(context.Context) error { return nil })
		})
		if err == nil || !strings.Contains(err.Error(), "output deadline") {
			t.Fatalf("blocked output: %v", err)
		}
		_, owned, _ := fake.counts()
		if owned != 0 || fake.writeCalls != 0 {
			t.Fatal("undrained output retained raw resources or sent frames")
		}
	})
}
