// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The demo's guarantees are all about what it does NOT do: nothing at or after
// the ceiling, no target skipped, no request replayed, nothing left running when
// the operator stops it. Each of those is a property of the plan or of the
// cursor, so they are asserted directly rather than by running a demo.

func targets(n int) []demoTarget {
	out := make([]demoTarget, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, demoTarget{
			backend: "ollama",
			port:    11434,
			model:   string(rune('a'+i%26)) + "-model",
		})
	}
	return out
}

func TestScheduleSubmitsNothingAtOrAfterTheCeiling(t *testing.T) {
	// The ceiling is the demo's one hard promise: a burst that outlives its
	// window is indistinguishable from a load generator someone forgot about.
	for _, count := range []int{1, 3, 7, 60, 61, 200} {
		schedule := buildDemoSchedule(targets(count))
		if len(schedule) == 0 {
			t.Fatalf("%d targets produced an empty schedule", count)
		}
		for _, req := range schedule {
			if req.at >= demoMaxSubmit {
				t.Errorf("%d targets: request planned at %s, ceiling is %s",
					count, req.at, demoMaxSubmit)
			}
		}
	}
}

func TestScheduleLastSubmissionLandsAtFiftyEight(t *testing.T) {
	// Pins the cohort/stage arithmetic against a silent change: if the omitted
	// 50s cohort were reinstated, or a stage offset moved, the window would
	// quietly stop being what both front ends document.
	schedule := buildDemoSchedule(targets(4))
	var last time.Duration
	for _, req := range schedule {
		if req.at > last {
			last = req.at
		}
	}
	if want := 58 * time.Second; last != want {
		t.Errorf("last submission at %s, want %s", last, want)
	}
}

func TestScheduleTouchesEveryTargetBeforeRepeatingOne(t *testing.T) {
	// The round-robin exists so a host with many models demonstrates all of
	// them. Assigning targets before sorting by time would still touch them all
	// eventually, but not before revisiting some — and a demo that sends four
	// requests to one model and none to another is not showing the router.
	const count = 17
	schedule := buildDemoSchedule(targets(count))
	if len(schedule) < count {
		t.Fatalf("schedule has %d requests, too few for %d targets", len(schedule), count)
	}

	seen := map[string]int{}
	for _, req := range schedule[:count] {
		seen[req.target.model]++
	}
	if len(seen) != count {
		t.Errorf("first %d requests covered %d distinct targets, want %d",
			count, len(seen), count)
	}
	for model, n := range seen {
		if n != 1 {
			t.Errorf("target %q received %d of the first %d requests, want 1", model, n, count)
		}
	}
}

func TestScheduleAddsAgentsSoEveryTargetFits(t *testing.T) {
	// Beyond the 60 requests the base agent count produces, the schedule has to
	// grow rather than drop targets off the end.
	const count = 130
	schedule := buildDemoSchedule(targets(count))

	seen := map[string]struct{}{}
	for _, req := range schedule {
		seen[req.target.model] = struct{}{}
	}
	// 130 targets over a 26-letter model alphabet is 26 distinct names; what
	// matters is that the schedule is long enough to cover the target count.
	if len(schedule) < count {
		t.Errorf("%d targets produced only %d requests", count, len(schedule))
	}
	if got := demoAgentsPerCohort(count); got <= demoBaseAgentsPerCohort {
		t.Errorf("agents per cohort %d did not grow past the base %d",
			got, demoBaseAgentsPerCohort)
	}
}

func TestScheduleIsEmptyWithoutTargets(t *testing.T) {
	if got := buildDemoSchedule(nil); got != nil {
		t.Errorf("no targets produced %d requests, want none", len(got))
	}
}

// tickAt runs the runner's tick as though the given time had elapsed.
func tickAt(t *testing.T, d *demoRunner, elapsed time.Duration) (int, bool) {
	t.Helper()
	d.started = time.Now().Add(-elapsed)
	cmds, finished := d.tick()
	return len(cmds), finished
}

// armedRunner is a runner mid-run, without spawning anything. The executable is
// a path that does not exist, which is safe because no test here runs a command.
func armedRunner(t *testing.T, targetCount int) *demoRunner {
	t.Helper()
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.executable = "/nonexistent/inference-dispatcher"
	d.status = demoPreparing
	d.gen = 1
	if !d.armed(demoTargetsMsg{gen: 1, targets: targets(targetCount)}) {
		t.Fatal("runner did not arm")
	}
	return d
}

func TestTickSubmitsEachRequestExactlyOnce(t *testing.T) {
	// The cursor is what prevents a replay. Ticking repeatedly over the same
	// window must not resend anything, which a "submit everything due" loop
	// without the cursor would do on every single tick.
	d := armedRunner(t, 3)
	planned := len(d.schedule)

	total := 0
	for elapsed := time.Duration(0); elapsed < demoMaxSubmit; elapsed += time.Second {
		n, finished := tickAt(t, d, elapsed)
		total += n
		if finished {
			break
		}
		// The counter drives the progress note, so it has to agree with the
		// spawns while the run is live. It is reset once the window closes,
		// which is why this is checked here and not after the loop.
		if d.submitted != total {
			t.Fatalf("at %s the runner counted %d submitted, %d were spawned",
				elapsed, d.submitted, total)
		}
	}
	if total != planned {
		t.Errorf("submitted %d of %d planned requests", total, planned)
	}
}

func TestTickSubmitsNothingOnceTheWindowHasClosed(t *testing.T) {
	// The wall clock, not the tick count, enforces the ceiling — so a process
	// that was suspended across the whole window must come back to a finished
	// demo rather than flushing sixty requests at once.
	d := armedRunner(t, 3)

	n, finished := tickAt(t, d, demoMaxSubmit+30*time.Second)
	if n != 0 {
		t.Errorf("submitted %d requests after the ceiling, want 0", n)
	}
	if !finished {
		t.Error("tick past the ceiling did not finish the run")
	}
	if d.status != demoIdle {
		t.Errorf("status %v after the ceiling, want idle", d.status)
	}
}

func TestStopEndsTheRunAndIgnoresLateDiscovery(t *testing.T) {
	// Discovery spawns processes and can take seconds. Without the generation
	// guard a stop during it would be undone by its own reply, starting a demo
	// the operator had already cancelled.
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.executable = "/nonexistent/inference-dispatcher"
	d.status = demoPreparing
	d.gen = 1

	d.stop()
	if d.status != demoIdle {
		t.Fatalf("status %v after stop, want idle", d.status)
	}

	if d.armed(demoTargetsMsg{gen: 1, targets: targets(2)}) {
		t.Error("a stale discovery reply started a run")
	}
	if d.status != demoIdle {
		t.Errorf("status %v after a stale reply, want idle", d.status)
	}
}

func TestStopMidRunLeavesNothingScheduled(t *testing.T) {
	d := armedRunner(t, 3)
	if n, _ := tickAt(t, d, 0); n == 0 {
		t.Fatal("no requests were due at the start of the window")
	}

	d.stop()
	n, finished := tickAt(t, d, 5*time.Second)
	if n != 0 {
		t.Errorf("submitted %d requests after stop, want 0", n)
	}
	if finished {
		t.Error("a tick after stop reported the run finishing again")
	}
}

func TestEmptyInventoryDoesNotStartARun(t *testing.T) {
	// An engine that is up but has no model is the common case on a fresh
	// install, and it must read as "install a model", not as a broken demo.
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.status = demoPreparing
	d.gen = 1

	if d.armed(demoTargetsMsg{gen: 1}) {
		t.Error("armed with no targets")
	}
	if d.status != demoIdle {
		t.Errorf("status %v, want idle", d.status)
	}
}

func TestStartRefusesWhenNoProxyIsListening(t *testing.T) {
	// The demo targets proxy ports, never an engine's own, so with no proxy up
	// there is nowhere legitimate to send traffic. It must refuse rather than
	// invent a port.
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.executable = "/nonexistent/inference-dispatcher"

	tracker := newProxyTracker()
	if _, err := d.start(tracker); err == nil {
		t.Fatal("start succeeded with both proxies down")
	}
	if d.status != demoIdle {
		t.Errorf("status %v after a refused start, want idle", d.status)
	}
}

func TestStartRefusesASecondConcurrentRun(t *testing.T) {
	d := armedRunner(t, 2)
	tracker := newProxyTracker()
	tracker.engines[0].ready = true
	tracker.engines[0].port = 11434

	if _, err := d.start(tracker); err == nil {
		t.Error("a second demo started while one was running")
	}
}

func TestDispatcherEnvDropsTheWholeDispatcherNamespace(t *testing.T) {
	// _CONFIG loads an arbitrary config, _LOOP runs past the ceiling, and the
	// log variables write inference metadata to disk. Stripping the prefix
	// rather than a list is what keeps a newly added variable from becoming a
	// way to redirect the demo.
	t.Setenv("INFERENCE_DISPATCHER_LOOP", "true")
	t.Setenv("INFERENCE_DISPATCHER_CONFIG", "/tmp/evil.json")
	// Lowercase because Windows matches environment names case-insensitively,
	// so this would still reach a child as INFERENCE_DISPATCHER_RESULT_LOG.
	t.Setenv("inference_dispatcher_result_log", "/tmp/leak.jsonl")
	t.Setenv("PAIR_DEMO_KEEPME", "1")

	var kept bool
	for _, kv := range dispatcherEnv() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToLower(name), "inference_dispatcher_") {
			t.Errorf("child environment still carries %q", name)
		}
		if name == "PAIR_DEMO_KEEPME" {
			kept = true
		}
	}
	if !kept {
		t.Error("stripping removed an unrelated variable")
	}
	if _, ok := os.LookupEnv("INFERENCE_DISPATCHER_LOOP"); !ok {
		t.Error("this process's own environment was modified")
	}
}

func TestGeneratesMirrorsTheDispatcher(t *testing.T) {
	// A model advertising neither a type nor capabilities gets the benefit of
	// the doubt, because that is what the dispatcher itself does — being
	// stricter here would silently exclude models the demo could have used.
	cases := []struct {
		name  string
		model dispatcherModel
		want  bool
	}{
		{"explicit llm", dispatcherModel{Type: "LLM"}, true},
		{"explicit embedding", dispatcherModel{Type: "embeddings"}, false},
		{"chat capability", dispatcherModel{Capabilities: []string{"vision", "chat"}}, true},
		{"no generation capability", dispatcherModel{Capabilities: []string{"embedding"}}, false},
		{"nothing declared", dispatcherModel{}, true},
		{"type wins over capabilities", dispatcherModel{
			Type: "embeddings", Capabilities: []string{"chat"}}, false},
		// Verbatim from a live LM Studio: a real chat model whose advertised
		// capabilities name neither chat nor completion. Checking capabilities
		// ahead of the type would exclude the only usable model on the host, so
		// the ordering above is load-bearing rather than arbitrary.
		{"real llm with unrelated capabilities", dispatcherModel{
			Type:         "llm",
			Capabilities: []string{"trained_for_tool_use", "vision"},
		}, true},
		{"real embedding model", dispatcherModel{Type: "embedding"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.model.generates(); got != tc.want {
				t.Errorf("generates() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeDispatcher writes an executable that records its arguments and prints the
// given stdout, and returns its path plus the path it records into.
func fakeDispatcher(t *testing.T, stdout string) (exe, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is not executable on Windows")
	}
	dir := t.TempDir()
	exe = filepath.Join(dir, "inference-dispatcher")
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat <<'JSON'\n" + stdout + "\nJSON\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, argsFile
}

func TestProbeAsksTheDispatcherCorrectlyAndKeepsOnlyGenerativeModels(t *testing.T) {
	// This runs the real command path. A mistyped flag would otherwise show up
	// only as a demo that always says no model is available — the failure mode
	// least likely to be read as a bug in this code.
	exe, argsFile := fakeDispatcher(t, `[
		{"name":"llama3.2:latest","type":"llm"},
		{"name":"nomic-embed-text","type":"embeddings"},
		{"name":"mystery-model"},
		{"name":"","type":"llm"}
	]`)

	got := probeModels(context.Background(), exe, "ollama", 11434)

	want := []string{"llama3.2:latest", "mystery-model"}
	if len(got) != len(want) {
		t.Fatalf("got %d targets %+v, want %d", len(got), got, len(want))
	}
	for i, model := range want {
		if got[i].model != model {
			t.Errorf("target %d is %q, want %q", i, got[i].model, model)
		}
		if got[i].backend != "ollama" || got[i].port != 11434 {
			t.Errorf("target %d addressed %s:%d, want ollama:11434",
				i, got[i].backend, got[i].port)
		}
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(raw))
	for _, want := range []string{"--backend", "ollama", "--port", "11434", "--list-models"} {
		if !slices.Contains(args, want) {
			t.Errorf("dispatcher was not passed %q; got %v", want, args)
		}
	}
}

func TestProbeTreatsAnUnreachableEngineAsNoTargets(t *testing.T) {
	// An engine that is down is not a demo failure — it just is not a target.
	if got := probeModels(context.Background(), "/nonexistent/dispatcher", "ollama", 1); got != nil {
		t.Errorf("got %d targets from a missing dispatcher, want none", len(got))
	}
}

func TestProbeIgnoresOutputThatIsNotAModelList(t *testing.T) {
	exe, _ := fakeDispatcher(t, "Model query failed: connection refused")
	if got := probeModels(context.Background(), exe, "ollama", 11434); got != nil {
		t.Errorf("got %d targets from non-JSON output, want none", len(got))
	}
}

func TestNoteNamesTheKeyThatStopsIt(t *testing.T) {
	// The note is the only place the stop key is stated while a demo runs, and
	// the footer's label is derived from the same state — so an operator who
	// wants it to stop has somewhere to look.
	d := armedRunner(t, 2)
	note := d.note()
	if !strings.Contains(note, "press t to stop") {
		t.Errorf("running note does not name the stop key: %q", note)
	}

	d.stop()
	if got := d.note(); got != "" {
		t.Errorf("idle runner still renders a note: %q", got)
	}
}
