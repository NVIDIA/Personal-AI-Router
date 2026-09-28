// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"strings"
)

type linuxFirewallManager string

const (
	linuxFirewallUFW       linuxFirewallManager = "ufw"
	linuxFirewallFirewalld linuxFirewallManager = "firewalld"
)

func classifyLinuxFirewallManager(
	ufwConfig []byte,
	readErr error,
) (linuxFirewallManager, error) {
	if readErr == nil {
		enabled, known := parseUFWEnabled(ufwConfig)
		if !known {
			return "", ErrFirewallUnavailable
		}
		if enabled {
			return linuxFirewallUFW, nil
		}
		return linuxFirewallFirewalld, nil
	}
	if errors.Is(readErr, fs.ErrNotExist) {
		return linuxFirewallFirewalld, nil
	}
	return "", ErrFirewallUnavailable
}

func firewalldOwnedRuleForEndpoint(address string) (string, error) {
	family, err := endpointAddressFamily(address)
	if err != nil {
		return "", err
	}
	return `rule family="` + family +
		`" port port="22" protocol="tcp" log prefix="` +
		windowsFirewallRuleName + `" accept`, nil
}

func endpointAddressFamily(address string) (string, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", ErrUnsupportedIdentity
	}
	family := "ipv6"
	if ip.To4() != nil {
		family = "ipv4"
	}
	return family, nil
}

func firewalldNativeEffects(
	ctx context.Context,
	runner commandRunner,
	rule string,
) []nativeEffect {
	scopes := []struct {
		id       string
		listArgs []string
		addArgs  []string
	}{
		{
			id:       "firewall:firewalld-runtime-add",
			listArgs: []string{"--list-rich-rules"},
			addArgs:  []string{"--add-rich-rule", rule},
		},
		{
			id:       "firewall:firewalld-permanent-add",
			listArgs: []string{"--permanent", "--list-rich-rules"},
			addArgs:  []string{"--permanent", "--add-rich-rule", rule},
		},
	}
	effects := make([]nativeEffect, 0, len(scopes))
	for _, scope := range scopes {
		scope := scope
		effects = append(effects, nativeEffect{
			ID:         scope.id,
			ReplaySafe: true,
			Satisfied: func() (bool, error) {
				output, err := runner.Run(ctx, commandSpec{
					Path: "/usr/bin/firewall-cmd",
					Args: scope.listArgs,
				})
				if err != nil {
					return false, ErrFirewallUnavailable
				}
				present, exact, err := parseFirewalldOwnedRule(output, rule)
				if err != nil {
					return false, ErrFirewallUnavailable
				}
				if present && !exact {
					return false, ErrForeignCollision
				}
				return exact, nil
			},
			Apply: func() error {
				_, err := runner.Run(ctx, commandSpec{
					Path: "/usr/bin/firewall-cmd",
					Args: scope.addArgs,
				})
				return err
			},
		})
	}
	return effects
}

func parseUFWEnabled(raw []byte) (bool, bool) {
	found := false
	enabled := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "ENABLED" {
			continue
		}
		if found {
			return false, false
		}
		found = true
		switch value {
		case "yes":
			enabled = true
		case "no":
		default:
			return false, false
		}
	}
	return enabled, found
}

func parseUFWIPv6(raw []byte) (bool, bool) {
	found := false
	enabled := false
	for _, line := range strings.Split(
		strings.ReplaceAll(string(raw), "\r\n", "\n"),
		"\n",
	) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "IPV6" {
			continue
		}
		if found {
			return false, false
		}
		found = true
		switch value {
		case "yes":
			enabled = true
		case "no":
		default:
			return false, false
		}
	}
	return enabled, found
}

func parseUFWOwnedRule(raw []byte) (bool, bool, error) {
	return parseUFWOwnedConfigRule(raw, "ipv4")
}

func parseUFWOwnedConfigRule(
	raw []byte,
	family string,
) (bool, bool, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	cidr := "0.0.0.0/0"
	chain := "ufw-user-input"
	if family == "ipv6" {
		cidr = "::/0"
		chain = "ufw6-user-input"
	} else if family != "ipv4" {
		return false, false, ErrFirewallUnavailable
	}
	tuple := "### tuple ### allow tcp 22 " + cidr + " any " + cidr +
		" in comment=" +
		windowsFirewallRuleName
	rawRule := "-A " + chain + " -p tcp --dport 22 -j ACCEPT"
	markerIndex := -1
	rawRuleCount := 0
	for index, line := range lines {
		if strings.Contains(line, windowsFirewallRuleName) {
			if markerIndex != -1 {
				return false, false, ErrFirewallUnavailable
			}
			markerIndex = index
		}
		if strings.TrimSpace(line) == rawRule {
			rawRuleCount++
		}
	}
	if rawRuleCount > 1 {
		return false, false, ErrFirewallUnavailable
	}
	if markerIndex == -1 {
		return false, false, nil
	}
	exact := strings.TrimSpace(lines[markerIndex]) == tuple &&
		markerIndex+1 < len(lines) &&
		strings.TrimSpace(lines[markerIndex+1]) == rawRule
	return true, exact, nil
}

func parseUFWRuntimeRule(
	output string,
	family string,
) (bool, bool, error) {
	chain := "ufw-user-input"
	if family == "ipv6" {
		chain = "ufw6-user-input"
	} else if family != "ipv4" {
		return false, false, ErrFirewallUnavailable
	}
	exactRule := "-A " + chain +
		" -p tcp -m tcp --dport 22 -j ACCEPT"
	count := 0
	exact := false
	for _, line := range strings.Split(
		strings.ReplaceAll(output, "\r\n", "\n"),
		"\n",
	) {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "-A "+chain) ||
			!strings.Contains(trimmed, "--dport 22") {
			continue
		}
		count++
		exact = trimmed == exactRule
	}
	if count > 1 {
		return false, false, ErrFirewallUnavailable
	}
	return count == 1, count == 1 && exact, nil
}

func parseFirewalldOwnedRule(
	output string,
	expectedRule string,
) (bool, bool, error) {
	count := 0
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.Contains(line, windowsFirewallRuleName) {
			count++
			if strings.TrimSpace(line) != expectedRule {
				return true, false, nil
			}
		}
	}
	if count > 1 {
		return false, false, ErrFirewallUnavailable
	}
	return count == 1, count == 1, nil
}
