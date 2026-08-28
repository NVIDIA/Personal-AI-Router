// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixture = "testdata/sample-bundle.txt"

type pipelineResult struct {
	entities []*Entity
	nodes    []*Node
	output   string
	sections map[string]string
	dropped  int
	path     string
	bundle   bool
}

// runPipeline mirrors the production flow for a single input: discover, allocate,
// rewrite into one output file in the format the input arrived in.
func runPipeline(t *testing.T, path string, dedupe bool) pipelineResult {
	return runPipelineOpts(t, path, dedupe, false)
}

func runPipelineOpts(t *testing.T, path string, dedupe, models bool) pipelineResult {
	t.Helper()

	isBundle := false
	d := newDiscovery(models)
	if err := scanFile(path, visitor{
		onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
		onSection: func(name, _ string, blob any) error {
			isBundle = true
			if blob != nil {
				d.scanSection(name, blob)
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("discovery pass: %v", err)
	}
	d.pruneEmptyNodes()
	d.allocate()
	entities := d.sortedEntities()
	for _, e := range entities {
		e.Count = 0
	}

	rw := newRewriter(entities)
	dd := newDeduper(250 * time.Millisecond)
	sections := map[string]string{}

	ext := "jsonl"
	if isBundle {
		ext = "txt"
	}
	outPath := filepath.Join(t.TempDir(), "node-out."+ext)
	out, err := newSink(outPath, isBundle)
	if err != nil {
		t.Fatalf("create sink: %v", err)
	}

	if err := scanFile(path, visitor{
		onRecord: func(rec Record, _ string) error {
			if dedupe && dd.duplicate(rec) {
				return nil
			}
			return out.record(rw.record(rec))
		},
		onSection: func(name, raw string, _ any) error {
			clean := rw.string(raw)
			sections[name] = clean
			return out.section(name, clean)
		},
	}); err != nil {
		t.Fatalf("rewrite pass: %v", err)
	}
	if err := out.close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	return pipelineResult{
		entities: entities,
		nodes:    d.nodes,
		output:   string(body),
		sections: sections,
		dropped:  dd.dropped,
		path:     outPath,
		bundle:   isBundle,
	}
}

// sectionJSON decodes a preserved section so its fields can be inspected.
func sectionJSON(t *testing.T, res pipelineResult, name string) map[string]any {
	t.Helper()
	raw, ok := res.sections[name]
	if !ok {
		t.Fatalf("section %q missing from output", name)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("section %q is not valid JSON after sanitizing: %v", name, err)
	}
	return out
}

func findEntity(entities []*Entity, value string) *Entity {
	for _, e := range entities {
		if e.Value == value {
			return e
		}
	}
	return nil
}

func TestDiscoveryLinksNodeIdentifiers(t *testing.T) {
	res := runPipeline(t, fixture, true)

	if len(res.nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(res.nodes))
	}

	// The bundle's own machine is linked from the header sections and is
	// labelled first.
	hostA := findEntity(res.entities, "TESTHOST-A")
	uuidA := findEntity(res.entities, "11111111-2222-4333-8444-555555555555")
	ipA := findEntity(res.entities, "192.168.50.10")
	for name, e := range map[string]*Entity{"hostname": hostA, "uuid": uuidA, "address": ipA} {
		if e == nil {
			t.Fatalf("node A %s was not discovered", name)
		}
	}
	if hostA.Node != uuidA.Node || hostA.Node != ipA.Node {
		t.Errorf("node A identifiers not linked: hostname=%q uuid=%q ip=%q",
			hostA.Node, uuidA.Node, ipA.Node)
	}
	if hostA.Token != "node-a" {
		t.Errorf("want hostname token node-a, got %q", hostA.Token)
	}
	if ipA.Token != "node-a-ip" {
		t.Errorf("want address token node-a-ip, got %q", ipA.Token)
	}

	// The UUID is still linked to the node, but left readable: it is a random
	// version 4 value that identifies nobody and is the primary key in most
	// payloads.
	if uuidA.Token != uuidA.Value {
		t.Errorf("uuid should stay readable, got token %q", uuidA.Token)
	}

	// The account in a machine's own file paths belongs to that machine.
	userA := findEntity(res.entities, "testuser")
	if userA == nil {
		t.Fatal("account name was not discovered")
	}
	if userA.Token != "node-a-user" {
		t.Errorf("want account token node-a-user, got %q", userA.Token)
	}
	if userA.Node != "node-a" {
		t.Errorf("account attributed to %q, want node-a", userA.Node)
	}
}

// Four-part version strings parse as valid addresses. Treating one as an address
// would replace it and make the log misleading.
func TestVersionStringIsNotTreatedAsAddress(t *testing.T) {
	res := runPipeline(t, fixture, true)

	for _, version := range []string{"14.8.178.33", "1.3.2.1"} {
		if e := findEntity(res.entities, version); e != nil {
			t.Errorf("version %q was treated as an address (token %q)", version, e.Token)
		}
	}

	meta := sectionJSON(t, res, "Metadata")
	versions, ok := meta["processVersions"].(map[string]any)
	if !ok {
		t.Fatal("processVersions missing from the preserved header")
	}
	if got := versions["v8"]; got != "14.8.178.33-electron.0" {
		t.Errorf("v8 version altered: %v", got)
	}
	if got := versions["zlib"]; got != "1.3.2.1-motley" {
		t.Errorf("zlib version altered: %v", got)
	}
}

// Some payloads are keyed by an identifier, so a walker that only visited values
// would leave those in place. nodesInitial is keyed by node UUID, which is now
// deliberately readable, so this exercises the traversal with a value that is
// still replaced.
func TestObjectKeysAreSanitized(t *testing.T) {
	src := `{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"a",` +
		`"message":"state","data":{"byHost":{"TESTHOST-A":{"ok":true}},` +
		`"id":"11111111-2222-4333-8444-555555555555","ipAddress":"192.168.50.10",` +
		`"name":"TESTHOST-A"}}` + "\n"
	path := filepath.Join(t.TempDir(), "keys.jsonl")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runPipeline(t, path, true)

	if strings.Contains(res.output, "TESTHOST-A") {
		t.Error("a host name used as an object key was not replaced")
	}
	if !strings.Contains(res.output, `"node-a"`) {
		t.Errorf("expected the host name key to become a token, got %s", res.output)
	}
}

// Node and cluster UUIDs are left readable on purpose.
func TestUUIDsAreLeftReadable(t *testing.T) {
	res := runPipeline(t, fixture, true)

	state := sectionJSON(t, res, "Current Modular State")
	nodes, ok := state["nodesInitial"].(map[string]any)
	if !ok {
		t.Fatal("nodesInitial missing")
	}
	found := 0
	for key := range nodes {
		if reUUID.MatchString(key) {
			found++
		}
	}
	if found != 2 {
		t.Errorf("want 2 readable UUID keys, got %d of %v", found, keysOf(nodes))
	}

	for _, e := range res.entities {
		if e.Kind != KindUUID {
			continue
		}
		if e.Token != e.Value {
			t.Errorf("uuid %q was replaced with %q", e.Value, e.Token)
		}
		if e.Class != "random" {
			t.Errorf("uuid %q class = %q, want random", e.Value, e.Class)
		}
	}
}

// The desktop logger encodes a message that the Go logger already quoted, so a
// path arrives double-escaped. Replacing on decoded values and re-marshalling
// must preserve the original escape depth.
func TestNestedEscapingRoundTrip(t *testing.T) {
	res := runPipeline(t, fixture, true)

	if strings.Contains(res.output, "testuser") {
		t.Error("username survived in the output")
	}
	if !strings.Contains(res.output, `C:\\\\Users\\\\node-a-user\\\\AppData`) {
		t.Error("double-escaped Windows path was not preserved at its original escape depth")
	}
	if !strings.Contains(res.output, `path=\"C:`) {
		t.Error("inner Go quoting was not preserved")
	}
}

func TestLoopbackLeftInTheClear(t *testing.T) {
	res := runPipeline(t, fixture, true)

	e := findEntity(res.entities, "127.0.0.1")
	if e == nil {
		t.Fatal("loopback address was not observed")
	}
	if e.Token != "127.0.0.1" {
		t.Errorf("loopback should stay readable, got token %q", e.Token)
	}
	if !strings.Contains(res.output, `"addr":"127.0.0.1"`) {
		t.Error("loopback address missing from output")
	}
}

// A certificate fingerprint is a long run of colon-separated hex pairs whose
// first six pairs look exactly like a MAC address.
func TestFingerprintIsNotPartiallyMatchedAsMAC(t *testing.T) {
	res := runPipeline(t, fixture, true)

	const fingerprint = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	if !strings.Contains(res.output, fingerprint) {
		t.Error("fingerprint was altered; its MAC-shaped prefix must not be replaced")
	}

	mac := findEntity(res.entities, "aa:bb:cc:dd:ee:ff")
	if mac == nil {
		t.Fatal("a genuine six-pair MAC address was not discovered")
	}
	if mac.Kind != KindMAC {
		t.Errorf("want KindMAC, got %v", mac.Kind)
	}
	if strings.Contains(res.output, `"mac":"aa:bb:cc:dd:ee:ff"`) {
		t.Error("MAC address was not replaced")
	}
}

func TestDedupeCollapsesCopiesButKeepsRealRepeats(t *testing.T) {
	deduped := runPipeline(t, fixture, true)
	if deduped.dropped != 1 {
		t.Errorf("want 1 duplicate copy collapsed, got %d", deduped.dropped)
	}
	if got := strings.Count(deduped.output, "settings loaded"); got != 1 {
		t.Errorf("want the duplicated line once, got %d copies", got)
	}
	// Two identical messages two seconds apart are separate events.
	if got := strings.Count(deduped.output, "polling node"); got != 2 {
		t.Errorf("want both repeated polls kept, got %d", got)
	}

	raw := runPipeline(t, fixture, false)
	if raw.dropped != 0 {
		t.Errorf("dedupe disabled should drop nothing, dropped %d", raw.dropped)
	}
	if got := strings.Count(raw.output, "settings loaded"); got != 2 {
		t.Errorf("want both copies without dedupe, got %d", got)
	}
}

func TestVerificationPassesOnSanitizedOutput(t *testing.T) {
	res := runPipeline(t, fixture, true)

	v := newVerifier(res.entities)
	if err := v.checkFile(res.path); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.ok() {
		t.Fatalf("verification reported findings on clean output: %v", v.findings)
	}

	for _, secret := range []string{
		"testuser", "TESTHOST-A", "TESTHOST-B",
		"192.168.50.10", "192.168.50.11",
	} {
		if strings.Contains(res.output, secret) {
			t.Errorf("%q survived in the sanitized records", secret)
		}
	}
}

// The verifier must fail when a learned value is present, otherwise it offers no
// guarantee.
func TestVerificationDetectsALeak(t *testing.T) {
	entities := []*Entity{
		{Kind: KindHostname, Value: "TESTHOST-A", Token: "node-a"},
		{Kind: KindIPv4, Value: "192.168.50.10", Token: "node-a-ip", Class: "lan"},
	}
	leaky := `{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"x","message":"talking to TESTHOST-A at 192.168.50.10"}` + "\n"

	path := filepath.Join(t.TempDir(), "leaky.jsonl")
	if err := os.WriteFile(path, []byte(leaky), 0o600); err != nil {
		t.Fatal(err)
	}

	v := newVerifier(entities)
	if err := v.checkFile(path); err != nil {
		t.Fatal(err)
	}
	if v.ok() {
		t.Fatal("verifier accepted output containing learned values")
	}
	if len(v.findings) < 2 {
		t.Errorf("want a finding per leaked value, got %v", v.findings)
	}
}

func TestIsHostnameRejectsNonHosts(t *testing.T) {
	for _, s := range []string{
		"qwen3.6:27b",                          // model tag
		"engine:status",                        // JSON-RPC method
		"ollama",                               // engine name
		"lmstudio",                             // engine name
		"127.0.0.1",                            // address
		"11111111-2222-4333-8444-555555555555", // uuid
		"C:\\Users\\bob",                       // path
		"a b",                                  // free text
		"",                                     // empty
	} {
		if isHostname(s) {
			t.Errorf("isHostname(%q) = true, want false", s)
		}
	}
	for _, s := range []string{"TESTHOST-A", "SethWork2", "DESKTOP-N1D9NDS", "node-01.lan"} {
		if !isHostname(s) {
			t.Errorf("isHostname(%q) = false, want true", s)
		}
	}
}

func TestClassifyIP(t *testing.T) {
	cases := map[string]struct {
		class string
		keep  bool
	}{
		"127.0.0.1":     {"loopback", true},
		"::1":           {"loopback", true},
		"0.0.0.0":       {"loopback", true},
		"192.168.1.10":  {"lan", false},
		"10.221.6.52":   {"lan", false},
		"172.16.4.4":    {"lan", false},
		"100.64.0.1":    {"cgnat", false},
		"8.8.8.8":       {"public", false},
		"169.254.10.10": {"link-local", true},
	}
	for input, want := range cases {
		class, keep := classifyIP(input)
		if class != want.class || keep != want.keep {
			t.Errorf("classifyIP(%q) = (%q, %v), want (%q, %v)",
				input, class, keep, want.class, want.keep)
		}
	}
}

// Tokens must be identical across runs on the same input, otherwise two
// collections of the same logs cannot be compared.
func TestTokenAssignmentIsDeterministic(t *testing.T) {
	first := runPipeline(t, fixture, true)
	second := runPipeline(t, fixture, true)

	if first.output != second.output {
		t.Error("two runs over the same input produced different output")
	}
	for _, e := range first.entities {
		other := findEntity(second.entities, e.Value)
		if other == nil {
			t.Fatalf("%q missing from the second run", e.Value)
		}
		if other.Token != e.Token {
			t.Errorf("%q: token %q then %q", e.Value, e.Token, other.Token)
		}
	}
}

// Hostnames are only trusted from structured fields. An input without them must
// say so, because verification passing does not mean nothing was missed.
func TestFragmentInputWarnsAboutUndetectedHostnames(t *testing.T) {
	fragment := `{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"x","message":"host WORKSTATION-9 at 192.168.77.5"}` + "\n"
	path := filepath.Join(t.TempDir(), "fragment.jsonl")
	if err := os.WriteFile(path, []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}

	d := newDiscovery(false)
	if err := scanFile(path, visitor{
		onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	d.pruneEmptyNodes()
	d.allocate()
	d.checkConfidence()

	// The address is still found by shape.
	if e := findEntity(d.sortedEntities(), "192.168.77.5"); e == nil {
		t.Error("address in free text was not detected")
	}

	found := false
	for _, w := range d.warnings {
		if strings.Contains(w, "no hostname was learned") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a warning about undetected hostnames, got %v", d.warnings)
	}
}

// A full bundle has the structured fields, so it must not carry that warning.
func TestFullBundleDoesNotWarnAboutHostnames(t *testing.T) {
	d := newDiscovery(false)
	if err := scanFile(fixture, visitor{
		onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
		onSection: func(name, _ string, blob any) error {
			if blob != nil {
				d.scanSection(name, blob)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	d.pruneEmptyNodes()
	d.allocate()
	d.checkConfidence()

	for _, w := range d.warnings {
		if strings.Contains(w, "no hostname was learned") {
			t.Errorf("full bundle should not warn about hostnames: %v", d.warnings)
		}
	}
}

// An exported bundle must come back as a bundle, with its header intact and a
// single record section — not split into separate files.
func TestBundleFormatIsPreservedAsOneFile(t *testing.T) {
	res := runPipeline(t, fixture, true)

	if !res.bundle {
		t.Fatal("fixture is an exported bundle but was not detected as one")
	}
	if !strings.HasPrefix(res.output, "# Personal AI Router Logs (sanitized)") {
		t.Error("bundle header missing from output")
	}
	for _, name := range []string{"Metadata", "Current Modular State"} {
		if !strings.Contains(res.output, "## "+name+"\n") {
			t.Errorf("section %q missing from output", name)
		}
	}
	if got := strings.Count(res.output, "## "+recordSectionTitle); got != 1 {
		t.Errorf("want exactly one record section, got %d", got)
	}

	// Sections are written back as text, so they must still parse.
	meta := sectionJSON(t, res, "Metadata")
	if meta["appVersion"] != "0.0.60-dev" {
		t.Errorf("appVersion altered: %v", meta["appVersion"])
	}
	if meta["hostname"] != "node-a" {
		t.Errorf("hostname should be tokenized in the header, got %v", meta["hostname"])
	}
}

// A raw nvpair.jsonl has no header, so it comes back as plain JSONL.
func TestRawJSONLInputStaysJSONL(t *testing.T) {
	src := `{"level":"info","time":"2026-07-21T22:11:31.000Z","source":"x","sublevel":"y","message":"listening","data":{"addr":"127.0.0.1"}}` + "\n"
	path := filepath.Join(t.TempDir(), "nvpair.jsonl")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runPipeline(t, path, true)
	if res.bundle {
		t.Fatal("plain JSONL was treated as a bundle")
	}
	if strings.Contains(res.output, "##") || strings.HasPrefix(res.output, "#") {
		t.Error("markdown sections were added to a plain JSONL output")
	}
	if !strings.HasPrefix(res.output, "{") {
		t.Error("output is not plain JSONL")
	}
}

// Each bundle was produced by a different machine. Reading several must not
// collapse their producers into one node, and an identifier seen in both must
// resolve to the same token.
func TestSeveralBundlesKeepProducersDistinct(t *testing.T) {
	const fixtureB = "testdata/sample-bundle-b.txt"

	d := newDiscovery(false)
	producers := map[string]*Node{}
	for _, path := range []string{fixture, fixtureB} {
		d.beginSource()
		if err := scanFile(path, visitor{
			onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
			onSection: func(name, _ string, blob any) error {
				if blob != nil {
					d.scanSection(name, blob)
				}
				return nil
			},
		}); err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		producers[path] = d.sourceNode()
	}
	d.pruneEmptyNodes()
	d.allocate()
	entities := d.sortedEntities()

	if len(d.nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(d.nodes))
	}
	for _, n := range d.nodes {
		if len(n.Hostnames) != 1 {
			t.Errorf("%s has %d hostnames, want 1 — producers were merged",
				n.Label, len(n.Hostnames))
		}
		if len(n.UUIDs) != 1 {
			t.Errorf("%s has %d uuids, want 1", n.Label, len(n.UUIDs))
		}
	}

	// TESTHOST-B is the producer of the second bundle and a discovered peer in
	// the first; both views must land on one node.
	hostB := findEntity(entities, "TESTHOST-B")
	uuidB := findEntity(entities, "66666666-7777-4888-8999-aaaaaaaaaaaa")
	ipB := findEntity(entities, "192.168.50.11")
	if hostB == nil || uuidB == nil || ipB == nil {
		t.Fatal("node B identifiers were not all discovered")
	}
	if hostB.Node != uuidB.Node || hostB.Node != ipB.Node {
		t.Errorf("node B identifiers split across groups: %q %q %q",
			hostB.Node, uuidB.Node, ipB.Node)
	}

	// Each account belongs to the machine whose log carried it.
	u1, u2 := findEntity(entities, "testuser"), findEntity(entities, "otheruser")
	if u1 == nil || u2 == nil {
		t.Fatal("expected both account names to be discovered")
	}
	if u1.Node == "" || u2.Node == "" {
		t.Errorf("accounts not attributed to a node: %q %q", u1.Node, u2.Node)
	}
	if u1.Node == u2.Node {
		t.Errorf("both accounts attributed to %q; they are on different machines", u1.Node)
	}
	if u1.Token != u1.Node+"-user" || u2.Token != u2.Node+"-user" {
		t.Errorf("account tokens = %q, %q; want <node>-user", u1.Token, u2.Token)
	}

	// Each input is attributed to its own producer, which is what names the
	// output files.
	pa, pb := producers[fixture], producers[fixtureB]
	if pa == nil || pb == nil {
		t.Fatal("a producer was not identified for every input")
	}
	if pa == pb || pa.Label == pb.Label {
		t.Errorf("both inputs attributed to the same producer %q", pa.Label)
	}
	if got := outputName(inputGroup{node: pa, bundle: true}, 0); got != pa.Label+".txt" {
		t.Errorf("output name = %q, want %q", got, pa.Label+".txt")
	}
	if got := outputName(inputGroup{bundle: false}, 3); got != "source-4.jsonl" {
		t.Errorf("fallback output name = %q, want source-4.jsonl", got)
	}
}

// Two inputs from the same machine each create a node group, and one is merged
// away during scanning. The group a source points at must still resolve to a
// label, or its output file would fall back to a source number.
func TestSameProducerTwiceStillNamesBothOutputs(t *testing.T) {
	d := newDiscovery(false)
	var groups []inputGroup
	for _, path := range []string{fixture, fixture} {
		d.beginSource()
		bundle := false
		if err := scanFile(path, visitor{
			onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
			onSection: func(name, _ string, blob any) error {
				bundle = true
				if blob != nil {
					d.scanSection(name, blob)
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		groups = append(groups, inputGroup{source: path, node: d.sourceNode(), bundle: bundle})
	}
	d.pruneEmptyNodes()
	d.allocate()

	used := map[string]int{}
	names := []string{
		uniqueOutputName(groups[0], 0, used),
		uniqueOutputName(groups[1], 1, used),
	}

	for i, name := range names {
		if strings.HasPrefix(name, "source-") {
			t.Errorf("output %d fell back to %q; the producer was identifiable", i, name)
		}
	}
	if names[0] == names[1] {
		t.Errorf("both outputs named %q; the second would overwrite the first", names[0])
	}
	if names[0] != "node-a.txt" || names[1] != "node-a-2.txt" {
		t.Errorf("names = %v, want [node-a.txt node-a-2.txt]", names)
	}
}

// A line that is not a record is dropped rather than sanitized. That is safe but
// silent, so it must be counted — otherwise pointing the tool at the wrong file
// yields an empty output that looks like a sanitized log.
func TestUnrecognisedLinesAreCounted(t *testing.T) {
	content := "not json at all\n" +
		`{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"a","message":"ok"}` + "\n" +
		`{"level":"info","time":"2026-07-21T22:11:32.000Z","sublevel":"a","message":"trunc`
	path := filepath.Join(t.TempDir(), "mixed.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	records, skipped := 0, 0
	if err := scanFile(path, visitor{
		onRecord: func(Record, string) error { records++; return nil },
		onSkip:   func(string) { skipped++ },
	}); err != nil {
		t.Fatal(err)
	}

	if records != 1 {
		t.Errorf("records = %d, want 1", records)
	}
	// The prose line and the truncated final line.
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}

// A bundle's title line and the blank lines around its sections are structure, not
// discarded content, and must not be reported as dropped.
func TestBundleStructureIsNotCountedAsSkipped(t *testing.T) {
	skipped := 0
	if err := scanFile(fixture, visitor{
		onRecord:  func(Record, string) error { return nil },
		onSection: func(string, string, any) error { return nil },
		onSkip:    func(line string) { skipped++; t.Logf("unexpected skip: %q", line) },
	}); err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 for a well-formed bundle", skipped)
	}
}

// A custom install directory can carry a name its owner chose. The root is
// replaced and the tail kept, so the layout still reads normally.
func TestInstallRootIsReplacedAtEveryEscapeDepth(t *testing.T) {
	res := runPipeline(t, fixture, true)

	root := findEntity(res.entities, `C:\Program Files\Seth Custom PAIR`)
	if root == nil {
		var got []string
		for _, e := range res.entities {
			if e.Kind == KindPath {
				got = append(got, e.Value)
			}
		}
		t.Fatalf("install root not learned; path entities: %v", got)
	}
	if root.Token != "<install>" {
		t.Errorf("install token = %q, want <install>", root.Token)
	}

	if strings.Contains(res.output, "Seth Custom PAIR") {
		t.Error("the custom install directory name survived")
	}
	// Depth one, as written in the metadata header.
	if !strings.Contains(res.output, `<install>\\resources\\cli-bin`) {
		t.Error("install root not replaced at the header's escape depth")
	}
	// Depth two, as written by a service that quoted the path first.
	if !strings.Contains(res.output, `<install>\\\\resources\\\\cli-bin`) {
		t.Error("install root not replaced at the service log's escape depth")
	}
	// The tail is diagnostic and must survive.
	if !strings.Contains(res.output, "nvpair-ui-broker.exe") {
		t.Error("the path tail was lost")
	}
}

// An install root must never be so shallow that replacing it swallows the drive
// or the filesystem root.
func TestInstallRootRejectsShallowPaths(t *testing.T) {
	for _, in := range []string{
		`C:\resources\cli-bin`, // one segment before resources
		`/resources/cli-bin`,
		`resources/cli-bin`,
		`C:\Program Files\PAIR\cli-bin`, // no resources anchor at all
		``,
	} {
		if got := installRoot(in); got != "" {
			t.Errorf("installRoot(%q) = %q, want empty", in, got)
		}
	}
	if got := installRoot(`C:\Program Files\PAIR\resources\cli-bin`); got != `C:\Program Files\PAIR` {
		t.Errorf("installRoot = %q, want C:\\Program Files\\PAIR", got)
	}
}

// A cluster's friendly name is free text the user typed. No shape test can find
// it, so it is recognised by the key that carried it.
func TestClusterFriendlyNameIsReplaced(t *testing.T) {
	res := runPipeline(t, fixture, true)

	label := findEntity(res.entities, "Seth's Basement Lab")
	if label == nil {
		t.Fatal("cluster friendly name was not learned")
	}
	if label.Kind != KindLabel {
		t.Errorf("kind = %v, want label", label.Kind)
	}
	if label.Token != "label-1" {
		t.Errorf("token = %q, want label-1", label.Token)
	}
	if strings.Contains(res.output, "Basement") {
		t.Error("the cluster friendly name survived")
	}
}

// Model names stay readable unless asked for, because model identity is usually
// what a routing problem is about.
func TestModelNamesOnlyReplacedWhenRequested(t *testing.T) {
	off := runPipelineOpts(t, fixture, true, false)
	if !strings.Contains(off.output, "qwen3.6:27b") {
		t.Error("model names should stay readable by default")
	}

	on := runPipelineOpts(t, fixture, true, true)
	if strings.Contains(on.output, "qwen3.6:27b") {
		t.Error("-models should replace model names")
	}
	if !strings.Contains(on.output, "model-") {
		t.Error("expected model tokens in the output")
	}
	// Engine names are keys in modelsByEngine, not model names.
	for _, e := range on.entities {
		if e.Kind == KindModel && (e.Value == "ollama" || e.Value == "lmstudio") {
			t.Errorf("engine name %q was treated as a model", e.Value)
		}
	}
}

// A key-driven rule describes what a value means and must not fire on the key
// text. In "models":[{"model":"x"}] the key "model" arrives under the path
// "models", and was previously read as a model named "model".
func TestKeyTextIsNotTreatedAsAValue(t *testing.T) {
	src := `{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"a",` +
		`"message":"m","data":{"models":[{"model":"qwen3.6:27b"}],` +
		`"clusterFriendlyName":"Lab One"}}` + "\n"
	path := filepath.Join(t.TempDir(), "keys.jsonl")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runPipelineOpts(t, path, true, true)

	for _, bad := range []string{"model", "models", "clusterFriendlyName"} {
		if e := findEntity(res.entities, bad); e != nil {
			t.Errorf("key text %q was learned as a %v", bad, e.Kind)
		}
	}
	// The values themselves are still found.
	if e := findEntity(res.entities, "qwen3.6:27b"); e == nil {
		t.Error("model value was not learned")
	}
	if e := findEntity(res.entities, "Lab One"); e == nil {
		t.Error("cluster label value was not learned")
	}
	// Structural keys must survive so the record still parses.
	if !strings.Contains(res.output, `"models"`) {
		t.Error("the models key was rewritten")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
