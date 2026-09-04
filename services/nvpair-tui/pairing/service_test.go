// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-tui/rpc"
)

// testPIN is the secret every leak assertion in this file looks for. It is
// distinctive enough that a substring match cannot fire by accident.
const testPIN = "402199"

// fakeBroker stands in for the TUI's broker client, recording what was asked
// and answering with whatever the test scripted.
type fakeBroker struct {
	mu      sync.Mutex
	calls   []brokerCall
	answers map[string]func(params any) (json.RawMessage, error)
}

type brokerCall struct {
	method string
	params map[string]any
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{answers: map[string]func(any) (json.RawMessage, error){}}
}

func (f *fakeBroker) answer(method string, fn func(params any) (json.RawMessage, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = fn
}

// reply is the common case: a fixed JSON result for a method.
func (f *fakeBroker) reply(method, result string) {
	f.answer(method, func(any) (json.RawMessage, error) {
		return json.RawMessage(result), nil
	})
}

func (f *fakeBroker) Call(_ context.Context, method string, params any) (*rpc.Message, error) {
	f.mu.Lock()
	recorded := brokerCall{method: method}
	if m, ok := params.(map[string]any); ok {
		recorded.params = m
	}
	f.calls = append(f.calls, recorded)
	fn := f.answers[method]
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("fake broker has no answer for %q", method)
	}
	result, err := fn(params)
	if err != nil {
		return nil, err
	}
	return &rpc.Message{JSONRPC: "2.0", Result: result}, nil
}

func (f *fakeBroker) lastParams(t *testing.T, method string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			return f.calls[i].params
		}
	}
	t.Fatalf("%s was never called; calls = %+v", method, f.calls)
	return nil
}

// inviteReceived builds the notification the cluster manager pushes to a node
// that has been invited. It never carries a PIN.
func inviteReceived(inviteID, fromName string) *rpc.Message {
	return &rpc.Message{
		JSONRPC: "2.0",
		Method:  "cluster:invite-received",
		Params: json.RawMessage(fmt.Sprintf(
			`{"inviteId":%q,"fromNodeId":"NODE-A","fromNodeUuid":"uuid-a","fromNodeName":%q,`+
				`"clusterId":"cluster-xyz","pin":null,"state":"pending","createdAt":1716998400000,"respondedAt":null}`,
			inviteID, fromName)),
	}
}

func TestInviteSplitsHostPortAndCarriesThePIN(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:invite-node",
		`{"inviteId":"inv-1","state":"pending","pin":"`+testPIN+`","fromNodeName":"Lab desk A"}`)
	svc := NewService(broker)

	res, err := svc.Invite(context.Background(), InviteRequest{Address: "10.0.0.5:14399"})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if res.Invite.PIN() != testPIN {
		t.Errorf("PIN = %q, want the manager's own", res.Invite.PIN())
	}
	// The result is relayed verbatim so a caller sees what the manager said,
	// not a re-encoding of the fields this package happens to model.
	if !bytes.Contains(res.Raw, []byte(`"inviteId":"inv-1"`)) {
		t.Errorf("raw result = %s, want the manager's own JSON", res.Raw)
	}

	params := broker.lastParams(t, "cluster:invite-node")
	if params["address"] != "10.0.0.5" {
		t.Errorf("address = %v, want the host split off", params["address"])
	}
	if params["port"] != 14399 {
		t.Errorf("port = %v, want the port split into its own field", params["port"])
	}
}

func TestInviteParams(t *testing.T) {
	tests := []struct {
		name     string
		req      InviteRequest
		want     map[string]any
		wantErr  bool
		errFrags []string
	}{
		{
			name: "a bare host lets the manager append its own port",
			req:  InviteRequest{Address: "gpu-box.tail1234.ts.net"},
			want: map[string]any{"address": "gpu-box.tail1234.ts.net"},
		},
		{
			name: "an explicit port wins over one embedded in the address",
			req:  InviteRequest{Address: "10.0.0.5:1111", Port: 2222},
			want: map[string]any{"address": "10.0.0.5:1111", "port": 2222},
		},
		{
			name: "a node id is passed through as the target identity",
			req:  InviteRequest{Address: "10.0.0.5", NodeID: "uuid-b"},
			want: map[string]any{"address": "10.0.0.5", "nodeId": "uuid-b"},
		},
		{
			name: "a bracketed IPv6 host:port splits like any other",
			req:  InviteRequest{Address: "[fd00::1]:14321"},
			want: map[string]any{"address": "fd00::1", "port": 14321},
		},
		{
			name:     "an empty address is rejected before any call",
			req:      InviteRequest{Address: "   "},
			wantErr:  true,
			errFrags: []string{"address is required"},
		},
		{
			name:     "an out-of-range port is rejected",
			req:      InviteRequest{Address: "10.0.0.5", Port: 70000},
			wantErr:  true,
			errFrags: []string{"out of range"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.req.params()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("params() = %v, want an error", got)
				}
				for _, frag := range tc.errFrags {
					if !strings.Contains(err.Error(), frag) {
						t.Errorf("error %q does not mention %q", err, frag)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("params(): %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("params() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInviteReportsARejectionAsAResultNotAnError(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:invite-node", `{"inviteId":"inv-1","state":"rejected","reason":"already-clustered","pin":null}`)
	svc := NewService(broker)
	events, stop := svc.Subscribe()
	defer stop()

	res, err := svc.Invite(context.Background(), InviteRequest{Address: "10.0.0.5"})
	if err != nil {
		t.Fatalf("a rejection is a result, not an error: %v", err)
	}
	if res.Invite.State != StateRejected || res.Invite.Reason != "already-clustered" {
		t.Fatalf("invite = %+v, want a rejection carrying its reason", res.Invite)
	}
	if ev := nextEvent(t, events); ev.Kind != EventInviteRejected {
		t.Errorf("event kind = %q, want %q", ev.Kind, EventInviteRejected)
	}
}

func TestPendingTracksInboundInvitesAndStripsThePin(t *testing.T) {
	svc := NewService(newFakeBroker())

	if got := svc.Pending(); len(got) != 0 {
		t.Fatalf("a fresh service has %d pending invites, want none", len(got))
	}

	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))
	svc.HandleNotification(inviteReceived("inv-2", "Lab desk B"))

	pending := svc.Pending()
	if len(pending) != 2 {
		t.Fatalf("pending = %d invites, want 2", len(pending))
	}
	if pending[0].InviteID != "inv-1" || pending[1].InviteID != "inv-2" {
		t.Errorf("pending order = %s,%s, want arrival order", pending[0].InviteID, pending[1].InviteID)
	}
	if pending[0].ReceivedAt == 0 {
		t.Error("an inbound invite must be stamped with this node's own clock for its age")
	}
	for _, inv := range pending {
		if inv.Pin != nil {
			t.Errorf("invite %s carries a pin; an inbound invite never does", inv.InviteID)
		}
	}

	// The raw form keeps every field the manager sent, minus the pin.
	raw := svc.PendingRaw()
	if len(raw) != 2 {
		t.Fatalf("PendingRaw = %d invites, want 2", len(raw))
	}
	var fields map[string]any
	if err := json.Unmarshal(raw[0], &fields); err != nil {
		t.Fatalf("decode raw pending invite: %v", err)
	}
	if _, ok := fields["pin"]; ok {
		t.Error("PendingRaw kept the pin member; it must be stripped")
	}
	if fields["clusterId"] != "cluster-xyz" {
		t.Errorf("PendingRaw dropped clusterId (%v); the manager's own fields must survive", fields["clusterId"])
	}
	if fields["receivedAt"] == nil {
		t.Error("PendingRaw did not stamp receivedAt")
	}
}

func TestAWithdrawnInviteStopsBeingPending(t *testing.T) {
	for _, method := range []string{"cluster:invite-canceled", "cluster:invite-expired"} {
		t.Run(method, func(t *testing.T) {
			svc := NewService(newFakeBroker())
			svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))
			events, stop := svc.Subscribe()
			defer stop()

			svc.HandleNotification(&rpc.Message{
				JSONRPC: "2.0",
				Method:  method,
				Params:  json.RawMessage(`{"inviteId":"inv-1","state":"expired","fromNodeName":"Lab desk A"}`),
			})
			if got := svc.Pending(); len(got) != 0 {
				t.Fatalf("pending = %d, want the withdrawn invite gone", len(got))
			}
			if ev := nextEvent(t, events); ev.Kind != EventInviteCleared {
				t.Errorf("event kind = %q, want %q", ev.Kind, EventInviteCleared)
			}
		})
	}
}

func TestAWithdrawalForSomebodyElsesInvitePublishesNothing(t *testing.T) {
	svc := NewService(newFakeBroker())
	events, stop := svc.Subscribe()
	defer stop()

	// An outbound invite of this node's expiring: not in the pending set, so
	// there is nothing to clear and nothing to say.
	svc.HandleNotification(&rpc.Message{
		JSONRPC: "2.0",
		Method:  "cluster:invite-expired",
		Params:  json.RawMessage(`{"inviteId":"inv-outbound","state":"expired"}`),
	})
	select {
	case ev := <-events:
		t.Fatalf("published %+v for an invite this node was not holding", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRespondResolvesTheInviteWhenExactlyOneIsPending(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:respond-to-invite", `{"inviteId":"inv-1","state":"paired","fromNodeName":"Lab desk A"}`)
	svc := NewService(broker)
	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))

	res, err := svc.Respond(context.Background(), "", true, testPIN)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.Invite.State != StatePaired {
		t.Errorf("state = %q, want paired", res.Invite.State)
	}
	params := broker.lastParams(t, "cluster:respond-to-invite")
	if params["inviteId"] != "inv-1" {
		t.Errorf("inviteId = %v, want the one pending invite resolved", params["inviteId"])
	}
	if params["pin"] != testPIN {
		t.Errorf("pin = %v, want it forwarded to the manager", params["pin"])
	}
	if got := svc.Pending(); len(got) != 0 {
		t.Errorf("an answered invite is still pending: %+v", got)
	}
}

func TestRespondWithNoPendingInvite(t *testing.T) {
	svc := NewService(newFakeBroker())
	_, err := svc.Respond(context.Background(), "", true, testPIN)
	if !errors.Is(err, ErrNoPendingInvite) {
		t.Fatalf("err = %v, want ErrNoPendingInvite", err)
	}
}

func TestRespondWithSeveralPendingNamesThemAll(t *testing.T) {
	svc := NewService(newFakeBroker())
	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))
	svc.HandleNotification(inviteReceived("inv-2", "Lab desk B"))

	_, err := svc.Respond(context.Background(), "", true, testPIN)
	var ambiguous *AmbiguousInviteError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want *AmbiguousInviteError", err)
	}
	if len(ambiguous.Invites) != 2 {
		t.Fatalf("named %d invites, want 2", len(ambiguous.Invites))
	}
	for _, want := range []string{"inv-1", "inv-2", "Lab desk A", "Lab desk B", "--invite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

func TestRespondDropsAnInviteTheManagerCanNoLongerAnswer(t *testing.T) {
	for _, code := range []int{-32001, -32002} {
		t.Run(fmt.Sprintf("code %d", code), func(t *testing.T) {
			broker := newFakeBroker()
			broker.answer("cluster:respond-to-invite", func(any) (json.RawMessage, error) {
				return nil, &rpc.RPCError{Code: code, Message: "gone"}
			})
			svc := NewService(broker)
			svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))

			if _, err := svc.Respond(context.Background(), "", true, testPIN); err == nil {
				t.Fatal("Respond must surface the manager's error")
			}
			if got := svc.Pending(); len(got) != 0 {
				t.Errorf("an unanswerable invite stayed pending: %+v", got)
			}
		})
	}
}

func TestRespondKeepsAnInviteAfterATransientFailure(t *testing.T) {
	broker := newFakeBroker()
	broker.answer("cluster:respond-to-invite", func(any) (json.RawMessage, error) {
		return nil, &rpc.RPCError{Code: -32603, Message: "internal error"}
	})
	svc := NewService(broker)
	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))

	if _, err := svc.Respond(context.Background(), "", true, testPIN); err == nil {
		t.Fatal("Respond must surface the manager's error")
	}
	if got := svc.Pending(); len(got) != 1 {
		t.Errorf("pending = %d, want the invite kept so it can be retried", len(got))
	}
}

func TestRespondDeclineSendsNoPin(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:respond-to-invite", `{"inviteId":"inv-1","state":"declined"}`)
	svc := NewService(broker)
	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))

	if _, err := svc.Respond(context.Background(), "inv-1", false, testPIN); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	params := broker.lastParams(t, "cluster:respond-to-invite")
	if _, ok := params["pin"]; ok {
		t.Error("a decline carried a pin; there is nothing to prove on a decline")
	}
}

func TestRespondReportsAWrongPinAsAResult(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:respond-to-invite", `{"inviteId":"inv-1","state":"failed","reason":"incorrect-pin"}`)
	svc := NewService(broker)
	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))

	res, err := svc.Respond(context.Background(), "", true, "000000")
	if err != nil {
		t.Fatalf("a wrong PIN is a result, not an error: %v", err)
	}
	if res.Invite.State != StateFailed || res.Invite.Reason != ReasonIncorrectPin {
		t.Errorf("invite = %+v, want failed/incorrect-pin", res.Invite)
	}
}

func TestMembersJoinsIdentityAndRoster(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:get-node-id",
		`{"nodeUuid":"uuid-a","nodeId":"NODE-A","name":"Lab desk A","clusterId":"cluster-xyz","clusterFriendlyName":"Lab 3 desks"}`)
	broker.reply("nodes:get-initial",
		`{"nodes":[{"id":"NODE-B","nodeUuid":"uuid-b","name":"Lab desk B","ipAddress":"10.0.0.5","port":14321,"state":"member"}]}`)
	svc := NewService(broker)

	got, err := svc.Members(context.Background())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if got.ClusterID != "cluster-xyz" || got.ClusterFriendlyName != "Lab 3 desks" {
		t.Errorf("identity = %+v, want the cluster it reported", got)
	}
	if len(got.Members) != 1 || got.Members[0].ID != "NODE-B" || got.Members[0].IPAddress != "10.0.0.5" {
		t.Errorf("members = %+v, want the roster", got.Members)
	}
}

func TestMembersOfAnUnclusteredNodeIsAnEmptyList(t *testing.T) {
	broker := newFakeBroker()
	broker.reply("cluster:get-node-id", `{"nodeUuid":"uuid-a","nodeId":"NODE-A","name":"Lab desk A","clusterId":""}`)
	broker.reply("nodes:get-initial", `{"nodes":null}`)
	svc := NewService(broker)

	got, err := svc.Members(context.Background())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if got.ClusterID != "" {
		t.Errorf("clusterId = %q, want empty", got.ClusterID)
	}
	if got.Members == nil || len(got.Members) != 0 {
		t.Errorf("members = %#v, want an empty list rather than null", got.Members)
	}
}

func TestAwaitPendingReturnsAnInviteThatIsAlreadyWaiting(t *testing.T) {
	svc := NewService(newFakeBroker())
	svc.HandleNotification(inviteReceived("inv-1", "Lab desk A"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inv, err := svc.AwaitPending(ctx)
	if err != nil {
		t.Fatalf("AwaitPending: %v", err)
	}
	if inv.InviteID != "inv-1" {
		t.Errorf("inviteId = %q, want inv-1", inv.InviteID)
	}
}

func TestAwaitPendingWakesOnAnInviteThatArrivesLater(t *testing.T) {
	svc := NewService(newFakeBroker())
	go func() {
		time.Sleep(30 * time.Millisecond)
		svc.HandleNotification(inviteReceived("inv-late", "Lab desk A"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inv, err := svc.AwaitPending(ctx)
	if err != nil {
		t.Fatalf("AwaitPending: %v", err)
	}
	if inv.InviteID != "inv-late" {
		t.Errorf("inviteId = %q, want inv-late", inv.InviteID)
	}
}

func TestAwaitPendingHonoursItsDeadline(t *testing.T) {
	svc := NewService(newFakeBroker())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := svc.AwaitPending(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline to expire", err)
	}
}

// TestASlowSubscriberCannotStallAPairing fills a subscriber's buffer and
// checks that the pairing it is not reading about still completes.
func TestASlowSubscriberCannotStallAPairing(t *testing.T) {
	svc := NewService(newFakeBroker())
	_, stop := svc.Subscribe() // subscribed, never read
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < eventBuffer*3; i++ {
			svc.HandleNotification(inviteReceived(fmt.Sprintf("inv-%d", i), "Lab desk A"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber that stopped reading blocked the pairing service")
	}
}

// TestThePINNeverReachesTheLog drives a whole pairing — both sides of it —
// through a service whose logger writes into a buffer, and asserts the PIN is
// nowhere in what was logged.
//
// The default logger is replaced rather than applog.SetOutput, which is a
// once-only hook that a second test cannot re-arm; a marker line proves the
// buffer really is capturing, so the assertion cannot pass vacuously.
func TestThePINNeverReachesTheLog(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	previousWriter := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
	})
	slog.Info("capture-marker")

	broker := newFakeBroker()
	broker.reply("cluster:invite-node", `{"inviteId":"inv-1","state":"pending","pin":"`+testPIN+`"}`)
	broker.reply("cluster:invite-status", `{"inviteId":"inv-1","state":"pending","pin":"`+testPIN+`"}`)
	broker.reply("cluster:respond-to-invite", `{"inviteId":"inv-2","state":"paired"}`)
	svc := NewService(broker)

	if _, err := svc.Invite(context.Background(), InviteRequest{Address: "10.0.0.5"}); err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if _, err := svc.InviteStatus(context.Background(), "inv-1"); err != nil {
		t.Fatalf("InviteStatus: %v", err)
	}
	svc.HandleNotification(inviteReceived("inv-2", "Lab desk A"))
	if _, err := svc.Respond(context.Background(), "", true, testPIN); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	captured := logged.String()
	if !strings.Contains(captured, "capture-marker") {
		t.Fatal("the log buffer captured nothing, so this assertion proves nothing")
	}
	if strings.Contains(captured, testPIN) {
		t.Errorf("the pairing PIN reached the log:\n%s", captured)
	}
}

// nextEvent takes the next pairing event, failing rather than hanging.
func nextEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("the event channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no pairing event arrived")
		return Event{}
	}
}
