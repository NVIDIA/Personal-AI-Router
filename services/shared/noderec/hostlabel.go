// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// hostLabelPrefix marks the host label as PAIR's own, so it can never be
// mistaken for a name the operating system owns.
const hostLabelPrefix = "nvpair-"

// hostLabelHexDigits is the width of the identity part of the label: twelve hex
// digits, the leading 48 bits of the node UUID. The width IS the collision
// guard — about 16.7 million hosts before a collision becomes likely, against
// about 65 thousand for eight digits — and the finished label is still only 19
// bytes against the 63-byte DNS label limit, so the wider form costs nothing.
// Do not narrow it later for tidiness: every host already advertised would
// change name.
const hostLabelHexDigits = 12

// HostLabel returns the single DNS label PAIR claims as its own host record
// when it advertises a node over mDNS, e.g. "nvpair-f0e1d2c34b5a".
//
// It is never the machine's hostname. Claiming the operating system's name puts
// two answers, or a cache-flushed retraction, under a name the host's own
// mDNS responder already owns, and RFC 6762 §9 resolves that contention by
// appending a digit to the computer's Local hostname ("indigo-2") — the
// defect this naming rule exists to avoid. The label instead comes from the
// per-host node UUID, which is stable across restarts, unique per host, and
// already advertised in TXT as uuid=, so a peer can tie the name it resolves to
// the identity inside the record.
//
// The rule, in order:
//
//  1. nodeUUID carries at least hostLabelHexDigits hex digits — dashes and any
//     other decoration removed, lowercase — so the label is hostLabelPrefix
//     plus the first twelve. Exactly the FIRST twelve: two node UUIDs sharing
//     those twelve digits share a label, which is the deliberate 48-bit
//     collision budget described above.
//  2. nodeUUID is empty, or is not a usable identity (non-empty but fewer than
//     hostLabelHexDigits hex digits once sanitized, so not a UUID at all), and
//     hostname is non-empty: fall back to hostLabelPrefix plus the first twelve
//     hex digits of sha256(hostname). Hashing rather than using the hostname is
//     what keeps the label from ever being the machine's name, and the hostname
//     is already broadcast to the whole link as the mDNS instance name, so the
//     hash leaks nothing. Such a label moves whenever the hostname moves, so
//     the caller warns next to the name it is advertising: HostLabel itself is
//     pure — no logging, no environment reads — which also keeps this path
//     reachable from a test, where os.Hostname() is never empty.
//  3. No usable node UUID — empty, or too short to be one — and an empty
//     hostname: no label, and an error naming what is missing. There is no
//     entropy to derive a name from, and a fixed label such as
//     "nvpair-000000000000" would give every host that reached this path the
//     same name, recreating the contention this rule removes. Advertising
//     nothing is strictly better.
//
// Whatever the inputs, a returned label is one lowercase DNS label with no dots
// and no spaces, exactly len(hostLabelPrefix)+hostLabelHexDigits = 19 bytes, so
// a responder can advertise it without checking it again.
func HostLabel(nodeUUID, hostname string) (string, error) {
	if digits := hexDigits(nodeUUID); len(digits) >= hostLabelHexDigits {
		return hostLabelPrefix + digits[:hostLabelHexDigits], nil
	}
	if hostname == "" {
		return "", fmt.Errorf("noderec: no usable node UUID and no hostname to derive a host label from")
	}
	sum := sha256.Sum256([]byte(hostname))
	return hostLabelPrefix + hex.EncodeToString(sum[:])[:hostLabelHexDigits], nil
}

// hexDigits lowercases s and keeps only its hex digits, so a hyphenated UUID —
// and any other spelling or decoration of the same identity — reduces to the
// same digits, and nothing that is not hex can reach a label.
func hexDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			b.WriteRune(r)
		case r >= 'A' && r <= 'F':
			b.WriteRune(r - 'A' + 'a')
		}
	}
	return b.String()
}
