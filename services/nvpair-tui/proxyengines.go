// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"nvpair-shared/engines"
)

func defaultProxyEngineCSV() string {
	return proxyEngineCSV(engines.ProxyDefaults())
}

func proxyEngineCSV(selected []engines.Engine) string {
	names := make([]string, len(selected))
	for i, engine := range selected {
		names[i] = engine.Name
	}
	return strings.Join(names, ",")
}

// parseProxyEngines narrows user input at the TUI boundary before the selected
// engines are passed to both the broker and the Proxies view.
func parseProxyEngines(csv string) ([]engines.Engine, error) {
	seen := map[string]bool{}
	var selected []engines.Engine
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		engine, ok := engines.ByName(name)
		if !ok {
			return nil, fmt.Errorf(
				"unknown engine %q; known engines are %s",
				name,
				strings.Join(engines.Names(), ", "),
			)
		}
		if !seen[name] {
			seen[name] = true
			selected = append(selected, engine)
		}
	}
	return selected, nil
}
