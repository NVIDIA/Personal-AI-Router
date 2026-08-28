// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// JWK is the JSON Web Key encoding of an ECDHE public key, as exchanged in the
// PKs/PKp message fields (RFC 9140, Section 3.3.2). Only the members required by
// cryptosuites 1 and 2 are modeled.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y,omitempty"`
}

// cryptosuite implements one EAP-NOOB cryptosuite (RFC 9140, Section 5.1).
// Both registered suites use SHA-256, so only the curve and JWK encoding vary.
type cryptosuite struct {
	id    int
	curve ecdh.Curve
	kty   string
	crv   string
	ec    bool // true for NIST curves (JWK carries both x and y)
}

// suiteByID returns the cryptosuite for an IANA identifier. Suite 1
// (Curve25519/SHA-256) is mandatory; suite 2 (NIST P-256/SHA-256) is
// recommended (RFC 9140, Section 5.1).
func suiteByID(id int) (*cryptosuite, error) {
	switch id {
	case 1:
		return &cryptosuite{id: 1, curve: ecdh.X25519(), kty: "OKP", crv: "X25519", ec: false}, nil
	case 2:
		return &cryptosuite{id: 2, curve: ecdh.P256(), kty: "EC", crv: "P-256", ec: true}, nil
	default:
		return nil, fmt.Errorf("eapnoob: unsupported cryptosuite %d", id)
	}
}

// supportedCryptosuites lists the suites this library implements, in the
// recommended server priority order.
func supportedCryptosuites() []int { return []int{2, 1} }

func isSupportedCryptosuite(id int) bool {
	return id == 1 || id == 2
}

// generateKeypair creates an ephemeral ECDHE key pair and returns the private
// key together with the JWK encoding of the public component.
func (cs *cryptosuite) generateKeypair() (*ecdh.PrivateKey, JWK, error) {
	priv, err := cs.curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, JWK{}, err
	}
	jwk, err := cs.encodeJWK(priv.PublicKey())
	if err != nil {
		return nil, JWK{}, err
	}
	return priv, jwk, nil
}

func (cs *cryptosuite) encodeJWK(pub *ecdh.PublicKey) (JWK, error) {
	raw := pub.Bytes()
	if cs.ec {
		// Uncompressed point: 0x04 || X(32) || Y(32).
		if len(raw) != 65 || raw[0] != 0x04 {
			return JWK{}, fmt.Errorf("eapnoob: unexpected P-256 public key encoding")
		}
		return JWK{
			Kty: cs.kty,
			Crv: cs.crv,
			X:   b64Encode(raw[1:33]),
			Y:   b64Encode(raw[33:65]),
		}, nil
	}
	return JWK{Kty: cs.kty, Crv: cs.crv, X: b64Encode(raw)}, nil
}

func (cs *cryptosuite) decodeJWK(jwk JWK) (*ecdh.PublicKey, error) {
	if jwk.Kty != cs.kty || jwk.Crv != cs.crv {
		return nil, fmt.Errorf("eapnoob: JWK kty/crv mismatch for cryptosuite %d", cs.id)
	}
	x, err := b64Decode(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("eapnoob: invalid JWK x: %w", err)
	}
	if cs.ec {
		y, err := b64Decode(jwk.Y)
		if err != nil {
			return nil, fmt.Errorf("eapnoob: invalid JWK y: %w", err)
		}
		if len(x) != 32 || len(y) != 32 {
			return nil, fmt.Errorf("eapnoob: invalid P-256 coordinate length")
		}
		point := make([]byte, 0, 65)
		point = append(point, 0x04)
		point = append(point, x...)
		point = append(point, y...)
		return cs.curve.NewPublicKey(point)
	}
	return cs.curve.NewPublicKey(x)
}

// computeZ derives the ECDHE shared secret Z (RFC 9140, Section 3.5).
func (cs *cryptosuite) computeZ(priv *ecdh.PrivateKey, peer JWK) ([]byte, error) {
	pub, err := cs.decodeJWK(peer)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

// hash returns H over the concatenation of the inputs (SHA-256 for all
// registered cryptosuites).
func (cs *cryptosuite) hash(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	sum := h.Sum(nil)
	return sum
}

// hmac computes HMAC-H over data with the given key.
func (cs *cryptosuite) hmac(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func b64Encode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func b64Decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// jwkRaw marshals a JWK to compact JSON for inclusion in messages and MAC inputs.
func jwkRaw(jwk JWK) (json.RawMessage, error) {
	return json.Marshal(jwk)
}
