// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-tui/pairing"
	"nvpair-tui/rpc"
)

// stubBroker answers the cluster-manager methods the endpoint relays.
type stubBroker struct {
	mu      sync.Mutex
	answers map[string]json.RawMessage
	errs    map[string]*rpc.RPCError
	seen    []string
}

func newStubBroker() *stubBroker {
	return &stubBroker{answers: map[string]json.RawMessage{}, errs: map[string]*rpc.RPCError{}}
}

func (s *stubBroker) reply(method, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[method] = json.RawMessage(result)
}

func (s *stubBroker) failWith(method string, code int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[method] = &rpc.RPCError{Code: code, Message: message}
}

func (s *stubBroker) Call(_ context.Context, method string, _ any) (*rpc.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, method)
	if err, ok := s.errs[method]; ok {
		return nil, err
	}
	result, ok := s.answers[method]
	if !ok {
		return nil, fmt.Errorf("stub broker has no answer for %q", method)
	}
	return &rpc.Message{JSONRPC: "2.0", Result: result}, nil
}

// endpoint is a running control socket with a client attached, the way a
// subcommand meets one.
type endpoint struct {
	client *Client
	path   string
	pairs  *pairing.Service
	broker *stubBroker
}

func startEndpoint(t *testing.T, ready bool) *endpoint {
	t.Helper()
	broker := newStubBroker()
	pairs := pairing.NewService(broker)

	path := socketPath(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = NewServer(pairs, "9.9.9", func() bool { return ready }).Serve(ctx, ln)
	}()

	client, err := Dial(path)
	if err != nil {
		cancel()
		t.Fatalf("Dial: %v", err)
	}
	ep := &endpoint{client: client, path: path, pairs: pairs, broker: broker}
	t.Cleanup(func() {
		_ = ep.client.Close()
		cancel()
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the control server did not stop")
		}
	})
	return ep
}

func (e *endpoint) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := e.client.Call(ctx, method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return raw
}

func (e *endpoint) callErr(t *testing.T, method string, params any) *Error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := e.client.Call(ctx, method, params)
	if err == nil {
		t.Fatalf("%s succeeded; an error was expected", method)
	}
	rpcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("%s returned %T (%v), want a JSON-RPC error", method, err, err)
	}
	return rpcErr
}

func TestPingReportsVersionAndBrokerReadiness(t *testing.T) {
	e := startEndpoint(t, true)
	var got struct {
		Version     string `json:"version"`
		BrokerReady bool   `json:"brokerReady"`
	}
	if err := json.Unmarshal(e.call(t, "ping", nil), &got); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if got.Version != "9.9.9" || !got.BrokerReady {
		t.Errorf("ping = %+v, want the stamped version and a ready broker", got)
	}
}

func TestPingReportsABrokerThatHasNotAnnouncedItself(t *testing.T) {
	e := startEndpoint(t, false)
	var got struct {
		BrokerReady bool `json:"brokerReady"`
	}
	if err := json.Unmarshal(e.call(t, "ping", nil), &got); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if got.BrokerReady {
		t.Error("brokerReady = true before the broker announced itself")
	}
}

func TestPairInviteRelaysTheManagersOwnInvite(t *testing.T) {
	e := startEndpoint(t, true)
	e.broker.reply("cluster:invite-node",
		`{"inviteId":"inv-1","state":"pending","pin":"402199","clusterFriendlyName":"Lab 3 desks","somethingNew":42}`)

	raw := e.call(t, "pair:invite", map[string]any{"address": "10.0.0.5", "port": 14321})
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fields["pin"] != "402199" {
		t.Errorf("pin = %v, want the manager's own", fields["pin"])
	}
	// A field this package does not model must survive the relay, or the
	// endpoint would silently drop parts of the manager's contract.
	if fields["somethingNew"] != float64(42) {
		t.Errorf("the relay dropped an unmodelled field: %v", fields)
	}
}

func TestPairInviteRejectsAnEmptyAddress(t *testing.T) {
	e := startEndpoint(t, true)
	rpcErr := e.callErr(t, "pair:invite", map[string]any{"address": ""})
	if !strings.Contains(rpcErr.Message, "address is required") {
		t.Errorf("message = %q, want it to name the missing address", rpcErr.Message)
	}
}

func TestPairInviteRelaysAManagerError(t *testing.T) {
	e := startEndpoint(t, true)
	e.broker.failWith("cluster:invite-node", -32603, "cluster manager is not available")

	rpcErr := e.callErr(t, "pair:invite", map[string]any{"address": "10.0.0.5"})
	if rpcErr.Code != -32603 {
		t.Errorf("code = %d, want the manager's own code relayed", rpcErr.Code)
	}
	if rpcErr.Message != "cluster manager is not available" {
		t.Errorf("message = %q, want the manager's own", rpcErr.Message)
	}
}

func TestPairInviteStatusRequiresAnInviteID(t *testing.T) {
	e := startEndpoint(t, true)
	rpcErr := e.callErr(t, "pair:invite-status", map[string]any{})
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestPairInviteStatusReturnsTheCurrentInvite(t *testing.T) {
	e := startEndpoint(t, true)
	e.broker.reply("cluster:invite-status", `{"inviteId":"inv-1","state":"paired"}`)

	var got pairing.Invite
	if err := json.Unmarshal(e.call(t, "pair:invite-status", map[string]any{"inviteId": "inv-1"}), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != pairing.StatePaired {
		t.Errorf("state = %q, want paired", got.State)
	}
}

func TestPairPendingIsAnEmptyListWhenNothingIsWaiting(t *testing.T) {
	e := startEndpoint(t, true)
	raw := e.call(t, "pair:pending", nil)
	if string(raw) != `{"invites":[]}` {
		t.Errorf("pair:pending = %s, want an empty list rather than null", raw)
	}
}

func TestPairPendingListsInboundInvitesWithoutAPin(t *testing.T) {
	e := startEndpoint(t, true)
	e.pairs.HandleNotification(&rpc.Message{
		JSONRPC: "2.0",
		Method:  "cluster:invite-received",
		Params:  json.RawMessage(`{"inviteId":"inv-1","fromNodeName":"Lab desk A","state":"pending","pin":null}`),
	})

	var got struct {
		Invites []map[string]any `json:"invites"`
	}
	if err := json.Unmarshal(e.call(t, "pair:pending", nil), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Invites) != 1 {
		t.Fatalf("pending = %d invites, want 1", len(got.Invites))
	}
	if got.Invites[0]["inviteId"] != "inv-1" {
		t.Errorf("inviteId = %v", got.Invites[0]["inviteId"])
	}
	if _, ok := got.Invites[0]["pin"]; ok {
		t.Error("pair:pending disclosed a pin member")
	}
	if got.Invites[0]["receivedAt"] == nil {
		t.Error("pair:pending did not stamp receivedAt, so a caller cannot age the invite")
	}
}

func TestPairRespondRequiresAcceptToBeStated(t *testing.T) {
	e := startEndpoint(t, true)
	rpcErr := e.callErr(t, "pair:respond", map[string]any{"pin": "402199"})
	if rpcErr.Code != CodeInvalidParams || !strings.Contains(rpcErr.Message, "accept is required") {
		t.Errorf("error = %+v, want accept to be required", rpcErr)
	}
}

func TestPairRespondWithNothingPending(t *testing.T) {
	e := startEndpoint(t, true)
	rpcErr := e.callErr(t, "pair:respond", map[string]any{"accept": true, "pin": "402199"})
	if rpcErr.Code != CodeNoPendingInvite {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeNoPendingInvite)
	}
}

func TestPairRespondWithSeveralPendingNamesThem(t *testing.T) {
	e := startEndpoint(t, true)
	for _, id := range []string{"inv-1", "inv-2"} {
		e.pairs.HandleNotification(&rpc.Message{
			JSONRPC: "2.0",
			Method:  "cluster:invite-received",
			Params:  json.RawMessage(fmt.Sprintf(`{"inviteId":%q,"fromNodeName":"Lab desk %s","state":"pending"}`, id, id)),
		})
	}

	rpcErr := e.callErr(t, "pair:respond", map[string]any{"accept": true, "pin": "402199"})
	if rpcErr.Code != CodeAmbiguousInvite {
		t.Fatalf("code = %d, want %d", rpcErr.Code, CodeAmbiguousInvite)
	}
	var data struct {
		Invites []struct {
			InviteID     string `json:"inviteId"`
			FromNodeName string `json:"fromNodeName"`
		} `json:"invites"`
	}
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if len(data.Invites) != 2 {
		t.Fatalf("error data named %d invites, want both", len(data.Invites))
	}
	for _, want := range []string{"inv-1", "inv-2"} {
		if !strings.Contains(rpcErr.Message, want) {
			t.Errorf("message %q does not name %q", rpcErr.Message, want)
		}
	}
}

func TestPairRespondAcceptsTheSolePendingInvite(t *testing.T) {
	e := startEndpoint(t, true)
	e.pairs.HandleNotification(&rpc.Message{
		JSONRPC: "2.0",
		Method:  "cluster:invite-received",
		Params:  json.RawMessage(`{"inviteId":"inv-1","fromNodeName":"Lab desk A","state":"pending"}`),
	})
	e.broker.reply("cluster:respond-to-invite", `{"inviteId":"inv-1","state":"paired"}`)

	var got pairing.Invite
	if err := json.Unmarshal(e.call(t, "pair:respond", map[string]any{"accept": true, "pin": "402199"}), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != pairing.StatePaired {
		t.Errorf("state = %q, want paired", got.State)
	}
	raw := e.call(t, "pair:pending", nil)
	if string(raw) != `{"invites":[]}` {
		t.Errorf("the answered invite is still pending: %s", raw)
	}
}

func TestPairRespondReportsAWrongPinAsAResult(t *testing.T) {
	e := startEndpoint(t, true)
	e.pairs.HandleNotification(&rpc.Message{
		JSONRPC: "2.0",
		Method:  "cluster:invite-received",
		Params:  json.RawMessage(`{"inviteId":"inv-1","state":"pending"}`),
	})
	e.broker.reply("cluster:respond-to-invite", `{"inviteId":"inv-1","state":"failed","reason":"incorrect-pin"}`)

	var got pairing.Invite
	if err := json.Unmarshal(e.call(t, "pair:respond", map[string]any{"accept": true, "pin": "000000"}), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != pairing.StateFailed || got.Reason != pairing.ReasonIncorrectPin {
		t.Errorf("invite = %+v, want a failed result carrying incorrect-pin", got)
	}
}

func TestPairMembers(t *testing.T) {
	e := startEndpoint(t, true)
	e.broker.reply("cluster:get-node-id",
		`{"nodeUuid":"uuid-a","nodeId":"NODE-A","name":"Lab desk A","clusterId":"cluster-xyz","clusterFriendlyName":"Lab 3 desks"}`)
	e.broker.reply("nodes:get-initial",
		`{"nodes":[{"id":"NODE-B","nodeUuid":"uuid-b","name":"Lab desk B","ipAddress":"10.0.0.5","port":14321,"state":"member"}]}`)

	var got pairing.Membership
	if err := json.Unmarshal(e.call(t, "pair:members", nil), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClusterID != "cluster-xyz" || got.NodeID != "NODE-A" {
		t.Errorf("identity = %+v", got)
	}
	if len(got.Members) != 1 || got.Members[0].ID != "NODE-B" {
		t.Errorf("members = %+v", got.Members)
	}
}

func TestUnknownMethod(t *testing.T) {
	e := startEndpoint(t, true)
	rpcErr := e.callErr(t, "pair:teleport", nil)
	if rpcErr.Code != CodeMethodNotFound {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeMethodNotFound)
	}
}

func TestTheEndpointServesSequentialClients(t *testing.T) {
	e := startEndpoint(t, true)
	// The first client is the one startEndpoint opened; use it, close it, and
	// then reach the same endpoint again — the shape every subcommand has,
	// each being its own short-lived process.
	e.call(t, "ping", nil)
	if err := e.client.Close(); err != nil {
		t.Fatalf("close the first client: %v", err)
	}

	for i := 0; i < 3; i++ {
		client, err := Dial(e.path)
		if err != nil {
			t.Fatalf("client %d could not reach the endpoint: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := client.Call(ctx, "ping", nil); err != nil {
			cancel()
			_ = client.Close()
			t.Fatalf("client %d ping: %v", i, err)
		}
		cancel()
		_ = client.Close()
	}
	// Keep the cleanup's Close idempotent-safe by handing back a live client.
	client, err := Dial(e.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e.client = client
}

func TestDialReportsNoRunningInstance(t *testing.T) {
	_, err := Dial(socketPath(t))
	if err == nil {
		t.Fatal("Dial succeeded against an endpoint that does not exist")
	}
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %T (%v), want it to unwrap to ErrNotRunning", err, err)
	}
	var notRunning *NotRunningError
	if !errors.As(err, &notRunning) {
		t.Fatalf("err = %T (%v), want *NotRunningError", err, err)
	}
	if !strings.Contains(err.Error(), "no nvpair-tui is listening") {
		t.Errorf("message = %q, want it to say nothing is listening", err)
	}
}
