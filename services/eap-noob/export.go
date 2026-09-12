// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"hash"
)

// ErrNotRegistered is returned when a secret export is attempted before a
// successful Completion Exchange has established the association key Kz.
var ErrNotRegistered = errors.New("eapnoob: no registered association")

// EphemeralKey returns the raw ECDH shared secret (z) established by the
// Key Exchange of the in-flight method execution, before any PIN is involved.
// Both sides hold the identical value once the Initial Exchange completes;
// a passive observer of the plaintext transport cannot compute it. Callers
// use it to authenticate transport-level session signals (cancel/decline/
// expire) that arrive outside the EAP-NOOB message stream. It returns an
// error when no Key Exchange has run yet.
//
// Promoted to Server.EphemeralKey and Peer.EphemeralKey via embedding.
func (ms *methodState) EphemeralKey() ([]byte, error) {
	if len(ms.z) == 0 {
		return nil, errors.New("eapnoob: no key exchange performed")
	}
	return append([]byte(nil), ms.z...), nil
}

// Export derives a shared secret of arbitrary length from the established
// association. Both the peer and the server, having reached the Registered
// state with the same Kz, derive identical bytes for the same label and
// context, which lets consumers exchange an arbitrary-length secret.
//
// The derivation is HKDF (RFC 5869) with SHA-256, using Kz as input keying
// material and the label and context as the info parameter. label and context
// are domain-separated by a length prefix so distinct inputs cannot collide.
func (a *Association) Export(label string, context []byte, length int) ([]byte, error) {
	if a == nil || len(a.Kz) == 0 {
		return nil, ErrNotRegistered
	}
	if length < 0 {
		return nil, errors.New("eapnoob: negative export length")
	}
	info := make([]byte, 0, 2+len(label)+len(context))
	info = append(info, byte(len(label)>>8), byte(len(label)))
	info = append(info, label...)
	info = append(info, context...)
	return hkdf(sha256.New, a.Kz, nil, info, length)
}

// hkdf implements HKDF-Extract followed by HKDF-Expand (RFC 5869).
func hkdf(newHash func() hash.Hash, ikm, salt, info []byte, length int) ([]byte, error) {
	h := newHash()
	hashLen := h.Size()
	if length > 255*hashLen {
		return nil, errors.New("eapnoob: requested export length too large")
	}
	if salt == nil {
		salt = make([]byte, hashLen)
	}

	extract := hmac.New(newHash, salt)
	extract.Write(ikm)
	prk := extract.Sum(nil)

	out := make([]byte, 0, length)
	var prev []byte
	for i := 1; len(out) < length; i++ {
		exp := hmac.New(newHash, prk)
		exp.Write(prev)
		exp.Write(info)
		exp.Write([]byte{byte(i)})
		prev = exp.Sum(nil)
		out = append(out, prev...)
	}
	return out[:length], nil
}
