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
	"testing"
	"testing/synctest"
	"time"
)

// A collectively ready coordinator is routable before it is published ready,
// and only then: participants and ordinary starting groups stay withdrawn.
func TestGroupPublishesReadyOnlyAfterTheProxyRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error { return nil })
		routed := make(chan struct{})
		entered := make(chan struct{})
		g.route = func(ctx context.Context, _ vllmGroupRun) error {
			close(entered)
			select {
			case <-routed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := vllmGroupTestStart(t, g, ctx, 3)
		<-entered
		if g.status().State != "starting" {
			t.Fatal("the group was published ready before the proxy route")
		}
		coordinator := g.routeStatus("node-a", EngineStatus{Healthy: true})
		if coordinator.ServingGroup == nil || !coordinator.ServingGroup.Routing || coordinator.ServingGroup.State != "starting" || !coordinator.Healthy {
			t.Fatalf("the collectively ready coordinator was not routable: %+v", coordinator.ServingGroup)
		}
		participant := g.routeStatus("node-b", EngineStatus{Healthy: true})
		if participant.ServingGroup.Routing || participant.Healthy {
			t.Fatal("a participant was offered for routing")
		}
		close(routed)
		synctest.Wait()
		ready := g.routeStatus("node-a", EngineStatus{Healthy: true})
		if g.status().State != "ready" || ready.ServingGroup.Routing || !ready.Healthy {
			t.Fatalf("the routed group was not published ready: %+v", ready.ServingGroup)
		}
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGroupWithoutProxyRouteFailsAndCleansUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error { return nil })
		g.route = func(context.Context, vllmGroupRun) error {
			return errors.New("the local proxy did not route the ready serving group within its bound")
		}
		vllmGroupTestStart(t, g, context.Background(), 3)
		synctest.Wait()
		run := g.status()
		if run.State != "failed" || !run.CleanupConfirmed || g.reserved() || g.routeStatus("node-a", EngineStatus{Healthy: true}).ServingGroup != nil {
			t.Fatalf("an unroutable group did not fail closed with confirmed cleanup: %+v", run)
		}
	})
}

// The gate asks the owning broker once per interval and accepts only a reply
// correlated to its own request.
func TestRouteGateAsksTheOwningBrokerUntilRoutable(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	wire := struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(nil), writer}
	m := &Manager{codec: NewCodec(wire), groupRoute: newVLLMGroupRouteGate()}
	asked := make(chan int, 1)
	go func() {
		lines := bufio.NewScanner(reader)
		for n := 1; lines.Scan(); n++ {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					RequestID string `json:"requestId"`
					Model     string `json:"model"`
				} `json:"params"`
			}
			if json.Unmarshal(lines.Bytes(), &msg) != nil || msg.Method != "engine:vllm-group-route-check" || msg.Params.Model != "example/model" {
				continue
			}
			m.groupRoute.receive(json.RawMessage(`{"requestId":"` + newOpID() + `","routable":true}`))
			reply, _ := json.Marshal(map[string]any{"requestId": msg.Params.RequestID, "routable": n >= 2})
			m.groupRoute.receive(reply)
			select {
			case asked <- n:
			default:
				<-asked
				asked <- n
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.awaitVLLMGroupRoute(ctx, vllmGroupRun{Plan: vllmGroupPlan{Model: "example/model"}}); err != nil {
		t.Fatal(err)
	}
	if n := <-asked; n != 2 {
		t.Fatalf("the gate accepted a route after %d checks", n)
	}
	writer.Close()
}

func TestRouteGateAcceptsAReplyAfterTheOldThreeSecondDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	wire := struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(nil), writer}
	m := &Manager{codec: NewCodec(wire), groupRoute: newVLLMGroupRouteGate()}
	go func() {
		lines := bufio.NewScanner(reader)
		if !lines.Scan() {
			return
		}
		var msg struct {
			Params struct {
				RequestID string `json:"requestId"`
			} `json:"params"`
		}
		if json.Unmarshal(lines.Bytes(), &msg) != nil {
			return
		}
		// A broker reconcile may legitimately exceed the former three-second
		// waiter while each nested operation remains inside its own bound.
		time.Sleep(4 * time.Second)
		reply, _ := json.Marshal(map[string]any{"requestId": msg.Params.RequestID, "routable": true})
		m.groupRoute.receive(reply)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.awaitVLLMGroupRoute(ctx, vllmGroupRun{Plan: vllmGroupPlan{Model: "example/model"}}); err != nil {
		t.Fatalf("a delayed in-budget broker reply was rejected: %v", err)
	}
	writer.Close()
}

func TestRouteGateFailsClosedAtItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		m := &Manager{codec: NewCodec(&output), groupRoute: newVLLMGroupRouteGate()}
		started := time.Now()
		if err := m.awaitVLLMGroupRoute(context.Background(), vllmGroupRun{Plan: vllmGroupPlan{Model: "example/model"}}); err == nil {
			t.Fatal("an unanswered route gate published ready")
		}
		if elapsed := time.Since(started); elapsed < vllmGroupRouteBudget || elapsed > vllmGroupRouteBudget+vllmGroupRouteReply+vllmGroupRouteInterval {
			t.Fatalf("the route gate did not honor its bound: %v", elapsed)
		}
		for _, malformed := range []string{`{"requestId":"x"}`, `{"requestId":"x","routable":"yes"}`, `{"requestId":"x","routable":true,"extra":1}`} {
			m.groupRoute.receive(json.RawMessage(malformed))
		}
	})
}
