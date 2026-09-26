// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-shared/enginelogs"
)

func encodedLogSnapshot(t *testing.T, buffer *logBuffer) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"lines": buffer.snapshot()})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestLogBufferKeepsNewestSnapshotWithinTextBudget(t *testing.T) {
	buffer := newLogBuffer()
	textBytes := enginelogs.MaxLineBytes - 1024
	for _, marker := range []string{"a", "b", "c", "d"} {
		buffer.append("stdout", marker+strings.Repeat("&", textBytes-1))
	}
	if buffer.textBytes != 4*textBytes || buffer.textBytes > enginelogs.MaxSnapshotTextBytes {
		t.Fatalf("tracked text bytes = %d", buffer.textBytes)
	}
	encoded := encodedLogSnapshot(t, buffer)
	if len(encoded) <= 1<<20 || len(encoded) >= enginelogs.MaxBrokerFrameBytes {
		t.Fatalf("escaped snapshot bytes = %d", len(encoded))
	}

	buffer.append("stdout", "e"+strings.Repeat("&", textBytes-1))
	lines := buffer.snapshot()
	if len(lines) != 4 || lines[0].Text[0] != 'b' || lines[3].Text[0] != 'e' {
		t.Fatalf("retained lines are not the newest four: count=%d", len(lines))
	}
	if buffer.textBytes != 4*textBytes || buffer.textBytes > enginelogs.MaxSnapshotTextBytes {
		t.Fatalf("retained text bytes = %d", buffer.textBytes)
	}
}

func TestLogBufferSkipsLineAboveScannerContract(t *testing.T) {
	buffer := newLogBuffer()
	buffer.append("stdout", strings.Repeat("x", enginelogs.MaxLineBytes+1))
	if lines := buffer.snapshot(); len(lines) != 0 {
		t.Fatalf("oversized line was retained: %d line(s)", len(lines))
	}
}
