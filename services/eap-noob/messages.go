// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import "encoding/json"

// EAP-NOOB message types (RFC 9140, Section 5.2, Table 9).
const (
	typeError       = 0 // Error notification
	typeDiscovery   = 1 // PeerId and PeerState discovery
	typeNegotiation = 2 // Version, cryptosuite, parameter negotiation (Initial)
	typeKeyExchange = 3 // ECDHE keys and nonces (Initial)
	typeWaiting     = 4 // Waiting indication
	typeNoobID      = 5 // NoobId discovery (Completion)
	typeCompletion  = 6 // Authentication and key confirmation (Completion)
	// Types 7-9 (Reconnect) are reserved for a future phase.
)

// defaultNAI is the generic identity used by a peer that has no server-assigned
// NAI (RFC 9140, Section 3.2.1).
const defaultNAI = "noob@eap-noob.arpa"

// eapResult is a non-protocol control message used by this transport-agnostic
// library to convey the terminating EAP-Success / EAP-Failure indication that a
// real EAP lower layer would deliver out of band of the EAP-NOOB payloads.
type eapResult struct {
	EAP string `json:"eap"` // "success" or "failure"
}

const (
	eapSuccess = "success"
	eapFailure = "failure"
)

// wireMessage models any EAP-NOOB request/response. Fields that participate in
// the Hoob/MAC input arrays are kept as json.RawMessage so their exact
// serialized bytes can be copied verbatim (RFC 9140, Section 3.3.2).
type wireMessage struct {
	Type      *int `json:"Type,omitempty"`
	PeerState *int `json:"PeerState,omitempty"`

	PeerId json.RawMessage `json:"PeerId,omitempty"`
	NAI    json.RawMessage `json:"NAI,omitempty"`
	NewNAI json.RawMessage `json:"NewNAI,omitempty"`

	Vers         json.RawMessage `json:"Vers,omitempty"`
	Verp         json.RawMessage `json:"Verp,omitempty"`
	Cryptosuites json.RawMessage `json:"Cryptosuites,omitempty"`
	Cryptosuitep json.RawMessage `json:"Cryptosuitep,omitempty"`
	Dirs         json.RawMessage `json:"Dirs,omitempty"`
	Dirp         json.RawMessage `json:"Dirp,omitempty"`
	ServerInfo   json.RawMessage `json:"ServerInfo,omitempty"`
	PeerInfo     json.RawMessage `json:"PeerInfo,omitempty"`

	PKs json.RawMessage `json:"PKs,omitempty"`
	PKp json.RawMessage `json:"PKp,omitempty"`
	Ns  json.RawMessage `json:"Ns,omitempty"`
	Np  json.RawMessage `json:"Np,omitempty"`

	SleepTime *int `json:"SleepTime,omitempty"`

	NoobId json.RawMessage `json:"NoobId,omitempty"`
	MACs   json.RawMessage `json:"MACs,omitempty"`
	MACp   json.RawMessage `json:"MACp,omitempty"`

	ErrorCode *int   `json:"ErrorCode,omitempty"`
	ErrorInfo string `json:"ErrorInfo,omitempty"`

	EAP string `json:"eap,omitempty"`
}

// encode marshals a message to compact JSON and parses it back so callers can
// capture the exact serialized bytes of each field (the bytes that travel on
// the wire and feed the MAC/Hoob arrays).
func encode(m *wireMessage) ([]byte, *wireMessage, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	parsed := new(wireMessage)
	if err := json.Unmarshal(b, parsed); err != nil {
		return nil, nil, err
	}
	return b, parsed, nil
}

func decode(b []byte) (*wireMessage, error) {
	m := new(wireMessage)
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

// rawString returns the JSON encoding of a string value (including quotes), for
// use as a verbatim MAC/Hoob array element.
func rawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// rawInt returns the JSON encoding of an integer value.
func rawInt(i int) json.RawMessage {
	b, _ := json.Marshal(i)
	return b
}

// rawIntSlice returns the JSON encoding of an integer array.
func rawIntSlice(v []int) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
