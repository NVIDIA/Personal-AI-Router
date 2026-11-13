// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
)

// ErrNotRegistered is returned when a secret export is attempted before a
// successful Completion Exchange has established the association key Kz.
var ErrNotRegistered = errors.New("eapnoob: no registered association")

// Export derives a shared application secret from the established association.
// Both roles derive identical bytes for the same label, context, and length.
// The length must be between 0 and 8160 bytes (255 SHA-256 blocks).
//
// The derivation is HKDF (RFC 5869) with SHA-256, using Kz as input keying
// material, no salt, and a two-byte big-endian label length followed by the
// label and context as info. Labels may contain at most 65535 bytes.
func (a *Association) Export(label string, context []byte, length int) ([]byte, error) {
	if a == nil || len(a.Kz) == 0 {
		return nil, ErrNotRegistered
	}
	if length < 0 {
		return nil, errors.New("eapnoob: negative export length")
	}
	if length > 255*sha256.Size {
		return nil, errors.New("eapnoob: requested export length too large")
	}
	if len(label) > 65535 {
		return nil, errors.New("eapnoob: export label too long")
	}
	if length == 0 {
		return []byte{}, nil
	}
	info := make([]byte, 0, 2+len(label)+len(context))
	info = append(info, byte(len(label)>>8), byte(len(label)))
	info = append(info, label...)
	info = append(info, context...)
	return hkdf.Key(sha256.New, a.Kz, nil, string(info), length)
}
