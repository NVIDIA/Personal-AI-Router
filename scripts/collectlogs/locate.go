// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// Directory and file names must match the desktop constants that create them:
//
//	appOrg / appDataDir -> desktop/src/shared/constants/app.ts
//	logs/, nvpair.jsonl -> desktop/src/shared/utils/log.ts
//
// There is one canonical location per platform. The app migrates its
// pre-rename data directory on launch, so this tool does not search
// alternative layouts.
const (
	appOrg         = "Nvidia Corporation"
	appDataDir     = "Personal AI Router"
	logDirName     = "logs"
	activeLogName  = "nvpair.jsonl"
	rotatedLogName = "nvpair.1.jsonl"
)

// localLogDir resolves the log directory for the current user on this machine.
func localLogDir() (string, error) {
	base, err := baseRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appOrg, appDataDir, logDirName), nil
}

func baseRoot() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("LOCALAPPDATA is not set")
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	default:
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return v, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config"), nil
	}
}

// localLogFiles returns the local log files oldest first, so that reading them
// in order yields an ascending timeline across a rotation boundary.
func localLogFiles() ([]string, error) {
	dir, err := localLogDir()
	if err != nil {
		return nil, err
	}
	var found []string
	for _, name := range []string{rotatedLogName, activeLogName} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no log files in %s (expected %s)", dir, activeLogName)
	}
	return found, nil
}

// inputGroup is one machine's logs, and produces exactly one output file.
//
// A rotation pair belongs to one machine, so nvpair.1.jsonl and nvpair.jsonl form
// a single group and are written back as one file. Separate exported bundles come
// from separate machines, so each is its own group.
type inputGroup struct {
	// source names where the group came from, for progress messages.
	source string
	// files in read order, oldest first, so the output stays ascending in time.
	files []string
	// node is the producing machine, learned during discovery. It supplies the
	// output file name once tokens are allocated.
	node *Node
	// bundle records whether the input carried the exported markdown sections,
	// which decides the output format.
	bundle bool
}

// resolveGroups turns the -in arguments into groups. With no arguments the local
// log directory is used.
func resolveGroups(inputs []string) ([]inputGroup, error) {
	if len(inputs) == 0 {
		files, err := localLogFiles()
		if err != nil {
			return nil, fmt.Errorf("%w\n       pass -in <path> to read logs collected elsewhere", err)
		}
		dir, _ := localLogDir()
		return []inputGroup{{source: dir, files: files}}, nil
	}

	var groups []inputGroup
	for _, p := range inputs {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			groups = append(groups, inputGroup{source: p, files: []string{p}})
			continue
		}

		rotation, bundles, err := logFilesInDir(p)
		if err != nil {
			return nil, err
		}
		if len(rotation) > 0 {
			groups = append(groups, inputGroup{source: p, files: rotation})
		}
		// Bundles in one directory were exported by different machines, so they
		// must not be pooled into a single output.
		for _, b := range bundles {
			groups = append(groups, inputGroup{source: b, files: []string{b}})
		}
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no input files found")
	}
	return groups, nil
}

// logFilesInDir reports the log rotation pair and any exported bundles found in a
// directory. Only these names are read; nothing else in the directory is touched.
func logFilesInDir(dir string) (rotation, bundles []string, err error) {
	for _, name := range []string{rotatedLogName, activeLogName} {
		path := filepath.Join(dir, name)
		if _, statErr := os.Stat(path); statErr == nil {
			rotation = append(rotation, path)
		}
	}

	// Exported bundles are what a user hands over after pressing Save logs.
	bundles, err = filepath.Glob(filepath.Join(dir, "nvpair-logs-*.txt"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(bundles)

	if len(rotation) == 0 && len(bundles) == 0 {
		return nil, nil, fmt.Errorf("no log files or exported bundles in %s", dir)
	}
	return rotation, bundles, nil
}
