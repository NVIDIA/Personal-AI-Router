// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only public marker storage and a mocked start callback are used. No native
// launcher, SSH, systemd, network or GPU path is invoked by these tests.
func mpiReviewFixture(t *testing.T) (*diagnosticService, *diagnosticMPIReviewBinding) {
	t.Helper()
	d, p, plan, request := mpiRecoveryFixture(t)
	pin, ok := d.m.mesh.PinSHA256("node-b")
	if !ok {
		t.Fatal("fixture controller pin missing")
	}
	bound := &diagnosticMPIReviewBinding{Controller: "node-b", ControllerPin: pin, Profile: p, Plan: plan, PrivateKey: []byte("volatile-mpi-review-key-fixture"),
		Public: diagnosticMPIReview{ReviewID: strings.Repeat("d", 32), BuildOperationID: strings.Repeat("c", 32), OperationID: request.OperationID, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, ExpiresAt: request.ExpiresAt}}
	d.mu.Lock()
	err := d.publishMPIReviewLocked(bound, mpiReviewStorage())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return d, bound
}

func mpiReviewTestOperation(bound *diagnosticMPIReviewBinding) diagnosticOperation {
	return diagnosticOperation{profileDigest: profileDigest(bound.Profile), OperationID: bound.Public.OperationID, GroupID: bound.Public.GroupID, OwnerNodeID: bound.Public.OwnerNodeID, Preset: diagnosticPreset, State: "preparing", MemberNodeIDs: []string{"node-a", "node-b"}}
}

func mpiReviewNoStart(t *testing.T) func(diagnosticProfile, json.RawMessage, []byte) (diagnosticOperation, error) {
	t.Helper()
	return func(_ diagnosticProfile, _ json.RawMessage, key []byte) (diagnosticOperation, error) {
		clear(key)
		t.Error("review fence unexpectedly invoked start")
		return diagnosticOperation{}, errors.New("unexpected start")
	}
}

func TestDiagnosticMPIReviewPublishesOnlyDurablePublicMarker(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	raw, err := readMPIReviewMarkerFile(d.mpiReviewMarkerPath(bound.Public.ReviewID))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{bound.PrivateKey, []byte("PRIVATE KEY"), []byte("privateKey"), []byte("/home/fixture"), []byte("password")} {
		if bytes.Contains(raw, forbidden) {
			t.Fatal("public review marker serialized volatile access or execution bytes")
		}
	}
	marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
	if err != nil || marker.State != "unconsumed" || marker.ProfileDigest != profileDigest(bound.Profile) || d.mpiReviews[marker.ReviewID] != bound {
		t.Fatalf("published review lacks its exact durable marker: %+v %v", marker, err)
	}
	// Publication must remain absent when the durable write cannot be confirmed.
	other := *bound
	other.Public.ReviewID = strings.Repeat("e", 32)
	storage := mpiReviewStorage()
	storage.write = func(string, diagnosticMPIReviewMarker, bool) error { return errors.New("write blocked") }
	d.mu.Lock()
	err = d.publishMPIReviewLocked(&other, storage)
	d.mu.Unlock()
	if err == nil || d.mpiReviews[other.Public.ReviewID] != nil {
		t.Fatal("unconfirmed marker published a volatile review")
	}
}

func TestDiagnosticMPIReviewCloseWinsAgainstDelayedApproval(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	keyAlias := bound.PrivateKey
	for range 2 {
		result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
		if err != nil || !result.ReviewClosed || result.Operation != nil {
			t.Fatalf("positive closure lost: %+v %v", result, err)
		}
	}
	if !bytes.Equal(keyAlias, make([]byte, len(keyAlias))) || bound.PrivateKey != nil || d.mpiReviews[bound.Public.ReviewID] != nil {
		t.Fatal("closed review retained a volatile key")
	}
	if _, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t)); err == nil {
		t.Fatal("delayed approval passed a closed marker")
	}
}

func TestDiagnosticMPIReviewApproveConsumesBeforeConcurrentClose(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), func(_ diagnosticProfile, _ json.RawMessage, key []byte) (diagnosticOperation, error) {
			defer clear(key)
			marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
			if err != nil || marker.State != "consumed" || bound.PrivateKey != nil {
				return diagnosticOperation{}, errors.New("start preceded confirmed consumption")
			}
			close(entered)
			<-release
			op := mpiReviewTestOperation(bound)
			d.mu.Lock()
			d.operations[op.OperationID] = op
			d.mu.Unlock()
			return op, nil
		})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("approval did not reach its mocked start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("approval deadlocked before mocked start")
	}
	result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err == nil || result.ReviewClosed || result.Operation != nil {
		t.Errorf("concurrent close inferred an uncertain start was absent: %+v %v", result, err)
	}
	if _, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t)); err == nil {
		t.Error("duplicate approval replayed a consumed start")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result, err = d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err != nil || result.ReviewClosed || result.Operation == nil || result.Operation.OperationID != bound.Public.OperationID {
		t.Fatalf("review selector did not recover the original operation: %+v %v", result, err)
	}
}

func TestDiagnosticMPIReviewLostStartReplyRecoversCheckpointAfterRestart(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	_, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), func(_ diagnosticProfile, _ json.RawMessage, key []byte) (diagnosticOperation, error) {
		defer clear(key)
		if err := d.saveOperation(mpiReviewTestOperation(bound)); err != nil {
			return diagnosticOperation{}, err
		}
		return diagnosticOperation{}, errors.New("start response lost")
	})
	if err == nil {
		t.Fatal("fixture did not lose the start reply")
	}
	d.mpiReviews = nil // Restart has neither the volatile key nor review map.
	result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err != nil || result.ReviewClosed || result.Operation == nil || result.Operation.OperationID != bound.Public.OperationID {
		t.Fatalf("durable operation lost after response/restart: %+v %v", result, err)
	}
	if op, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t)); err != nil || op.OperationID != bound.Public.OperationID {
		t.Fatalf("duplicate approval failed to return retained operation: %+v %v", op, err)
	}
	d.recoverOperations() // Real restart loader drops unexported profileDigest.
	result, err = d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err != nil || result.Operation == nil || result.ReviewClosed {
		t.Fatalf("restart map did not recover exact profile sidecar: %+v %v", result, err)
	}
	if err := os.WriteFile(d.operationPath(bound.Public.OperationID)+".profile", []byte(strings.Repeat("0", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err == nil || result.Operation != nil || result.ReviewClosed {
		t.Fatalf("restart operation map bypassed changed profile evidence: %+v %v", result, err)
	}
}

func TestDiagnosticMPIReviewRejectedBeforeConsumptionCanClose(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	d.runtimeActive = strings.Repeat("f", 32)
	if _, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t)); err == nil {
		t.Fatal("busy lane admitted approval")
	}
	result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err != nil || !result.ReviewClosed || result.Operation != nil {
		t.Fatalf("known pre-consumption rejection did not remain closable: %+v %v", result, err)
	}
	d.runtimeActive = ""
}

func TestDiagnosticMPIReviewConsumedWithoutOperationMustHold(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	_, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), func(_ diagnosticProfile, _ json.RawMessage, key []byte) (diagnosticOperation, error) {
		clear(key)
		return diagnosticOperation{}, errors.New("post-consumption admission result uncertain")
	})
	if err == nil {
		t.Fatal("fixture start unexpectedly succeeded")
	}
	d.mpiReviews = nil
	result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err == nil || result.ReviewClosed || result.Operation != nil {
		t.Fatalf("missing operation was treated as proof a consumed review never started: %+v %v", result, err)
	}
}

func TestDiagnosticMPIReviewRestartClosesExpiredUnconsumedMarker(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
	if err != nil {
		t.Fatal(err)
	}
	marker.ExpiresAt = 1
	if err := writeMPIReviewMarkerFile(d.mpiReviewMarkerPath(marker.ReviewID), marker, false); err != nil {
		t.Fatal(err)
	}
	clear(bound.PrivateKey)
	d.mpiReviews = nil
	result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err != nil || !result.ReviewClosed || result.Operation != nil {
		t.Fatalf("restart lost the unconsumed closure proof: %+v %v", result, err)
	}
}

func TestDiagnosticMPIReviewStorageFailuresNeverMintClosureOrStart(t *testing.T) {
	for _, action := range []string{"close", "approve"} {
		for _, failure := range []string{"write", "sync", "readback"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				d, bound := mpiReviewFixture(t)
				keyAlias := bound.PrivateKey
				storage := mpiReviewStorage()
				switch failure {
				case "write":
					storage.write = func(string, diagnosticMPIReviewMarker, bool) error { return errors.New("write failed") }
				case "sync":
					storage.sync = func(string) error { return errors.New("persistent sync failure") }
				case "readback":
					// Permit admission's first read, then fail transition readback.
					reads := 0
					storage.read = func(path string) ([]byte, error) {
						reads++
						if reads > 1 {
							return nil, errors.New("readback lost")
						}
						return readMPIReviewMarkerFile(path)
					}
				}
				var err error
				if action == "approve" {
					_, err = d.approveMPIWithIO("node-b", bound.Public.ReviewID, storage, mpiReviewNoStart(t))
				} else {
					var result diagnosticMPIReviewStatus
					result, err = d.closeUnstartedMPIReviewWithIO("node-b", bound.Public.ReviewID, storage)
					if result.ReviewClosed || result.Operation != nil {
						t.Fatal("unconfirmed storage produced a positive closure")
					}
				}
				if err == nil || !bytes.Equal(keyAlias, make([]byte, len(keyAlias))) || bound.PrivateKey != nil {
					t.Fatalf("failed transition retained an approvable volatile key: %v", err)
				}
			})
		}
	}
}

func TestDiagnosticMPIReviewLostWriteAckNeedsFreshSyncAndReadback(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	storage := mpiReviewStorage()
	storage.write = func(path string, marker diagnosticMPIReviewMarker, create bool) error {
		if err := writeMPIReviewMarkerFile(path, marker, create); err != nil {
			return err
		}
		return errors.New("write acknowledgement lost after durable commit")
	}
	result, err := d.closeUnstartedMPIReviewWithIO("node-b", bound.Public.ReviewID, storage)
	if err != nil || !result.ReviewClosed || result.Operation != nil {
		t.Fatalf("positive sync/readback did not recover a lost write acknowledgement: %+v %v", result, err)
	}
}

func TestDiagnosticMPIReviewMissingMalformedAndContradictoryEvidenceHold(t *testing.T) {
	for _, evidence := range []string{"missing", "malformed", "foreign-controller", "oversized", "profile-only", "bootstrap-only", "malformed-bootstrap", "run-directory"} {
		t.Run(evidence, func(t *testing.T) {
			d, bound := mpiReviewFixture(t)
			markerPath := d.mpiReviewMarkerPath(bound.Public.ReviewID)
			switch evidence {
			case "missing":
				if err := os.Remove(markerPath); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(markerPath, []byte(`{"state":"unconsumed"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-controller":
				marker, _ := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
				marker.ControllerPin = strings.Repeat("0", 64)
				if err := writeMPIReviewMarkerFile(markerPath, marker, false); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(markerPath, make([]byte, 8193), 0600); err != nil {
					t.Fatal(err)
				}
			case "profile-only":
				path := d.operationPath(bound.Public.OperationID) + ".profile"
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(profileDigest(bound.Profile)), 0600); err != nil {
					t.Fatal(err)
				}
			case "bootstrap-only", "malformed-bootstrap":
				path := d.coordinatorBootstrapPath(bound.Public.OperationID)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				content := []byte(`{"owner":"pair-mpi-coordinator-v1"}`)
				if evidence == "malformed-bootstrap" {
					content = []byte("incomplete snapshot")
				}
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			case "run-directory":
				if err := os.MkdirAll(d.runDir(bound.Public.OperationID), 0700); err != nil {
					t.Fatal(err)
				}
			}
			result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
			if err == nil || result.ReviewClosed || result.Operation != nil {
				t.Fatalf("invalid or contradictory evidence yielded closure: %+v %v", result, err)
			}
		})
	}
}

func TestDiagnosticMPIReviewDurableNamespaceHasFiniteCapacity(t *testing.T) {
	d, bound := mpiReviewFixture(t)
	marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < diagnosticMPIReviewMarkerLimit; i++ {
		marker.ReviewID = fmt.Sprintf("%032x", i)
		if err := writeMPIReviewMarkerFile(d.mpiReviewMarkerPath(marker.ReviewID), marker, true); err != nil {
			t.Fatal(err)
		}
	}
	marker.ReviewID = strings.Repeat("e", 32)
	if err := writeMPIReviewMarkerFile(d.mpiReviewMarkerPath(marker.ReviewID), marker, true); err == nil {
		t.Fatal("unbounded durable review markers admitted")
	}
	if _, err := os.Lstat(d.mpiReviewMarkerPath(marker.ReviewID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capacity rejection left a review marker: %v", err)
	}
}
