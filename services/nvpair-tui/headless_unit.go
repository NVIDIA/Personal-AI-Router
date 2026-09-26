// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const headlessUnit = "nvidia-pair-headless.service"
const headlessUnitStopSeconds = 145 // Above the 130-second parent drain.
const headlessServiceStopTimeout = 160 * time.Second
const headlessUpgradeTimeout = 375 * time.Second

func headlessServiceTimeout(action string) time.Duration {
	if action == "upgrade" {
		return headlessUpgradeTimeout
	}
	if action == "stop" || action == "uninstall" {
		return 250 * time.Second
	}
	return 30 * time.Second
}

func systemdQuoted(value string) (string, error) {
	if value == "" {
		return "", errors.New("empty systemd path")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", errors.New("systemd paths cannot contain control characters")
		}
	}
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(value)
	return "\"" + value + "\"", nil
}

// These are Linux unit paths even when the pure contract is tested on Windows.
// WorkingDirectory is literal: systemd does not remove quotes or C-unescape it.
func systemdWorkingDirectory(value string) (string, error) {
	if !path.IsAbs(value) {
		return "", errors.New("systemd working directory must be absolute")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", errors.New("systemd paths cannot contain control characters")
		}
	}
	value = strings.ReplaceAll(value, "%", "%%")
	if strings.HasSuffix(value, "\\") || strings.TrimRightFunc(value, unicode.IsSpace) != value {
		value += "/"
	}
	return value, nil
}

func headlessUnitText(executable, broker, config string) (string, error) {
	return headlessUnitTextWithStop(executable, broker, config, headlessUnitStopSeconds)
}

// Only the previous shipped 25-second unit and the current unit are recognized.
// This does not parse or rewrite arbitrary operator/systemd configuration.
func headlessUnitTextWithStop(executable, broker, config string, stopSeconds int) (string, error) {
	if stopSeconds != 25 && stopSeconds != headlessUnitStopSeconds {
		return "", errors.New("unsupported PAIR service stop contract")
	}
	for _, value := range []string{executable, broker, config} {
		if !path.IsAbs(value) || path.Clean(value) != value {
			return "", errors.New("headless service requires absolute installed paths")
		}
	}
	if path.Dir(executable) != path.Dir(broker) {
		return "", errors.New("headless service requires the TUI and broker from the same installed bundle")
	}
	self, err := systemdQuoted(strings.ReplaceAll(executable, "$", "$$"))
	if err != nil {
		return "", err
	}
	child, err := systemdQuoted(strings.ReplaceAll(broker, "$", "$$"))
	if err != nil {
		return "", err
	}
	directory, err := systemdWorkingDirectory(path.Dir(broker))
	if err != nil {
		return "", err
	}
	environment, err := systemdQuoted("XDG_CONFIG_HOME=" + config)
	if err != nil {
		return "", err
	}
	return "# Owned by NVIDIA Personal AI Router headless service; do not replace a foreign unit.\n[Unit]\nDescription=NVIDIA Personal AI Router headless backend\n\n[Service]\nType=simple\nExecStart=" + self + " --headless --broker-path " + child + "\nWorkingDirectory=" + directory + "\nEnvironment=" + environment + "\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec=" + strconv.Itoa(stopSeconds) + "\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n", nil
}
