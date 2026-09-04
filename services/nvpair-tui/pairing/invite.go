// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pairing is the single implementation of node pairing inside
// nvpair-tui. Both drivers — the interactive Cluster tab and the control
// socket that backs the non-interactive subcommands — go through one
// Service, so the two can never disagree about what an invite is or which
// one is pending.
//
// Everything here is expressed in the vocabulary nvpair-cluster-manager
// already uses (see ../../nvpair-cluster-manager/README.md); this package
// adds no pairing semantics of its own. It only relays through the broker,
// remembers which inbound invites are still unanswered, and tells its
// subscribers when that changed.
package pairing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Invite mirrors nvpair-cluster-manager's Invite: one pairing session, from
// the invite-node result, an invite-status poll, or an invite-* push.
//
// Pin is populated only in the inviting node's own invite-node result. It is
// a secret carried by a human out of band: it may be printed to the terminal
// the operator asked for and returned over the control socket, and it must
// never reach a log. Nothing in this package logs an Invite.
type Invite struct {
	InviteID            string  `json:"inviteId"`
	FromNodeID          string  `json:"fromNodeId,omitempty"`
	FromNodeUUID        string  `json:"fromNodeUuid,omitempty"`
	FromNodeName        string  `json:"fromNodeName,omitempty"`
	ToNodeID            *string `json:"toNodeId,omitempty"`
	ClusterID           string  `json:"clusterId,omitempty"`
	ClusterFriendlyName string  `json:"clusterFriendlyName,omitempty"`
	Pin                 *string `json:"pin,omitempty"`
	State               string  `json:"state"`
	Reason              string  `json:"reason,omitempty"`
	CreatedAt           int64   `json:"createdAt,omitempty"`
	RespondedAt         *int64  `json:"respondedAt,omitempty"`

	// ReceivedAt is this node's own wall clock (epoch ms) at the moment the
	// invite-received push arrived. CreatedAt is the *inviter's* clock, which
	// is why an age computed from it can come out negative between two
	// unsynchronised machines. Set only on inbound invites.
	ReceivedAt int64 `json:"receivedAt,omitempty"`
}

// Invite states, as nvpair-cluster-manager reports them.
const (
	StatePending  = "pending"
	StatePaired   = "paired"
	StateDeclined = "declined"
	StateCanceled = "canceled"
	StateExpired  = "expired"
	StateFailed   = "failed"
	StateRejected = "rejected"
)

// ReasonIncorrectPin is the failure reason for a well-formed but wrong PIN.
// A malformed PIN is a JSON-RPC error instead, not a failed result.
const ReasonIncorrectPin = "incorrect-pin"

// Terminal reports whether the invite has reached a state it will not leave,
// so a caller waiting on it can stop.
func (i Invite) Terminal() bool {
	switch i.State {
	case "", StatePending:
		return false
	default:
		return true
	}
}

// PIN returns the six-digit pairing PIN, or "" when this invite carries none
// (every invite but the inviter's own invite-node result).
func (i Invite) PIN() string {
	if i.Pin == nil {
		return ""
	}
	return *i.Pin
}

// Describe names the invite for a human without disclosing the PIN. It is
// safe to put in an error message that may be logged.
func (i Invite) Describe() string {
	if i.FromNodeName == "" {
		return i.InviteID
	}
	return fmt.Sprintf("%s (from %s)", i.InviteID, i.FromNodeName)
}

// Result is one broker Invite response: the manager's own JSON, relayed
// untouched so the control socket hands callers exactly what the manager
// said, plus the decode this package and the CLI act on.
type Result struct {
	Raw    json.RawMessage
	Invite Invite
}

// ErrNoPendingInvite is returned when a response was asked for without an
// invite id and no inbound invite is waiting.
var ErrNoPendingInvite = errors.New("no invite is pending on this node")

// AmbiguousInviteError is returned when a response was asked for without an
// invite id and more than one inbound invite is waiting. It names them so the
// operator can pick one with --invite.
type AmbiguousInviteError struct{ Invites []Invite }

func (e *AmbiguousInviteError) Error() string {
	names := make([]string, 0, len(e.Invites))
	for _, inv := range e.Invites {
		names = append(names, inv.Describe())
	}
	return fmt.Sprintf("%d invites are pending; name one with --invite: %s",
		len(e.Invites), strings.Join(names, ", "))
}

// InviteRequest is an outbound pairing target. Address is a bare host; Port
// overrides the manager's default pairing port; NodeID pins the target's
// identity when the caller already knows it (the Nodes tab does, an operator
// typing an address does not).
type InviteRequest struct {
	Address string
	Port    int
	NodeID  string
}

// params renders the request as cluster:invite-node params.
//
// The manager takes "address" as a bare host and appends the port itself
// (default 14321), so a "host:port" the operator typed has to be split: glued
// together it would dial [host:port]:14321. An explicit Port always wins over
// one embedded in Address.
func (r InviteRequest) params() (map[string]any, error) {
	address := strings.TrimSpace(r.Address)
	if address == "" {
		return nil, errors.New("an address is required to invite a node")
	}
	port := r.Port
	if port == 0 {
		if host, portStr, err := net.SplitHostPort(address); err == nil {
			if p, perr := strconv.Atoi(portStr); perr == nil {
				address, port = host, p
			}
		}
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	params := map[string]any{"address": address}
	if port > 0 {
		params["port"] = port
	}
	if r.NodeID != "" {
		params["nodeId"] = r.NodeID
	}
	return params, nil
}

// ClusterNode mirrors nvpair-cluster-manager's ClusterNode: a cluster member
// or a node with a pairing still in flight.
type ClusterNode struct {
	ID        string `json:"id"`
	NodeUUID  string `json:"nodeUuid"`
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress"`
	Port      int    `json:"port"`
	ClusterID string `json:"clusterId,omitempty"`
	State     string `json:"state"`
	JoinedAt  *int64 `json:"joinedAt,omitempty"`
	LastSeen  *int64 `json:"lastSeen,omitempty"`
}

// Membership is this node's own cluster identity plus its roster, the answer
// to pair:members.
type Membership struct {
	ClusterID           string        `json:"clusterId"`
	ClusterFriendlyName string        `json:"clusterFriendlyName,omitempty"`
	NodeID              string        `json:"nodeId"`
	NodeUUID            string        `json:"nodeUuid"`
	Name                string        `json:"name"`
	Members             []ClusterNode `json:"members"`
}

// stripPin returns raw with any "pin" member removed and receivedAt stamped
// in, preserving every other field the manager sent (including ones this
// package does not model). A blob that will not parse as an object yields
// nil, and the caller falls back to re-encoding the decoded Invite.
func stripPin(raw json.RawMessage, receivedAt int64) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	delete(fields, "pin")
	if receivedAt > 0 {
		fields["receivedAt"] = json.RawMessage(strconv.FormatInt(receivedAt, 10))
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return out
}
