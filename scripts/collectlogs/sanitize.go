// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rewriter replaces every learned identifier with its token.
//
// Replacement happens on decoded string values, never on raw file text. The same
// value appears at different escape depths in the source — a path logged by a Go
// service is quoted by the Go logger and then encoded again by the desktop
// logger — so matching raw text would need a separate pattern per depth. After
// decoding there is one form to match, and re-marshalling restores whatever
// escaping the output position requires.
type rewriter struct {
	re     *regexp.Regexp
	exact  map[string]*Entity
	folded map[string]*Entity
	// paths is keyed by a separator- and case-normalized form, because a matched
	// path may differ from the learned value in separator style and depth.
	paths    map[string]*Entity
	replaced int
}

func newRewriter(entities []*Entity) *rewriter {
	r := &rewriter{
		exact:  map[string]*Entity{},
		folded: map[string]*Entity{},
		paths:  map[string]*Entity{},
	}

	// Longest first: Go's regexp prefers the alternative a backtracking search
	// would reach first, so ordering by descending length makes the longest
	// candidate win at any position. That is what stops 192.168.1.1 from
	// clipping 192.168.1.17, and a username from being replaced inside a
	// hostname that contains it.
	ordered := make([]*Entity, 0, len(entities))
	for _, e := range entities {
		if e.Token == "" || e.Token == e.Value {
			continue
		}
		ordered = append(ordered, e)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if len(ordered[i].Value) != len(ordered[j].Value) {
			return len(ordered[i].Value) > len(ordered[j].Value)
		}
		return ordered[i].Value < ordered[j].Value
	})

	alts := make([]string, 0, len(ordered))
	for _, e := range ordered {
		r.exact[e.Value] = e
		if e.Kind.caseInsensitive() {
			r.folded[strings.ToLower(e.Value)] = e
		}
		if e.Kind.pathLike() {
			r.paths[normalizePath(e.Value)] = e
		}
		alts = append(alts, entityPattern(e))
	}

	if len(alts) > 0 {
		r.re = regexp.MustCompile(strings.Join(alts, "|"))
	}
	return r
}

// entityPattern builds the expression that finds one entity's value.
func entityPattern(e *Entity) string {
	if e.Kind.pathLike() {
		// A path reaches the log at more than one escape depth, and separators
		// vary, so each separator run matches one or two of either kind. Matching
		// the quoted literal would find only whichever depth happened to be
		// learned first.
		segments := splitPath(e.Value)
		quoted := make([]string, 0, len(segments))
		for _, seg := range segments {
			quoted = append(quoted, regexp.QuoteMeta(seg))
		}
		return `(?i:` + strings.Join(quoted, `[\\/]{1,2}`) + `)`
	}

	pattern := regexp.QuoteMeta(e.Value)
	if e.Kind.caseInsensitive() {
		pattern = `(?i:` + pattern + `)`
	}
	// Bound the match when the value's edges are word characters, so a short
	// value cannot be replaced inside a longer identifier. Values that begin or
	// end with punctuation cannot use \b there.
	if isWordEdge(e.Value) {
		return `\b` + pattern + `\b`
	}
	return pattern
}

func isWordEdge(s string) bool {
	if s == "" {
		return false
	}
	runes := []rune(s)
	return isWordRune(runes[0]) && isWordRune(runes[len(runes)-1])
}

func isWordRune(r rune) bool {
	return r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

// normalizePath collapses separator style, depth and case so that the same
// directory written in different forms resolves to one key.
func normalizePath(value string) string {
	return strings.ToLower(strings.Join(splitPath(value), "/"))
}

func (r *rewriter) string(s string) string {
	if r.re == nil || s == "" {
		return s
	}
	return r.re.ReplaceAllStringFunc(s, func(m string) string {
		e, ok := r.exact[m]
		if !ok {
			e, ok = r.folded[strings.ToLower(m)]
		}
		if !ok {
			e, ok = r.paths[normalizePath(m)]
		}
		if !ok {
			return m
		}
		// Counting here rather than during discovery makes the legend report
		// what a reader will actually encounter in the output.
		e.Count++
		r.replaced++
		return e.Token
	})
}

func (r *rewriter) record(rec Record) Record {
	out := rec
	out.Source = r.string(rec.Source)
	out.Sublevel = r.string(rec.Sublevel)
	out.Message = r.string(rec.Message)
	if rec.Data != nil {
		out.Data = mapStrings(rec.Data, r.string)
	}
	return out
}

// deduper collapses the repeated copies of a single event.
//
// A subprocess line reaches the log file more than once: once as a verbose entry
// carrying the stream name, once as a warn entry, and again from the in-memory
// ring that an exported bundle appends. Matching on message text within a short
// window collapses those without touching a message that legitimately repeats
// seconds later.
type deduper struct {
	window  time.Duration
	lastAt  map[string]int64
	dropped int
	pruneAt int64
}

func newDeduper(window time.Duration) *deduper {
	return &deduper{window: window, lastAt: map[string]int64{}}
}

func (d *deduper) duplicate(rec Record) bool {
	if rec.Message == "" {
		return false
	}
	ms, ok := parseTimeMs(rec.Time)
	if !ok {
		return false
	}

	windowMs := d.window.Milliseconds()
	if prev, seen := d.lastAt[rec.Message]; seen {
		delta := ms - prev
		if delta < 0 {
			delta = -delta
		}
		if delta <= windowMs {
			d.dropped++
			return true
		}
	}
	d.lastAt[rec.Message] = ms

	// Keep the table bounded on long sessions.
	if ms-d.pruneAt > 30_000 {
		d.pruneAt = ms
		for k, v := range d.lastAt {
			if ms-v > 30_000 {
				delete(d.lastAt, k)
			}
		}
	}
	return false
}

func parseTimeMs(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// legend renders the token table. Cardinality is low in practice — a handful of
// nodes and addresses across a whole session — so the legend stays short enough
// to read before opening the log itself.
func legend(entities []*Entity, nodes []*Node, warnings []string) string {
	var sb strings.Builder
	sb.WriteString("# Token legend\n\n")
	sb.WriteString("Identifiers below were replaced in the sanitized log.\n")
	sb.WriteString("Tokens are assigned per collection run and are not derived from the\n")
	sb.WriteString("original values, so they cannot be reversed from this file alone.\n\n")

	if len(nodes) > 0 {
		sb.WriteString("## Nodes\n\n")
		for _, n := range nodes {
			sb.WriteString("- **" + n.Label + "**")
			parts := []string{}
			if len(n.Hostnames) > 0 {
				parts = append(parts, strconv.Itoa(len(n.Hostnames))+" hostname(s)")
			}
			if len(n.UUIDs) > 0 {
				parts = append(parts, strconv.Itoa(len(n.UUIDs))+" uuid(s)")
			}
			if len(n.IPs) > 0 {
				parts = append(parts, strconv.Itoa(len(n.IPs))+" address(es)")
			}
			if len(n.Users) > 0 {
				parts = append(parts, strconv.Itoa(len(n.Users))+" account(s)")
			}
			if len(parts) > 0 {
				sb.WriteString(" — " + strings.Join(parts, ", "))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Tokens\n\n")
	sb.WriteString("| Token | Kind | Class | Occurrences |\n")
	sb.WriteString("| --- | --- | --- | --- |\n")
	shown := append([]*Entity{}, entities...)
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].Token < shown[j].Token })
	for _, e := range shown {
		if e.Token == e.Value {
			continue
		}
		class := e.Class
		if class == "" {
			class = "-"
		}
		sb.WriteString("| `" + e.Token + "` | " + e.Kind.String() + " | " + class + " | " +
			strconv.Itoa(e.Count) + " |\n")
	}

	kept := []*Entity{}
	for _, e := range entities {
		if e.Token == e.Value {
			kept = append(kept, e)
		}
	}
	if len(kept) > 0 {
		sb.WriteString("\n## Left in the clear\n\n")
		sb.WriteString("Readable on purpose. Loopback and link-local addresses describe no\n")
		sb.WriteString("particular machine. Node and cluster UUIDs are random version 4 values\n")
		sb.WriteString("that say nothing about a person or a network, and they are the primary\n")
		sb.WriteString("key in most payloads, so replacing them would cost far more in\n")
		sb.WriteString("readability than it gains.\n\n")
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].Value < kept[j].Value })
		for _, e := range kept {
			sb.WriteString("- `" + e.Value + "` (" + e.Class + ")\n")
		}
	}

	if len(warnings) > 0 {
		sb.WriteString("\n## Warnings\n\n")
		for _, w := range warnings {
			sb.WriteString("- " + w + "\n")
		}
	}
	return sb.String()
}
