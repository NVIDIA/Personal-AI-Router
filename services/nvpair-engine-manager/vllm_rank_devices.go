// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var vllmUverbsDevice = regexp.MustCompile(`^uverbs[0-9]{1,3}$`)

func vllmRankDevicePaths(gpuUUID string, lanes []vllmGroupRDMALane) ([]string, error) {
	return vllmRankDevicePathsAt(gpuUUID, lanes, "/proc/driver/nvidia/gpus", "/sys/class/infiniband", "/dev", vllmExactCharacterDevice)
}

func vllmExactCharacterDevice(name string) bool {
	info, err := os.Lstat(name)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice != 0
}

func vllmRankDevicePathsAt(gpuUUID string, lanes []vllmGroupRDMALane, gpuRoot, infinibandRoot, deviceRoot string, characterDevice func(string) bool) ([]string, error) {
	entries, err := os.ReadDir(gpuRoot)
	if err != nil || len(entries) == 0 || len(entries) > 64 {
		return nil, errors.New("bounded NVIDIA device inventory is unavailable")
	}
	minor := -1
	for _, entry := range entries {
		path := filepath.Join(gpuRoot, entry.Name(), "information")
		file, openErr := os.Open(path)
		if openErr != nil {
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 64<<10))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.New("NVIDIA device identity could not be read")
		}
		fields := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok {
				fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
		if !strings.EqualFold(fields["GPU UUID"], gpuUUID) {
			continue
		}
		value, parseErr := strconv.Atoi(fields["Device Minor"])
		if parseErr != nil || value < 0 || value > 255 || minor >= 0 {
			return nil, errors.New("reviewed GPU has an ambiguous device minor")
		}
		minor = value
	}
	if minor < 0 {
		return nil, errors.New("reviewed GPU device node is unavailable")
	}
	devices := []string{filepath.Join(deviceRoot, "nvidia"+strconv.Itoa(minor)), filepath.Join(deviceRoot, "nvidiactl"), filepath.Join(deviceRoot, "nvidia-modeset"), filepath.Join(deviceRoot, "nvidia-uvm"), filepath.Join(deviceRoot, "nvidia-uvm-tools")}
	seenHCA := map[string]bool{}
	for _, lane := range lanes {
		if !vllmGroupHCA.MatchString(lane.RDMADevice) || seenHCA[lane.RDMADevice] {
			return nil, errors.New("reviewed RDMA device identity is missing or duplicated")
		}
		seenHCA[lane.RDMADevice] = true
		verbs, readErr := os.ReadDir(filepath.Join(infinibandRoot, lane.RDMADevice, "device", "infiniband_verbs"))
		if readErr != nil || len(verbs) == 0 || len(verbs) > 8 {
			return nil, errors.New("reviewed HCA has no bounded uverbs identity")
		}
		matched := ""
		for _, entry := range verbs {
			if vllmUverbsDevice.MatchString(entry.Name()) {
				if matched != "" {
					return nil, errors.New("reviewed HCA has ambiguous uverbs identities")
				}
				matched = entry.Name()
			}
		}
		if matched == "" {
			return nil, errors.New("reviewed HCA has no uverbs identity")
		}
		devices = append(devices, filepath.Join(deviceRoot, "infiniband", matched))
	}
	slices.Sort(devices)
	for index, name := range devices {
		if index > 0 && name == devices[index-1] || !filepath.IsAbs(name) || filepath.Clean(name) != name || !characterDevice(name) {
			return nil, errors.New("rank device allowlist contains a duplicate, redirected or non-character device")
		}
	}
	return devices, nil
}
