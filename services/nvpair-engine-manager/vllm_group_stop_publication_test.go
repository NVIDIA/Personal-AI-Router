// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
)

func TestVLLMSystemStopPublishesUnlockedClosedAndFailedState(t *testing.T) {
	for _, state := range []string{"stopped", "cleanup-required"} {
		t.Run(state, func(t *testing.T) {
			f := vllmResourceFixture(t)
			closed := state == "stopped"
			rank := &vllmManagedRank{e: f.e, st: f.st,
				binding: vllmGroupBinding{Plan: vllmGroupTestPlan(2)},
				receipt: vllmRankReceipt{State: state, CleanupConfirmed: closed}}
			if closed {
				rank.receipt.SystemPlan = &vllmRankSystemPlan{NodeID: "node-a"}
			}
			f.st.vllmRank, f.st.running = rank, !closed
			var events []EngineStatus
			f.e.emit = func(method string, value any) {
				if method != "engine:state-changed" {
					return
				}
				if !rank.mu.TryLock() {
					t.Fatal("Stop publication retained the rank lock")
				}
				rank.mu.Unlock()
				if !f.st.mu.TryLock() {
					t.Fatal("Stop publication retained the engine lock")
				}
				f.st.mu.Unlock()
				status, ok := value.(EngineStatus)
				if !ok {
					t.Fatal("publication was not a status")
				}
				events = append(events, status)
			}
			// Both cases return before native invocation: already closed, or no
			// native plan. Failure must retain the existing uncertainty/running fact.
			err := rank.stopSystem(context.Background(), nil)
			if (err == nil) != closed || len(events) != 1 {
				t.Fatalf("Stop result/publication mismatch: %v, %+v", err, events)
			}
			if events[0].Running != !closed || events[0].Healthy || events[0].Starting || rank.receipt.State != state || rank.receipt.CleanupConfirmed != closed || rank.reserved() != !closed {
				t.Fatal("publication fabricated Off, readiness or confirmed cleanup")
			}
		})
	}
}
