// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// vllmGroupTestRun mirrors the retained generation-13 journal: three ranks
// attempted, none cleaned, held as cleanup-required.
func vllmGroupTestRun() vllmGroupRun {
	digest := strings.Repeat("a", 64)
	run := vllmGroupRun{
		RunID:      "facfa61ad839f1108ea0972f3f389b3c",
		Generation: 13,
		PlanDigest: digest,
		Plan: vllmGroupPlan{
			Coordinator: "node-a", Model: "example/model", Runtime: "0.28.0",
			Topology: vllmGroupTopology{TensorParallel: 3, PipelineParallel: 1, DataParallel: 1, ConfigSHA256: digest},
		},
		State:   "cleanup-required",
		Failure: "a participant action failed; all owned ranks require cleanup",
	}
	for i, node := range []string{"node-a", "node-b", "node-c"} {
		run.Plan.Members = append(run.Plan.Members, vllmGroupMember{
			NodeID: node, PinSHA256: strings.Repeat(string(rune('1'+i)), 64), GPUUUID: "GPU-" + node,
			ModelDigest: digest, RuntimeDigest: digest, RuntimeCompatibilitySHA256: digest,
		})
		run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: node, Attempted: true})
	}
	return run
}

func writeVLLMGroupJournal(t *testing.T, installDir string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(installDir, vllmGroupJournalFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func marshalVLLMGroupRun(t *testing.T, run vllmGroupRun) []byte {
	t.Helper()
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVLLMGroupStatusReadsGenerationThirteenAsHeldCleanupRequired(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "vllm")
	// Writer-private fields ride along in the real journal and must not break the read.
	var raw map[string]any
	if err := json.Unmarshal(marshalVLLMGroupRun(t, vllmGroupTestRun()), &raw); err != nil {
		t.Fatal(err)
	}
	raw["plan"].(map[string]any)["limits"] = map[string]any{"runtimeSeconds": 600}
	raw["plan"].(map[string]any)["members"].([]any)[0].(map[string]any)["placement"] = map[string]any{"address": "192.168.0.43"}
	before, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := writeVLLMGroupJournal(t, installDir, before)

	got, err := readVLLMGroupStatus(installDir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reserved || got.ActivationEnabled || got.Run == nil || !strings.Contains(got.Reason, "fresh native admission required") {
		t.Fatalf("held status = %+v", got)
	}
	run := got.Run
	if run.RunID != "facfa61ad839f1108ea0972f3f389b3c" || run.Generation != 13 || run.State != "cleanup-required" || run.CleanupConfirmed || len(run.Plan.Members) != 3 || len(run.Ranks) != 3 || run.Failure == "" {
		t.Fatalf("retained run changed across the read: %+v", run)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read path rewrote the retained journal: %v", err)
	}
	entries, err := os.ReadDir(installDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read path created files beside the journal: %v %v", entries, err)
	}
}

func TestVLLMGroupStatusMissingJournalIsInactive(t *testing.T) {
	for name, installDir := range map[string]string{"empty owner": t.TempDir(), "absent owner": filepath.Join(t.TempDir(), "vllm")} {
		got, err := readVLLMGroupStatus(installDir)
		if err != nil || got.Reserved || got.ActivationEnabled || got.Run != nil || got.Reason != "" {
			t.Fatalf("%s: inactive status = %+v err=%v", name, got, err)
		}
	}
}

func TestVLLMGroupStatusReleasesCleanTerminalRun(t *testing.T) {
	run := vllmGroupTestRun()
	run.State, run.CleanupConfirmed = "stopped", true
	for i := range run.Ranks {
		run.Ranks[i].Started, run.Ranks[i].CleanupConfirmed = true, true
	}
	// A never-attempted rank needs no cleanup evidence.
	run.Ranks[2] = vllmGroupRank{NodeID: "node-c"}
	installDir := t.TempDir()
	writeVLLMGroupJournal(t, installDir, marshalVLLMGroupRun(t, run))
	got, err := readVLLMGroupStatus(installDir)
	if err != nil || got.Reserved || got.ActivationEnabled || got.Reason != "" || got.Run == nil || !got.Run.CleanupConfirmed {
		t.Fatalf("clean terminal status = %+v err=%v", got, err)
	}
}

func TestVLLMGroupStatusFailsClosed(t *testing.T) {
	digest := strings.Repeat("a", 64)
	mutations := map[string]func(*vllmGroupRun){
		"unknown state":                func(r *vllmGroupRun) { r.State = "running" },
		"zero generation":              func(r *vllmGroupRun) { r.Generation = 0 },
		"short run id":                 func(r *vllmGroupRun) { r.RunID = r.RunID[:31] },
		"uppercase run id":             func(r *vllmGroupRun) { r.RunID = strings.ToUpper(r.RunID) },
		"short plan digest":            func(r *vllmGroupRun) { r.PlanDigest = digest[:63] },
		"uppercase plan digest":        func(r *vllmGroupRun) { r.PlanDigest = strings.ToUpper(digest) },
		"coordinator not first member": func(r *vllmGroupRun) { r.Plan.Coordinator = "node-b" },
		"coordinator unknown":          func(r *vllmGroupRun) { r.Plan.Coordinator = "node-x" },
		"empty model":                  func(r *vllmGroupRun) { r.Plan.Model = "" },
		"empty runtime":                func(r *vllmGroupRun) { r.Plan.Runtime = "" },
		"zero tensor parallel":         func(r *vllmGroupRun) { r.Plan.Topology.TensorParallel = 0 },
		"bad topology digest":          func(r *vllmGroupRun) { r.Plan.Topology.ConfigSHA256 = "nope" },
		"no members":                   func(r *vllmGroupRun) { r.Plan.Members, r.Ranks = nil, nil },
		"empty member id":              func(r *vllmGroupRun) { r.Plan.Members[1].NodeID, r.Ranks[1].NodeID = "", "" },
		"duplicate member id":          func(r *vllmGroupRun) { r.Plan.Members[2].NodeID, r.Ranks[2].NodeID = "node-b", "node-b" },
		"duplicate pin":                func(r *vllmGroupRun) { r.Plan.Members[2].PinSHA256 = r.Plan.Members[1].PinSHA256 },
		"short pin":                    func(r *vllmGroupRun) { r.Plan.Members[0].PinSHA256 = digest[:63] },
		"empty gpu":                    func(r *vllmGroupRun) { r.Plan.Members[0].GPUUUID = "" },
		"bad model digest":             func(r *vllmGroupRun) { r.Plan.Members[0].ModelDigest = "x" },
		"bad runtime digest":           func(r *vllmGroupRun) { r.Plan.Members[0].RuntimeDigest = "x" },
		"bad compatibility digest":     func(r *vllmGroupRun) { r.Plan.Members[0].RuntimeCompatibilitySHA256 = "x" },
		"missing rank":                 func(r *vllmGroupRun) { r.Ranks = r.Ranks[:2] },
		"extra rank":                   func(r *vllmGroupRun) { r.Ranks = append(r.Ranks, vllmGroupRank{NodeID: "node-d"}) },
		"rank drift":                   func(r *vllmGroupRun) { r.Ranks[2].NodeID = "node-x" },
		"rank order drift":             func(r *vllmGroupRun) { r.Ranks[1], r.Ranks[2] = r.Ranks[2], r.Ranks[1] },
		"started without attempt":      func(r *vllmGroupRun) { r.Ranks[0] = vllmGroupRank{NodeID: "node-a", Started: true} },
		"cleanup without attempt":      func(r *vllmGroupRun) { r.Ranks[0] = vllmGroupRank{NodeID: "node-a", CleanupConfirmed: true} },
		"run clean while rank unclean": func(r *vllmGroupRun) { r.State, r.CleanupConfirmed = "stopped", true },
		"run clean in live state": func(r *vllmGroupRun) {
			r.State, r.CleanupConfirmed = "ready", true
			for i := range r.Ranks {
				r.Ranks[i].CleanupConfirmed = true
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			run := vllmGroupTestRun()
			mutate(&run)
			installDir := t.TempDir()
			writeVLLMGroupJournal(t, installDir, marshalVLLMGroupRun(t, run))
			if got, err := readVLLMGroupStatus(installDir); err == nil {
				t.Fatalf("invalid journal was admitted: %+v", got)
			}
		})
	}
	rawCases := map[string][]byte{
		"malformed json":      []byte(`{"runId":`),
		"array journal":       []byte(`[]`),
		"negative generation": []byte(strings.Replace(string(marshalVLLMGroupRun(t, vllmGroupTestRun())), `"generation":13`, `"generation":-13`, 1)),
		"string boolean":      []byte(strings.Replace(string(marshalVLLMGroupRun(t, vllmGroupTestRun())), `"cleanupConfirmed":false}`, `"cleanupConfirmed":"false"}`, 1)),
		"oversized":           append(marshalVLLMGroupRun(t, vllmGroupTestRun()), bytes.Repeat([]byte(" "), vllmGroupJournalMaxBytes)...),
	}
	for name, data := range rawCases {
		t.Run(name, func(t *testing.T) {
			installDir := t.TempDir()
			writeVLLMGroupJournal(t, installDir, data)
			if got, err := readVLLMGroupStatus(installDir); err == nil {
				t.Fatalf("unreadable journal was admitted: %+v", got)
			}
		})
	}
	t.Run("directory journal", func(t *testing.T) {
		installDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(installDir, vllmGroupJournalFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if got, err := readVLLMGroupStatus(installDir); err == nil {
			t.Fatalf("directory journal was admitted: %+v", got)
		}
	})
	t.Run("redirected journal", func(t *testing.T) {
		valid := writeVLLMGroupJournal(t, t.TempDir(), marshalVLLMGroupRun(t, vllmGroupTestRun()))
		installDir := t.TempDir()
		if err := os.Symlink(valid, filepath.Join(installDir, vllmGroupJournalFile)); err != nil {
			t.Skipf("symlinks unavailable on this %s host: %v", runtime.GOOS, err)
		}
		if got, err := readVLLMGroupStatus(installDir); err == nil {
			t.Fatalf("symlinked journal was admitted: %+v", got)
		}
	})
	t.Run("redirected owner", func(t *testing.T) {
		real := t.TempDir()
		writeVLLMGroupJournal(t, real, marshalVLLMGroupRun(t, vllmGroupTestRun()))
		installDir := filepath.Join(t.TempDir(), "vllm")
		if err := os.Symlink(real, installDir); err != nil {
			t.Skipf("symlinks unavailable on this %s host: %v", runtime.GOOS, err)
		}
		if got, err := readVLLMGroupStatus(installDir); err == nil {
			t.Fatalf("journal under a redirected owner was admitted: %+v", got)
		}
	})
	t.Run("empty owner path", func(t *testing.T) {
		if got, err := readVLLMGroupStatus(""); err == nil {
			t.Fatalf("empty owner path was admitted: %+v", got)
		}
	})
}

// vllmGroupStatusManager registers a host-platform vLLM manifest so the owned
// state resolves on any test host without probing, spawning or listening.
func vllmGroupStatusManager(t *testing.T, base string) (*Manager, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	m := &Manifest{Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1, Platforms: map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: {}}}
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, base)
	manager := NewManager(NewCodec(&out), ex, nil)
	// A non-root Linux host installs the system owner; these expectations
	// describe a controller without it.
	ex.groupPeer.native = nil
	return manager, &out
}

func TestManagerExposesReadOnlyVLLMGroupStatus(t *testing.T) {
	base := t.TempDir()
	installDir := filepath.Join(base, "vllm")
	before := marshalVLLMGroupRun(t, vllmGroupTestRun())
	path := writeVLLMGroupJournal(t, installDir, before)
	id := json.RawMessage("1")
	ctx := context.Background()

	m, out := vllmGroupStatusManager(t, base)
	m.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-status"})
	for _, want := range []string{`"activationEnabled":false`, `"reserved":true`, `"generation":13`, `"state":"cleanup-required"`, `"runId":"facfa61ad839f1108ea0972f3f389b3c"`, "fresh native admission required"} {
		mustContain(t, out.String(), want)
	}
	if strings.Contains(out.String(), `"error"`) {
		t.Fatalf("held status was reported as an error: %s", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status RPC rewrote the retained journal: %v", err)
	}

	m, out = vllmGroupStatusManager(t, base)
	m.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-reconcile", Params: json.RawMessage(`{"runId":"facfa61ad839f1108ea0972f3f389b3c","generation":13}`)})
	for i := 0; i < 100 && out.Len() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	mustContain(t, out.String(), "-32000")
	if after, err = os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("absent reconcile surface touched the retained journal: %v", err)
	}

	if err := os.WriteFile(path, []byte(`{"runId":`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, out = vllmGroupStatusManager(t, base)
	m.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-status"})
	mustContain(t, out.String(), "-32000")
	mustContain(t, out.String(), "preserve it for recovery")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m, out = vllmGroupStatusManager(t, base)
	m.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-status"})
	mustContain(t, out.String(), `"reserved":false`)
	mustContain(t, out.String(), `"activationEnabled":false`)
	if strings.Contains(out.String(), `"run"`) {
		t.Fatalf("inactive status carried a run: %s", out.String())
	}

	var noOwner bytes.Buffer
	unownedBase := t.TempDir()
	unowned := NewManager(NewCodec(&noOwner), NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, unownedBase), nil)
	unowned.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-status"})
	mustContain(t, noOwner.String(), `"reserved":false`)
	if strings.Contains(noOwner.String(), `"error"`) {
		t.Fatalf("platform-independent ownership read inherited local engine eligibility: %s", noOwner.String())
	}
	writeVLLMGroupJournal(t, filepath.Join(unownedBase, "vllm"), marshalVLLMGroupRun(t, vllmGroupTestRun()))
	noOwner.Reset()
	unowned.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-status"})
	mustContain(t, noOwner.String(), `"reserved":true`)
	mustContain(t, noOwner.String(), `"generation":13`)
}

func TestVLLMGroupMutationFenceRereadsChangedJournal(t *testing.T) {
	base := t.TempDir()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, base)
	if err := ex.rejectVLLMGroupMutation("vllm", "start"); err != nil {
		t.Fatalf("missing journal should release ordinary vLLM: %v", err)
	}
	path := writeVLLMGroupJournal(t, filepath.Join(base, "vllm"), marshalVLLMGroupRun(t, vllmGroupTestRun()))
	if err := ex.rejectVLLMGroupMutation("vllm", "start"); err == nil || !strings.Contains(err.Error(), vllmGroupHeldReason) {
		t.Fatalf("held journal was not fenced: %v", err)
	}
	clean := vllmGroupTestRun()
	clean.State, clean.CleanupConfirmed = "stopped", true
	for i := range clean.Ranks {
		clean.Ranks[i].CleanupConfirmed = true
	}
	if err := os.WriteFile(path, marshalVLLMGroupRun(t, clean), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ex.rejectVLLMGroupMutation("vllm", "start"); err != nil {
		t.Fatalf("fresh clean journal should release ordinary vLLM: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"runId":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ex.rejectVLLMGroupMutation("vllm", "start"); err == nil || !strings.Contains(err.Error(), "ownership is unknown") {
		t.Fatalf("changed unreadable journal did not fail closed: %v", err)
	}
}

func TestHeldVLLMGroupFencesEveryExecutorMutationEntry(t *testing.T) {
	base := t.TempDir()
	writeVLLMGroupJournal(t, filepath.Join(base, "vllm"), marshalVLLMGroupRun(t, vllmGroupTestRun()))
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, base)
	operations := map[string]func() error{
		"install":   func() error { return ex.Install(context.Background(), "vllm") },
		"update":    func() error { return ex.Update(context.Background(), "vllm") },
		"uninstall": func() error { return ex.Uninstall(context.Background(), "vllm") },
		"start":     func() error { return ex.Start(context.Background(), "vllm") },
		"stop":      func() error { return ex.Stop("vllm") },
		"restart":   func() error { return ex.Restart(context.Background(), "vllm") },
		"set-port": func() error {
			_, err := ex.SetPort(context.Background(), "vllm", 18080)
			return err
		},
		"action": func() error {
			_, err := ex.Action(context.Background(), "vllm", "run_model", json.RawMessage(`{"model":"x"}`))
			return err
		},
		"stream-pull": func() error {
			_, err := ex.PullModelStream(context.Background(), "vllm", "x", nil)
			return err
		},
	}
	for name, run := range operations {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil || !strings.Contains(err.Error(), vllmGroupHeldReason) {
				t.Fatalf("mutation reached a later path instead of the owner fence: %v", err)
			}
		})
	}
}

func TestHeldVLLMGroupFencesInternalRestoreAndRemoteBypasses(t *testing.T) {
	base := t.TempDir()
	m, _ := vllmGroupStatusManager(t, base)
	if err := m.exec.setDesiredEnabled("vllm", true); err != nil {
		t.Fatal(err)
	}
	writeVLLMGroupJournal(t, filepath.Join(base, "vllm"), marshalVLLMGroupRun(t, vllmGroupTestRun()))
	if err := m.exec.RestoreEnabled(context.Background()); err == nil || !strings.Contains(err.Error(), vllmGroupHeldReason) {
		t.Fatalf("restore-enabled bypassed the owner fence: %v", err)
	}
	st, err := m.exec.state("vllm")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.exec.doStart(context.Background(), st, "vllm", startOpts{}); err == nil || !strings.Contains(err.Error(), vllmGroupHeldReason) {
		t.Fatalf("direct doStart bypassed the owner fence: %v", err)
	}
	if err := m.exec.doStop(st, "vllm"); err == nil || !strings.Contains(err.Error(), vllmGroupHeldReason) {
		t.Fatalf("direct doStop bypassed the owner fence: %v", err)
	}

	for _, method := range []string{"engine:remote-install", "engine:remote-pull-model", "engine:remote-load-model", "engine:remote-unload-model", "engine:remote-delete-model", "engine:remote-start", "engine:remote-stop"} {
		t.Run(method, func(t *testing.T) {
			var out bytes.Buffer
			remote := NewManager(NewCodec(&out), m.exec, nil)
			id := json.RawMessage("1")
			remote.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(`{"node":"missing","engine":"vllm","model":"x"}`)})
			mustContain(t, out.String(), vllmGroupHeldReason)
			if strings.Contains(out.String(), "not a discovered") {
				t.Fatalf("remote operation reached peer lookup before the owner fence: %s", out.String())
			}
		})
	}
}

func TestHeldVLLMGroupFencesStatusTriggeredActivationRecovery(t *testing.T) {
	base := t.TempDir()
	st := &engineState{
		manifest: &Manifest{Engine: "vllm", DisplayName: "vLLM"}, plat: &Platform{},
		installDir: filepath.Join(base, "vllm"), modelDir: filepath.Join(base, "models", "vllm"), logs: newLogBuffer(),
	}
	record := vllmRuntimeRecord{
		Schema: vllmRuntimeRecordSchema, Active: "v0-old", Staged: "v0-new",
		Activating: &vllmActivationIntent{Candidate: "v0-new", PriorActive: "v0-old", WasRunning: true},
	}
	if err := writeVLLMRuntimeRecord(st, record); err != nil {
		t.Fatal(err)
	}
	writeVLLMGroupJournal(t, st.installDir, marshalVLLMGroupRun(t, vllmGroupTestRun()))
	effects := 0
	control := vllmRuntimeControl{
		stop: func() error { effects++; return nil }, start: func(context.Context) error { effects++; return nil },
		recoveryStart: func(context.Context) error { effects++; return nil }, detect: func(bool) error { effects++; return nil },
		write: func(vllmRuntimeRecord) error { effects++; return nil },
	}
	ex := &Executor{baseDir: base, reporter: NewReporter(nil)}
	if err := ex.reconcileManagedVLLMActivationWithControl(context.Background(), st, control); err == nil || !strings.Contains(err.Error(), vllmGroupHeldReason) {
		t.Fatalf("status-triggered recovery bypassed the owner fence: %v", err)
	}
	if effects != 0 {
		t.Fatalf("held activation recovery performed %d effects", effects)
	}
	got, err := readVLLMRuntimeRecord(st)
	if err != nil || got.Activating == nil || got.Activating.Candidate != "v0-new" {
		t.Fatalf("held activation record changed: %+v err=%v", got, err)
	}
}

func TestVLLMGroupCleanupRequiresFreshExactBindingAndRemainsEffectFree(t *testing.T) {
	base := t.TempDir()
	installDir := filepath.Join(base, "vllm")
	run := vllmGroupTestRun()
	before := marshalVLLMGroupRun(t, run)
	path := writeVLLMGroupJournal(t, installDir, before)
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, base)
	exact := vllmGroupCleanupRequest{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest}
	cases := map[string]vllmGroupCleanupRequest{
		"unbound":          {},
		"stale run":        {RunID: strings.Repeat("b", 32), Generation: run.Generation, PlanDigest: run.PlanDigest},
		"stale generation": {RunID: run.RunID, Generation: run.Generation - 1, PlanDigest: run.PlanDigest},
		"stale digest":     {RunID: run.RunID, Generation: run.Generation, PlanDigest: strings.Repeat("b", 64)},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ex.ReconcileVLLMGroupCleanup(context.Background(), req); err == nil || strings.Contains(err.Error(), "cleanup owner is unavailable") {
				t.Fatalf("invalid binding reached admission hold: %v", err)
			}
		})
	}
	if _, err := ex.ReconcileVLLMGroupCleanup(context.Background(), exact); err == nil || !strings.Contains(err.Error(), "cleanup owner is unavailable") {
		t.Fatalf("exact request without an owner did not stay held: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cleanup admission rewrote the retained journal: %v", err)
	}

	// The same request becomes stale immediately when the owner generation changes.
	next := run
	next.Generation++
	if err := os.WriteFile(path, marshalVLLMGroupRun(t, next), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ReconcileVLLMGroupCleanup(context.Background(), exact); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed owner generation accepted a stale request: %v", err)
	}
}

func TestManagerExposesBoundVLLMGroupCleanup(t *testing.T) {
	base := t.TempDir()
	run := vllmGroupTestRun()
	writeVLLMGroupJournal(t, filepath.Join(base, "vllm"), marshalVLLMGroupRun(t, run))
	m, out := vllmGroupStatusManager(t, base)
	id := json.RawMessage("1")
	params, err := json.Marshal(vllmGroupCleanupRequest{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest})
	if err != nil {
		t.Fatal(err)
	}
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:vllm-group-cleanup", Params: params})
	for i := 0; i < 100 && out.Len() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	mustContain(t, out.String(), "-32000")
	mustContain(t, out.String(), "cleanup owner is unavailable")
}

func TestVLLMGroupCleanupUsesExactOwnerAndPreservesFailure(t *testing.T) {
	f := vllmResourceFixture(t)
	plan := vllmGroupTestPlan(2)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{RunID: strings.Repeat("d", 32), Generation: 13, PlanDigest: digest, Plan: plan, State: "cleanup-required", Failure: "original participant failure"}
	for _, member := range plan.Members {
		run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: member.NodeID, Attempted: true})
	}
	if err := os.MkdirAll(f.st.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, vllmGroupJournalFile), run); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g, err := newVLLMServingGroup(f.st, filepath.Join(f.st.installDir, vllmGroupJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	g.call = func(_ context.Context, _ vllmGroupBinding, action string) error {
		if action != "stop" {
			t.Fatalf("cleanup piggybacked action %q", action)
		}
		calls++
		return nil
	}
	status, err := f.e.ReconcileVLLMGroupCleanup(context.Background(), vllmGroupCleanupRequest{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest})
	if err != nil || calls != 2 || status.Reserved || status.Run == nil || !status.Run.CleanupConfirmed || status.Run.State != "failed" || status.Run.Failure != run.Failure {
		t.Fatalf("cleanup status=%+v calls=%d err=%v", status, calls, err)
	}
}
