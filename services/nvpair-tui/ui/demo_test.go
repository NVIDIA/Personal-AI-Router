// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		require.NotEmpty(t, schedule, "%d targets produced an empty schedule", count)
		for _, req := range schedule {
			assert.Less(t, req.at, demoMaxSubmit, "%d targets: request must precede the ceiling", count)
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
	assert.Equal(t, 58*time.Second, last)
}

func TestScheduleTouchesEveryTargetBeforeRepeatingOne(t *testing.T) {
	// The round-robin exists so a host with many models demonstrates all of
	// them. Assigning targets before sorting by time would still touch them all
	// eventually, but not before revisiting some — and a demo that sends four
	// requests to one model and none to another is not showing the router.
	const count = 17
	schedule := buildDemoSchedule(targets(count))
	require.GreaterOrEqual(t, len(schedule), count, "schedule must cover every target")

	seen := map[string]int{}
	for _, req := range schedule[:count] {
		seen[req.target.model]++
	}
	assert.Len(t, seen, count, "first requests must cover distinct targets")
	for model, n := range seen {
		assert.Equal(t, 1, n, "target %q among the first %d requests", model, count)
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
	assert.GreaterOrEqual(t, len(schedule), count, "schedule must cover every target")
	assert.Greater(t, demoAgentsPerCohort(count), demoBaseAgentsPerCohort, "agents per cohort must grow past the base")
}

func TestScheduleIsEmptyWithoutTargets(t *testing.T) {
	assert.Empty(t, buildDemoSchedule(nil))
}

// tickAt runs the runner's tick as though the given time had elapsed.
func tickAt(t *testing.T, d *demoRunner, elapsed time.Duration) (int, bool) {
	t.Helper()
	d.started = time.Now().Add(-elapsed)
	cmds, finished := d.tick()
	return len(cmds), finished
}

// armedRunner is a runner mid-run, without spawning anything. The executable is
// a path that does not exist, so a test that runs a submission only sees it fail
// to start.
func armedRunner(t *testing.T, targetCount int) *demoRunner {
	t.Helper()
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.executable = "/nonexistent/inference-dispatcher"
	d.status = demoPreparing
	d.gen = 1
	require.True(t, d.armed(demoTargetsMsg{gen: 1, targets: targets(targetCount)}), "runner did not arm")
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
	}
	assert.Equal(t, planned, total, "submitted requests")
}

// TestOnlyStartedRequestsCount is the regression guard for the progress note
// counting a request as sent when its dispatcher never started. The desktop
// counts on spawn, and so does this.
func TestOnlyStartedRequestsCount(t *testing.T) {
	d := armedRunner(t, 3)
	d.started = time.Now()
	cmds, _ := d.tick()
	require.NotEmpty(t, cmds, "no requests were due at the start of the window")
	assert.Equal(t, 0, d.submitted, "counted sent before any dispatcher started")
	// armedRunner's executable does not exist, so every one of these fails.
	for _, cmd := range cmds {
		assert.Nil(t, cmd(), "a dispatcher that never started reported a message")
	}

	d.spawned(demoSpawnedMsg{gen: d.gen})
	assert.Equal(t, 1, d.submitted, "a started dispatcher must be counted once")
	d.spawned(demoSpawnedMsg{gen: d.gen - 1})
	assert.Equal(t, 1, d.submitted, "an earlier run's request was counted in this one")
}

func TestTickSubmitsNothingOnceTheWindowHasClosed(t *testing.T) {
	// The wall clock, not the tick count, enforces the ceiling — so a process
	// that was suspended across the whole window must come back to a finished
	// demo rather than flushing sixty requests at once.
	d := armedRunner(t, 3)

	n, finished := tickAt(t, d, demoMaxSubmit+30*time.Second)
	assert.Equal(t, 0, n, "submitted requests after the ceiling")
	assert.True(t, finished, "tick past the ceiling did not finish the run")
	assert.Equal(t, demoIdle, d.status, "status after the ceiling")
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
	require.Equal(t, demoIdle, d.status, "status after stop")

	assert.False(t, d.armed(demoTargetsMsg{gen: 1, targets: targets(2)}), "a stale discovery reply started a run")
	assert.Equal(t, demoIdle, d.status, "status after a stale reply")
}

func TestStopMidRunLeavesNothingScheduled(t *testing.T) {
	d := armedRunner(t, 3)
	n, _ := tickAt(t, d, 0)
	require.NotEqual(t, 0, n, "no requests were due at the start of the window")

	d.stop()
	n, finished := tickAt(t, d, 5*time.Second)
	assert.Equal(t, 0, n, "submitted requests after stop")
	assert.False(t, finished, "a tick after stop reported the run finishing again")
}

func TestEmptyInventoryDoesNotStartARun(t *testing.T) {
	// An engine that is up but has no model is the common case on a fresh
	// install, and it must read as "install a model", not as a broken demo.
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.status = demoPreparing
	d.gen = 1

	assert.False(t, d.armed(demoTargetsMsg{gen: 1}), "armed with no targets")
	assert.Equal(t, demoIdle, d.status)
}

func TestStartRefusesWhenNoProxyIsListening(t *testing.T) {
	// The demo targets proxy ports, never an engine's own, so with no proxy up
	// there is nowhere legitimate to send traffic. It must refuse rather than
	// invent a port.
	d := newDemoRunner()
	t.Cleanup(d.close)
	d.executable = "/nonexistent/inference-dispatcher"

	tracker := newProxyTracker()
	_, err := d.start(tracker)
	require.Error(t, err, "start succeeded with both proxies down")
	assert.Equal(t, demoIdle, d.status, "status after a refused start")
}

func TestStartRefusesASecondConcurrentRun(t *testing.T) {
	d := armedRunner(t, 2)
	tracker := newProxyTracker()
	tracker.engines[0].ready = true
	tracker.engines[0].port = 11434

	_, err := d.start(tracker)
	assert.Error(t, err, "a second demo started while one was running")
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
		assert.False(t, strings.HasPrefix(strings.ToLower(name), "inference_dispatcher_"), "child environment still carries %q", name)
		if name == "PAIR_DEMO_KEEPME" {
			kept = true
		}
	}
	assert.True(t, kept, "stripping removed an unrelated variable")
	_, ok := os.LookupEnv("INFERENCE_DISPATCHER_LOOP")
	assert.True(t, ok, "this process's own environment was modified")
}

func TestGeneratesMirrorsTheDispatcher(t *testing.T) {
	// A model advertising neither a type nor capabilities gets the benefit of
	// the doubt, because that is what the dispatcher itself does — being
	// stricter here would silently exclude models the demo could have used.
	test := func(name string, model dispatcherModel, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, model.generates())
		})
	}
	test("explicit llm", dispatcherModel{Type: "LLM"}, true)
	test("explicit embedding", dispatcherModel{Type: "embeddings"}, false)
	test("chat capability", dispatcherModel{Capabilities: []string{"vision", "chat"}}, true)
	test("no generation capability", dispatcherModel{Capabilities: []string{"embedding"}}, false)
	test("nothing declared", dispatcherModel{}, true)
	test("type wins over capabilities", dispatcherModel{Type: "embeddings", Capabilities: []string{"chat"}}, false)
	// Verbatim from a live LM Studio: a real chat model whose advertised
	// capabilities name neither chat nor completion. Checking capabilities
	// ahead of the type would exclude the only usable model on the host, so
	// the ordering above is load-bearing rather than arbitrary.
	test("real llm with unrelated capabilities", dispatcherModel{Type: "llm", Capabilities: []string{"trained_for_tool_use", "vision"}}, true)
	test("real embedding model", dispatcherModel{Type: "embedding"}, false)
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
	require.NoError(t, os.WriteFile(exe, []byte(script), 0o700))
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

	assert.Equal(t, []demoTarget{
		{backend: "ollama", port: 11434, model: "llama3.2:latest"},
		{backend: "ollama", port: 11434, model: "mystery-model"},
	}, probeModels(context.Background(), exe, "ollama", 11434))

	raw, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	args := strings.Fields(string(raw))
	assert.Contains(t, args, "--backend", "dispatcher argument")
	assert.Contains(t, args, "ollama", "dispatcher argument")
	assert.Contains(t, args, "--port", "dispatcher argument")
	assert.Contains(t, args, "11434", "dispatcher argument")
	assert.Contains(t, args, "--list-models", "dispatcher argument")
}

func TestProbeTreatsAnUnreachableEngineAsNoTargets(t *testing.T) {
	// An engine that is down is not a demo failure — it just is not a target.
	assert.Empty(t, probeModels(context.Background(), "/nonexistent/dispatcher", "ollama", 1), "missing dispatcher must produce no targets")
}

func TestProbeIgnoresOutputThatIsNotAModelList(t *testing.T) {
	exe, _ := fakeDispatcher(t, "Model query failed: connection refused")
	assert.Empty(t, probeModels(context.Background(), exe, "ollama", 11434), "non-JSON output must produce no targets")
}

func TestNoteNamesTheKeyThatStopsIt(t *testing.T) {
	// The note is the only place the stop key is stated while a demo runs, and
	// the footer's label is derived from the same state — so an operator who
	// wants it to stop has somewhere to look.
	d := armedRunner(t, 2)
	assert.Contains(t, d.note(), "press t to stop", "running note must name the stop key")

	d.stop()
	assert.Empty(t, d.note(), "idle runner still renders a note")
}
