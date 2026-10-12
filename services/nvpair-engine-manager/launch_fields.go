// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// These are PAIR's policy fields, independent of any engine or CLI syntax.
// Their validators cannot be weakened by a manifest. Proxy ports belong to the
// broker and are deliberately not injectable into an engine's arguments.
type launchField struct {
	match     func(host string) string
	normalize func(value, host string) (string, error)
	canonical func(string) string
	managed   func(host, port string) string
	localOnly bool
}

var launchFields = map[string]launchField{
	"server.port": {
		match:     func(string) string { return `[+-]?[0-9]+` },
		normalize: normalizeServerPort, managed: func(_, port string) string { return port },
	},
	"server.host": {
		match:     regexp.QuoteMeta,
		normalize: normalizeServerHost, managed: func(host, _ string) string { return host },
	},
	"cors.origins": {
		match:     func(string) string { return `.*` },
		normalize: func(value, _ string) (string, error) { return normalizeCORSOrigins(value) },
		canonical: func(value string) string { return strings.Join(canonicalCORSPolicy(strings.Split(value, ",")), ",") }, localOnly: true,
	},
	"cors.enabled": {
		match:     func(string) string { return `(?i:true|false|t|f|1|0)` },
		normalize: normalizeCORSBoolean, localOnly: true,
	},
}

func normalizeServerPort(value, _ string) (string, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("the launch text contains an invalid server port")
	}
	return strconv.Itoa(port), nil
}

func normalizeServerHost(value, host string) (string, error) {
	if value != host {
		return "", fmt.Errorf("the engine bind address is fixed to loopback")
	}
	return host, nil
}

func normalizeCORSBoolean(value, _ string) (string, error) {
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return "", fmt.Errorf("CORS boolean controls require true or false")
	}
	return strconv.FormatBool(enabled), nil
}

// Canonicalize before validation and before handing values to the engine. This
// closes differences such as Ollama stripping extra quotes around an origin list.
func normalizeCORSOrigins(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "\"'")
	origins := []string{}
	for _, origin := range strings.Split(value, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		scheme, authority, ok := strings.Cut(origin, "://")
		if origin == "*" || authority == "*" || strings.HasPrefix(authority, "*:") {
			return "", fmt.Errorf("CORS origins cannot allow every origin; list each origin that needs access")
		}
		if !ok {
			return "", fmt.Errorf("CORS origins require a URL, for example \"http://localhost\"")
		}
		authority = strings.TrimSuffix(authority, "/")
		if authority == "" || strings.ContainsAny(authority, "/?#@\"'\\ \t\r\n") || strings.Contains(scheme, "*") {
			return "", fmt.Errorf("CORS origins must name a scheme and a specific host without credentials, paths, or quotes")
		}
		// net/url deliberately rejects wildcard ports, which origin matchers
		// support. Parse a numeric stand-in but preserve the declared wildcard.
		parseAuthority := authority
		if strings.HasSuffix(parseAuthority, ":*") {
			parseAuthority = strings.TrimSuffix(parseAuthority, "*") + "1"
		}
		u, err := url.Parse(scheme + "://" + parseAuthority)
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			return "", fmt.Errorf("invalid CORS origin")
		}
		host := u.Hostname()
		if strings.Contains(host, "*") && !(strings.HasPrefix(host, "*.") && len(host) > 2 && strings.Count(host, "*") == 1) {
			return "", fmt.Errorf("CORS origins cannot allow every origin; use only a specific host or wildcard subdomain")
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return "", fmt.Errorf("invalid CORS origin port")
			}
		}
		origins = append(origins, strings.ToLower(scheme+"://"+authority))
	}
	return strings.Join(origins, ","), nil
}

func canonicalCORSPolicy(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}
