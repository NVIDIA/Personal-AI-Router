// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	settings "nvpair-shared/enginesettings"
)

// externalInstance reports what the engine's own runtime state says about
// the instance PAIR adopted at port: the ready file that nominated it and,
// through the manifest's instance_info, the engine's launch config linked to
// it by name. Settings display only - nothing here is ever applied, started
// or stopped. Returns nil when the engine declares no instance_info, no live
// ready file matches the port, or the linked config cannot be read.
func (e *Executor) externalInstance(st *engineState, port int) *settings.ExternalInstance {
	st.mu.Lock()
	apf := st.plat.Runtime.AdoptPortFiles
	st.mu.Unlock()
	if apf == nil || apf.InstanceInfo == nil || port < 1 {
		return nil
	}
	ii := apf.InstanceInfo
	portField := apf.PortField
	if portField == "" {
		portField = "port"
	}
	for _, g := range apf.Globs {
		matches, err := filepath.Glob(expandPath(g))
		if err != nil {
			continue
		}
		for _, path := range matches {
			doc := readJSONDoc(path)
			if doc == nil || intField(doc, portField) != port {
				continue
			}
			// A stale ready file for a dead instance must not describe the
			// live port; the same check discovery would want if it re-probed.
			if pid := intField(doc, "pid"); pid > 0 && !processAlive(pid) {
				continue
			}
			info := &settings.ExternalInstance{
				Port:      port,
				Host:      stringField(doc, "bound_host"),
				Model:     stringField(doc, "model_id"),
				ReadyFile: path,
			}
			base := strings.TrimSuffix(filepath.Base(path), ".ready.json")
			base = strings.TrimPrefix(base, ii.StripPrefix)
			preset := stripTrailingHexToken(base)
			if preset == "" {
				continue
			}
			info.Preset = preset
			configPath := filepath.Join(expandPath(ii.ConfigDir), preset+".json")
			cfg := readJSONDoc(configPath)
			if cfg == nil {
				continue
			}
			info.ConfigFile = configPath
			if h := stringField(cfg, "host"); h != "" {
				info.Host = h
			}
			argsField := ii.ArgsField
			if argsField == "" {
				argsField = "args"
			}
			info.Args = stringSlice(cfg, argsField)
			if mf := ii.ModelField; mf != "" {
				if m := stringField(cfg, mf); m != "" {
					info.Model = m
				}
			}
			return info
		}
	}
	return nil
}

func readJSONDoc(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return doc
}

func intField(doc map[string]any, field string) int {
	if field == "" {
		return 0
	}
	v, ok := doc[field].(float64)
	if !ok {
		return 0
	}
	return int(v)
}

func stringField(doc map[string]any, field string) string {
	if field == "" {
		return ""
	}
	s, _ := doc[field].(string)
	return s
}

func stringSlice(doc map[string]any, field string) []string {
	if field == "" {
		return nil
	}
	list, ok := doc[field].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// stripTrailingHexToken removes a final "-<hex>" segment of 12 or more
// characters: the token engines append to per-instance state file names
// (Strata: launch-<preset>-<hash>.ready.json). A preset name whose last
// segment is itself long lowercase hex would lose it - an accepted trade
// for name-based linking, documented in MANIFEST.md.
func stripTrailingHexToken(s string) string {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return s
	}
	tok := s[i+1:]
	if len(tok) < 12 {
		return s
	}
	for _, c := range []byte(tok) {
		hex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !hex {
			return s
		}
	}
	return s[:i]
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	sigErr := p.Signal(syscall.Signal(0))
	// EPERM means the process exists but belongs to someone else.
	return sigErr == nil || errors.Is(sigErr, syscall.EPERM)
}
