// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func onboardingRelayFixture(t *testing.T) (*Broker, *rpcWorker, *rpcWorker, net.Conn, net.Conn, net.Conn) {
	t.Helper()
	oldClient, oldServer := net.Pipe()
	newClient, newServer := net.Pipe()
	cmClient, cmServer := net.Pipe()
	t.Cleanup(func() {
		for _, c := range []net.Conn{oldClient, oldServer, newClient, newServer, cmClient, cmServer} {
			_ = c.Close()
		}
	})
	old := &rpcWorker{peer: NewPeer(NewCodec(oldClient))}
	replacement := &rpcWorker{peer: NewPeer(NewCodec(newClient))}
	cm := &clusterManagerProcess{peer: NewPeer(NewCodec(cmClient))}
	go cm.peer.Serve(nil, nil)
	b := &Broker{}
	b.setEngineMgr(old)
	b.setClusterMgr(cm)
	return b, old, replacement, oldServer, newServer, cmServer
}

func TestOnboardingReplacedWorkerQueuedCallCannotMutateCluster(t *testing.T) {
	b, old, replacement, _, newServer, cmServer := onboardingRelayFixture(t)
	b.setEngineMgr(replacement)
	request := json.RawMessage(`{"requestId":"` + strings.Repeat("a", 32) + `","method":"cluster:invite-node","params":{"requestKey":"` + strings.Repeat("b", 32) + `"}}`)
	b.replyOnboardingCluster(old, request)
	_ = cmServer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := NewCodec(cmServer).Read(); err == nil {
		t.Fatal("replaced worker's queued call reached CM")
	}
	_ = newServer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := NewCodec(newServer).Read(); err == nil {
		t.Fatal("replaced worker's response reached new worker")
	}
}

func TestOnboardingAdmittedReplyNeverMovesToReplacementWorker(t *testing.T) {
	b, old, replacement, oldServer, newServer, cmServer := onboardingRelayFixture(t)
	requestID := strings.Repeat("a", 32)
	done := make(chan struct{})
	go func() {
		b.replyOnboardingCluster(old, json.RawMessage(`{"requestId":"`+requestID+`","method":"cluster:invite-status","params":{"requestKey":"`+strings.Repeat("b", 32)+`"}}`))
		close(done)
	}()
	cmCodec := NewCodec(cmServer)
	_ = cmServer.SetReadDeadline(time.Now().Add(time.Second))
	msg, err := cmCodec.Read()
	if err != nil || msg.Method != "cluster:invite-status" {
		t.Fatalf("wrong admitted cluster call: %v", err)
	}
	b.setEngineMgr(replacement)
	if err = cmCodec.Respond(msg.ID, map[string]any{"inviteId": "fixture-invite", "pin": "654321"}); err != nil {
		t.Fatal(err)
	}
	_ = oldServer.SetReadDeadline(time.Now().Add(time.Second))
	reply, err := NewCodec(oldServer).Read()
	if err != nil || reply.Method != "engine:onboarding-cluster-response" || !strings.Contains(string(reply.Params), requestID) {
		t.Fatal("admitted reply lost originating worker")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not settle")
	}
	_ = newServer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err = NewCodec(newServer).Read(); err == nil {
		t.Fatal("PIN crossed worker replacement boundary")
	}
}

func TestClusteringOnboardingNotificationKeepsOriginWithoutClientFanout(t *testing.T) {
	b, old, replacement, oldServer, newServer, cmServer := onboardingRelayFixture(t)
	var client bytes.Buffer
	b.codec = NewCodec(&client)
	b.engineSubscribed = true
	requestID := strings.Repeat("a", 32)
	b.forwardEngineNotificationFrom(old, "engine:onboarding-cluster-call", json.RawMessage(`{"requestId":"`+requestID+`","method":"cluster:invite-status","params":{"requestKey":"`+strings.Repeat("b", 32)+`"}}`))
	cmCodec := NewCodec(cmServer)
	_ = cmServer.SetReadDeadline(time.Now().Add(time.Second))
	request, err := cmCodec.Read()
	if err != nil || request.Method != "cluster:invite-status" {
		t.Fatalf("internal notification did not reach cluster route: %v", err)
	}
	b.setEngineMgr(replacement)
	if err := cmCodec.Respond(request.ID, map[string]string{"inviteId": "fixture-invite", "pin": "654321"}); err != nil {
		t.Fatal(err)
	}
	_ = oldServer.SetReadDeadline(time.Now().Add(time.Second))
	reply, err := NewCodec(oldServer).Read()
	if err != nil || reply.Method != "engine:onboarding-cluster-response" || !bytes.Contains(reply.Params, []byte(requestID)) {
		t.Fatal("internal response lost its originating worker")
	}
	_ = newServer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := NewCodec(newServer).Read(); err == nil {
		t.Fatal("onboarding response reached a replacement worker")
	}
	if client.Len() != 0 {
		t.Fatal("internal onboarding request or response reached an engine subscriber")
	}
}
