// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The Inference Demo's runtime half: model discovery, submission, and the
// node-local state the Jobs tab renders. The plan itself is in demoschedule.go.
//
// Demo state is deliberately node-local and unsynchronized. A demo running on a
// peer is that peer's business; this tracks only what this process started, which
// is why none of it goes near the service push bus.

// dispatcherName is the HTTP client the demo spawns once per request.
//
// It is not a worker: it speaks no JSON-RPC, nothing supervises it, and it is
// absent from versions.json. It ships beside this binary because services/build.sh
// puts it there, which is the same rule used to find the broker.
const dispatcherName = "inference-dispatcher"

// demoStatus is where a run is.
//
// There is deliberately no draining state. Stopping, or reaching the end of the
// schedule, ends the demo immediately as far as the operator is concerned:
// requests already submitted are left to finish on their own and land as
// ordinary job activity. Waiting on them would mean waiting on work the demo is
// explicitly not allowed to cancel or report on.
type demoStatus int

const (
	demoIdle demoStatus = iota
	demoPreparing
	demoRunning
)

// demoRunner owns one node's demo.
//
// The schedule is driven by the shell's one-second tick rather than by timers of
// its own. Every submission offset in demoschedule.go is a whole number of
// seconds, so a one-second tick is exactly enough resolution, and it keeps the
// whole thing inside Bubble Tea's update loop where it can be tested by stepping
// a clock instead of by sleeping.
type demoRunner struct {
	status   demoStatus
	started  time.Time
	schedule []demoRequest
	// next is the index of the first request not yet submitted. The schedule is
	// sorted by time, so this cursor is all that is needed to find what is due.
	next        int
	submitted   int
	targetCount int
	engineCount int

	// gen invalidates an in-flight discovery whose run has since been stopped.
	// Discovery spawns processes and can take seconds, and without this a stop
	// during it would be overwritten by its own reply.
	gen int

	// ctx bounds every child this runner spawns, and is cancelled when the
	// process is shutting down — not when a run stops. Stop must leave
	// in-flight requests alone; quitting should not orphan them.
	ctx    context.Context
	cancel context.CancelFunc

	// executable is resolved once, at first start.
	executable string
}

func newDemoRunner() *demoRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &demoRunner{ctx: ctx, cancel: cancel}
}

// close kills anything still running. Called when the client is quitting.
func (d *demoRunner) close() {
	if d.cancel != nil {
		d.cancel()
	}
}

// demoTargetsMsg is the outcome of model discovery.
type demoTargetsMsg struct {
	gen     int
	targets []demoTarget
	err     error
}

// resolveDispatcher finds the dispatcher beside this executable.
//
// The same rule the broker uses, and for the same reason: an installed bundle
// puts every binary in one directory, so "next to me" is the only location that
// is correct for the tarball, the Debian package, and a packaged desktop app
// alike. There is deliberately no search path — a missing dispatcher is a broken
// install, and saying so is more useful than finding some other copy.
func resolveDispatcher() (string, error) {
	bin := dispatcherName
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(exe), bin)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("%s not found next to nvpair-tui", bin)
	}
	return candidate, nil
}

// dispatcherEnv is the environment for a demo child.
//
// The whole INFERENCE_DISPATCHER_* namespace is dropped rather than any single
// variable: _CONFIG would load an arbitrary JSON config, _RESULT_LOG and
// _DEBUG_ERROR_LOG would make the child write inference metadata to disk, and
// _LOOP would run past the sixty-second ceiling. The schedule is the only thing
// that decides what a demo child does.
//
// The comparison is case-insensitive because Windows matches environment names
// that way: a lowercase inference_dispatcher_loop in this process's environment
// would still reach the child as INFERENCE_DISPATCHER_LOOP.
func dispatcherEnv() []string {
	const prefix = "inference_dispatcher_"
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// dispatcherModel is one entry of the dispatcher's --list-models output.
type dispatcherModel struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Capabilities []string `json:"capabilities"`
}

// generates reports whether a model can serve a text-generation request.
//
// Mirrors supportsGeneration in the Go client: an explicit type wins, otherwise
// capabilities decide, and a model advertising neither gets the benefit of the
// doubt — which is what the dispatcher itself does.
func (m dispatcherModel) generates() bool {
	if m.Type != "" {
		return strings.EqualFold(m.Type, "llm")
	}
	if len(m.Capabilities) == 0 {
		return true
	}
	for _, c := range m.Capabilities {
		switch strings.ToLower(c) {
		case "completion", "chat", "generate":
			return true
		}
	}
	return false
}

// start begins discovery for a run.
//
// Targets are the engine/model pairs the local proxies expose. Ports come from
// the broker's live proxy status, never a constant, and a proxy that is not ready
// is simply not a target — an absent engine is not an error.
func (d *demoRunner) start(proxy *proxyTracker) (tea.Cmd, error) {
	if d.status != demoIdle {
		return nil, fmt.Errorf("a demo is already running on this node")
	}
	if d.executable == "" {
		exe, err := resolveDispatcher()
		if err != nil {
			return nil, err
		}
		d.executable = exe
	}

	type probe struct {
		backend string
		port    int
	}
	probes := make([]probe, 0, len(proxy.engines))
	for _, e := range proxy.engines {
		if !e.ready || e.port == 0 {
			continue
		}
		probes = append(probes, probe{backend: dispatcherBackend(e.engine), port: e.port})
	}
	if len(probes) == 0 {
		return nil, fmt.Errorf("no proxy is listening yet - wait for the endpoints above to come up")
	}

	d.gen++
	gen := d.gen
	d.status = demoPreparing
	d.schedule, d.next, d.submitted = nil, 0, 0
	d.targetCount, d.engineCount = 0, 0

	exe, ctx := d.executable, d.ctx
	return func() tea.Msg {
		var targets []demoTarget
		for _, p := range probes {
			targets = append(targets, probeModels(ctx, exe, p.backend, p.port)...)
		}
		return demoTargetsMsg{gen: gen, targets: targets}
	}, nil
}

// dispatcherBackend maps an engine-manager engine name to the dispatcher's own
// spelling of it. They agree for Ollama and differ for LM Studio, and the
// dispatcher rejects a name it does not know.
func dispatcherBackend(engine string) string {
	if strings.EqualFold(engine, "lmstudio") {
		return "lmstudio"
	}
	return engine
}

// probeModels asks one engine for its inventory. Any failure means that engine
// contributes no targets; it is never a demo failure.
func probeModels(ctx context.Context, exe, backend string, port int) []demoTarget {
	ctx, cancel := context.WithTimeout(ctx, demoProbeTimeout+2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe,
		"--backend", backend,
		"--port", strconv.Itoa(port),
		"--timeout", strconv.Itoa(int(demoProbeTimeout.Seconds())),
		"--list-models",
	)
	cmd.Env = dispatcherEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var models []dispatcherModel
	if err := json.Unmarshal(out, &models); err != nil {
		return nil
	}
	targets := make([]demoTarget, 0, len(models))
	for _, m := range models {
		if m.Name == "" || !m.generates() {
			continue
		}
		targets = append(targets, demoTarget{backend: backend, port: port, model: m.Name})
	}
	return targets
}

// armed folds a discovery reply into a live run, reporting whether the run
// started. A stale generation or an empty inventory both leave the runner idle.
func (d *demoRunner) armed(msg demoTargetsMsg) bool {
	if msg.gen != d.gen || d.status != demoPreparing {
		return false
	}
	if len(msg.targets) == 0 {
		d.reset()
		return false
	}

	engines := map[string]struct{}{}
	for _, t := range msg.targets {
		engines[fmt.Sprintf("%s:%d", t.backend, t.port)] = struct{}{}
	}

	d.schedule = buildDemoSchedule(msg.targets)
	d.next, d.submitted = 0, 0
	d.targetCount = len(msg.targets)
	d.engineCount = len(engines)
	d.started = time.Now()
	d.status = demoRunning
	return true
}

// tick submits whatever the clock has made due and reports whether the window
// has closed.
//
// Elapsed time is measured against the wall clock rather than counted in ticks,
// so a stalled or suspended process resumes at the right point in the schedule
// instead of stretching the run. The ceiling is enforced on the same clock,
// which is what makes "nothing at or after sixty seconds" true even if the
// update loop was blocked across it.
func (d *demoRunner) tick() (cmds []tea.Cmd, finished bool) {
	if d.status != demoRunning {
		return nil, false
	}
	elapsed := time.Since(d.started)
	if elapsed >= demoMaxSubmit {
		d.reset()
		return nil, true
	}

	exe, ctx := d.executable, d.ctx
	for d.next < len(d.schedule) && d.schedule[d.next].at <= elapsed {
		req := d.schedule[d.next]
		d.next++
		d.submitted++
		cmds = append(cmds, submitDemoRequest(ctx, exe, req))
	}
	if d.next >= len(d.schedule) {
		// Every planned request is away. The window is over as far as the
		// operator is concerned; the requests themselves finish on their own.
		d.reset()
		return cmds, true
	}
	return cmds, false
}

// submitDemoRequest spawns one dispatcher and does not wait for it.
//
// The command is returned rather than run inline so the spawn happens off the
// update loop. Nothing about demo state depends on when the child finishes: the
// evidence it produces is a job on the table, which arrives over the workload
// stream like any other.
func submitDemoRequest(ctx context.Context, exe string, req demoRequest) tea.Cmd {
	stage := demoStages[req.stage]
	args := []string{
		"--backend", req.target.backend,
		"--port", strconv.Itoa(req.target.port),
		"--model", req.target.model,
		"--prompt", stage.prompt,
		"--count", "1",
		"--mode", "series",
		"--concurrency", "1",
		"--timeout", strconv.Itoa(int(demoRequestTimeout.Seconds())),
		"--max-tokens", strconv.Itoa(stage.maxTokens),
		"--temperature", strconv.FormatFloat(stage.temperature, 'f', -1, 64),
	}
	// Ollama's separate reasoning channel would inflate latency and token counts
	// without changing what the demo shows, so it is switched off where it exists.
	if req.target.backend == "ollama" {
		args = append(args, "--ollama-think", "false")
	}

	return func() tea.Msg {
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Env = dispatcherEnv()
		// No pipes: the child's output is inference content, and this process
		// has no business reading it. Start rather than Run, and Wait in a
		// goroutine purely to reap the child.
		if err := cmd.Start(); err != nil {
			// One request failing to spawn is not a demo failure. It is also not
			// worth a message: the operator asked for traffic, not a per-request
			// report, and the schedule carries on.
			return nil
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
}

// stop ends the run now. In-flight requests are deliberately left alone.
func (d *demoRunner) stop() {
	d.gen++
	d.reset()
}

func (d *demoRunner) reset() {
	d.status = demoIdle
	d.schedule, d.next, d.submitted = nil, 0, 0
	d.targetCount, d.engineCount = 0, 0
	d.started = time.Time{}
}

// note is the demo's line on the Jobs tab, or empty when nothing is running.
func (d *demoRunner) note() string {
	switch d.status {
	case demoPreparing:
		return footerStyle.Render("  Inference demo: finding models...")
	case demoRunning:
		left := demoMaxSubmit - time.Since(d.started)
		if left < 0 {
			left = 0
		}
		return statusOKStyle.Render(fmt.Sprintf(
			"  Inference demo: %d/%d sent across %d model(s) on %d engine(s) - %ds left, press t to stop",
			d.submitted, len(d.schedule), d.targetCount, d.engineCount,
			int(left.Round(time.Second).Seconds())))
	default:
		return ""
	}
}
