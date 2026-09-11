// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"nvpair-tui/rpc"
)

// Broker is the half of the broker client this package needs. The TUI's
// rpc.Client satisfies it; a test supplies its own.
type Broker interface {
	Call(ctx context.Context, method string, params any) (*rpc.Message, error)
}

// eventBuffer bounds a subscriber's queue. Pairing events are rare — a
// handful per pairing — so this only has to absorb the case of a subscriber
// that is briefly not reading (the Bubble Tea loop between frames). A
// subscriber that falls this far behind loses events rather than stalling
// the pairing that produced them.
const eventBuffer = 64

// EventKind names what happened to a pairing. Subscribers switch on it.
type EventKind string

const (
	// EventInviteSent — this node created an outbound invite. The event
	// carries the PIN, which is why it goes only to in-process subscribers
	// (the Cluster tab's status line) and never to a log.
	EventInviteSent EventKind = "invite-sent"
	// EventInviteRejected — the target refused to pair (already clustered).
	EventInviteRejected EventKind = "invite-rejected"
	// EventInviteFailed — an outbound invite could not be created at all.
	EventInviteFailed EventKind = "invite-failed"
	// EventInviteReceived — an inbound invite is now waiting for an answer.
	EventInviteReceived EventKind = "invite-received"
	// EventInviteCleared — a waiting inbound invite went away on its own
	// (canceled by the inviter, superseded, or expired).
	EventInviteCleared EventKind = "invite-cleared"
	// EventResponded — this node answered an inbound invite.
	EventResponded EventKind = "responded"
	// EventRespondFailed — answering an inbound invite errored out.
	EventRespondFailed EventKind = "respond-failed"
)

// Event is one pairing state change, delivered to every subscriber. Invite is
// the session it concerns; Accept distinguishes an accept from a decline on
// EventResponded; Err carries the failure on the *Failed kinds.
type Event struct {
	Kind   EventKind
	Invite Invite
	Accept bool
	Err    error
}

// pendingInvite is an inbound invite still waiting for an answer: the decode
// this package reasons about, and the manager's own JSON with the pin removed
// and receivedAt stamped in, which is what pair:pending returns.
type pendingInvite struct {
	invite Invite
	raw    json.RawMessage
}

// Service drives pairing over one broker connection and remembers which
// inbound invites are still unanswered.
//
// Its pending set is deliberately session state: nvpair-cluster-manager has no
// "list the invites you are holding" call, and nodes:get-initial reports a
// pending-inbound peer without the invite id needed to answer it. A TUI
// restarted mid-pairing therefore reports nothing pending even though the
// manager still holds a live invite; the inviter has to re-invite. This is
// recorded in the component README.
type Service struct {
	broker Broker
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]pendingInvite
	arrived []string // pending ids, oldest first
	subs    map[int]chan Event
	nextSub int
}

// NewService builds a Service over a connected broker client.
func NewService(broker Broker) *Service {
	return &Service{
		broker:  broker,
		now:     time.Now,
		pending: make(map[string]pendingInvite),
		subs:    make(map[int]chan Event),
	}
}

// Subscribe returns a channel of pairing events and a function that stops the
// subscription and releases the channel. Every subscriber sees every event
// from the moment it subscribes.
func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, eventBuffer)
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, id)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// publish fans an event out to every subscriber. A subscriber whose buffer is
// full loses this event: a pairing must never block on a slow reader.
//
// The caller must not hold s.mu — a subscriber may call back into the Service
// from its own goroutine.
func (s *Service) publish(ev Event) {
	s.mu.Lock()
	targets := make([]chan Event, 0, len(s.subs))
	for _, ch := range s.subs {
		targets = append(targets, ch)
	}
	s.mu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Invite asks this node's cluster manager to pair with req's target and
// returns the manager's Invite. On success the result carries the six-digit
// PIN the operator reads to the other machine.
//
// If this node is not in a cluster yet the manager auto-founds a cluster of
// one first; there is no separate create step to drive.
func (s *Service) Invite(ctx context.Context, req InviteRequest) (Result, error) {
	params, err := req.params()
	if err != nil {
		s.publish(Event{Kind: EventInviteFailed, Err: err})
		return Result{}, err
	}
	res, err := s.call(ctx, "cluster:invite-node", params)
	if err != nil {
		s.publish(Event{Kind: EventInviteFailed, Err: err})
		return Result{}, err
	}
	if res.Invite.State == StateRejected {
		s.publish(Event{Kind: EventInviteRejected, Invite: res.Invite})
		return res, nil
	}
	s.publish(Event{Kind: EventInviteSent, Invite: res.Invite})
	return res, nil
}

// InviteStatus returns the manager's current view of one invite, whichever
// side of the pairing created it.
func (s *Service) InviteStatus(ctx context.Context, inviteID string) (Result, error) {
	if inviteID == "" {
		return Result{}, fmt.Errorf("an invite id is required")
	}
	return s.call(ctx, "cluster:invite-status", map[string]any{"inviteId": inviteID})
}

// Pending returns the inbound invites still waiting for an answer, oldest
// first. The PIN is never part of an inbound invite, and is stripped again
// here so no caller can leak one it did not have.
func (s *Service) Pending() []Invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Invite, 0, len(s.arrived))
	for _, id := range s.arrived {
		if p, ok := s.pending[id]; ok {
			inv := p.invite
			inv.Pin = nil
			out = append(out, inv)
		}
	}
	return out
}

// PendingRaw returns the same invites as Pending, each as the manager's own
// JSON with the pin removed and receivedAt stamped in. The control socket
// returns these so a caller sees exactly what the manager reported.
func (s *Service) PendingRaw() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, 0, len(s.arrived))
	for _, id := range s.arrived {
		p, ok := s.pending[id]
		if !ok {
			continue
		}
		if p.raw != nil {
			out = append(out, p.raw)
			continue
		}
		inv := p.invite
		inv.Pin = nil
		if encoded, err := json.Marshal(inv); err == nil {
			out = append(out, encoded)
		}
	}
	return out
}

// Respond answers an inbound invite. An empty inviteID means "the one that is
// pending": with none waiting that is ErrNoPendingInvite, and with several it
// is an *AmbiguousInviteError naming them all.
//
// A wrong PIN is not an error here — it comes back as a successful result in
// state "failed" with reason "incorrect-pin", which is what the manager
// reports and what the caller's exit code turns on.
func (s *Service) Respond(ctx context.Context, inviteID string, accept bool, pin string) (Result, error) {
	if inviteID == "" {
		resolved, err := s.solePending()
		if err != nil {
			s.publish(Event{Kind: EventRespondFailed, Accept: accept, Err: err})
			return Result{}, err
		}
		inviteID = resolved
	}
	params := map[string]any{"inviteId": inviteID, "accept": accept}
	if accept && pin != "" {
		params["pin"] = pin
	}
	res, err := s.call(ctx, "cluster:respond-to-invite", params)
	if err != nil {
		// An invite the manager does not know, or one it considers already
		// terminal, will never be answerable. Drop it so it cannot sit in
		// pair:pending forever.
		if unanswerable(err) {
			s.forget(inviteID)
		}
		s.publish(Event{Kind: EventRespondFailed, Invite: Invite{InviteID: inviteID}, Accept: accept, Err: err})
		return Result{}, err
	}
	s.forget(inviteID)
	s.publish(Event{Kind: EventResponded, Invite: res.Invite, Accept: accept})
	return res, nil
}

// Members returns this node's cluster identity and roster.
func (s *Service) Members(ctx context.Context) (Membership, error) {
	idMsg, err := s.broker.Call(ctx, "cluster:get-node-id", nil)
	if err != nil {
		return Membership{}, err
	}
	var identity struct {
		NodeUUID            string `json:"nodeUuid"`
		NodeID              string `json:"nodeId"`
		Name                string `json:"name"`
		ClusterID           string `json:"clusterId"`
		ClusterFriendlyName string `json:"clusterFriendlyName"`
	}
	if err := decode(idMsg.Result, &identity); err != nil {
		return Membership{}, fmt.Errorf("decode cluster:get-node-id: %w", err)
	}

	nodesMsg, err := s.broker.Call(ctx, "nodes:get-initial", nil)
	if err != nil {
		return Membership{}, err
	}
	var roster struct {
		Nodes []ClusterNode `json:"nodes"`
	}
	if err := decode(nodesMsg.Result, &roster); err != nil {
		return Membership{}, fmt.Errorf("decode nodes:get-initial: %w", err)
	}
	if roster.Nodes == nil {
		roster.Nodes = []ClusterNode{}
	}
	return Membership{
		ClusterID:           identity.ClusterID,
		ClusterFriendlyName: identity.ClusterFriendlyName,
		NodeID:              identity.NodeID,
		NodeUUID:            identity.NodeUUID,
		Name:                identity.Name,
		Members:             roster.Nodes,
	}, nil
}

// AwaitPending returns an inbound invite, immediately if one is already
// waiting and otherwise as soon as one arrives. It is what `accept --wait`
// blocks on. ctx bounds the wait.
func (s *Service) AwaitPending(ctx context.Context) (Invite, error) {
	// Subscribe before looking, so an invite that lands between the two is
	// seen on the channel rather than missed by both.
	events, unsubscribe := s.Subscribe()
	defer unsubscribe()

	if waiting := s.Pending(); len(waiting) > 0 {
		return waiting[0], nil
	}
	for {
		select {
		case <-ctx.Done():
			return Invite{}, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return Invite{}, fmt.Errorf("pairing service stopped while waiting for an invite")
			}
			if ev.Kind == EventInviteReceived {
				return ev.Invite, nil
			}
		}
	}
}

// HandleNotification folds one broker push into the pending set. The caller
// feeds it every notification; anything that is not a pairing push is ignored.
func (s *Service) HandleNotification(msg *rpc.Message) {
	if msg == nil {
		return
	}
	switch msg.Method {
	case "cluster:invite-received":
		var inv Invite
		if err := decode(msg.Params, &inv); err != nil || inv.InviteID == "" {
			return
		}
		inv.Pin = nil
		inv.ReceivedAt = s.now().UnixMilli()
		s.remember(inv, stripPin(msg.Params, inv.ReceivedAt))
		s.publish(Event{Kind: EventInviteReceived, Invite: inv})

	case "cluster:invite-canceled", "cluster:invite-expired":
		var inv Invite
		if err := decode(msg.Params, &inv); err != nil || inv.InviteID == "" {
			return
		}
		inv.Pin = nil
		if !s.forget(inv.InviteID) {
			// Not one of ours — an outbound invite of this node's expiring.
			return
		}
		s.publish(Event{Kind: EventInviteCleared, Invite: inv})
	}
}

// remember adds an inbound invite to the pending set. A replacement invite
// carrying an id already held simply overwrites it, keeping its position.
func (s *Service) remember(inv Invite, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.pending[inv.InviteID]; !exists {
		s.arrived = append(s.arrived, inv.InviteID)
	}
	s.pending[inv.InviteID] = pendingInvite{invite: inv, raw: raw}
}

// forget drops an invite from the pending set, reporting whether it was there.
func (s *Service) forget(inviteID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[inviteID]; !ok {
		return false
	}
	delete(s.pending, inviteID)
	for i, id := range s.arrived {
		if id == inviteID {
			s.arrived = append(s.arrived[:i], s.arrived[i+1:]...)
			break
		}
	}
	return true
}

// solePending resolves "the invite that is pending" for a caller that named
// none, and reports precisely why it could not when there is not exactly one.
func (s *Service) solePending() (string, error) {
	waiting := s.Pending()
	switch len(waiting) {
	case 0:
		return "", ErrNoPendingInvite
	case 1:
		return waiting[0].InviteID, nil
	default:
		sorted := append([]Invite(nil), waiting...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ReceivedAt < sorted[j].ReceivedAt })
		return "", &AmbiguousInviteError{Invites: sorted}
	}
}

// call issues one broker request and decodes the Invite it answers with,
// keeping the manager's own JSON alongside the decode.
func (s *Service) call(ctx context.Context, method string, params any) (Result, error) {
	msg, err := s.broker.Call(ctx, method, params)
	if err != nil {
		return Result{}, err
	}
	if msg == nil {
		return Result{}, fmt.Errorf("%s returned no response", method)
	}
	var inv Invite
	if err := decode(msg.Result, &inv); err != nil {
		return Result{}, fmt.Errorf("decode %s: %w", method, err)
	}
	return Result{Raw: msg.Result, Invite: inv}, nil
}

// unanswerable reports whether a respond-to-invite error means the invite can
// never be answered: -32001 unknown invite id, -32002 invalid invite state.
func unanswerable(err error) bool {
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == -32001 || rpcErr.Code == -32002
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
