// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"nvpair-tui/rpc"
)

type headlessFakeCaller struct {
	method string
	params json.RawMessage
	err    error
	result json.RawMessage
}

func (f *headlessFakeCaller) Call(_ context.Context, method string, params any) (*rpc.Message, error) {
	f.method = method
	f.params = params.(json.RawMessage)
	return &rpc.Message{Result: f.result}, f.err
}

func TestHeadlessPrivateControlUsesExistingPairingMethods(t *testing.T) {
	client := &headlessFakeCaller{result: json.RawMessage(`{"state":"paired","pin":null}`)}
	request := headlessRequest{Method: "cluster:respond-to-invite", Params: json.RawMessage(`{"inviteId":"one-approved-invite","accept":true,"pin":"test-only-canary"}`)}
	response := callHeadless(context.Background(), client, true, request)
	if response.Error != nil || client.method != request.Method || string(client.params) != string(request.Params) || string(response.Result) != string(client.result) {
		t.Fatalf("private existing-RPC relay failed: %+v", response)
	}
}

func TestHeadlessControlRejectsFreeformAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{"method":"engine:start","params":{"engine":"ollama"}}`, `{"method":"cluster:respond-to-invite","params":{"inviteId":"x","accept":true,"shell":"bad"}}`, `{"method":"cluster:get-node-id","host":"unexpected"}`, `{"method":"cluster:get-node-id"} {}`} {
		var request headlessRequest
		err := decodeHeadless([]byte(raw), &request)
		if err == nil {
			err = validateHeadlessRequest(request)
		}
		if err == nil {
			t.Fatalf("unsupported control request accepted: %s", raw)
		}
	}
	var request headlessRequest
	if err := decodeHeadless([]byte(strings.Repeat("x", headlessRequestLimit+1)), &request); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestHeadlessErrorsDoNotEchoPairingMaterial(t *testing.T) {
	canary := "test-only-sensitive-canary"
	client := &headlessFakeCaller{err: &rpc.RPCError{Code: -32000, Message: canary, Data: json.RawMessage(`{"pin":"` + canary + `"}`)}}
	response := callHeadless(context.Background(), client, true, headlessRequest{Method: "cluster:invite-status", Params: json.RawMessage(`{"inviteId":"x"}`)})
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), canary) || response.Error == nil || response.Error.RPCCode != -32000 {
		t.Fatalf("unsafe error response: %s", encoded)
	}
	client.err = errors.New(canary)
	response = callHeadless(context.Background(), client, true, headlessRequest{Method: "cluster:get-node-id"})
	if response.Error == nil || response.Error.Code != "completion-unknown" || strings.Contains(response.Error.Message, canary) {
		t.Fatalf("unsafe transport error: %+v", response)
	}
}

func TestHeadlessStartingDoesNotInventIdentityOrPairing(t *testing.T) {
	client := &headlessFakeCaller{}
	response := callHeadless(context.Background(), client, false, headlessRequest{Method: "headless:status"})
	if string(response.Result) != `{"mode":"headless","state":"starting","version":"`+Version+`"}` || client.method != "" {
		t.Fatalf("status=%s", response.Result)
	}
	response = callHeadless(context.Background(), client, false, headlessRequest{Method: "cluster:get-node-id"})
	if response.Error == nil || response.Error.Code != "not-ready" || client.method != "" {
		t.Fatal("identity was queried before broker readiness")
	}
}

func TestHeadlessInputDeadlineDoesNotWaitForAnotherFrame(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := runHeadlessControl(ctx, reader, io.Discard)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("unbounded input read: %v", err)
	}
}
