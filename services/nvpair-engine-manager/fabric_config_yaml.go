// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// fabricNetplanUnrelated recognizes a small, disjoint Wi-Fi subset, not the
// complete Netplan schema or its merged effective configuration. Callers retain
// the complete configuration inventory and file digests. YAML errors and values
// are never returned or logged: access-point names and auth values may be secret.
func fabricNetplanUnrelated(data []byte, targets []fabricInterface, links map[string]fabricConfigLink) bool {
	if len(data) > fabricNativeMaxBytes || bytes.ContainsRune(data, 0) {
		return false
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err == io.EOF // Empty and comment-only input has no definition.
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return false
	}
	nodes := 0
	if !fabricYAMLTree(&document, 0, &nodes) || document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return false
	}
	root, ok := fabricYAMLFields(document.Content[0], "network")
	if !ok || len(root) != 1 {
		return false
	}
	network, ok := fabricYAMLFields(root["network"], "version", "renderer", "wifis", "ethernets")
	if !ok || network["version"] == nil || network["version"].Tag != "!!int" || network["version"].Value != "2" {
		return false
	}
	if renderer := network["renderer"]; renderer != nil && !fabricYAMLStringIs(renderer, "NetworkManager") {
		return false
	}
	wifis, ethernets := network["wifis"], network["ethernets"]
	if wifis == nil && ethernets == nil {
		return len(network) == 2 && fabricYAMLStringIs(network["renderer"], "NetworkManager")
	}
	if wifis != nil && ethernets != nil {
		return false
	}
	selectedNames, ok := fabricYAMLSelectedNames(targets, links)
	if !ok {
		return false
	}
	if wifis != nil {
		return fabricYAMLWiFisUnrelated(wifis, network["renderer"], selectedNames)
	}
	return fabricYAMLEthernetsUnrelated(ethernets, network["renderer"], selectedNames, targets)
}

func fabricYAMLSelectedNames(targets []fabricInterface, links map[string]fabricConfigLink) (map[string]bool, bool) {
	if len(targets) == 0 {
		return nil, false
	}
	selectedNames := map[string]bool{}
	for _, target := range targets {
		link, found := links[target.Name]
		if !found || !link.Known || target.Name == "" || !slices.Contains(link.Names, target.Name) {
			return nil, false
		}
		for _, name := range link.Names {
			if name == "" {
				return nil, false
			}
			selectedNames[name] = true
		}
	}
	return selectedNames, true
}

func fabricYAMLDisjointMatch(node *yaml.Node, selectedNames map[string]bool) bool {
	match, ok := fabricYAMLFields(node, "name")
	return ok && len(match) == 1 && fabricYAMLLiteralName(match["name"]) && !selectedNames[match["name"].Value]
}

func fabricYAMLWiFisUnrelated(wifis, networkRenderer *yaml.Node, selectedNames map[string]bool) bool {
	if wifis.Kind != yaml.MappingNode || len(wifis.Content) == 0 {
		return false
	}
	for i := 1; i < len(wifis.Content); i += 2 {
		wifi, ok := fabricYAMLFields(wifis.Content[i], "renderer", "match", "dhcp4", "dhcp6", "access-points", "networkmanager")
		if !ok {
			return false
		}
		renderer := wifi["renderer"]
		if renderer == nil {
			renderer = networkRenderer
		}
		if !fabricYAMLStringIs(renderer, "NetworkManager") {
			return false
		}
		if !fabricYAMLDisjointMatch(wifi["match"], selectedNames) {
			return false
		}
		for _, field := range []string{"dhcp4", "dhcp6"} {
			if value := wifi[field]; value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!bool") {
				return false
			}
		}
		if !fabricYAMLNetworkManager(wifi["networkmanager"], "wifi") || !fabricYAMLAccessPoints(wifi["access-points"]) {
			return false
		}
	}
	return true
}

func fabricYAMLEthernetsUnrelated(ethernets, networkRenderer *yaml.Node, selectedNames map[string]bool, targets []fabricInterface) bool {
	if ethernets.Kind != yaml.MappingNode || len(ethernets.Content) == 0 {
		return false
	}
	for i := 1; i < len(ethernets.Content); i += 2 {
		ethernet, ok := fabricYAMLFields(ethernets.Content[i], "renderer", "match", "addresses", "mtu", "wakeonlan", "networkmanager")
		if !ok || ethernet["renderer"] == nil || ethernet["networkmanager"] == nil {
			return false
		}
		renderer := ethernet["renderer"]
		if renderer == nil {
			renderer = networkRenderer
		}
		if !fabricYAMLStringIs(renderer, "NetworkManager") || !fabricYAMLDisjointMatch(ethernet["match"], selectedNames) ||
			!fabricYAMLEthernetAddresses(ethernet["addresses"], targets) || !fabricYAMLInteger(ethernet["mtu"], 576, 65535) ||
			!fabricYAMLBool(ethernet["wakeonlan"]) || !fabricYAMLNetworkManager(ethernet["networkmanager"], "ethernet") {
			return false
		}
	}
	return true
}

func fabricYAMLEthernetAddresses(node *yaml.Node, targets []fabricInterface) bool {
	if node == nil || node.Kind != yaml.SequenceNode || len(node.Content) == 0 || len(node.Content) > 16 {
		return false
	}
	for _, value := range node.Content {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return false
		}
		prefix, err := netip.ParsePrefix(value.Value)
		if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() {
			return false
		}
		for _, target := range targets {
			selected, err := fabricNativePrefix(target)
			if err != nil || prefix.Overlaps(selected) {
				return false
			}
		}
	}
	return true
}

func fabricYAMLInteger(node *yaml.Node, minimum, maximum int64) bool {
	if node == nil || node.Kind != yaml.ScalarNode || (node.Tag != "!!int" && node.Tag != "!!str") || node.Value == "" {
		return false
	}
	value, err := strconv.ParseInt(node.Value, 10, 64)
	return err == nil && value >= minimum && value <= maximum
}

func fabricYAMLUnsigned(node *yaml.Node, maximum uint64) bool {
	if node == nil || node.Kind != yaml.ScalarNode || (node.Tag != "!!int" && node.Tag != "!!str") || node.Value == "" || strings.IndexFunc(node.Value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return false
	}
	value, err := strconv.ParseUint(node.Value, 10, 64)
	return err == nil && value <= maximum
}

func fabricYAMLBool(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!bool"
}

// Walk the syntax tree before projecting fields, including opaque auth leaves.
// In particular, Node decoding must not silently resolve aliases or merge keys.
func fabricYAMLTree(node *yaml.Node, depth int, count *int) bool {
	*count += 1
	if node == nil || depth > 32 || *count > 8192 || node.Anchor != "" || node.Alias != nil {
		return false
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if node.Tag != "" || len(node.Content) != 1 {
			return false
		}
	case yaml.MappingNode:
		if node.Tag != "!!map" || len(node.Content)%2 != 0 {
			return false
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || seen[key.Value] {
				return false
			}
			seen[key.Value] = true
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return false
		}
	case yaml.ScalarNode:
		if !slices.Contains([]string{"!!str", "!!bool", "!!int", "!!null"}, node.Tag) {
			return false
		}
	default:
		return false
	}
	for _, child := range node.Content {
		if !fabricYAMLTree(child, depth+1, count) {
			return false
		}
	}
	return true
}

func fabricYAMLFields(node *yaml.Node, allowed ...string) (map[string]*yaml.Node, bool) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, false
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !slices.Contains(allowed, key) {
			return nil, false
		}
		fields[key] = node.Content[i+1]
	}
	return fields, true
}

func fabricYAMLStringIs(node *yaml.Node, value string) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!str" && node.Value == value
}

func fabricYAMLLiteralName(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Value == "" || len(node.Value) > 127 {
		return false
	}
	for _, ch := range node.Value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("_.-", ch)) {
			return false
		}
	}
	return true
}

func fabricYAMLNetworkManager(node *yaml.Node, profile string) bool {
	if profile != "wifi" && profile != "ethernet" {
		return false
	}
	if node == nil {
		return profile == "wifi"
	}
	fields, ok := fabricYAMLFields(node, "name", "uuid", "passthrough")
	if !ok || profile == "ethernet" && (fields["name"] == nil || fields["uuid"] == nil || fields["passthrough"] == nil) {
		return false
	}
	for _, key := range []string{"name", "uuid"} {
		if value := fields[key]; value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
			return false
		}
	}
	if fields["passthrough"] == nil {
		return true
	}
	allowed := []string{"ipv6.addr-gen-mode", "ipv6.ip6-privacy", "proxy._"}
	switch profile {
	case "wifi":
		allowed = append(allowed, "wifi-security.auth-alg", "connection.timestamp", "wifi.powersave")
	case "ethernet":
		allowed = append(allowed, "ipv4.never-default", "ipv6.method")
	}
	// Both definition and AP passthrough can override generated NM settings.
	// Only observed, non-binding values are admitted; all other passthrough is held.
	passthrough, ok := fabricYAMLFields(fields["passthrough"], allowed...)
	if !ok {
		return false
	}
	for key, value := range passthrough {
		switch key {
		case "connection.timestamp":
			if !fabricYAMLUnsigned(value, ^uint64(0)) {
				return false
			}
		case "wifi.powersave":
			if !fabricYAMLUnsigned(value, 3) {
				return false
			}
		case "ipv4.never-default":
			if value == nil || value.Kind != yaml.ScalarNode || (value.Tag != "!!str" && value.Tag != "!!bool") || (value.Value != "true" && value.Value != "false") {
				return false
			}
		case "ipv6.method":
			if !fabricYAMLStringIs(value, "disabled") {
				return false
			}
		default:
			if value == nil || value.Kind != yaml.ScalarNode || (value.Tag != "!!str" && value.Tag != "!!int") || len(value.Value) > 128 {
				return false
			}
			for _, ch := range value.Value {
				if ch < 32 || ch > 126 {
					return false
				}
			}
			if key == "proxy._" && value.Value != "" {
				return false
			}
		}
	}
	return true
}

func fabricYAMLAccessPoints(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 1; i < len(node.Content); i += 2 {
		ap, ok := fabricYAMLFields(node.Content[i], "password", "auth", "networkmanager", "mode", "hidden")
		if !ok || !fabricYAMLNetworkManager(ap["networkmanager"], "wifi") {
			return false
		}
		if value := ap["password"]; value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
			return false
		}
		if value := ap["mode"]; value != nil && !fabricYAMLStringIs(value, "infrastructure") {
			return false
		}
		if value := ap["hidden"]; value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!bool") {
			return false
		}
		if ap["auth"] != nil {
			auth, ok := fabricYAMLFields(ap["auth"], "key-management", "password")
			if !ok {
				return false
			}
			for _, value := range auth {
				if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					return false
				}
			}
		}
	}
	return true
}
