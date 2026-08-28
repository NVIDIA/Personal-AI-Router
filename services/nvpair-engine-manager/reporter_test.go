// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestReporterDedupCapClear(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(NewCodec(&buf))

	// Same id reported twice → one entry, latest wins.
	r.report(serviceError{ID: "a", Message: "first"})
	r.report(serviceError{ID: "a", Message: "second"})
	if snap := r.snapshot(); len(snap) != 1 || snap[0].Message != "second" {
		t.Fatalf("expected 1 deduped error (latest wins), got %+v", snap)
	}

	r.clear("a")
	if len(r.snapshot()) != 0 {
		t.Fatalf("expected empty after clear, got %+v", r.snapshot())
	}

	// Both wire frames should have been emitted on the codec.
	if out := buf.String(); !strings.Contains(out, `"errors:report"`) || !strings.Contains(out, `"errors:clear"`) {
		t.Fatalf("expected report + clear frames emitted, got: %s", out)
	}

	// Ring is bounded.
	r2 := NewReporter(nil)
	for i := 0; i < maxRecentErrors+25; i++ {
		r2.report(serviceError{ID: fmt.Sprintf("e%d", i)})
	}
	if got := len(r2.snapshot()); got > maxRecentErrors {
		t.Fatalf("ring exceeded cap: %d > %d", got, maxRecentErrors)
	}
}
