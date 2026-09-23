// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
	"time"

	svcerrors "nvpair-shared/errors"
	"nvpair-shared/noderec"

	tea "github.com/charmbracelet/bubbletea"
)

// These tests measure what a view *renders*, before the shell clamps it.
//
// TestViewFrameIsExactlyTerminalSized cannot catch an overflowing view: the
// shell's fitLines makes the frame the right height by construction, so a view
// that emits too many rows still produces a correctly sized frame — with its
// last line silently deleted. And the last line is always the worst one to
// lose, because the optional rows at the bottom are the messages: the status
// toast, the feed warning, the inline editor's prompt.
//
// So these assert the pre-truncation line count against the same budget the
// shell hands the view, with every combination of optional chrome present.

// renderedRows is how many terminal rows a view's View() actually emits.
func renderedRows(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// contentBudget is the row budget the shell gives a view at the given size.
func contentBudget(t *testing.T, w, h int) int {
	t.Helper()
	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = w, h
	m.resizeViews()
	return m.contentHeight()
}

// TestNodesViewNeverOverflowsItsBudget covers the tab with the most optional
// chrome: a filter note, a feed warning, an inbound-invite prompt, an inline
// editor, and a status toast can all be present at once.
func TestNodesViewNeverOverflowsItsBudget(t *testing.T) {
	const w, h = 80, 24
	budget := contentBudget(t, w, h)

	build := func() *nodesView {
		v := newNodesView(nil)
		v.SetSize(w, budget)
		nodes := make([]availableNode, 12)
		for i := range nodes {
			nodes[i] = availableNode{
				HostUUID:  string(rune('a' + i)),
				Name:      "node-" + string(rune('a'+i)),
				IPAddress: "10.0.0." + string(rune('1'+i)),
			}
		}
		v.feeds.discovered = nodes
		v.rebuild()
		return v
	}

	cases := map[string]func(*nodesView){
		"plain":   func(*nodesView) {},
		"filter":  func(v *nodesView) { v.filter = "node"; v.rebuild() },
		"warning": func(v *nodesView) { v.noteFeed(feedManual, errStub{}) },
		"status":  func(v *nodesView) { v.status.error("something failed") },
		"inbound": func(v *nodesView) { v.inbound = &clusterInvite{FromNodeName: "peer"} },
		"editor":  func(v *nodesView) { v.beginInput(nodesInputManualAddress, "host") },
		"filter+warning": func(v *nodesView) {
			v.filter = "node"
			v.rebuild()
			v.noteFeed(feedManual, errStub{})
		},
		"everything": func(v *nodesView) {
			v.filter = "node"
			v.rebuild()
			v.noteFeed(feedManual, errStub{})
			v.inbound = &clusterInvite{FromNodeName: "peer"}
			v.beginInput(nodesInputManualAddress, "host")
			v.status.error("something failed")
		},
		"filter matches nothing": func(v *nodesView) {
			v.filter = "no-such-node"
			v.rebuild()
			v.status.error("something failed")
		},
	}

	for name, setup := range cases {
		v := build()
		setup(v)
		if got := renderedRows(v.View()); got > budget {
			t.Errorf("%s: rendered %d rows into a %d-row budget; the shell will delete the last %d line(s)",
				name, got, budget, got-budget)
		}
	}
}

// TestErrorsViewNeverOverflowsItsBudget covers the context line added for the
// selected error, which appears exactly when an engine failure is highlighted.
func TestErrorsViewNeverOverflowsItsBudget(t *testing.T) {
	const w, h = 80, 24
	budget := contentBudget(t, w, h)

	build := func(n int) *errorsView {
		v := newErrorsView(nil)
		v.SetSize(w, budget)
		errs := make([]svcerrors.ServiceError, n)
		for i := range errs {
			errs[i] = svcerrors.ServiceError{
				ID:         "e" + string(rune('a'+i)),
				Message:    "install failed",
				Timestamp:  time.Now().UnixMilli(),
				Severity:   "error",
				EngineType: "ollama",
				Operation:  "install",
				ModelName:  "llama3.2",
				Action:     "retry",
			}
		}
		v.setErrors(errs)
		return v
	}

	for _, n := range []int{0, 1, 30} {
		v := build(n)
		if got := renderedRows(v.View()); got > budget {
			t.Errorf("%d errors: rendered %d rows into %d", n, got, budget)
		}

		v = build(n)
		v.status.ok("cleared")
		if got := renderedRows(v.View()); got > budget {
			t.Errorf("%d errors + status: rendered %d rows into %d; the status line is what gets cut",
				n, got, budget)
		}
	}
}

// TestNodeDetailNeverOverflowsItsBudget covers the screen with two tables
// sharing one budget plus a hardware block whose height depends on the node.
func TestNodeDetailNeverOverflowsItsBudget(t *testing.T) {
	const w, h = 80, 24
	budget := contentBudget(t, w, h)

	build := func() *nodeDetail {
		d := newNodeDetail(nil, nodeRow{key: "self", name: "this-host", self: true})
		d.engines = []engineStatus{
			{Engine: "ollama", Installed: true, Running: true, Port: 11434},
			{Engine: "lmstudio", Installed: true, Running: false, Port: 1234},
		}
		models := make([]string, 40)
		for i := range models {
			models[i] = "model-" + string(rune('a'+i%26))
		}
		d.models = modelsResult{Models: models, ModelsByEngine: map[string][]string{"ollama": models}}
		d.SetSize(w, budget)
		d.refreshEngines()
		d.refreshModels()
		return d
	}

	cases := map[string]func(*nodeDetail){
		"plain":  func(*nodeDetail) {},
		"editor": func(d *nodeDetail) { d.mode = detailInputEnginePort },
		"status": func(d *nodeDetail) { d.status.error("start failed: no such engine") },
		"editor+status": func(d *nodeDetail) {
			d.mode = detailInputModelName
			d.status.error("download failed")
		},
		"hardware": func(d *nodeDetail) {
			d.telemetryOK = true
			d.telemetry = nodeTelemetry{TelemetryValid: true}
			d.SetSize(w, budget)
		},
	}

	for name, setup := range cases {
		d := build()
		setup(d)
		if got := renderedRows(d.View()); got > budget {
			t.Errorf("%s: rendered %d rows into a %d-row budget", name, got, budget)
		}
	}
}

// TestEveryViewRendersWithinBudget is the blanket guard.
//
// The per-view tests below it were written one at a time and the Jobs tab was
// simply never given one — which is how a splice bug that panicked on the
// tab's ordinary state reached a review. This walks every view the shell can
// show, at every size worth caring about, with and without a status line, and
// asserts two things: it does not panic, and it does not overflow. A new view
// is covered the moment it is added to defaultViews.
func TestEveryViewRendersWithinBudget(t *testing.T) {
	// Every height from the supported minimum upward, not a handful of sampled
	// sizes. The sampled list jumped from a budget of 21 to 11 and stepped
	// straight over the band where the Service tab overran by one row — a hole
	// exactly where a view overflowed, in the test whose whole purpose is to
	// prove none does.
	sizes := make([][2]int, 0, 64)
	for h := minTerminalHeight; h <= 44; h++ {
		sizes = append(sizes, [2]int{80, h})
	}
	sizes = append(sizes, [2]int{200, 60}, [2]int{120, 40}, [2]int{60, 14}, [2]int{40, 12})

	for _, size := range sizes {
		w, h := size[0], size[1]
		budget := contentBudget(t, w, h)
		if budget <= 0 {
			continue
		}

		for _, withStatus := range []bool{false, true} {
			for _, v := range populatedViews(t) {
				if withStatus {
					noteStatus(v)
				}
				v.SetSize(w, budget)

				// A panic here is a crash of the whole program, so it is worth
				// naming the view and size rather than letting the suite die.
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s panicked at %dx%d (status=%v): %v",
								v.Title(), w, h, withStatus, r)
						}
					}()
					if got := renderedRows(v.View()); got > budget {
						t.Errorf("%s rendered %d rows into a %d-row budget at %dx%d (status=%v)",
							v.Title(), got, budget, w, h, withStatus)
					}
				}()
			}
		}
	}
}

// TestServiceConfirmationSurvivesEveryHeight is the regression guard for the
// one irreversible action losing its prompt.
//
// The hidden-worker note used to be guessed before the table was sized and
// substituted afterwards, so across a band of ordinary heights one unbudgeted
// row appeared and the shell deleted the last line — which is the status row
// carrying "press y to confirm" for the data reset. The operator pressed enter
// on the wipe, saw nothing change, and was left armed with no prompt.
func TestServiceConfirmationSurvivesEveryHeight(t *testing.T) {
	for h := minTerminalHeight; h <= 44; h++ {
		budget := contentBudget(t, 80, h)
		if budget <= 0 {
			continue
		}
		v := newServiceView(nil)
		v.refreshWorkers()
		v.SetSize(80, budget)
		v.confirming = len(v.items) - 1
		v.status.arm("Reset all data and quit - press y to confirm, any other key to cancel")

		out := v.View()
		if got := renderedRows(out); got > budget {
			t.Errorf("80x%d (budget %d): rendered %d rows", h, budget, got)
		}
		// Within the budget is necessary but not sufficient: the prompt must be
		// among the rows that survive the shell's clamp.
		if !contains(fitLines(out, budget), "press y to confirm") {
			t.Errorf("80x%d: the reset confirmation is not on the frame:\n%s", h, out)
		}
	}
}

// TestDetailSurvivesAManyGpuHost guards the one block whose height comes from
// the machine rather than the layout. A host reports a line per GPU, and an
// eight-GPU box produced ten unshrinkable lines that pushed the engine table,
// the models list, and the status line off the frame — leaving a hardware
// readout and no sign anything was missing.
func TestDetailSurvivesAManyGpuHost(t *testing.T) {
	for _, gpuCount := range []int{1, 2, 4, 8, 16} {
		gpus := make([]noderec.GPUInfo, gpuCount)
		for i := range gpus {
			gpus[i] = noderec.GPUInfo{Name: "NVIDIA GPU", VramBytes: 1 << 30}
		}
		for h := minTerminalHeight; h <= 30; h++ {
			budget := contentBudget(t, 80, h)
			if budget <= 0 {
				continue
			}
			d := newNodeDetail(nil, nodeRow{key: "self", name: "host", self: true})
			d.engines = []engineStatus{
				{Engine: "ollama", Installed: true, Running: true},
				{Engine: "lmstudio", Installed: true},
			}
			d.telemetryOK = true
			d.telemetry = nodeTelemetry{
				TelemetryValid: true, GPUs: gpus,
				CPU:    &noderec.CPUInfo{Name: "CPU", Cores: 8},
				Memory: &noderec.MemoryInfo{TotalBytes: 1 << 34, UsedBytes: 1 << 33},
			}
			d.SetSize(80, budget)
			d.refreshEngines()
			d.status.error("start lmstudio failed: port in use")

			out := d.View()
			if got := renderedRows(out); got > budget {
				t.Errorf("%d GPUs at 80x%d (budget %d): rendered %d rows",
					gpuCount, h, budget, got)
			}
			// The status line is what the operator just caused; it must not be
			// the thing a long device list displaces.
			if !contains(fitLines(out, budget), "start lmstudio failed") {
				t.Errorf("%d GPUs at 80x%d: the status line was displaced:\n%s",
					gpuCount, h, out)
			}
		}
	}
}

// TestArmedActionsOwnTheKeyboardEverywhere enumerates the confirmation gates.
//
// An armed action that does not capture input can be escaped with tab or a
// digit, which leaves it armed behind a prompt that is no longer on screen — to
// be confirmed by whatever the operator presses on returning. One of the three
// sites had this and the other two did not.
func TestArmedActionsOwnTheKeyboardEverywhere(t *testing.T) {
	// Armed through the real key paths, so the prompt is set the way it is at
	// runtime rather than by poking the flag.
	nodesLeave := newNodesView(nil)
	nodesLeave.SetSize(80, 20)
	nodesLeave.identity.ClusterID = "cluster-1"
	nodesLeave.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if !nodesLeave.confirmLeave {
		t.Fatal("leave did not arm")
	}

	nodesRemove := newNodesView(nil)
	nodesRemove.SetSize(80, 20)
	nodesRemove.feeds.discovered = []availableNode{{
		HostUUID: "peer", Name: "peer", IPAddress: "10.0.0.2", Trusted: true,
	}}
	nodesRemove.rebuild()
	nodesRemove.selectedKey = "peer"
	nodesRemove.removeSelected()
	if nodesRemove.confirmRemove == "" {
		t.Fatal("remove did not arm")
	}

	svc := newServiceView(nil)
	svc.SetSize(80, 20)
	svc.cursor = len(svc.items) - 1
	svc.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if svc.confirming < 0 {
		t.Fatal("reset did not arm")
	}

	detail := localDetail()
	detail.models = modelsResult{ModelsByEngine: map[string][]string{"ollama": {"victim"}}}
	detail.refreshModels()
	detail.pane = detailModels
	detail.deleteSelectedModel()

	for name, ic := range map[string]inputCapturer{
		"nodes/leave":   nodesLeave,
		"nodes/remove":  nodesRemove,
		"service/reset": svc,
		"detail/delete": detail,
	} {
		if !ic.CapturingInput() {
			t.Errorf("%s: armed but not capturing input; tab or a digit escapes the confirmation", name)
		}
	}

	// And every armed prompt must stay on screen for as long as it is armed.
	for name, s := range map[string]*toast{
		"nodes/leave":   &nodesLeave.status,
		"nodes/remove":  &nodesRemove.status,
		"service/reset": &svc.status,
		"detail/delete": &detail.status,
	} {
		if s.expired() {
			t.Errorf("%s: the confirmation prompt expired while the action stayed armed", name)
		}
		if !contains(s.render(), "press y to confirm") {
			t.Errorf("%s: the prompt does not say how to confirm: %q", name, s.render())
		}
	}
}

// TestDetailStaysInBudgetWithAStaleEngineList is the regression guard for the
// fifth round's blocker.
//
// A peer that has gone away fails its engine poll, and the "not answering" note
// makes the engines section one row taller than its table. The drop guard was
// comparing the room left against the table's minimum rather than against what
// it was about to render, so the section went through at exactly the sizes
// where it did not fit — and the shell deleted the status line, which is where
// an armed "press y to confirm" lives.
func TestDetailStaysInBudgetWithAStaleEngineList(t *testing.T) {
	gpus := make([]noderec.GPUInfo, 4)
	for i := range gpus {
		gpus[i] = noderec.GPUInfo{Name: "GPU", VramBytes: 1 << 30}
	}
	for _, stale := range []bool{false, true} {
		for h := minTerminalHeight; h <= 30; h++ {
			budget := contentBudget(t, 80, h)
			if budget <= 0 {
				continue
			}
			d := newNodeDetail(nil, nodeRow{key: "peer", name: "peer-01"})
			d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
			d.enginesStale = stale
			d.telemetryOK = true
			d.telemetry = nodeTelemetry{
				TelemetryValid: true, GPUs: gpus,
				CPU:    &noderec.CPUInfo{Name: "CPU", Cores: 8},
				Memory: &noderec.MemoryInfo{TotalBytes: 1 << 34, UsedBytes: 1 << 33},
			}
			d.SetSize(80, budget)
			d.refreshEngines()
			d.status.error("start failed: port in use")

			out := d.View()
			if got := renderedRows(out); got > budget {
				t.Errorf("stale=%v at 80x%d (budget %d): rendered %d rows",
					stale, h, budget, got)
			}
			if !contains(fitLines(out, budget), "start failed") {
				t.Errorf("stale=%v at 80x%d: the status line was displaced", stale, h)
			}
		}
	}
}

// TestFooterHidesGlobalsWhileCapturing is the regression guard for a footer
// that undid every view's input help at the composition point.
//
// Each view narrows its own help to enter and esc while a field or a
// confirmation owns the keyboard, and the footer prepended the five global
// bindings regardless — so the composed line advertised keys that no longer
// reached the shell. In a port field the digits are what you are meant to type,
// and q typed a q instead of quitting.
func TestFooterHidesGlobalsWhileCapturing(t *testing.T) {
	m := newTestModel(defaultViews(nil)...)
	m.width, m.height = 80, 24
	m.resizeViews()

	nodes, ok := m.views[0].(*nodesView)
	if !ok {
		t.Fatal("first view is not the nodes tab")
	}

	// Not capturing: the globals are there, and first, so a narrow terminal
	// cannot truncate away the way out.
	if got := m.footerView(); !contains(got, "quit") {
		t.Errorf("idle footer does not offer quit: %s", got)
	}

	for name, arm := range map[string]func(){
		"text field": func() { nodes.beginInput(nodesInputManualAddress, "host") },
		"confirmation": func() {
			nodes.cancelInput()
			nodes.confirmLeave = true
		},
	} {
		arm()
		footer := m.footerView()
		for _, dead := range []string{"quit", "go to tab", "next"} {
			if contains(footer, dead) {
				t.Errorf("%s: footer advertises %q, which does not reach the shell: %s",
					name, dead, footer)
			}
		}
	}
}

// populatedViews builds every tab with enough content that its table is real
// rather than an empty state, since an empty table cannot overflow.
func populatedViews(t *testing.T) []View {
	t.Helper()

	nodes := newNodesView(nil)
	discovered := make([]availableNode, 12)
	for i := range discovered {
		discovered[i] = availableNode{
			HostUUID:  string(rune('a' + i)),
			Name:      "node-" + string(rune('a'+i)),
			IPAddress: "10.0.0." + string(rune('1'+i)),
		}
	}
	nodes.feeds.discovered = discovered
	nodes.rebuild()

	jobs := newJobsView(nil)
	for i := range 20 {
		jobs.upsert(workload{
			ID: "j" + string(rune('a'+i)), OriginatedFrom: "node",
			Model: "llama3.2", Engine: "ollama", State: "running",
		})
	}

	errs := newErrorsView(nil)
	entries := make([]svcerrors.ServiceError, 8)
	for i := range entries {
		entries[i] = svcerrors.ServiceError{
			ID: "e" + string(rune('a'+i)), Message: "install failed",
			Timestamp: time.Now().UnixMilli(), Severity: "error",
			EngineType: "ollama", Operation: "install", ModelName: "llama3.2",
		}
	}
	errs.setErrors(entries)

	svc := newServiceView(nil)
	svc.refreshWorkers()

	logs := newLogsView(nil)

	return []View{nodes, jobs, svc, errs, logs}
}

// noteStatus turns on every optional row a view has, since those are the rows
// most likely to be the ones pushed off the frame — they are at the bottom, and
// they are the messages.
func noteStatus(v View) {
	switch t := v.(type) {
	case *nodesView:
		t.status.error("something failed")
	case *jobsView:
		t.status.error("something failed")
		// A running demo adds a progress line, and a trimmed history adds
		// another. Both sit below the table, so the tab has to be measured with
		// them present.
		t.showAll, t.trimmed = true, 3
		t.demo.status = demoPreparing
		t.demo.gen = 1
		t.demo.armed(demoTargetsMsg{gen: 1, targets: []demoTarget{
			{backend: "ollama", port: 11434, model: "llama3.2"},
		}})
	case *serviceView:
		t.status.error("something failed")
	case *errorsView:
		t.status.error("something failed")
	}
}

// TestCatalogBrowserStaysInBudget covers the overlay, which replaces a whole
// screen and so gets the full content budget.
func TestCatalogBrowserStaysInBudget(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}, {60, 12}, {40, 12}} {
		w, h := size[0], size[1]
		budget := contentBudget(t, w, h)
		if budget <= 0 {
			continue
		}

		b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
		models := make([]catalogModel, 30)
		for i := range models {
			models[i] = catalogModel{Name: "model-" + string(rune('a'+i)), Size: 1 << 30}
		}
		b.all = models
		b.loading = false
		b.refresh()
		b.SetSize(w, budget)

		if got := renderedRows(b.View()); got > budget {
			t.Errorf("catalog rendered %d rows into %d at %dx%d", got, budget, w, h)
		}

		b.status.error("download failed")
		if got := renderedRows(b.View()); got > budget {
			t.Errorf("catalog + status rendered %d rows into %d at %dx%d", got, budget, w, h)
		}
	}
}

// TestNodeDetailStaysInBudgetWhenShort covers the drill-down at sizes below the
// 80x24 the per-view test uses, where its two tables and hardware block compete.
func TestNodeDetailStaysInBudgetWhenShort(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {80, 16}, {80, 13}, {60, 12}, {40, 12}} {
		w, h := size[0], size[1]
		budget := contentBudget(t, w, h)
		if budget <= 0 {
			continue
		}

		d := newNodeDetail(nil, nodeRow{key: "self", name: "this-host", self: true})
		d.engines = []engineStatus{
			{Engine: "ollama", Installed: true, Running: true, Port: 11434},
			{Engine: "lmstudio", Installed: true, Running: false, Port: 1234},
		}
		models := make([]string, 20)
		for i := range models {
			models[i] = "model-" + string(rune('a'+i))
		}
		d.models = modelsResult{Models: models, ModelsByEngine: map[string][]string{"ollama": models}}
		d.SetSize(w, budget)
		d.refreshEngines()
		d.refreshModels()
		d.status.error("start failed: no such engine")
		d.mode = detailInputEnginePort

		if got := renderedRows(d.View()); got > budget {
			t.Errorf("node detail rendered %d rows into %d at %dx%d", got, budget, w, h)
		}
	}
}

// TestNarrowAndShortTerminalsStayInBudget checks the smallest sizes anyone
// plausibly uses, where the budget can go to almost nothing.
func TestNarrowAndShortTerminalsStayInBudget(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {80, 14}, {60, 12}, {40, 12}} {
		w, h := size[0], size[1]
		budget := contentBudget(t, w, h)
		if budget <= 0 {
			continue
		}

		v := newNodesView(nil)
		v.SetSize(w, budget)
		v.feeds.discovered = []availableNode{
			{HostUUID: "a", Name: "alpha", IPAddress: "10.0.0.1"},
			{HostUUID: "b", Name: "beta", IPAddress: "10.0.0.2"},
		}
		v.rebuild()
		v.filter = "a"
		v.rebuild()
		v.noteFeed(feedManual, errStub{})
		v.status.error("failed")

		if got := renderedRows(v.View()); got > budget {
			t.Errorf("%dx%d: nodes rendered %d rows into %d", w, h, got, budget)
		}
	}
}

type errStub struct{}

func (errStub) Error() string { return "worker down" }
