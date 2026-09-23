// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"sort"
	"time"
)

// The Inference Demo's planning half: a fixed, node-local burst of synthetic
// traffic sent through the local proxies so an operator can watch real work move
// through the router.
//
// Where each request lands is the backend's decision. Requests are addressed to
// a proxy exactly as a third-party client would address one, so whether that
// produces work on this machine or several depends on the cluster the proxies
// see. The demo is not a benchmark and not a diagnostic: no prompt, response,
// score, or timing is ever shown, and the only visible output is ordinary job
// activity on the Jobs table.
//
// This mirrors the desktop app's schedule (desktop/src/shared/types/
// inference-demo.ts and src/electron/inference-demo-schedule.ts) deliberately
// and exactly, because the two front ends are meant to demonstrate the same
// thing. The constants below are the contract between them; changing one without
// the other makes "run the demo" mean two different things depending on which
// interface the operator happened to use.
//
// Kept free of process spawning and Bubble Tea so the guarantees that matter —
// nothing submitted at or after the ceiling, every target touched before any is
// revisited, open-loop overlap — can be asserted directly.

// Wall-clock offsets at which a cohort of simulated agents starts.
var demoCohortOffsets = []time.Duration{
	0,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
	40 * time.Second,
}

// Simulated agent workloads started per cohort, before target-count scaling.
const demoBaseAgentsPerCohort = 2

// demoMaxSubmit is the hard ceiling on the submission window. Nothing is
// submitted at or after this point; work already in flight is left alone. The
// 50-second cohort is omitted on purpose so the last submission lands at 58s.
const demoMaxSubmit = 60 * time.Second

// demoRequestTimeout is the per-request timeout handed to the dispatcher.
const demoRequestTimeout = 120 * time.Second

// demoProbeTimeout bounds the model-discovery calls that run before the window
// opens. Short, because discovery is the operator waiting with nothing on screen.
const demoProbeTimeout = 10 * time.Second

// demoStage is one request shape of a simulated agent. Offsets are relative to
// the owning agent's start, so the final submission of the 40-second cohort
// lands at 58s.
//
// The prompts are internal and intentionally generic: they exercise an engine
// without depending on any real workload. They are never displayed, logged, or
// retained.
type demoStage struct {
	offset      time.Duration
	maxTokens   int
	temperature float64
	prompt      string
}

var demoStages = []demoStage{
	{
		offset:      0,
		maxTokens:   48,
		temperature: 0.0,
		prompt:      `Classify the following support request into one category: billing, technical, or account. Request: "My export finished but the download link returns a 404." Answer with the category only.`,
	},
	{
		offset:      3 * time.Second,
		maxTokens:   192,
		temperature: 0.1,
		prompt:      "Draft a short numbered plan for diagnosing an intermittent HTTP 404 on a file download endpoint that only affects large exports. Keep it under six steps.",
	},
	{
		offset:      7 * time.Second,
		maxTokens:   96,
		temperature: 0.0,
		prompt:      "Given the tools list_objects, read_log, restart_worker, and notify_user, choose the single best next tool for confirming whether an exported file was ever written to object storage. Answer with the tool name and one sentence of justification.",
	},
	{
		offset:      10 * time.Second,
		maxTokens:   160,
		temperature: 0.0,
		prompt:      "Rewrite this function so it returns an explicit error instead of nil when the key is missing:\n\nfunc get(m map[string]string, k string) string { return m[k] }",
	},
	{
		offset:      14 * time.Second,
		maxTokens:   128,
		temperature: 0.0,
		prompt:      "Summarize in two sentences: the worker log shows 14 successful uploads, 2 uploads that timed out after 30s, and no retry attempts recorded for the timeouts.",
	},
	{
		offset:      18 * time.Second,
		maxTokens:   256,
		temperature: 0.1,
		prompt:      "Write a brief incident note covering root cause, user impact, and the single highest-value follow-up action, for an issue where large export uploads timed out and were never retried.",
	},
}

// demoTarget is one engine/model pair discovered at demo start.
//
// backend is the dispatcher's own engine name, and port is a proxy's listen
// port — never an engine's own. Addressing the proxy is what makes this a demo
// of PAIR rather than of Ollama: the request enters the router and the backend
// places it. Hitting an engine's port directly would bypass routing, which
// proxy-inference-routing.mdc prohibits.
type demoTarget struct {
	backend string
	port    int
	model   string
}

// demoRequest is one planned submission.
type demoRequest struct {
	at     time.Duration // after the window opens
	target demoTarget
	stage  int // index into demoStages
}

// demoAgentsPerCohort is enough simulated agents that the schedule has at least
// one request per target.
//
// A run produces cohorts x agents x stages requests — 5 x 2 x 6 = 60 at the base
// count — and targets are assigned per request, so the base already covers up to
// 60 targets. Beyond that this adds agents rather than dropping targets, so a
// host exposing eighty models still demonstrates all of them.
func demoAgentsPerCohort(targets int) int {
	if targets <= 0 {
		return demoBaseAgentsPerCohort
	}
	capacity := len(demoCohortOffsets) * len(demoStages)
	needed := (targets + capacity - 1) / capacity
	if needed < demoBaseAgentsPerCohort {
		return demoBaseAgentsPerCohort
	}
	return needed
}

// buildDemoSchedule plans a whole run up front.
//
// Slots are generated per (cohort, agent, stage), sorted by submission time, and
// only then assigned targets round-robin. Assigning in time order is what gives
// "touch every target before revisiting one" for free: the first pass through
// the ring covers them all.
//
// The schedule is open-loop. Offsets are absolute positions in the window and no
// slot waits on an earlier request finishing, which is the point — overlapping
// requests are what put more than one job in flight at once.
func buildDemoSchedule(targets []demoTarget) []demoRequest {
	if len(targets) == 0 {
		return nil
	}

	perCohort := demoAgentsPerCohort(len(targets))
	type slot struct {
		at    time.Duration
		stage int
	}
	slots := make([]slot, 0, len(demoCohortOffsets)*perCohort*len(demoStages))
	for _, cohort := range demoCohortOffsets {
		for agent := 0; agent < perCohort; agent++ {
			for i, stage := range demoStages {
				at := cohort + stage.offset
				// The ceiling is a planning invariant as well as a runtime one.
				if at >= demoMaxSubmit {
					continue
				}
				slots = append(slots, slot{at: at, stage: i})
			}
		}
	}

	// A stable sort keeps equal-time slots in generation order, so the
	// round-robin assignment below is reproducible for a given target list —
	// which is what makes the schedule testable at all.
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].at < slots[j].at })

	out := make([]demoRequest, len(slots))
	for i, s := range slots {
		out[i] = demoRequest{
			at:     s.at,
			target: targets[i%len(targets)],
			stage:  s.stage,
		}
	}
	return out
}
