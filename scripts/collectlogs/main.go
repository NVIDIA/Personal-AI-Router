// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command collectlogs reads Personal AI Router logs and replaces the identifiers
// in them, so a log can be handed over without carrying host, account or address
// details.
//
// The source logs are only ever opened for reading. Output goes to a separate
// directory, so a node's log keeps accumulating and can be collected again later.
//
// One input produces one output, in the format it arrived in. Logs from two nodes
// give two files, each named after its anonymized producer. Tokens are allocated
// once across every input, so the same machine reads as the same node in all of
// them — which is what makes logs from different nodes comparable.
//
// Doing this after logging rather than while logging keeps the on-disk log usable
// for local debugging, keeps one implementation instead of one per process, and
// lets a single run see every record at once. That whole-corpus view is what
// allows a machine's hostname, UUID and address to be recognised as one node, and
// what allows the result to be verified.
//
// Usage:
//
//	collectlogs [flags]
//
// With no -in flag the local log directory for the current user is used.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	exitOK      = 0
	exitLeak    = 1
	exitFailure = 2
	legendOut   = "legend.md"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type options struct {
	inputs   stringList
	out      string
	mapPath  string
	raw      bool
	models   bool
	noDedupe bool
	window   time.Duration
	quiet    bool
}

func main() {
	os.Exit(run())
}

func run() int {
	var opts options

	// The wrappers build to a temporary path, so report a stable name instead of
	// os.Args[0].
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintln(out, "Read Personal AI Router logs and replace the identifiers in them.")
		fmt.Fprintln(out, "\nUsage:\n  collect-logs [flags]")
		fmt.Fprintln(out, "\nWith no flags the local log directory for the current user is read.")
		fmt.Fprintln(out, "Source logs are never modified.")
		fmt.Fprintln(out, "\nEach -in produces one output file, named after its anonymized node")
		fmt.Fprintln(out, "and in the same format as the input. A "+legendOut+" describes the tokens.")
		fmt.Fprintln(out, "\nFlags:")
		flag.PrintDefaults()
	}

	flag.Var(&opts.inputs, "in", "log file, log directory, or exported bundle; one output per flag (repeatable; default: local log directory)")
	flag.StringVar(&opts.out, "out", "", "output directory (default: ./pair-logs-<timestamp>)")
	flag.StringVar(&opts.mapPath, "map", "", "also write the token reversal map to this path; keep it local, never include it with the logs")
	flag.BoolVar(&opts.raw, "raw", false, "copy through without replacing anything, for local debugging only")
	flag.BoolVar(&opts.models, "models", false, "also replace model names; off by default because model identity is usually needed to read a routing problem")
	flag.BoolVar(&opts.noDedupe, "no-dedupe", false, "keep every copy of a repeated subprocess line")
	flag.DurationVar(&opts.window, "dedupe-window", 250*time.Millisecond, "window for treating identical messages as one event")
	flag.BoolVar(&opts.quiet, "quiet", false, "only report errors")
	flag.Parse()

	if opts.out == "" {
		opts.out = "pair-logs-" + time.Now().UTC().Format("20060102-150405")
	}

	groups, err := resolveGroups(opts.inputs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collectlogs:", err)
		return exitFailure
	}

	if err := os.MkdirAll(opts.out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "collectlogs:", err)
		return exitFailure
	}

	logf := func(format string, args ...any) {
		if !opts.quiet {
			fmt.Printf(format+"\n", args...)
		}
	}

	logf("Reading %d source(s):", len(groups))
	for _, g := range groups {
		logf("  %s (%d file(s))", g.source, len(g.files))
	}

	if opts.raw {
		return runRaw(opts, groups, logf)
	}
	return runSanitized(opts, groups, logf)
}

// runRaw copies through without replacing anything. The file name says raw so
// that one produced this way is not mistaken for one that is safe to hand over.
func runRaw(opts options, groups []inputGroup, logf func(string, ...any)) int {
	for i, g := range groups {
		name := fmt.Sprintf("raw-%d.jsonl", i+1)
		path := filepath.Join(opts.out, name)
		out, err := newSink(path, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "collectlogs:", err)
			return exitFailure
		}

		dedupe := newDeduper(opts.window)
		for _, file := range g.files {
			if err := scanFile(file, visitor{
				onRecord: func(rec Record, _ string) error {
					if !opts.noDedupe && dedupe.duplicate(rec) {
						return nil
					}
					return out.record(rec)
				},
			}); err != nil {
				out.close()
				fmt.Fprintln(os.Stderr, "collectlogs:", err)
				return exitFailure
			}
		}
		if err := out.close(); err != nil {
			fmt.Fprintln(os.Stderr, "collectlogs:", err)
			return exitFailure
		}
		logf("Wrote %s (%d records, %d duplicate copies collapsed)", path, out.count(), dedupe.dropped)
	}

	logf("")
	logf("These files are NOT sanitized. Do not hand them over.")
	return exitOK
}

func runSanitized(opts options, groups []inputGroup, logf func(string, ...any)) int {
	// Pass one: learn what is in every input before changing any of it. Tokens
	// must be allocated across the whole set, or the same machine would get a
	// different name in each file.
	d := newDiscovery(opts.models)
	skipped := 0
	for i := range groups {
		d.beginSource()
		for _, file := range groups[i].files {
			if err := scanFile(file, visitor{
				onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
				onSection: func(name, _ string, blob any) error {
					groups[i].bundle = true
					if blob != nil {
						d.scanSection(name, blob)
					}
					return nil
				},
				onSkip: func(string) { skipped++ },
			}); err != nil {
				fmt.Fprintln(os.Stderr, "collectlogs:", err)
				return exitFailure
			}
		}
		groups[i].node = d.sourceNode()
	}

	// An input that yielded nothing recognisable is a mistake worth stopping for,
	// rather than writing an empty file that looks like a sanitized log.
	if d.records == 0 {
		if skipped > 0 {
			fmt.Fprintf(os.Stderr,
				"collectlogs: no log records found, and %d line(s) were not recognised.\n"+
					"       Expected %s, %s, or an exported nvpair-logs-*.txt bundle.\n",
				skipped, activeLogName, rotatedLogName)
			return exitFailure
		}
		fmt.Fprintln(os.Stderr, "collectlogs: the input contains no log records; nothing to do.")
		return exitFailure
	}

	d.pruneEmptyNodes()
	d.allocate()
	d.checkConfidence()
	entities := d.sortedEntities()

	logf("")
	logf("Scanned %d records; found %d identifier(s) across %d node(s).",
		d.records, len(entities), len(d.nodes))
	if skipped > 0 {
		// Dropped lines are never sanitized and never written, so this is a
		// completeness question rather than a disclosure one. A truncated final
		// line is normal; a large count means the wrong file was read.
		d.warnings = append(d.warnings, fmt.Sprintf(
			"%d line(s) were not log records and were dropped, not sanitized", skipped))
	}

	// Occurrence counts are recounted during the rewrite so the legend reports
	// what remains in the output rather than what discovery happened to visit.
	for _, e := range entities {
		e.Count = 0
	}
	rw := newRewriter(entities)

	// Pass two: one output file per input.
	written := make([]string, 0, len(groups))
	usedNames := map[string]int{}
	for i, g := range groups {
		path := filepath.Join(opts.out, uniqueOutputName(g, i, usedNames))
		out, err := newSink(path, g.bundle)
		if err != nil {
			fmt.Fprintln(os.Stderr, "collectlogs:", err)
			return exitFailure
		}

		dedupe := newDeduper(opts.window)
		for _, file := range g.files {
			if err := scanFile(file, visitor{
				onRecord: func(rec Record, _ string) error {
					if !opts.noDedupe && dedupe.duplicate(rec) {
						return nil
					}
					return out.record(rw.record(rec))
				},
				onSection: func(name, raw string, _ any) error {
					// Sections are written back as text rather than
					// re-serialized, which keeps their original key order and
					// formatting. No identifier kind contains a backslash or a
					// quote, so replacing in the text is equivalent here.
					return out.section(name, rw.string(raw))
				},
			}); err != nil {
				out.close()
				fmt.Fprintln(os.Stderr, "collectlogs:", err)
				return exitFailure
			}
		}
		if err := out.close(); err != nil {
			fmt.Fprintln(os.Stderr, "collectlogs:", err)
			return exitFailure
		}

		written = append(written, path)
		logf("Wrote %s (%d records, %d duplicate copies collapsed)", path, out.count(), dedupe.dropped)
	}

	legendPath := filepath.Join(opts.out, legendOut)
	if err := os.WriteFile(legendPath, []byte(legend(entities, d.nodes, d.warnings)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "collectlogs:", err)
		return exitFailure
	}
	logf("Wrote %s", legendPath)

	// The reversal map is opt-in and written where the caller asks, so it cannot
	// be swept up with the logs by a later archive step.
	if opts.mapPath != "" {
		if err := writeTokenMap(opts.mapPath, entities); err != nil {
			fmt.Fprintln(os.Stderr, "collectlogs:", err)
			return exitFailure
		}
		logf("Wrote %s — reverses the tokens, keep it local", opts.mapPath)
	}

	// Pass three: confirm every artifact.
	v := newVerifier(entities)
	for _, path := range written {
		if err := v.checkFile(path); err != nil {
			fmt.Fprintln(os.Stderr, "collectlogs:", err)
			return exitFailure
		}
	}
	if !v.ok() {
		fmt.Fprintf(os.Stderr, "\ncollectlogs: verification failed, %d finding(s):\n", len(v.findings))
		for _, finding := range v.findings {
			fmt.Fprintln(os.Stderr, "  "+finding)
		}
		fmt.Fprintln(os.Stderr, "\nDo not hand these over. Report the findings so detection can be fixed,")
		fmt.Fprintln(os.Stderr, "then run again — the source logs are unchanged.")
		return exitLeak
	}

	logf("Verification passed: no learned value or unexpected identifier remains.")
	for _, warning := range d.warnings {
		fmt.Fprintln(os.Stderr, "collectlogs: warning: "+warning)
	}
	return exitOK
}

// writeTokenMap records the reversal table. It is the counterpart to replacing
// identifiers on the way out: whoever owns the machine keeps the ability to read
// their own logs, while what they hand over carries none of it.
func writeTokenMap(path string, entities []*Entity) error {
	payload := struct {
		Warning     string    `json:"warning"`
		GeneratedAt time.Time `json:"generatedAt"`
		Entities    []*Entity `json:"entities"`
	}{
		Warning:     "Reverses the tokens in the sanitized logs. Keep local; do not hand over.",
		GeneratedAt: time.Now().UTC(),
		Entities:    entities,
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0o600)
}
