// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"encoding/json"
)

// macInputs holds the verbatim JSON-encoded fields, copied byte-for-byte from
// the exchanged messages, that make up the Hoob and MAC input arrays (RFC 9140,
// Section 3.3.2). Nonce fields (Ns, Np, Noob) are the base64url JSON strings as
// they appear on the wire.
type macInputs struct {
	Vers         json.RawMessage
	Verp         json.RawMessage
	PeerId       json.RawMessage
	Cryptosuites json.RawMessage
	Dirs         json.RawMessage
	ServerInfo   json.RawMessage
	Cryptosuitep json.RawMessage
	Dirp         json.RawMessage
	NAI          json.RawMessage
	PeerInfo     json.RawMessage
	PKs          json.RawMessage
	Ns           json.RawMessage
	PKp          json.RawMessage
	Np           json.RawMessage
	Noob         json.RawMessage
}

// jsonArray concatenates pre-encoded JSON values into a JSON array without
// adding any whitespace, preserving each element verbatim.
func jsonArray(elems ...json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			buf.WriteByte(',')
		}
		if len(e) == 0 {
			buf.WriteString(`""`)
		} else {
			buf.Write(e)
		}
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// noobArray builds the 17-element array shared by the Hoob fingerprint and the
// Completion MACs/MACp, parameterized by the leading element (RFC 9140,
// Section 3.3.2). The KeyingMode element is the literal 0 for the Completion
// Exchange.
func noobArray(lead json.RawMessage, in macInputs) []byte {
	return jsonArray(
		lead,
		in.Vers,
		in.Verp,
		in.PeerId,
		in.Cryptosuites,
		in.Dirs,
		in.ServerInfo,
		in.Cryptosuitep,
		in.Dirp,
		in.NAI,
		in.PeerInfo,
		rawInt(0),
		in.PKs,
		in.Ns,
		in.PKp,
		in.Np,
		in.Noob,
	)
}

// computeHoob returns the 16-byte fingerprint Hoob for the given OOB direction
// (RFC 9140, Section 3.3.2): the cryptosuite hash of the input array truncated
// to its 16 leftmost bytes.
func (cs *cryptosuite) computeHoob(dir int, in macInputs) []byte {
	arr := noobArray(rawInt(dir), in)
	return cs.hash(arr)[:16]
}

// computeNoobID returns the 16-byte NoobId = H("NoobId", Noob), truncated to 16
// bytes, where Noob is its base64url JSON string form (RFC 9140, Section 3.3.2).
func (cs *cryptosuite) computeNoobID(noobB64 json.RawMessage) []byte {
	arr := jsonArray(rawString("NoobId"), noobB64)
	return cs.hash(arr)[:16]
}

// computeMAC returns a Completion-Exchange MAC truncated to 32 bytes. The
// leading array element is 2 for MACs (server) and 1 for MACp (peer)
// (RFC 9140, Section 3.3.2).
func (cs *cryptosuite) computeMAC(key []byte, lead int, in macInputs) []byte {
	arr := noobArray(rawInt(lead), in)
	return cs.hmac(key, arr)[:32]
}
