// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestVLLMGroupCleanupBudgetCoversSerialStopsWithoutExtendingParticipantCap(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var stopped []int
				g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
					if action != "stop" {
						return nil
					}
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) < vllmGroupParticipantCleanupBudget {
						t.Fatal("serial cleanup shortened a later participant's existing cap")
					}
					stopped = append(stopped, b.Rank)
					time.Sleep(vllmGroupParticipantCleanupBudget - time.Second)
					return ctx.Err()
				})
				vllmGroupTestStart(t, g, ctx, count)
				synctest.Wait()
				began := time.Now()
				if err := stopVLLMGroupLocked(g.engine); err != nil {
					t.Fatal(err)
				}
				want := []int{1, 0}
				if count == 3 {
					want = []int{2, 1, 0}
				}
				if !slices.Equal(stopped, want) || g.reserved() || !g.status().CleanupConfirmed {
					t.Fatalf("serial stops did not finish cleanly: ranks=%v status=%+v", stopped, g.status())
				}
				if time.Since(began) != time.Duration(count)*(vllmGroupParticipantCleanupBudget-time.Second) || vllmGroupCleanupBudget >= managerShutdownBudget {
					t.Fatal("cleanup is not bounded inside the existing manager shutdown epoch")
				}
			})
		})
	}
	t.Run("failed cleanup does not renew Stop deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := vllmGroupTestOwner(t, func(ctx context.Context, _ vllmGroupBinding, action string) error {
				if action != "stop" {
					return nil
				}
				select {
				case <-time.After(vllmGroupParticipantCleanupBudget - time.Second):
					return context.DeadlineExceeded
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			vllmGroupTestStart(t, g, ctx, 3)
			synctest.Wait()
			began := time.Now()
			if err := stopVLLMGroupLocked(g.engine); err == nil || !g.reserved() || g.status().CleanupConfirmed {
				t.Fatal("failed cleanup did not retain its hold")
			}
			if time.Since(began) != vllmGroupCleanupBudget {
				t.Fatal("fallback reconciliation renewed Stop's total budget")
			}
		})
	})
	t.Run("participant cap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			peer, request, _, _ := vllmGroupPeerFixture(t, true)
			request.Action = "stop"
			peer.native = func(ctx context.Context, _ vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) != vllmGroupParticipantCleanupBudget {
					t.Fatal("participant stop bound changed")
				}
				<-ctx.Done()
				return vllmGroupPeerResult{}, ctx.Err()
			}
			began := time.Now()
			if _, err := peer.control(context.Background(), request); err == nil || time.Since(began) != vllmGroupParticipantCleanupBudget {
				t.Fatal("participant timeout did not remain bounded")
			}
		})
	})
}
