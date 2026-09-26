// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"strings"
	"testing"
)

func TestOnboardingHistoryViewIsReadOnlyAndTruthful(t *testing.T) {
	v := newOnboardingHistoryView(nil)
	v.SetSize(100, 20)
	v.Update(onboardingHistoryMsg{summary: onboardingHistorySummary{
		Total: 2, HistoryOnly: 1, Current: 1,
		Operations: []onboardingHistoryOperation{
			{OperationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "completed", Classification: "history-only", TargetCount: 2},
			{OperationID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", State: "completed", Classification: "current", TargetCount: 1},
		},
	}})
	view := v.View()
	for _, want := range []string{"2 records", "1 history-only", "1 current", "history never grants setup authority"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %s", want, view)
		}
	}
}

func TestOnboardingHistoryViewDoesNotPresentFailureAsEmptySuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  onboardingHistoryMsg
		want string
	}{
		{"rpc-error", onboardingHistoryMsg{err: errors.New("offline")}, "Setup history unavailable"},
		{"scan-failure", onboardingHistoryMsg{summary: onboardingHistorySummary{Total: 1, Invalid: 1, RecoveryRequired: true, DiscoveryBlocked: true}}, "inventory failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newOnboardingHistoryView(nil)
			v.SetSize(100, 20)
			v.Update(tc.msg)
			view := v.View()
			if !strings.Contains(view, tc.want) || strings.Contains(view, "No retained setup history") {
				t.Fatalf("view = %q", view)
			}
		})
	}
}

func TestOnboardingHistoryViewDoesNotPresentPendingAsEmptySuccess(t *testing.T) {
	v := newOnboardingHistoryView(nil)
	v.SetSize(100, 20)
	view := v.View()
	if !strings.Contains(view, "Loading setup history") || strings.Contains(view, "No retained setup history") {
		t.Fatalf("view = %q", view)
	}
}
