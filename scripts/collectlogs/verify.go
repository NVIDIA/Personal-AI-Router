// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The verification pass re-reads the finished artifact and fails if anything
// sensitive survived. Sanitizing after the fact is the only arrangement that can
// check its own work over the whole corpus: a redactor running at emit time sees
// one line at a time and never gets to confirm the result.
//
// Two independent questions are asked of the output.
//
// Did any value we learned survive verbatim? That is asked of the raw artifact
// text, with no interpretation. None of the identifier kinds handled here contain
// a backslash or a quote, so they read the same encoded as decoded, and this
// check sees exactly what a reader of the file would see.
//
// Does any identifier shape remain that is neither a token nor deliberately
// readable? That is asked of the parsed records, because the shape tests need the
// same key-path context discovery had. Without it, a four-part version string
// would be reported as an address that escaped replacement.
type verifier struct {
	forbidden *regexp.Regexp
	allowed   map[string]bool
	findings  []string
	seen      map[string]bool
}

func newVerifier(entities []*Entity) *verifier {
	v := &verifier{
		allowed: map[string]bool{},
		seen:    map[string]bool{},
	}

	var alts []string
	for _, e := range entities {
		if e.Token == e.Value {
			// Deliberately readable — loopback addresses and the random node and
			// cluster UUIDs. Recorded so the shape scan does not report them.
			v.allowed[e.Value] = true
			continue
		}
		v.allowed[e.Token] = true
		alts = append(alts, entityPattern(e))
	}
	sort.SliceStable(alts, func(i, j int) bool { return len(alts[i]) > len(alts[j]) })
	if len(alts) > 0 {
		v.forbidden = regexp.MustCompile(strings.Join(alts, "|"))
	}
	return v
}

func (v *verifier) report(where, kind, value string) {
	key := kind + "\x00" + value
	if v.seen[key] {
		return
	}
	v.seen[key] = true
	v.findings = append(v.findings,
		fmt.Sprintf("%s: %s %q survived sanitization", where, kind, value))
}

// checkFile verifies one finished artifact, in either output format.
//
// The same reader the inputs went through is used, so records and sections are
// separated the same way and each is checked with the context its own rules need.
func (v *verifier) checkFile(path string) error {
	name := filepath.Base(path)
	records := 0

	return scanFile(path, visitor{
		onRecord: func(rec Record, raw string) error {
			records++
			where := fmt.Sprintf("%s line %d", name, records)

			// Context-free: did any learned value survive anywhere in the line?
			v.checkLiterals(where, raw)

			// Context-aware: the shape tests need the key path discovery had, or
			// a four-part version string reads as an escaped address.
			v.checkStrings(where, "", rec.Message)
			v.checkStrings(where, "", rec.Source)
			v.checkStrings(where, "", rec.Sublevel)
			if rec.Data != nil {
				walkStrings(rec.Data, "", func(p, s string, _ bool) {
					v.checkStrings(where, p, s)
				})
			}
			return nil
		},
		onSection: func(sectionName, raw string, blob any) error {
			where := fmt.Sprintf("%s section %q", name, sectionName)
			v.checkLiterals(where, raw)
			if blob != nil {
				walkStrings(blob, "", func(p, s string, _ bool) {
					v.checkStrings(where, p, s)
				})
				return nil
			}
			v.checkStrings(where, "", raw)
			return nil
		},
	})
}

func (v *verifier) checkLiterals(where, text string) {
	if v.forbidden == nil {
		return
	}
	for _, m := range v.forbidden.FindAllString(text, -1) {
		v.report(where, "learned value", m)
	}
}

func (v *verifier) checkStrings(where, path, s string) {
	if s == "" {
		return
	}

	// Mirror the candidate rules discovery used so verification does not report
	// as a leak something detection deliberately did not treat as an identifier.
	claimed := claimAmbiguousSpans(s)
	overlaps := func(m []int) bool {
		for _, c := range claimed {
			if m[0] < c.hi && c.lo < m[1] {
				return true
			}
		}
		return false
	}

	for _, m := range reUUID.FindAllStringIndex(s, -1) {
		val := s[m[0]:m[1]]
		if overlaps(m) || v.allowed[val] {
			continue
		}
		v.report(where, "uuid", val)
	}
	for _, m := range reMAC.FindAllStringIndex(s, -1) {
		val := s[m[0]:m[1]]
		if overlaps(m) || v.allowed[val] {
			continue
		}
		v.report(where, "mac", val)
	}
	if !versionContext(path) {
		for _, m := range reIPv4.FindAllStringIndex(s, -1) {
			val := s[m[0]:m[1]]
			if overlaps(m) || net.ParseIP(val) == nil || v.allowed[val] {
				continue
			}
			v.report(where, "ipv4", val)
		}
		for _, m := range reIPv6.FindAllStringIndex(s, -1) {
			val := s[m[0]:m[1]]
			if overlaps(m) || strings.Count(val, ":") < 2 || net.ParseIP(val) == nil || v.allowed[val] {
				continue
			}
			v.report(where, "ipv6", val)
		}
	}
	for _, m := range reUserPath.FindAllStringSubmatch(s, -1) {
		if len(m) < 2 {
			continue
		}
		if v.allowed[m[1]] || !isUsername(m[1]) {
			continue
		}
		v.report(where, "username", m[1])
	}
}

func (v *verifier) ok() bool { return len(v.findings) == 0 }
