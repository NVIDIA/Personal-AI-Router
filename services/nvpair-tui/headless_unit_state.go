// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strconv"
	"strings"
)

var headlessUnitStateFields = []string{"LoadState", "ActiveState", "SubState", "MainPID", "ControlPID", "FragmentPath", "DropInPaths", "Job", "ControlGroup"}

// This classifies one complete manager observation. Native executable and
// cgroup checks must still agree before it becomes an ownership/stop receipt.
func parseHeadlessUnitState(raw, unitPath string) (string, int, string, error) {
	properties := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return "", 0, "", errors.New("headless unit manager response is incomplete")
		}
		if _, exists := properties[key]; exists {
			return "", 0, "", errors.New("headless unit manager response is ambiguous")
		}
		properties[key] = value
	}
	if len(properties) != len(headlessUnitStateFields) {
		return "", 0, "", errors.New("headless unit manager response is incomplete")
	}
	for _, field := range headlessUnitStateFields {
		if _, ok := properties[field]; !ok {
			return "", 0, "", errors.New("headless unit manager response is incomplete")
		}
	}
	if properties["LoadState"] != "loaded" || properties["FragmentPath"] != unitPath || properties["DropInPaths"] != "" || (properties["Job"] != "" && properties["Job"] != "0") {
		return "", 0, "", errors.New("headless unit owner, overrides or pending manager job changed")
	}
	pid, err := strconv.Atoi(properties["MainPID"])
	control, controlErr := strconv.Atoi(properties["ControlPID"])
	if err != nil || controlErr != nil || pid < 0 || control != 0 {
		return "", 0, "", errors.New("headless unit process state is unconfirmed")
	}
	if pid > 0 && properties["ActiveState"] == "active" && properties["SubState"] == "running" {
		return "running", pid, properties["ControlGroup"], nil
	}
	if pid == 0 && ((properties["ActiveState"] == "inactive" && properties["SubState"] == "dead") || (properties["ActiveState"] == "failed" && properties["SubState"] == "failed")) {
		return "stopped", 0, properties["ControlGroup"], nil
	}
	return "", 0, "", errors.New("headless unit is transitioning or termination is unconfirmed")
}
