// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

func intp(i int) *int { return &i }

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// newPeerId generates a fresh, unguessable 16-byte identifier base64url-encoded
// to a 22-character string (RFC 9140, Section 3.3.1).
func newPeerId() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64Encode(b), nil
}

// marshalInfo encodes the identity object included in the authenticated transcript.
func marshalInfo(info PairingInfo) json.RawMessage {
	// PairingInfo contains only strings and integers, so encoding cannot fail.
	b, _ := json.Marshal(info)
	return b
}

// ensureRaw returns the given raw value or an empty JSON object if it is absent.
func ensureRaw(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("{}")
	}
	return r
}

func resultBytes(kind string) []byte {
	b, _ := json.Marshal(eapResult{EAP: kind})
	return b
}

func decodeNoobID(r json.RawMessage) ([]byte, error) {
	s, err := rawToString(r)
	if err != nil {
		return nil, err
	}
	b, err := b64Decode(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 16 {
		return nil, fmt.Errorf("eapnoob: NoobId must be 16 bytes")
	}
	return b, nil
}

func decodeMAC(r json.RawMessage) ([]byte, error) {
	s, err := rawToString(r)
	if err != nil {
		return nil, err
	}
	b, err := b64Decode(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("eapnoob: MAC must be 32 bytes")
	}
	return b, nil
}

func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }

// oobOutput builds a server-to-peer OOB message, generating a fresh
// secret nonce Noob and computing the fingerprint Hoob over the negotiated
// Initial Exchange parameters (RFC 9140, Section 3.3.2).
func oobOutput(ms *methodState) (OOBMessage, error) {
	noob := make([]byte, 16)
	if _, err := rand.Read(noob); err != nil {
		return OOBMessage{}, err
	}
	ms.setNoob(noob)
	hoob := ms.suite.computeHoob(ms.in)
	return OOBMessage{
		PeerId: ms.peerId,
		Noob:   b64Encode(noob),
		Hoob:   b64Encode(hoob),
	}, nil
}

// setOOBNoob records a caller-supplied 16-byte Noob without an OOBMessage
// envelope. Unlike oobInput it does not verify a received Hoob (there is none
// to verify for a human-carried Noob); transcript agreement is instead proven
// by the Completion Exchange MACs.
func setOOBNoob(ms *methodState, noob []byte) error {
	if len(noob) != 16 {
		return fmt.Errorf("eapnoob: Noob must be 16 bytes")
	}
	ms.setNoob(noob)
	return nil
}

// oobInput verifies a received OOB message: the PeerId must match and the
// locally recomputed Hoob must equal the received value (RFC 9140,
// Section 3.2.3).
func oobInput(ms *methodState, msg OOBMessage) error {
	if ms.peerId != "" && msg.PeerId != ms.peerId {
		return fmt.Errorf("eapnoob: OOB PeerId mismatch")
	}
	noob, err := b64Decode(msg.Noob)
	if err != nil || len(noob) != 16 {
		return fmt.Errorf("eapnoob: invalid OOB Noob")
	}
	ms.setNoob(noob)
	hoob := ms.suite.computeHoob(ms.in)
	if b64Encode(hoob) != msg.Hoob {
		return fmt.Errorf("eapnoob: OOB Hoob verification failed")
	}
	return nil
}
