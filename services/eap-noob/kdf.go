// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"crypto/sha256"
	"encoding/binary"
)

// algorithmID is the fixed 8-byte AlgorithmId used in FixedInfo (RFC 9140,
// Section 3.5, Table 4).
var algorithmID = []byte("EAP-NOOB")

// keyMaterial holds the labeled slices of the KDF output (RFC 9140, Table 5).
type keyMaterial struct {
	MSK      []byte // 64
	EMSK     []byte // 64
	AMSK     []byte // 64
	MethodID []byte // 32
	Kms      []byte // 32
	Kmp      []byte // 32
	Kz       []byte // 32 (Completion / KeyingMode 3 only)
}

// oneStepKDF implements the NIST SP 800-56C one-step KDF with a 4-byte
// big-endian counter and SHA-256 as the auxiliary function H (RFC 9140,
// Section 3.5).
func oneStepKDF(z, fixedInfo []byte, outLen int) []byte {
	out := make([]byte, 0, outLen+sha256.Size)
	var counter uint32 = 1
	for len(out) < outLen {
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], counter)
		h := sha256.New()
		h.Write(c[:])
		h.Write(z)
		h.Write(fixedInfo)
		out = h.Sum(out)
		counter++
	}
	return out[:outLen]
}

// completionFixedInfo builds FixedInfo for the Completion Exchange
// (KeyingMode=0): AlgorithmId || PartyUInfo(Np) || PartyVInfo(Ns) ||
// SuppPrivInfo(Noob with a one-byte Datalen prefix). PartyUInfo is the peer
// nonce and PartyVInfo is the server nonce (RFC 9140, Table 4).
func completionFixedInfo(np, ns, noob []byte) []byte {
	fi := make([]byte, 0, len(algorithmID)+len(np)+len(ns)+1+len(noob))
	fi = append(fi, algorithmID...)
	fi = append(fi, np...)
	fi = append(fi, ns...)
	fi = append(fi, byte(len(noob))) // one-byte Datalen counter for SuppPrivInfo
	fi = append(fi, noob...)
	return fi
}

// deriveCompletion runs the Completion Exchange key derivation and slices the
// 320-byte output per RFC 9140, Table 5.
func deriveCompletion(z, np, ns, noob []byte) keyMaterial {
	out := oneStepKDF(z, completionFixedInfo(np, ns, noob), 320)
	return keyMaterial{
		MSK:      out[0:64],
		EMSK:     out[64:128],
		AMSK:     out[128:192],
		MethodID: out[192:224],
		Kms:      out[224:256],
		Kmp:      out[256:288],
		Kz:       out[288:320],
	}
}
