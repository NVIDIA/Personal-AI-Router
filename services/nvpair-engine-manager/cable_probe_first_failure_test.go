// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestCableProbePreservesFirstFailureAcrossFinalization(t *testing.T) {
	for _, test := range []struct {
		name, first, finalCode string
		clean                  bool
	}{
		{"transmit-and-cleanup", "Fixed-profile cable transmission failed.", "", false},
		{"receive-and-final-validation", "Cable receive failed.", "final-revalidation-failed", true},
		{"completed-cleanup-and-final-validation", "Cable descriptor cleanup is not confirmed.", "final-revalidation-failed", false},
		{"cancelled-cleanup-and-final-validation", "Cable descriptor cleanup is not confirmed.", "final-revalidation-failed", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newProbeTestIO()
				if !test.clean {
					f.failClose = 10
				}
				if test.name == "transmit-and-cleanup" {
					f.onWrite = func() error { return errors.New("synthetic transmit failure") }
				}
				if test.name == "receive-and-final-validation" {
					reads := 0
					f.onRead = func(time.Duration) (cableProbePacket, error) {
						reads++
						if reads == 1 {
							return probeTestPacket(t, time.Now(), 1), nil
						}
						return cableProbePacket{}, errors.New("synthetic receive failure")
					}
				}
				current := func(context.Context) error {
					_, _, closed := f.counts()
					if closed != 0 {
						return errors.New("synthetic final revalidation failure")
					}
					return nil
				}
				session := probeTestPrepare(t, f, probeTestTargets(1, 1), current)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if test.name == "cancelled-cleanup-and-final-validation" {
					cancel()
				}
				result := session.Run(ctx)
				if result.State != "failed" || result.Message != test.first || result.CleanupConfirmed != test.clean || len(result.Observations) != 0 {
					t.Fatalf("first failure or finalization safety was lost: %+v", result)
				}
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]any
				if json.Unmarshal(data, &wire) != nil {
					t.Fatal("invalid result JSON")
				}
				if test.finalCode == "" {
					if _, present := wire["finalValidationCode"]; present {
						t.Fatal("early return fabricated final validation")
					}
				} else if wire["finalValidationCode"] != test.finalCode {
					t.Fatalf("separate final-validation outcome was lost: %s", data)
				}
				_, _, closed := f.counts()
				if closed != 2 {
					t.Fatalf("cleanup was repeated or skipped: %d closes", closed)
				}
				if test.name == "cancelled-cleanup-and-final-validation" {
					failure := cableWorkerFailure(cableWorkerMessage{cableProbeResult: result})
					if failure == nil || failure.Code != "cleanup-unconfirmed" {
						t.Fatal("cancellation hid the first actual finalization failure")
					}
				}
			})
		})
	}
}
