// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
)

func TestWorkloadsShowReportedTargetWithoutInferringOrigin(t *testing.T) {
	v := newWorkloadsView(nil)
	v.SetSize(100, 20)
	w := workload{ID: "1", Engine: "llamacpp", OriginatedFrom: "origin-node", ScheduledOn: "serving-node", State: "running"}
	v.upsert(w)
	if !strings.Contains(v.View(), "Runs on: serving-node") {
		t.Fatal("reported serving node missing from selected workload")
	}
	w.ScheduledOn = ""
	v.upsert(w)
	if !strings.Contains(v.View(), "Runs on: unknown (not reported)") {
		t.Fatal("missing target must remain unknown")
	}
}

func TestWorkloadsKeepEngineRunIdentityAndTerminalTruth(t *testing.T) {
	v := newWorkloadsView(nil)
	v.SetSize(100, 20)
	for _, w := range []workload{
		{ID: "1", Engine: "ollama", RunID: "a", OriginatedFrom: "self", State: "running"},
		{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "self", State: "completed"},
		{ID: "1", Engine: "llamacpp", RunID: "b", OriginatedFrom: "self", State: "running"},
	} {
		v.upsert(w)
	}
	v.Update(workloadsInitialMsg{workloads: []workload{{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "self", State: "running"}}})
	if len(v.byKey) != 3 {
		t.Fatalf("identity collision: %v", v.byKey)
	}
	if v.byKey[workloadKey("self", "1", "llamacpp", "a")].State != "completed" {
		t.Fatal("late baseline regressed completed request")
	}
}
