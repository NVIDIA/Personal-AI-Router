// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestDispatchInternalDiagnosticRank(t *testing.T) {
	id := strings.Repeat("a", 32)
	called := 0
	run := func(group, operation string) int {
		called++
		if group != "pair-smoke-test" || operation != id {
			t.Fatalf("changed rank selector: %q %q", group, operation)
		}
		return 7
	}

	handled, code := dispatchInternalDiagnosticRank([]string{"--diagnostic-rank", "pair-smoke-test", id}, run)
	if !handled || code != 7 || called != 1 {
		t.Fatalf("valid internal rank dispatch = handled %t code %d calls %d", handled, code, called)
	}

	for _, args := range [][]string{
		{"--diagnostic-rank"},
		{"--diagnostic-rank", "pair-smoke-test", id, "extra"},
		{"--diagnostic-rank", "not a token", id},
		{"--diagnostic-rank", "pair-smoke-test", strings.Repeat("a", 31)},
	} {
		handled, code = dispatchInternalDiagnosticRank(args, run)
		if !handled || code != 2 {
			t.Fatalf("malformed internal rank dispatch = handled %t code %d for %q", handled, code, args)
		}
	}
	if called != 1 {
		t.Fatalf("malformed selector reached rank path: %d calls", called)
	}

	for _, args := range [][]string{nil, {"--version"}, {"--diagnostic-rank-other", "pair-smoke-test", id}} {
		handled, code = dispatchInternalDiagnosticRank(args, run)
		if handled || code != 0 {
			t.Fatalf("ordinary CLI dispatch was intercepted: handled %t code %d for %q", handled, code, args)
		}
	}
}
