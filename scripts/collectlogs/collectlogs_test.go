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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, scanFile(path, visitor{
		onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
		onSection: func(name, _ string, blob any) error {
			isBundle = true
			if blob != nil {
				d.scanSection(name, blob)
			}
			return nil
		},
	}), "discovery pass")
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
	require.NoError(t, err, "create sink")

	require.NoError(t, scanFile(path, visitor{
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
	}), "rewrite pass")
	require.NoError(t, out.close(), "close sink")

	body, err := os.ReadFile(outPath)
	require.NoError(t, err, "read output")

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
	require.True(t, ok, "missing section %q", name)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out), "decode section %q", name)
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

	require.Len(t, res.nodes, 2, "want 2 nodes,")

	// The bundle's own machine is linked from the header sections and is
	// labelled first.
	hostA := findEntity(res.entities, "TESTHOST-A")
	uuidA := findEntity(res.entities, "11111111-2222-4333-8444-555555555555")
	ipA := findEntity(res.entities, "192.168.50.10")
	for name, e := range map[string]*Entity{"hostname": hostA, "uuid": uuidA, "address": ipA} {
		require.NotNil(t, e, "node A %s", name)
	}
	assert.Equal(t, uuidA.Node, hostA.Node, "node A identifiers not linked: hostname")
	assert.Equal(t, ipA.Node, hostA.Node, "node A identifiers not linked: hostname")
	assert.Equal(t, "node-a", hostA.Token, "want hostname token node-a,")
	assert.Equal(t, "node-a-ip", ipA.Token, "want address token node-a-ip,")

	// The UUID is still linked to the node, but left readable: it is a random
	// version 4 value that identifies nobody and is the primary key in most
	// payloads.
	assert.Equal(t, uuidA.Value, uuidA.Token, "uuid should stay readable, got token")

	// The account in a machine's own file paths belongs to that machine.
	userA := findEntity(res.entities, "testuser")
	require.NotNil(t, userA, "account name was not discovered")
	assert.Equal(t, "node-a-user", userA.Token, "want account token node-a-user,")
	assert.Equal(t, "node-a", userA.Node, "account attributed to")
}

// Four-part version strings parse as valid addresses. Treating one as an address
// would replace it and make the log misleading.
func TestVersionStringIsNotTreatedAsAddress(t *testing.T) {
	res := runPipeline(t, fixture, true)

	assert.Nil(t, findEntity(res.entities, "14.8.178.33"), "version must not be treated as an address")
	assert.Nil(t, findEntity(res.entities, "1.3.2.1"), "version must not be treated as an address")

	meta := sectionJSON(t, res, "Metadata")
	versions, ok := meta["processVersions"].(map[string]any)
	require.True(t, ok, "processVersions missing from the preserved header")
	assert.Equal(t, "14.8.178.33-electron.0", versions["v8"], "v8 version altered")
	assert.Equal(t, "1.3.2.1-motley", versions["zlib"], "zlib version altered")
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
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))

	res := runPipeline(t, path, true)

	assert.NotContains(t, res.output, "TESTHOST-A", "a host name used as an object key was not replaced")
	assert.Contains(t, res.output, `"node-a"`, "expected the host name key to become a token,")
}

// Node and cluster UUIDs are left readable on purpose.
func TestUUIDsAreLeftReadable(t *testing.T) {
	res := runPipeline(t, fixture, true)

	state := sectionJSON(t, res, "Current Modular State")
	nodes, ok := state["nodesInitial"].(map[string]any)
	require.True(t, ok, "nodesInitial missing")
	found := 0
	for key := range nodes {
		if reUUID.MatchString(key) {
			found++
		}
	}
	assert.Equal(t, 2, found, "want 2 readable UUID keys,")

	for _, e := range res.entities {
		if e.Kind != KindUUID {
			continue
		}
		assert.Equal(t, e.Value, e.Token)
		assert.Equal(t, "random", e.Class)
	}
}

// The desktop logger encodes a message that the Go logger already quoted, so a
// path arrives double-escaped. Replacing on decoded values and re-marshalling
// must preserve the original escape depth.
func TestNestedEscapingRoundTrip(t *testing.T) {
	res := runPipeline(t, fixture, true)

	assert.NotContains(t, res.output, "testuser", "username survived in the output")
	assert.Contains(t, res.output, `C:\\\\Users\\\\node-a-user\\\\AppData`, "double-escaped Windows path was not preserved at its original escape depth")
	assert.Contains(t, res.output, `path=\"C:`, "inner Go quoting was not preserved")
}

func TestLoopbackLeftInTheClear(t *testing.T) {
	res := runPipeline(t, fixture, true)

	e := findEntity(res.entities, "127.0.0.1")
	require.NotNil(t, e, "loopback address was not observed")
	assert.Equal(t, "127.0.0.1", e.Token, "loopback should stay readable, got token")
	assert.Contains(t, res.output, `"addr":"127.0.0.1"`, "loopback address missing from output")
}

// A certificate fingerprint is a long run of colon-separated hex pairs whose
// first six pairs look exactly like a MAC address.
func TestFingerprintIsNotPartiallyMatchedAsMAC(t *testing.T) {
	res := runPipeline(t, fixture, true)

	const fingerprint = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	assert.Contains(t, res.output, fingerprint, "fingerprint was altered; its MAC-shaped prefix must not be replaced")

	mac := findEntity(res.entities, "aa:bb:cc:dd:ee:ff")
	require.NotNil(t, mac, "a genuine six-pair MAC address was not discovered")
	assert.Equal(t, KindMAC, mac.Kind, "want KindMAC,")
	assert.NotContains(t, res.output, `"mac":"aa:bb:cc:dd:ee:ff"`, "MAC address was not replaced")
}

func TestDedupeCollapsesCopiesButKeepsRealRepeats(t *testing.T) {
	deduped := runPipeline(t, fixture, true)
	assert.Equal(t, 1, deduped.dropped, "want 1 duplicate copy collapsed,")
	assert.Equal(t, 1, strings.Count(deduped.output, "settings loaded"), "want the duplicated line once,")
	// Two identical messages two seconds apart are separate events.
	assert.Equal(t, 2, strings.Count(deduped.output, "polling node"), "want both repeated polls kept,")

	raw := runPipeline(t, fixture, false)
	assert.Equal(t, 0, raw.dropped, "dedupe disabled should drop nothing, dropped")
	assert.Equal(t, 2, strings.Count(raw.output, "settings loaded"), "want both copies without dedupe,")
}

func TestVerificationPassesOnSanitizedOutput(t *testing.T) {
	res := runPipeline(t, fixture, true)

	v := newVerifier(res.entities)
	require.NoError(t, v.checkFile(res.path), "verify")
	assert.True(t, v.ok(), "verification reported findings on clean output")

	assert.NotContains(t, res.output, "testuser")
	assert.NotContains(t, res.output, "TESTHOST-A")
	assert.NotContains(t, res.output, "TESTHOST-B")
	assert.NotContains(t, res.output, "192.168.50.10")
	assert.NotContains(t, res.output, "192.168.50.11")
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
	require.NoError(t, os.WriteFile(path, []byte(leaky), 0o600))

	v := newVerifier(entities)
	require.NoError(t, v.checkFile(path))
	assert.False(t, v.ok(), "verifier accepted output containing learned values")
	assert.GreaterOrEqual(t, len(v.findings), 2, "want a finding per leaked value,")
}

func TestIsHostnameRejectsNonHosts(t *testing.T) {
	assert.False(t, isHostname("qwen3.6:27b"))                          // model tag
	assert.False(t, isHostname("engine:status"))                        // JSON-RPC method
	assert.False(t, isHostname("ollama"))                               // engine name
	assert.False(t, isHostname("lmstudio"))                             // engine name
	assert.False(t, isHostname("127.0.0.1"))                            // address
	assert.False(t, isHostname("11111111-2222-4333-8444-555555555555")) // uuid
	assert.False(t, isHostname("C:\\Users\\bob"))                       // path
	assert.False(t, isHostname("a b"))                                  // free text
	assert.False(t, isHostname(""))                                     // empty
	assert.True(t, isHostname("TESTHOST-A"))
	assert.True(t, isHostname("SethWork2"))
	assert.True(t, isHostname("DESKTOP-N1D9NDS"))
	assert.True(t, isHostname("node-01.lan"))
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
		assert.Equal(t, want.class, class, "IP %q", input)
		assert.Equal(t, want.keep, keep, "IP %q", input)
	}
}

// Tokens must be identical across runs on the same input, otherwise two
// collections of the same logs cannot be compared.
func TestTokenAssignmentIsDeterministic(t *testing.T) {
	first := runPipeline(t, fixture, true)
	second := runPipeline(t, fixture, true)

	assert.Equal(t, second.output, first.output, "two runs over the same input produced different output")
	for _, e := range first.entities {
		other := findEntity(second.entities, e.Value)
		require.NotNil(t, other)
		assert.Equal(t, e.Token, other.Token)
	}
}

// Hostnames are only trusted from structured fields. An input without them must
// say so, because verification passing does not mean nothing was missed.
func TestFragmentInputWarnsAboutUndetectedHostnames(t *testing.T) {
	fragment := `{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"x","message":"host WORKSTATION-9 at 192.168.77.5"}` + "\n"
	path := filepath.Join(t.TempDir(), "fragment.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(fragment), 0o600))

	d := newDiscovery(false)
	require.NoError(t, scanFile(path, visitor{
		onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
	}))
	d.pruneEmptyNodes()
	d.allocate()
	d.checkConfidence()

	// The address is still found by shape.
	assert.NotNil(t, findEntity(d.sortedEntities(), "192.168.77.5"), "address in free text was not detected")

	found := false
	for _, w := range d.warnings {
		if strings.Contains(w, "no hostname was learned") {
			found = true
		}
	}
	assert.True(t, found, "want a warning about undetected hostnames,")
}

// A full bundle has the structured fields, so it must not carry that warning.
func TestFullBundleDoesNotWarnAboutHostnames(t *testing.T) {
	d := newDiscovery(false)
	require.NoError(t, scanFile(fixture, visitor{
		onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
		onSection: func(name, _ string, blob any) error {
			if blob != nil {
				d.scanSection(name, blob)
			}
			return nil
		},
	}))
	d.pruneEmptyNodes()
	d.allocate()
	d.checkConfidence()

	for _, w := range d.warnings {
		assert.NotContains(t, w, "no hostname was learned", "full bundle should not warn about hostnames")
	}
}

// An exported bundle must come back as a bundle, with its header intact and a
// single record section — not split into separate files.
func TestBundleFormatIsPreservedAsOneFile(t *testing.T) {
	res := runPipeline(t, fixture, true)

	assert.True(t, res.bundle, "fixture is an exported bundle but was not detected as one")
	assert.True(t, strings.HasPrefix(res.output, "# NVIDIA PAIR Logs (sanitized)"), "bundle header missing from output")
	assert.Contains(t, res.output, "## Metadata\n")
	assert.Contains(t, res.output, "## Current Modular State\n")
	assert.Equal(t, 1, strings.Count(res.output, "## "+recordSectionTitle), "want exactly one record section,")

	// Sections are written back as text, so they must still parse.
	meta := sectionJSON(t, res, "Metadata")
	assert.Equal(t, "0.0.60-dev", meta["appVersion"], "appVersion altered")
	assert.Equal(t, "node-a", meta["hostname"], "hostname should be tokenized in the header,")
}

// A raw nvpair.jsonl has no header, so it comes back as plain JSONL.
func TestRawJSONLInputStaysJSONL(t *testing.T) {
	src := `{"level":"info","time":"2026-07-21T22:11:31.000Z","source":"x","sublevel":"y","message":"listening","data":{"addr":"127.0.0.1"}}` + "\n"
	path := filepath.Join(t.TempDir(), "nvpair.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))

	res := runPipeline(t, path, true)
	assert.False(t, res.bundle, "plain JSONL was treated as a bundle")
	assert.NotContains(t, res.output, "##", "markdown sections were added to a plain JSONL output")
	assert.False(t, strings.HasPrefix(res.output, "#"), "markdown sections were added to a plain JSONL output")
	assert.True(t, strings.HasPrefix(res.output, "{"), "output is not plain JSONL")
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
		{
			err := scanFile(path, visitor{
				onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
				onSection: func(name, _ string, blob any) error {
					if blob != nil {
						d.scanSection(name, blob)
					}
					return nil
				},
			})
			require.NoError(t, err, "scan")
		}
		producers[path] = d.sourceNode()
	}
	d.pruneEmptyNodes()
	d.allocate()
	entities := d.sortedEntities()

	require.Len(t, d.nodes, 2, "want 2 nodes,")
	for _, n := range d.nodes {
		assert.Len(t, n.Hostnames, 1)
		assert.Len(t, n.UUIDs, 1)
	}

	// TESTHOST-B is the producer of the second bundle and a discovered peer in
	// the first; both views must land on one node.
	hostB := findEntity(entities, "TESTHOST-B")
	uuidB := findEntity(entities, "66666666-7777-4888-8999-aaaaaaaaaaaa")
	ipB := findEntity(entities, "192.168.50.11")
	require.NotNil(t, hostB, "node B identifiers were not all discovered")
	require.NotNil(t, uuidB, "node B identifiers were not all discovered")
	require.NotNil(t, ipB, "node B identifiers were not all discovered")
	assert.Equal(t, uuidB.Node, hostB.Node, "node B identifiers split across groups")
	assert.Equal(t, ipB.Node, hostB.Node, "node B identifiers split across groups")

	// Each account belongs to the machine whose log carried it.
	u1, u2 := findEntity(entities, "testuser"), findEntity(entities, "otheruser")
	require.NotNil(t, u1, "expected both account names to be discovered")
	require.NotNil(t, u2, "expected both account names to be discovered")
	assert.NotEqual(t, "", u1.Node, "accounts not attributed to a node")
	assert.NotEqual(t, "", u2.Node, "accounts not attributed to a node")
	assert.NotEqual(t, u2.Node, u1.Node, "both accounts attributed to")
	assert.Equal(t, u1.Node+"-user", u1.Token, "account tokens")
	assert.Equal(t, u2.Node+"-user", u2.Token, "account tokens")

	// Each input is attributed to its own producer, which is what names the
	// output files.
	pa, pb := producers[fixture], producers[fixtureB]
	require.NotNil(t, pa, "a producer was not identified for every input")
	require.NotNil(t, pb, "a producer was not identified for every input")
	assert.Equal(t, map[string]bool{"TESTHOST-A": true}, pa.Hostnames, "first input producer")
	assert.Equal(t, map[string]bool{"11111111-2222-4333-8444-555555555555": true}, pa.UUIDs, "first input producer")
	assert.Equal(t, map[string]bool{"TESTHOST-B": true}, pb.Hostnames, "second input producer")
	assert.Equal(t, map[string]bool{"66666666-7777-4888-8999-aaaaaaaaaaaa": true}, pb.UUIDs, "second input producer")
	assert.NotEqual(t, pb.Label, pa.Label, "both inputs attributed to the same producer")
	assert.Equal(t, pa.Label+".txt", outputName(inputGroup{node: pa, bundle: true}, 0), "output name")
	assert.Equal(t, "source-4.jsonl", outputName(inputGroup{bundle: false}, 3), "fallback output name")
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
		require.NoError(t, scanFile(path, visitor{
			onRecord: func(rec Record, _ string) error { d.scanRecord(rec); return nil },
			onSection: func(name, _ string, blob any) error {
				bundle = true
				if blob != nil {
					d.scanSection(name, blob)
				}
				return nil
			},
		}))
		groups = append(groups, inputGroup{source: path, node: d.sourceNode(), bundle: bundle})
	}
	d.pruneEmptyNodes()
	d.allocate()

	used := map[string]int{}
	names := []string{
		uniqueOutputName(groups[0], 0, used),
		uniqueOutputName(groups[1], 1, used),
	}

	assert.Equal(t, []string{"node-a.txt", "node-a-2.txt"}, names, "both outputs must keep distinct producer names")
}

// A line that is not a record is dropped rather than sanitized. That is safe but
// silent, so it must be counted — otherwise pointing the tool at the wrong file
// yields an empty output that looks like a sanitized log.
func TestUnrecognisedLinesAreCounted(t *testing.T) {
	content := "not json at all\n" +
		`{"level":"info","time":"2026-07-21T22:11:31.000Z","sublevel":"a","message":"ok"}` + "\n" +
		`{"level":"info","time":"2026-07-21T22:11:32.000Z","sublevel":"a","message":"trunc`
	path := filepath.Join(t.TempDir(), "mixed.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	records, skipped := 0, 0
	require.NoError(t, scanFile(path, visitor{
		onRecord: func(Record, string) error { records++; return nil },
		onSkip:   func(string) { skipped++ },
	}))

	assert.Equal(t, 1, records)
	// The prose line and the truncated final line.
	assert.Equal(t, 2, skipped)
}

// A bundle's title line and the blank lines around its sections are structure, not
// discarded content, and must not be reported as dropped.
func TestBundleStructureIsNotCountedAsSkipped(t *testing.T) {
	skipped := 0
	require.NoError(t, scanFile(fixture, visitor{
		onRecord:  func(Record, string) error { return nil },
		onSection: func(string, string, any) error { return nil },
		onSkip:    func(line string) { skipped++; t.Logf("unexpected skip: %q", line) },
	}))
	assert.Equal(t, 0, skipped)
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
		require.FailNow(t, "test expectation failed", "install root not learned; path entities")
	}
	assert.Equal(t, "<install>", root.Token, "install token")

	assert.NotContains(t, res.output, "Seth Custom PAIR", "the custom install directory name survived")
	// Depth one, as written in the metadata header.
	assert.Contains(t, res.output, `<install>\\resources\\cli-bin`, "install root not replaced at the header's escape depth")
	// Depth two, as written by a service that quoted the path first.
	assert.Contains(t, res.output, `<install>\\\\resources\\\\cli-bin`, "install root not replaced at the service log's escape depth")
	// The tail is diagnostic and must survive.
	assert.Contains(t, res.output, "nvpair-ui-broker.exe", "the path tail was lost")
}

// An install root must never be so shallow that replacing it swallows the drive
// or the filesystem root.
func TestInstallRootRejectsShallowPaths(t *testing.T) {
	assert.Equal(t, "", installRoot(`C:\resources\cli-bin`)) // one segment before resources
	assert.Equal(t, "", installRoot(`/resources/cli-bin`))
	assert.Equal(t, "", installRoot(`resources/cli-bin`))
	assert.Equal(t, "", installRoot(`C:\Program Files\PAIR\cli-bin`)) // no resources anchor at all
	assert.Equal(t, "", installRoot(``))
	assert.Equal(t, `C:\Program Files\PAIR`, installRoot(`C:\Program Files\PAIR\resources\cli-bin`))
}

// A cluster's friendly name is free text the user typed. No shape test can find
// it, so it is recognised by the key that carried it.
func TestClusterFriendlyNameIsReplaced(t *testing.T) {
	res := runPipeline(t, fixture, true)

	label := findEntity(res.entities, "Seth's Basement Lab")
	require.NotNil(t, label, "cluster friendly name was not learned")
	assert.Equal(t, KindLabel, label.Kind)
	assert.Equal(t, "label-1", label.Token)
	assert.NotContains(t, res.output, "Basement", "the cluster friendly name survived")
}

// Model names stay readable unless asked for, because model identity is usually
// what a routing problem is about.
func TestModelNamesOnlyReplacedWhenRequested(t *testing.T) {
	off := runPipelineOpts(t, fixture, true, false)
	assert.Contains(t, off.output, "qwen3.6:27b", "model names should stay readable by default")

	on := runPipelineOpts(t, fixture, true, true)
	assert.NotContains(t, on.output, "qwen3.6:27b", "-models should replace model names")
	assert.Contains(t, on.output, "model-", "expected model tokens in the output")
	// Engine names are keys in modelsByEngine, not model names.
	for _, e := range on.entities {
		assert.False(t, e.Kind == KindModel && (e.Value == "ollama" || e.Value == "lmstudio"), "engine %q must not be treated as a model name", e.Value)
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
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))

	res := runPipelineOpts(t, path, true, true)

	assert.Nil(t, findEntity(res.entities, "model"), "key must not be treated as a value")
	assert.Nil(t, findEntity(res.entities, "models"), "key must not be treated as a value")
	assert.Nil(t, findEntity(res.entities, "clusterFriendlyName"), "key must not be treated as a value")
	// The values themselves are still found.
	assert.NotNil(t, findEntity(res.entities, "qwen3.6:27b"), "model value was not learned")
	assert.NotNil(t, findEntity(res.entities, "Lab One"), "cluster label value was not learned")
	// Structural keys must survive so the record still parses.
	assert.Contains(t, res.output, `"models"`, "the models key was rewritten")
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
