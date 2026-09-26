// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"
)

func TestVLLMGroupControlRouteExcludesInactiveDockerBridge(t *testing.T) {
	selection, facts, pins := groupPlanFactsFixture(2)
	// Both nodes have docker0 UP administratively, but NO-CARRIER/DOWN. It
	// shares the same address/prefix; it did not carry the paired connection.
	for rank := range facts {
		facts[rank].Addresses = append(facts[rank].Addresses, vllmGroupAddress{IP: "172.17.0.1", Prefix: "172.17.0.0/16"})
	}
	if _, err := assembleVLLMGroupPlan(selection, facts, pins); err == nil {
		t.Fatal("fixture did not reproduce the multiple-private-subnet refusal")
	}
	route, err := vllmObservedControlRoute("https://192.168.4.11:14323", "192.168.4.10:50123", "192.168.4.11:14323")
	if err != nil {
		t.Fatal(err)
	}
	facts[1].controlRoute = route
	if err := bindVLLMGroupControlRoute(&facts[0], &facts[1]); err != nil {
		t.Fatal(err)
	}
	plan, err := assembleVLLMGroupPlan(selection, facts, pins)
	if err != nil {
		t.Fatal(err)
	}
	for rank, want := range []string{"192.168.4.10", "192.168.4.11"} {
		member := plan.Members[rank]
		if member.Placement.Address != want || member.PinSHA256 != pins[rank] || member.ModelDigest != facts[rank].ModelDigest || member.RuntimeDigest != facts[rank].RuntimeDigest || member.RuntimeCompatibilitySHA256 != facts[rank].RuntimeCompatibilitySHA256 || member.Resources == nil {
			t.Fatal("the authenticated route or existing provenance was lost")
		}
	}
	if _, err := vllmObservedControlRoute("https://192.168.4.11:14323", "192.168.4.10:50123", "192.168.4.90:14323"); err == nil {
		t.Fatal("a different peer socket became the selected participant")
	}
	facts[1].controlRoute = &vllmGroupControlRoute{Local: "192.168.4.10", Peer: "192.168.4.90"}
	if bindVLLMGroupControlRoute(&facts[0], &facts[1]) == nil {
		t.Fatal("unobserved local interface was accepted")
	}
}

func TestQwen38GroupFactsUsesPinnedLongResponseClientAndCallerBudget(t *testing.T) {
	selection, facts, _ := groupPlanFactsFixture(2)
	selection.Model = vllmQwen38ModelID
	ordinaryCalled, longCalled := false, false
	client := &remoteClient{base: "https://192.168.4.11:14323"}
	client.http = &http.Client{Transport: remoteActionTransport(func(*http.Request) (*http.Response, error) {
		ordinaryCalled = true
		return nil, errors.New("ordinary client must not carry Qwen facts")
	})}
	client.readyHTTP = &http.Client{Transport: remoteActionTransport(func(request *http.Request) (*http.Response, error) {
		longCalled = true
		deadline, ok := request.Context().Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining <= 10*time.Minute || remaining > 11*time.Minute {
			return nil, errors.New("Qwen facts request lacks its bounded caller deadline")
		}
		trace := httptrace.ContextClientTrace(request.Context())
		trace.GotConn(httptrace.GotConnInfo{Conn: groupRouteTestConn{
			local: &net.TCPAddr{IP: net.ParseIP("192.168.4.10"), Port: 50123},
			peer:  &net.TCPAddr{IP: net.ParseIP("192.168.4.11"), Port: 14323}}, Reused: true})
		body, _ := json.Marshal(facts[1])
		return remoteActionResponse(request, http.StatusOK, string(body)), nil
	})}
	if _, err := client.vllmGroupFacts(context.Background(), selection); err != nil {
		t.Fatal(err)
	}
	if ordinaryCalled || !longCalled {
		t.Fatalf("wrong Qwen facts client: ordinary=%t long=%t", ordinaryCalled, longCalled)
	}
}

type groupRouteTestConn struct{ local, peer net.Addr }

func (c groupRouteTestConn) LocalAddr() net.Addr            { return c.local }
func (c groupRouteTestConn) RemoteAddr() net.Addr           { return c.peer }
func (groupRouteTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (groupRouteTestConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (groupRouteTestConn) Close() error                     { return nil }
func (groupRouteTestConn) SetDeadline(time.Time) error      { return nil }
func (groupRouteTestConn) SetReadDeadline(time.Time) error  { return nil }
func (groupRouteTestConn) SetWriteDeadline(time.Time) error { return nil }

func TestVLLMGroupFactsUsesActualReusedControlConnection(t *testing.T) {
	selection, facts, _ := groupPlanFactsFixture(2)
	for _, wrong := range []bool{false, true} {
		client := &remoteClient{base: "https://192.168.4.11:14323", http: &http.Client{Transport: remoteActionTransport(func(request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			if trace == nil || trace.GotConn == nil {
				t.Fatal("facts request did not observe existing transport")
			}
			peer := "192.168.4.11"
			if wrong {
				peer = "192.168.4.90"
			}
			trace.GotConn(httptrace.GotConnInfo{Conn: groupRouteTestConn{
				local: &net.TCPAddr{IP: net.ParseIP("192.168.4.10"), Port: 50123},
				peer:  &net.TCPAddr{IP: net.ParseIP(peer), Port: 14323}}, Reused: true})
			body, _ := json.Marshal(facts[1])
			return remoteActionResponse(request, http.StatusOK, string(body)), nil
		})}}
		result, err := client.vllmGroupFacts(context.Background(), selection)
		if wrong {
			if err == nil {
				t.Fatal("mismatched peer accepted")
			}
			continue
		}
		if err != nil || result.controlRoute == nil || result.controlRoute.Local != "192.168.4.10" || result.controlRoute.Peer != "192.168.4.11" {
			t.Fatalf("connection evidence lost: %+v %v", result.controlRoute, err)
		}
		body, _ := json.Marshal(result)
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields["controlRoute"] != nil {
			t.Fatal("local transport evidence entered peer facts JSON")
		}
	}
}
