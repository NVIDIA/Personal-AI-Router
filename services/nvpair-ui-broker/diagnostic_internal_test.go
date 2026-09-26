// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"nvpair-ui-broker/workloadstore"
)

func TestDiagnosticWorkloadSnapshotIsRequestCorrelatedAndFresh(t *testing.T) {
	var output bytes.Buffer
	b := &Broker{engineMgr: &rpcWorker{peer: NewPeer(NewCodec(&output))}, workloads: workloadstore.New()}
	id := strings.Repeat("a", 32)
	query := json.RawMessage(`{"requestId":"` + id + `"}`)
	read := func(wantIdle bool) {
		t.Helper()
		b.forwardEngineNotification("engine:diagnostic-workload-check", query)
		var msg struct {
			Method string `json:"method"`
			Params struct {
				RequestID string `json:"requestId"`
				Idle      bool   `json:"idle"`
			} `json:"params"`
		}
		if err := json.Unmarshal(output.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		output.Reset()
		if msg.Method != "engine:diagnostic-workload-state" || msg.Params.RequestID != id || msg.Params.Idle != wantIdle {
			t.Fatalf("snapshot=%+v", msg)
		}
	}
	read(true)
	info := json.RawMessage(`{"id":"request-1","originatedFrom":"owner","scheduledOn":"participant","state":"running","createdAt":1,"engine":"ollama"}`)
	in, ok := workloadstore.ParseIncoming(info)
	if !ok {
		t.Fatal("bad test workload")
	}
	b.workloads.Apply(in)
	read(false)
	info = json.RawMessage(`{"id":"request-1","originatedFrom":"owner","scheduledOn":"participant","state":"completed","createdAt":1,"engine":"ollama"}`)
	in, ok = workloadstore.ParseIncoming(info)
	if !ok {
		t.Fatal("bad terminal workload")
	}
	b.workloads.Apply(in)
	read(true)
}

func TestDiagnosticMalformedWorkloadCheckHasNoEffect(t *testing.T) {
	var output bytes.Buffer
	b := &Broker{engineMgr: &rpcWorker{peer: NewPeer(NewCodec(&output))}, workloads: workloadstore.New()}
	b.replyDiagnosticWorkloads(b.getEngineMgr(), json.RawMessage(`{"requestId":""}`))
	if output.Len() != 0 {
		t.Fatal("malformed check produced a response")
	}
}

func TestDiagnosticReplacedWorkerCannotReceiveOrRedirectReply(t *testing.T) {
	var oldOutput, replacementOutput bytes.Buffer
	old := &rpcWorker{peer: NewPeer(NewCodec(&oldOutput))}
	replacement := &rpcWorker{peer: NewPeer(NewCodec(&replacementOutput))}
	b := &Broker{workloads: workloadstore.New()}
	b.setEngineMgr(old)
	b.setEngineMgr(replacement)
	b.replyDiagnosticWorkloads(old, json.RawMessage(`{"requestId":"`+strings.Repeat("a", 32)+`"}`))
	if oldOutput.Len() != 0 || replacementOutput.Len() != 0 {
		t.Fatal("replaced worker's diagnostic request produced a reply")
	}

	b.replyDiagnosticWorkloads(replacement, json.RawMessage(`{"requestId":"`+strings.Repeat("b", 32)+`"}`))
	if oldOutput.Len() != 0 || replacementOutput.Len() == 0 {
		t.Fatal("current worker's diagnostic reply crossed process generations")
	}
}
