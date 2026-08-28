// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package eapnoob implements the EAP-NOOB authentication and key-derivation
// method defined in RFC 9140 ("Nimble Out-of-Band Authentication for EAP").
//
// The implementation is transport-agnostic: it consumes and produces the
// EAP-NOOB message bytes (JSON objects) but does not perform EAP/RADIUS framing
// or networking. A caller drives a Server and a Peer by relaying each side's
// outbound bytes to the other, and relays the single out-of-band message
// (PeerId, Noob, Hoob) through whatever user-assisted channel it chooses.
//
// Once the Completion Exchange succeeds, both sides reach the Registered state
// and share the association key Kz, from which Export derives a shared secret
// of any requested length.
package eapnoob

import (
	"crypto/ecdh"
	"encoding/json"
	"fmt"
)

// State is the EAP-NOOB server-peer association state (RFC 9140, Figure 1).
type State int

const (
	StateUnregistered State = 0
	StateWaiting      State = 1 // Waiting for OOB
	StateOOBReceived  State = 2
	StateReconnecting State = 3
	StateRegistered   State = 4
)

func (s State) String() string {
	switch s {
	case StateUnregistered:
		return "Unregistered"
	case StateWaiting:
		return "WaitingForOOB"
	case StateOOBReceived:
		return "OOBReceived"
	case StateReconnecting:
		return "Reconnecting"
	case StateRegistered:
		return "Registered"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Outcome is the result of processing one inbound message.
type Outcome struct {
	// Send is the message to transmit to the other party, or nil if there is
	// nothing to send.
	Send []byte
	// State is the local association state after processing.
	State State
	// Done reports that this method execution (EAP conversation) has ended.
	Done bool
	// Success reports that the Completion Exchange succeeded and the
	// association is now Registered.
	Success bool
	// Err is set when an EAP-NOOB error notification was produced or received.
	Err *ProtocolError
}

// OOBMessage is the out-of-band payload delivered through the user-assisted
// channel (RFC 9140, Section 3.2.3). The string fields are base64url-encoded
// without padding.
type OOBMessage struct {
	PeerId string `json:"PeerId"`
	Noob   string `json:"Noob"`
	Hoob   string `json:"Hoob"`
}

// methodState holds the ephemeral per-execution data shared by both roles.
type methodState struct {
	suite *cryptosuite

	peerId string
	nai    string

	in macInputs // verbatim fields for Hoob/MAC arrays

	priv *ecdh.PrivateKey
	z    []byte
	ns   []byte // raw server nonce
	np   []byte // raw peer nonce

	noob    []byte // raw 16-byte secret nonce
	noobB64 json.RawMessage
	noobID  []byte // raw 16-byte NoobId

	keyM       keyMaterial
	registered *Association
}

func (ms *methodState) deriveCompletionKeys() {
	ms.keyM = deriveCompletion(ms.z, ms.np, ms.ns, ms.noob)
}

// setNoob records a secret nonce (raw bytes) and its derived encodings on the
// side that holds it.
func (ms *methodState) setNoob(noob []byte) {
	ms.noob = noob
	b64 := b64Encode(noob)
	ms.noobB64 = rawString(b64)
	ms.in.Noob = ms.noobB64
	ms.noobID = ms.suite.computeNoobID(ms.noobB64)
}

// raw-value helpers -----------------------------------------------------------

func rawToInt(r json.RawMessage) (int, error) {
	var v int
	if err := json.Unmarshal(r, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func rawToIntSlice(r json.RawMessage) ([]int, error) {
	var v []int
	if err := json.Unmarshal(r, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func rawToString(r json.RawMessage) (string, error) {
	var v string
	if err := json.Unmarshal(r, &v); err != nil {
		return "", err
	}
	return v, nil
}

func rawToJWK(r json.RawMessage) (JWK, error) {
	var v JWK
	if err := json.Unmarshal(r, &v); err != nil {
		return JWK{}, err
	}
	return v, nil
}
