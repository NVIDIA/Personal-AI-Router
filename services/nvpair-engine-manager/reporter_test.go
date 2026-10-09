// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestReporterDedupCapClear(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(NewCodec(&buf))

	// Same id reported twice → one entry, latest wins.
	r.report(serviceError{ID: "a", Message: "first"})
	r.report(serviceError{ID: "a", Message: "second"})
	snap := r.snapshot()
	require.Len(t, snap, 1, "expected 1 deduped error (latest wins)")
	require.Equal(t, "second", snap[0].Message, "expected 1 deduped error (latest wins) (%v)", snap)

	r.clear("a")
	require.Empty(t, r.snapshot(), "expected empty after clear")

	// Both wire frames should have been emitted on the codec.
	out := buf.String()
	require.Contains(t, out, `"errors:report"`, "expected report + clear frames emitted")
	require.Contains(t, out, `"errors:clear"`, "expected report + clear frames emitted")

	// Ring is bounded.
	r2 := NewReporter(nil)
	for i := 0; i < maxRecentErrors+25; i++ {
		r2.report(serviceError{ID: fmt.Sprintf("e%d", i)})
	}
	require.LessOrEqual(t, len(r2.snapshot()), maxRecentErrors, "ring exceeded cap")
}
