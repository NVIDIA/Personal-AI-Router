// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const managedVLLMVersion = "0.29.0"

func pathInside(root, target string) (string, error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	targetResolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootResolved, targetResolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes the PAIR-owned vLLM directory")
	}
	return targetResolved, nil
}

func detectManagedVLLM(st *engineState) (installed bool, resultErr error) {
	return detectManagedVLLMRecord(st, false)
}

func detectManagedVLLMRecord(st *engineState, allowActivating bool) (installed bool, resultErr error) {
	defer func() {
		if resultErr != nil {
			st.mu.Lock()
			st.installed, st.binPath, st.version = false, "", ""
			st.mu.Unlock()
		}
	}()
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return false, err
	}
	if record.Removing {
		return false, fmt.Errorf("vLLM runtime removal is incomplete; retry Uninstall")
	}
	if record.Activating != nil && !allowActivating {
		return false, fmt.Errorf("vLLM activation recovery is incomplete")
	}
	if record.Active == "" {
		st.mu.Lock()
		st.installed, st.binPath, st.version = false, "", ""
		st.mu.Unlock()
		return false, nil
	}
	cli, receipt, err := validateVLLMEnvironment(st, record.Active)
	if err != nil {
		return false, err
	}
	if !recognizedManagedVLLMReceipt(st, receipt) {
		return false, fmt.Errorf("managed vLLM receipt does not match a pinned accepted recipe")
	}
	if err := verifyManagedVLLMVersion(context.Background(), st, record.Active, receipt); err != nil {
		return false, err
	}
	st.mu.Lock()
	st.installed, st.binPath, st.version = true, cli, receipt.Version
	st.mu.Unlock()
	return true, nil
}
