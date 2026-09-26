// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type headlessCommandOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
}

func (w *headlessCommandOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer.Len()+len(data) > 8192 {
		w.exceeded = true
		w.cancel()
		return 0, errors.New("service command output exceeded its limit")
	}
	return w.buffer.Write(data)
}

func boundedHeadlessCommand(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &headlessCommandOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.exceeded {
		return "", errors.New("service command output exceeded its limit")
	}
	return strings.TrimSpace(output.buffer.String()), err
}

// Publish a fully written same-directory file without replacing any existing
// name. A failed write leaves only our temporary file, never a partial unit.
func installHeadlessUnit(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".nvpair-headless-unit-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err != nil {
		return errors.New("headless unit ownership changed during install; existing unit was not replaced")
	}
	return nil
}

func readOwnedHeadlessUnit(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || identity.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("headless unit is not a private operator-owned regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return nil, errors.New("headless unit exceeds its supported size")
	}
	return data, nil
}

func verifyLoadedHeadlessUnit(ctx context.Context, path string) error {
	fragment, err := headlessSystemctl(ctx, "show", headlessUnit, "--property=FragmentPath", "--value")
	if err != nil || filepath.Clean(fragment) != filepath.Clean(path) {
		return errors.New("systemd resolved a different headless unit; no service action was performed")
	}
	dropins, err := headlessSystemctl(ctx, "show", headlessUnit, "--property=DropInPaths", "--value")
	if err != nil || dropins != "" {
		return errors.New("headless unit has unreviewed drop-in overrides; no service action was performed")
	}
	return nil
}

func verifyHeadlessRunningExecutable(ctx context.Context, executable string) error {
	raw, err := headlessSystemctl(ctx, "show", headlessUnit, "--property=MainPID", "--value")
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || pid < 0 {
		return errors.New("running unit ownership could not be verified")
	}
	if pid == 0 {
		return nil
	}
	actual, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return errors.New("running unit process could not be identified")
	}
	expected, err := os.Stat(executable)
	if err != nil || !os.SameFile(actual, expected) {
		return errors.New("running headless unit belongs to a different executable; it was not stopped or replaced")
	}
	return nil
}

// A missing unit file does not prove that the user manager forgot a running
// unit. Require a complete, mutually consistent observation before reporting
// not-installed or allowing a new definition to shadow the fixed unit name.
func confirmHeadlessUnitAbsent(ctx context.Context, run func(context.Context, ...string) (string, error)) error {
	output, err := run(ctx, "show", headlessUnit, "--property=LoadState", "--property=ActiveState", "--property=MainPID", "--property=FragmentPath", "--property=DropInPaths")
	if err != nil {
		return errors.New("unit file is absent but user-manager state is unavailable; cleanup is unconfirmed")
	}
	properties := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("unit-file absence was not confirmed by a supported user-manager response")
		}
		if _, duplicate := properties[key]; duplicate {
			return errors.New("user-manager unit state is inconsistent")
		}
		properties[key] = value
	}
	for _, key := range []string{"LoadState", "ActiveState", "MainPID", "FragmentPath", "DropInPaths"} {
		if _, ok := properties[key]; !ok {
			return errors.New("user-manager unit state is incomplete; cleanup is unconfirmed")
		}
	}
	pid, err := strconv.ParseUint(properties["MainPID"], 10, 64)
	if err != nil {
		return errors.New("user-manager process ownership is unknown")
	}
	if properties["LoadState"] != "not-found" || properties["ActiveState"] != "inactive" || pid != 0 || properties["FragmentPath"] != "" || properties["DropInPaths"] != "" {
		return errors.New("unit file is absent but a loaded, active, or overridden unit remains; preserve its owner and reconcile before cleanup")
	}
	return nil
}

// Commands are fixed product lifecycle actions, never UI-supplied shell text.
// The user manager, not an SSH session or tmux, owns the resulting parent.
func headlessSystemctl(ctx context.Context, args ...string) (string, error) {
	budget := 25 * time.Second
	if len(args) > 0 && args[0] == "stop" {
		budget = headlessServiceStopTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	output, err := boundedHeadlessCommand(ctx, "/usr/bin/systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return "", fmt.Errorf("user systemd action failed (%s); verify the owning account's user manager", args[0])
	}
	return output, nil
}

func headlessLifetime(requested string, lingering bool) (string, error) {
	if requested != "persistent" && requested != "session" {
		return "", errors.New("startup lifetime must be persistent or session")
	}
	if lingering {
		return "persistent", nil
	}
	if requested == "session" {
		return "session", nil
	}
	return "", errors.New("persistent PAIR background startup requires approved user lingering; no fallback to session-only startup was applied")
}

func headlessLinger(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := boundedHeadlessCommand(ctx, "/usr/bin/loginctl", "show-user", strconv.Itoa(os.Geteuid()), "--property=Linger", "--value")
	if err != nil || (output != "yes" && output != "no") {
		return false, errors.New("the owning account's background-startup lifetime could not be verified")
	}
	return output == "yes", nil
}

func headlessPersistencePrerequisites(ctx context.Context, requested string) (string, error) {
	if _, err := headlessSocketPath(); err != nil {
		return "", err
	}
	if _, err := headlessSystemctl(ctx, "show", "--property=Version", "--value"); err != nil {
		return "", err
	}
	lingering, err := headlessLinger(ctx)
	if err != nil {
		return "", err
	}
	return headlessLifetime(requested, lingering)
}

func runHeadlessService(ctx context.Context, action, brokerOverride, lifetime string) (any, error) {
	if lifetime != "persistent" && lifetime != "session" {
		return nil, errors.New("startup lifetime must be persistent or session")
	}
	switch action {
	case "install", "start", "stop", "status", "uninstall":
	default:
		return nil, errors.New("unknown headless service action")
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	broker, err := resolveBrokerPath(brokerOverride)
	if err != nil {
		return nil, err
	}
	broker, err = filepath.Abs(broker)
	if err != nil {
		return nil, err
	}
	content, err := headlessUnitText(executable, broker, config)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(config, "systemd", "user", headlessUnit)
	current, readErr := readOwnedHeadlessUnit(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, errors.New("cannot inspect existing headless service ownership")
	}
	installed := readErr == nil
	legacy, legacyErr := headlessUnitTextWithStop(executable, broker, config, 25)
	legacyInstalled := installed && legacyErr == nil && string(current) == legacy
	if !installed {
		if err := confirmHeadlessUnitAbsent(ctx, headlessSystemctl); err != nil {
			return nil, err
		}
	}
	if installed && string(current) != content && !legacyInstalled {
		return nil, errors.New("an existing headless unit has different ownership or configuration; it was not replaced")
	}
	if installed && action != "install" {
		if err := verifyLoadedHeadlessUnit(ctx, path); err != nil {
			return nil, err
		}
		if err := verifyHeadlessRunningExecutable(ctx, executable); err != nil {
			return nil, err
		}
	}
	effective := "unknown"
	result := func(state string) any {
		persistence := "user-systemd"
		if effective == "session" {
			persistence = "user-session"
		}
		return map[string]string{"unit": headlessUnit, "operation": action, "state": state, "persistence": persistence, "requestedLifetime": lifetime, "effectiveLifetime": effective}
	}
	if action == "status" {
		if !installed {
			return result("not-installed"), nil
		}
		state, err := headlessSystemctl(ctx, "show", headlessUnit, "--property=ActiveState", "--value")
		if err != nil {
			return nil, err
		}
		if lingering, err := headlessLinger(ctx); err == nil {
			effective = "session"
			if lingering {
				effective = "persistent"
			}
		}
		return result(state), nil
	}
	if action == "install" || action == "start" {
		var err error
		if effective, err = headlessPersistencePrerequisites(ctx, lifetime); err != nil {
			return nil, err
		}
	}
	if legacyInstalled {
		// The same executable/config owner may adopt its longer drain budget.
		// A different installed bundle still requires the reviewed upgrade API.
		if err := verifyLoadedHeadlessUnit(ctx, path); err != nil {
			return nil, err
		}
		if err := verifyHeadlessRunningExecutable(ctx, executable); err != nil {
			return nil, err
		}
		if err := (nativeHeadlessUpgrade{unitPath: path}).replaceUnit(string(current), content); err != nil {
			return nil, err
		}
		if _, err := headlessSystemctl(ctx, "daemon-reload"); err != nil {
			return nil, err
		}
	}
	if action == "install" {
		// A failed prior daemon-reload can be retried for our exact complete
		// file; an already-loaded unit elsewhere is not ours to shadow.
		fragment, err := headlessSystemctl(ctx, "show", headlessUnit, "--property=FragmentPath", "--value")
		if err != nil {
			return nil, err
		}
		if fragment != "" && filepath.Clean(fragment) != filepath.Clean(path) {
			return nil, errors.New("another headless unit is already registered; it was not shadowed")
		}
		if fragment != "" {
			if err := verifyHeadlessRunningExecutable(ctx, executable); err != nil {
				return nil, err
			}
		}
		if !installed {
			if err := installHeadlessUnit(path, content); err != nil {
				return nil, err
			}
		}
		if _, err := headlessSystemctl(ctx, "daemon-reload"); err != nil {
			return nil, err
		}
		if err := verifyLoadedHeadlessUnit(ctx, path); err != nil {
			return nil, err
		}
		if _, err := headlessSystemctl(ctx, "enable", headlessUnit); err != nil {
			return nil, err
		}
		return result("installed"), nil
	}
	if !installed {
		if action == "uninstall" {
			return result("not-installed"), nil
		}
		return nil, errors.New("PAIR headless service is not installed")
	}
	if action == "start" {
		if _, err := headlessSystemctl(ctx, "start", headlessUnit); err != nil {
			return nil, err
		}
		return result("started"), nil
	}
	if _, err := headlessSystemctl(ctx, "stop", headlessUnit); err != nil {
		return nil, err
	}
	if action == "stop" {
		return result("stopped"), nil
	}
	if _, err := headlessSystemctl(ctx, "disable", headlessUnit); err != nil {
		return nil, err
	}
	// Recheck exact content immediately before removing only this fixed unit.
	current, err = readOwnedHeadlessUnit(path)
	if err != nil || string(current) != content {
		return nil, errors.New("headless unit changed during removal; no file was removed")
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	if _, err := headlessSystemctl(ctx, "daemon-reload"); err != nil {
		return nil, err
	}
	return result("not-installed"), nil
}
