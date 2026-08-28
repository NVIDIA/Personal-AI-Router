// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// Detection runs in two layers.
//
// Layer one learns identifiers from the places the log guarantees they appear
// in structured form: the exported Metadata section, and node-descriptor objects
// inside discovery and proxy frames. Layer two sweeps every string with shape
// patterns to catch identifiers the first layer did not reach, including those
// embedded in free-text Go log messages.
//
// Hostnames are deliberately only accepted from layer one. There is no reliable
// shape test that separates a hostname from a model name or an engine name, so
// confidence comes from structure: a hostname is trusted when it is the
// bundle's own hostname, or when it appeared in an object that also yielded a
// UUID or an address.
var (
	reUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reMAC  = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)
	reIPv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reIPv6 = regexp.MustCompile(`(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}`)
	// Runs of seven or more colon-separated hex pairs, which is a certificate
	// fingerprint rather than a MAC address.
	reHexRun = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){6,}[0-9a-f]{2}\b`)
	// Matches one or two path separators so it works on strings that were
	// quoted by the Go logger before Electron encoded them.
	reUserPath = regexp.MustCompile(`(?i)[/\\]{1,2}(?:Users|home)[/\\]{1,2}([^/\\"'\s,;)\]}]+)`)
)

// Key names that may carry a node identifier. The key only decides whether a
// value is a candidate; the value's own shape decides what it is.
var (
	hostnameKeys = map[string]bool{
		"id": true, "name": true, "host": true, "hostname": true,
		"node": true, "display": true, "nodeName": true,
	}
	uuidKeys = map[string]bool{
		"id": true, "uuid": true, "hostUuid": true, "nodeId": true,
		"node_id": true, "selfId": true, "clusterUuid": true, "cluster-uuid": true,
	}
	addressKeys = map[string]bool{
		"ip": true, "ipAddress": true, "addr": true, "address": true,
		"addresses": true, "allIpAddresses": true, "remote": true,
		"target": true, "reachableAddress": true,
	}
)

// Values that are never a hostname even when they arrive under a hostname key.
var notHostnames = map[string]bool{
	"": true, "localhost": true, "unknown": true, "ollama": true,
	"lmstudio": true, "stdout": true, "stderr": true, "broker": true,
	"true": true, "false": true, "null": true,
}

// Keys whose value locates the installation. The install root is replaced while
// the tail is kept, so a custom install directory name does not travel but the
// layout still reads normally.
//
// Deliberately excludes the bare "path" key: it carries HTTP routes such as
// /api/chat far more often than a filesystem path.
var installRootKeys = map[string]bool{
	"cliBinDir": true, "binDir": true, "resourcesPath": true,
	"appPath": true, "binaryPath": true,
}

// Keys whose value is free text the user chose.
var labelKeys = map[string]bool{
	"clusterFriendlyName": true, "cluster_friendly_name": true,
}

// Keys whose value is a model identifier. Only consulted when model replacement
// is asked for.
var modelKeys = map[string]bool{
	"model": true, "models": true, "loadedModels": true,
}

// modelParentKeys hold per-engine maps, so their leaf values are model names but
// their keys are engine names.
var modelParentKeys = map[string]bool{
	"modelsByEngine": true, "loadedByEngine": true,
}

type discovery struct {
	entities map[string]*Entity
	nodes    []*Node
	nodeOf   map[string]*Node
	local    *Node
	nodeSeq  int
	warnings []string
	records  int
	// replaceModels turns on model-name replacement, off by default because model
	// identity is usually needed to read a routing problem.
	replaceModels bool
}

// newNodeGroup creates and registers a node group, stamping it with its creation
// order so labels do not depend on which side of a later merge survived.
func (d *discovery) newNodeGroup(firstSeen string) *Node {
	d.nodeSeq++
	n := newNode()
	n.seq = d.nodeSeq
	n.FirstSeen = firstSeen
	d.nodes = append(d.nodes, n)
	return n
}

func newDiscovery(replaceModels bool) *discovery {
	return &discovery{
		entities:      map[string]*Entity{},
		nodeOf:        map[string]*Node{},
		replaceModels: replaceModels,
	}
}

// beginSource marks the start of a new input. Each bundle was produced by a
// different machine, so the local-node reference must not carry over — otherwise
// the second bundle's hostname would be linked to the first bundle's node and two
// machines would collapse into one.
func (d *discovery) beginSource() {
	d.local = nil
}

// sourceNode reports the producer identified for the source just read, or nil
// when the input carried no structured header to identify one. Output files are
// named from this.
func (d *discovery) sourceNode() *Node {
	return d.local
}

// localNode is the machine that produced the bundle currently being read. Its
// hostname and self id arrive in separate sections of an exported bundle, so they
// are linked here rather than by co-occurrence. An empty FirstSeen sorts ahead of
// every record timestamp, which makes the first bundle's producer node-a.
func (d *discovery) localNode() *Node {
	if d.local == nil {
		d.local = d.newNodeGroup("")
	}
	return d.local
}

// pruneEmptyNodes drops node groups that never gathered an identifier, which
// happens when an input has no structured header to seed the local node.
func (d *discovery) pruneEmptyNodes() {
	kept := d.nodes[:0]
	for _, n := range d.nodes {
		if n.size() > 0 {
			kept = append(kept, n)
		}
	}
	d.nodes = kept
}

// observe records one identifier occurrence.
func (d *discovery) observe(kind Kind, value, when string) *Entity {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	e, ok := d.entities[value]
	if !ok {
		e = &Entity{Kind: kind, Value: value, FirstSeen: when}
		switch kind {
		case KindIPv4, KindIPv6:
			class, keep := classifyIP(value)
			e.Class = class
			if keep {
				// Loopback, link-local and multicast addresses identify nobody
				// and are much easier to read in the clear.
				e.Token = value
			}
		case KindUUID:
			// Node and cluster identifiers are random version 4 UUIDs. Nobody
			// chose them, they say nothing about a person or a network, and they
			// are the primary key in most payloads — so leaving them readable
			// costs no privacy and makes the log much easier to follow. They are
			// still tracked, because they are what links a machine's records to
			// its other identifiers.
			e.Class = "random"
			e.Token = value
		}
		d.entities[value] = e
	}
	e.Count++
	return e
}

// link joins identifiers that were observed describing the same machine.
func (d *discovery) link(when string, hostnames, uuids, ips []string) {
	members := append(append(append([]string{}, hostnames...), uuids...), ips...)
	if len(members) == 0 {
		return
	}

	var target *Node
	for _, m := range members {
		if n, ok := d.nodeOf[m]; ok {
			if target == nil {
				target = n
			} else if target != n {
				d.mergeNodes(target, n)
			}
		}
	}
	if target == nil {
		target = d.newNodeGroup(when)
	}

	for _, h := range hostnames {
		target.Hostnames[h] = true
		d.nodeOf[h] = target
	}
	for _, u := range uuids {
		target.UUIDs[u] = true
		d.nodeOf[u] = target
	}
	for _, ip := range ips {
		// Classify directly rather than reading entity state: linking runs
		// before the shape sweep for a given record, so the entity may not
		// exist yet. Addresses left in the clear are not node-owned.
		if _, keepLiteral := classifyIP(ip); keepLiteral {
			continue
		}
		target.IPs[ip] = true
		d.nodeOf[ip] = target
	}
}

func (d *discovery) mergeNodes(into, from *Node) {
	if into == from {
		return
	}
	for k := range from.Hostnames {
		into.Hostnames[k] = true
		d.nodeOf[k] = into
	}
	for k := range from.UUIDs {
		into.UUIDs[k] = true
		d.nodeOf[k] = into
	}
	for k := range from.IPs {
		into.IPs[k] = true
		d.nodeOf[k] = into
	}
	for k := range from.Users {
		into.Users[k] = true
	}
	// An empty FirstSeen marks a group identified from a bundle header, which
	// sorts ahead of every group first seen in the record stream. Merging must
	// not replace it with a timestamp, or a machine that produced one of the
	// inputs could be labelled after one that only appeared as a peer.
	switch {
	case from.FirstSeen == "":
		into.FirstSeen = ""
	case into.FirstSeen != "" && from.FirstSeen < into.FirstSeen:
		into.FirstSeen = from.FirstSeen
	}
	if from.seq < into.seq {
		into.seq = from.seq
	}
	from.merged = into
	for i, n := range d.nodes {
		if n == from {
			d.nodes = append(d.nodes[:i], d.nodes[i+1:]...)
			break
		}
	}
}

// scanRecord performs both detection layers for one record.
func (d *discovery) scanRecord(rec Record) {
	d.records++
	when := rec.Time

	trees := []any{}
	if rec.Data != nil {
		trees = append(trees, rec.Data)
	}
	if embedded, ok := parseEmbedded(rec.Message); ok {
		trees = append(trees, embedded)
	}

	for _, tree := range trees {
		d.linkObjects(tree, when)
		walkStrings(tree, "", func(path, s string, isKey bool) {
			d.sweep(path, s, when, isKey)
		})
	}

	// Free-text fields. The Go log stream arrives here as one opaque string.
	d.sweep("", rec.Message, when, false)
	d.sweep("", rec.Source, when, false)
	d.sweep("", rec.Sublevel, when, false)
}

// scanSection reads the structured header of an exported bundle. The bundle's
// own hostname and self id describe the machine that produced it, so they are
// linked as one node even though they arrive in separate sections.
func (d *discovery) scanSection(name string, blob any) {
	obj, _ := blob.(map[string]any)

	switch name {
	case sectionMetadata:
		if obj != nil {
			if h, ok := obj["hostname"].(string); ok && isHostname(h) {
				d.observe(KindHostname, h, "")
				local := d.localNode()
				local.Hostnames[h] = true
				d.nodeOf[h] = local
			}
		}
	case sectionModularState:
		if obj != nil {
			if id, ok := obj["selfId"].(string); ok && isUUID(id) {
				d.observe(KindUUID, id, "")
				local := d.localNode()
				local.UUIDs[id] = true
				d.nodeOf[id] = local
			}
		}
	}

	if blob != nil {
		d.linkObjects(blob, "")
		walkStrings(blob, "", func(path, s string, isKey bool) {
			d.sweep(path, s, "", isKey)
		})
	}
}

// linkObjects finds node-descriptor objects anywhere in a tree. An object
// qualifies when it yields at least two of the three identifier groups, which is
// what distinguishes a node record from an incidental object that merely has an
// id field.
func (d *discovery) linkObjects(v any, when string) {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			d.linkObjects(item, when)
		}
	case map[string]any:
		var hostnames, uuids, ips []string
		for k, raw := range t {
			collect(raw, func(s string) {
				switch {
				case isUUID(s) && (uuidKeys[k] || hostnameKeys[k]):
					uuids = appendUnique(uuids, s)
				case isIP(s) && (addressKeys[k] || hostnameKeys[k]):
					ips = appendUnique(ips, s)
				case hostnameKeys[k] && isHostname(s):
					hostnames = appendUnique(hostnames, s)
				}
			})
		}
		groups := 0
		for _, g := range [][]string{hostnames, uuids, ips} {
			if len(g) > 0 {
				groups++
			}
		}
		if groups >= 2 {
			// Observe every member here so the entity exists before linking,
			// independent of when the shape sweep reaches the same value.
			for _, h := range hostnames {
				d.observe(KindHostname, h, when)
			}
			for _, u := range uuids {
				d.observe(KindUUID, u, when)
			}
			for _, ip := range ips {
				d.observe(ipKind(ip), ip, when)
			}
			d.link(when, hostnames, uuids, ips)
		}
		for _, item := range t {
			d.linkObjects(item, when)
		}
	}
}

// sweep applies the shape patterns to one string. path is the dotted key path
// the string came from, or empty for free text.
func (d *discovery) sweep(path, s string, when string, isKey bool) {
	if s == "" {
		return
	}

	// Key-driven rules describe what a value means, so they must not be applied
	// to the key text. Shape tests below run on both, because some payloads are
	// keyed by an identifier.
	if !isKey {
		d.sweepByKey(path, s, when)
	}

	claimed := claimAmbiguousSpans(s)
	overlaps := func(lo, hi int) bool {
		for _, c := range claimed {
			if lo < c.hi && c.lo < hi {
				return true
			}
		}
		return false
	}
	claim := func(lo, hi int) { claimed = append(claimed, span{lo, hi}) }

	for _, m := range reUUID.FindAllStringIndex(s, -1) {
		if overlaps(m[0], m[1]) {
			continue
		}
		d.observe(KindUUID, s[m[0]:m[1]], when)
		claim(m[0], m[1])
	}
	for _, m := range reMAC.FindAllStringIndex(s, -1) {
		if overlaps(m[0], m[1]) {
			continue
		}
		d.observe(KindMAC, s[m[0]:m[1]], when)
		claim(m[0], m[1])
	}

	// Four-part version strings parse as valid addresses, so address detection
	// is suppressed where the key path says the value is a version.
	if !versionContext(path) {
		for _, m := range reIPv4.FindAllStringIndex(s, -1) {
			if overlaps(m[0], m[1]) {
				continue
			}
			v := s[m[0]:m[1]]
			if net.ParseIP(v) == nil {
				continue
			}
			d.observe(KindIPv4, v, when)
			claim(m[0], m[1])
		}
		for _, m := range reIPv6.FindAllStringIndex(s, -1) {
			if overlaps(m[0], m[1]) {
				continue
			}
			v := s[m[0]:m[1]]
			// Timestamps and durations also contain colons; ParseIP is the
			// authority on whether this is an address.
			if strings.Count(v, ":") < 2 || net.ParseIP(v) == nil {
				continue
			}
			d.observe(KindIPv6, v, when)
			claim(m[0], m[1])
		}
	}

	for _, m := range reUserPath.FindAllStringSubmatchIndex(s, -1) {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		user := s[m[2]:m[3]]
		if !isUsername(user) {
			continue
		}
		d.observe(KindUsername, user, when)

		// Attribute the account to the machine whose log is being read, so it
		// reads as node-a-user rather than being numbered on its own. Kept out
		// of nodeOf: accounts must not take part in node merging.
		if d.local != nil {
			d.local.Users[user] = true
		}
	}
}

// sweepByKey handles values that no shape test can recognise, where the key that
// carried the value is the only evidence of what it is.
func (d *discovery) sweepByKey(path, s string, when string) {
	leaf, parent := pathLeaf(path)

	if installRootKeys[leaf] {
		if root := installRoot(s); root != "" {
			d.observe(KindPath, root, when)
		}
	}

	if labelKeys[leaf] {
		// A friendly name is often just the hostname, in which case observe finds
		// the existing entity and it keeps the node's own token.
		if isLabel(s) {
			d.observe(KindLabel, s, when)
		}
	}

	if d.replaceModels && (modelKeys[leaf] || modelParentKeys[parent]) && isModelName(s) {
		d.observe(KindModel, s, when)
	}
}

// pathLeaf splits a dotted key path into its last and second-to-last segments.
func pathLeaf(path string) (leaf, parent string) {
	if path == "" {
		return "", ""
	}
	parts := strings.Split(path, ".")
	leaf = parts[len(parts)-1]
	if len(parts) >= 2 {
		parent = parts[len(parts)-2]
	}
	return leaf, parent
}

// installRoot extracts the directory holding the application from a path inside
// it. A packaged build always has a "resources" directory under the install root,
// which is the anchor used here. An unpackaged development tree has no such
// segment and yields nothing: those paths sit under the developer's home
// directory, where the account name is already replaced.
func installRoot(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	fields := splitPath(value)
	for i := len(fields) - 1; i >= 1; i-- {
		if strings.EqualFold(fields[i], "resources") {
			if i < 2 {
				// Too shallow to be an install root; replacing it would swallow
				// a drive letter or the filesystem root.
				return ""
			}
			// Rejoin with the separator the input used, not the one this machine
			// happens to prefer: logs are routinely collected on a different
			// operating system than the one that wrote them.
			return strings.Join(fields[:i], pathSeparatorOf(value))
		}
	}
	return ""
}

func pathSeparatorOf(value string) string {
	if strings.Contains(value, `\`) {
		return `\`
	}
	return "/"
}

// splitPath breaks a path on either separator, tolerating the doubled separators
// that appear when a value was quoted before being encoded.
func splitPath(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isLabel(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 2 && len(s) <= 128
}

// isModelName keeps obvious non-values out of the model set.
func isModelName(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 128 {
		return false
	}
	switch strings.ToLower(s) {
	case "ollama", "lmstudio", "true", "false", "null", "unknown":
		return false
	}
	return true
}

type span struct{ lo, hi int }

// claimAmbiguousSpans reserves regions whose text would otherwise be partially
// matched by a narrower pattern. A certificate fingerprint is a long run of
// colon-separated hex pairs, and its first six pairs look exactly like a MAC
// address; replacing that prefix would corrupt the fingerprint. Discovery and
// verification share this so both agree on what is a candidate.
func claimAmbiguousSpans(s string) []span {
	var out []span
	for _, m := range reHexRun.FindAllStringIndex(s, -1) {
		out = append(out, span{m[0], m[1]})
	}
	return out
}

func versionContext(path string) bool {
	return strings.Contains(strings.ToLower(path), "version")
}

// allocate assigns tokens. Nodes are labelled in first-seen order and their
// identifiers share that label, so one machine reads as node-a throughout
// instead of as three unrelated tokens.
func (d *discovery) allocate() {
	sort.SliceStable(d.nodes, func(i, j int) bool {
		if d.nodes[i].FirstSeen != d.nodes[j].FirstSeen {
			return d.nodes[i].FirstSeen < d.nodes[j].FirstSeen
		}
		return d.nodes[i].seq < d.nodes[j].seq
	})

	for i, n := range d.nodes {
		n.Label = nodeLabel(i)
		d.assignNodeTokens(n)
	}

	// Anything not owned by a node gets a counter within its own kind. Sorting
	// by first appearance keeps assignment stable for a given input.
	counters := map[string]int{}
	for _, e := range d.sortedEntities() {
		if e.Token != "" {
			continue
		}
		switch e.Kind {
		case KindIPv4, KindIPv6:
			key := "ip-" + e.Class
			counters[key]++
			e.Token = fmt.Sprintf("%s-%d", key, counters[key])
		case KindUUID:
			counters["uuid"]++
			e.Token = fmt.Sprintf("uuid-%d", counters["uuid"])
		case KindMAC:
			counters["mac"]++
			e.Token = fmt.Sprintf("mac-%d", counters["mac"])
		case KindUsername:
			counters["user"]++
			e.Token = fmt.Sprintf("user-%d", counters["user"])
		case KindHostname:
			counters["host"]++
			e.Token = fmt.Sprintf("host-%d", counters["host"])
		case KindPath:
			// Angle brackets are not legal in a Windows path, which makes it
			// obvious the value is a placeholder rather than a real directory.
			counters["install"]++
			if counters["install"] == 1 {
				e.Token = "<install>"
			} else {
				e.Token = fmt.Sprintf("<install-%d>", counters["install"])
			}
		case KindLabel:
			counters["label"]++
			e.Token = fmt.Sprintf("label-%d", counters["label"])
		case KindModel:
			counters["model"]++
			e.Token = fmt.Sprintf("model-%d", counters["model"])
		}
	}
}

func (d *discovery) assignNodeTokens(n *Node) {
	assign := func(values map[string]bool, suffix string) {
		// Number by tokens actually assigned, not by position in the source
		// list, so a skipped entry cannot leave a gap like node-a-ip2 with no
		// node-a-ip.
		assigned := 0
		for _, v := range sortedKeys(values) {
			e := d.entities[v]
			if e == nil {
				continue
			}
			// Record ownership even for values left readable, so the legend and
			// the token map still say which machine a UUID belongs to.
			if e.Node == "" {
				e.Node = n.Label
			}
			if e.Token != "" {
				continue
			}
			token := n.Label
			if suffix != "" {
				token += "-" + suffix
			}
			if assigned > 0 {
				token = fmt.Sprintf("%s%d", token, assigned+1)
			}
			e.Token = token
			e.Node = n.Label
			assigned++
		}
	}
	assign(n.Hostnames, "")
	assign(n.UUIDs, "uuid")
	assign(n.IPs, "ip")
	assign(n.Users, "user")
}

func (d *discovery) sortedEntities() []*Entity {
	out := make([]*Entity, 0, len(d.entities))
	for _, e := range d.entities {
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FirstSeen != out[j].FirstSeen {
			return out[i].FirstSeen < out[j].FirstSeen
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// checkConfidence reports the limits of what this input allowed.
func (d *discovery) checkConfidence() {
	hostnames := 0
	for _, e := range d.sortedEntities() {
		if e.Kind == KindHostname {
			hostnames++
		}
		if e.Kind != KindHostname && e.Kind != KindUsername {
			continue
		}
		if len(e.Value) < 4 {
			d.warnings = append(d.warnings, fmt.Sprintf(
				"%s %q is short; literal replacement may affect unrelated text",
				e.Kind, e.Value))
		}
	}

	// Hostnames are only trusted from structured fields, because no shape test
	// separates a hostname from a model name or a method name. When an input
	// carries no such field, a hostname that appears only in free text is not
	// detected, and the reader needs to know that before sharing the result.
	if hostnames == 0 {
		d.warnings = append(d.warnings,
			"no hostname was learned from a structured field, so a hostname appearing "+
				"only in free text was not replaced; check that the input is a full "+
				"exported bundle rather than a fragment")
	}
}

func nodeLabel(i int) string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	if i < len(letters) {
		return "node-" + string(letters[i])
	}
	return fmt.Sprintf("node-%d", i+1)
}

func isUUID(s string) bool { return reUUID.MatchString(s) && len(strings.TrimSpace(s)) == 36 }

func isIP(s string) bool { return net.ParseIP(strings.TrimSpace(s)) != nil }

func ipKind(s string) Kind {
	if strings.Contains(s, ":") {
		return KindIPv6
	}
	return KindIPv4
}

// isHostname is intentionally strict. It gates values arriving under a hostname
// key, where model names and method names also appear.
func isHostname(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 253 {
		return false
	}
	if notHostnames[strings.ToLower(s)] {
		return false
	}
	if isUUID(s) || isIP(s) {
		return false
	}
	if strings.ContainsAny(s, " \t:/\\@=\"'") {
		return false
	}
	hasLetter := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return hasLetter
}

func isUsername(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	switch strings.ToLower(s) {
	case "all", "public", "default", "shared", "users", "home", "root":
		return false
	}
	return !strings.ContainsAny(s, " \t/\\:*?\"<>|")
}

func collect(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				fn(s)
			}
		}
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
