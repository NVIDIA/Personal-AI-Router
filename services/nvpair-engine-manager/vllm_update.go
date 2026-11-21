// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
)

func managedVLLMBinary(st *engineState, bin string) bool {
	if !managedUpdateBinary(st, bin) {
		return false
	}
	configured, err := launchForState(st, 0)
	if err != nil {
		return false
	}
	protected := func(args []string, env hfEnv) bool {
		stores := []string{env.home(), env.hubCache(), hfLaunchHub(args, env)}
		if model, ok := launchFlagValue(args, "--model"); ok && filepath.IsAbs(model) {
			stores = append(stores, model)
		}
		return modelStoresOutsideInstall(st, stores...)
	}
	if !protected(configured.Args, engineHFEnv(st)) {
		return false
	}
	st.mu.Lock()
	proc := st.proc
	st.mu.Unlock()
	if proc == nil {
		return true
	}
	launched := hfEnv{}
	for _, kv := range proc.cmd.Env {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			launched[key] = value
		}
	}
	return protected(proc.cmd.Args, launched)
}
