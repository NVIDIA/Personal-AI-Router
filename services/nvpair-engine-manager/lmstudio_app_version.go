// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// lmStudioAppVersion reads the desktop application's version without opening a window.
func lmStudioAppVersion(root, runningVersion string) (string, error) {
	version := runningVersion
	if version == "" {
		path, err := lmStudioInstallPath(root, "app-install-location.json")
		if err != nil || path == "" {
			return "", err
		}
		dir := filepath.Dir(path)
		resources := filepath.Join(dir, "resources")
		if filepath.Base(dir) == "MacOS" {
			resources = filepath.Join(filepath.Dir(dir), "Resources")
		}
		var app struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := readLMStudioJSONFile(filepath.Join(resources, "app", "package.json"), &app, 1<<20); err != nil {
			return "", fmt.Errorf("read installed LM Studio desktop version: %w", err)
		}
		if app.Name != "lm-studio" {
			return "", errors.New("expected the installed LM Studio desktop package")
		}
		version = app.Version
	}
	version = strings.Replace(version, "+", "-", 1)
	if !lmStudioVersionRE.MatchString(version) {
		return "", errors.New("invalid LM Studio desktop version")
	}
	// After an update, migration history still describes the previous launch.
	if runningVersion == "" || strings.Contains(version, "-") {
		return version, nil
	}
	var history struct {
		// LM Studio kept its desktop version at the top level before adding per-product records.
		DesktopVersion string `json:"lastRecorderdAppVersion"`
		DesktopBuild   string `json:"lastRecordedAppBuildVersion"`
		Targets        []struct {
			Target  string `json:"target"`
			Version string `json:"lastRecordedAppVersion"`
			Build   string `json:"lastRecordedAppBuildVersion"`
		} `json:"targetHistories"`
	}
	// The CLI can omit the running build. Only use the desktop entry for this exact release.
	if readLMStudioJSONFile(filepath.Join(root, ".internal", "historical-version-info.json"), &history, 1<<20) == nil {
		for _, target := range history.Targets {
			if target.Target == "lmstudio" {
				history.DesktopVersion, history.DesktopBuild = target.Version, target.Build
				break
			}
		}
		full := history.DesktopVersion + "-" + history.DesktopBuild
		if history.DesktopVersion == version && lmStudioVersionRE.MatchString(full) {
			return full, nil
		}
	}
	return version, nil
}

// lmStudioInstallPath resolves either the desktop app or daemon marker published by LM Studio.
func lmStudioInstallPath(root, filename string) (string, error) {
	var location struct {
		Path string `json:"path"`
	}
	err := readLMStudioJSONFile(filepath.Join(root, ".internal", filename), &location, 8<<10)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filename, err)
	}
	if !filepath.IsAbs(location.Path) {
		return "", fmt.Errorf("invalid executable path in %s", filename)
	}
	info, err := os.Stat(location.Path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid executable in %s", filename)
	}
	return location.Path, nil
}

func readLMStudioJSONFile(path string, result any, limit int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), limit)
	}
	return json.Unmarshal(data, result)
}
