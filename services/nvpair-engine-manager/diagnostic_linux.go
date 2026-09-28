// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func diagnosticProfileOwnership(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || identity.Uid != uint32(os.Getuid()) {
		return errors.New("diagnostic profiles must be operator-owned regular files, not group/world writable")
	}
	return nil
}

func diagnosticManagedTrustCurrentUID() (int, bool) { return os.Getuid(), true }

func diagnosticManagedTrustFileUID(info os.FileInfo) (int, bool) {
	if info == nil {
		return 0, false
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(identity.Uid), true
}

func diagnosticManagedTrustOpen(filename string) (*os.File, error) {
	return os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// Only this newly started process group is signalled. Parent death kills the
// rank child as well; no executable-name, GPU-PID, or host-wide kill is used.
func diagnosticProcess(ctx context.Context, path string, args, env []string, started func(*exec.Cmd) error) ([]byte, error) {
	return diagnosticProcessInput(ctx, path, args, env, nil, started)
}

func diagnosticProcessInput(ctx context.Context, path string, args, env []string, input io.Reader, started func(*exec.Cmd) error) ([]byte, error) {
	output, _, err := diagnosticProcessInputStreams(ctx, path, args, env, input, started, false)
	return output, err
}

func diagnosticProcessInputStreams(ctx context.Context, path string, args, env []string, input io.Reader, started func(*exec.Cmd) error, separateStderr bool) ([]byte, []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdin = input
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out := &diagnosticOutput{cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, out
	if separateStderr {
		cmd.Stderr = diagnosticStderr{out}
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	var startErr error
	if started != nil {
		startErr = started(cmd)
		if startErr != nil {
			cancel()
		}
	}
	err := cmd.Wait()
	if startErr != nil {
		err = errors.Join(err, startErr)
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	if out.exceeded {
		err = errors.Join(err, errors.New("diagnostic output limit exceeded"))
	}
	return append([]byte(nil), out.data.Bytes()...), append([]byte(nil), out.stderr.Bytes()...), err
}

func diagnosticProcessTicks(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// comm can contain spaces and parentheses; fields after its final ')' start
	// with stat field3. starttime is field22, hence index19 in this suffix.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}
func diagnosticSameProcess(pid int, ticks string) bool {
	return ticks != "" && diagnosticProcessTicks(pid) == ticks
}

func diagnosticLock(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func diagnosticCancelRank(ctx context.Context, dir string) (bool, error) {
	unlock, err := diagnosticLock(ctx, dir)
	if err != nil {
		return false, err
	}
	// The tombstone and launch use the same cross-process lock. A cancel that
	// overtakes MPI launch cannot acknowledge cleanup then allow a late rank.
	err = os.WriteFile(filepath.Join(dir, "cancelled"), []byte("cancelled\n"), 0600)
	var rank diagnosticRankRecord
	readErr := readDiagnosticJSON(filepath.Join(dir, "rank.json"), &rank)
	unlock()
	if err != nil {
		return false, err
	}
	if errors.Is(readErr, os.ErrNotExist) {
		return true, nil
	}
	if readErr != nil {
		return false, readErr
	}
	for {
		if rank.Done {
			if !rank.Clean {
				return false, errors.New("rank did not confirm cleanup")
			}
			return true, nil
		}
		// A launcher can kill the wrapper before it records Done. Its child has
		// parent-death SIGKILL; verify the saved PID/starttime is gone instead of
		// trusting a process name or killing whatever reused that PID.
		if rank.PID > 0 && rank.StartTicks != "" {
			_, statErr := os.Stat(fmt.Sprintf("/proc/%d", rank.PID))
			if errors.Is(statErr, os.ErrNotExist) {
				return true, nil
			}
			if statErr != nil {
				return false, statErr
			}
			if ticks := diagnosticProcessTicks(rank.PID); ticks != "" && ticks != rank.StartTicks {
				return true, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, errors.New("owned rank did not acknowledge cleanup before deadline")
		case <-time.After(50 * time.Millisecond):
		}
		if err := readDiagnosticJSON(filepath.Join(dir, "rank.json"), &rank); err != nil {
			return false, err
		}
	}
}

func diagnosticRankMain(group, id string) int {
	if !diagnosticToken.MatchString(group) || !diagnosticDigest.MatchString(id+id) {
		return 2
	}
	_, base := userPaths()
	dir := filepath.Join(base, "diagnostic-runs", id)
	var lease diagnosticLeaseRecord
	if err := readDiagnosticJSON(filepath.Join(dir, "lease.json"), &lease); err != nil || lease.Request.GroupID != group || lease.Request.OperationID != id {
		return 2
	}
	var profile diagnosticProfile
	managed := lease.Request.BootstrapPlanDigest != ""
	if managed {
		if !diagnosticDigest.MatchString(lease.Request.BootstrapPlanDigest) {
			return 2
		}
		var err error
		profile, err = loadDiagnosticBootstrapProfile(base, group, id, lease.Request.ProfileDigest)
		if err != nil {
			return 2
		}
	} else {
		profiles, err := loadDiagnosticProfiles(base)
		if err != nil {
			return 2
		}
		for _, p := range profiles {
			if p.GroupID == group {
				profile = p
				break
			}
		}
		if profile.Bootstrap != (diagnosticBootstrapProfile{}) {
			return 2
		}
	}
	if (!managed && profileDigest(profile) != lease.Request.ProfileDigest) || profile.validate() != nil {
		return 2
	}
	rankArgs, recipe := diagnosticNCCLArgs, diagnosticMPILegacyRecipe
	if managed {
		raw, err := readOnboardingFile(filepath.Join(dir, "bootstrap-plan.json"), 96<<10)
		if err != nil {
			return 2
		}
		plan, err := validateDiagnosticMPIBinding(profile, raw, lease.Request, true)
		if err != nil {
			return 2
		}
		recipe = plan.RecipeID
		rankArgs, _, err = diagnosticMPIRecipeArgs(recipe)
		if err != nil {
			return 2
		}
	}
	member, known := profile.member(lease.Member.NodeID)
	if !known || member != lease.Member {
		return 2
	}
	if managed {
		account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
		if err != nil || verifyDiagnosticManagedMember(member, os.Geteuid(), account.HomeDir, verifyDiagnosticManagedTool) != nil {
			return 2
		}
	} else {
		if err := lease.Member.NCCL.verify(); err != nil {
			return 2
		}
	}
	deadline := time.UnixMilli(lease.Request.ExpiresAt)
	if time.Until(deadline) <= 0 || time.Until(deadline) > diagnosticLease+time.Second {
		return 2
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	unlock, err := diagnosticLock(ctx, dir)
	if err != nil {
		return 2
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	var currentLease diagnosticLeaseRecord
	if readDiagnosticJSON(filepath.Join(dir, "lease.json"), &currentLease) != nil || currentLease != lease || time.Now().After(deadline) {
		return 2
	}
	if managed {
		currentProfile, err := loadDiagnosticBootstrapProfile(base, group, id, lease.Request.ProfileDigest)
		if err != nil {
			return 2
		}
		currentMember, ok := currentProfile.member(member.NodeID)
		if !ok || currentMember != lease.Member {
			return 2
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cancelled")); !errors.Is(err, os.ErrNotExist) {
		return 2
	}
	if _, err := os.Stat(filepath.Join(dir, "rank.json")); !errors.Is(err, os.ErrNotExist) {
		return 2
	}
	var env []string
	if managed {
		if member.Fabric != (diagnosticMemberFabric{}) && verifyDiagnosticMemberFabric(member.Fabric, net.InterfaceByName, (*net.Interface).Addrs) != nil {
			return 2
		}
		var err error
		env, err = diagnosticManagedRankEnvironment(profile, member, os.Environ())
		if err != nil {
			return 2
		}
		if recipe == diagnosticMPIQuickRecipe || recipe == diagnosticMPITripleRecipe {
			// UCX logs to stdout by default; preserve the numeric table on stdout.
			// This changes only logging destination, never transport selection.
			env = append(env, "UCX_LOG_FILE=stderr")
		}
	} else {
		for _, v := range os.Environ() {
			if strings.HasPrefix(v, "OMPI_") || strings.HasPrefix(v, "PMIX_") || strings.HasPrefix(v, "PMI_") {
				env = append(env, v)
			}
		}
		env = append(env, "PATH=/usr/bin:/bin", "HOME="+os.Getenv("HOME"), "CUDA_VISIBLE_DEVICES="+lease.Member.GPU, "NCCL_SOCKET_IFNAME=="+lease.Member.Interface, "NCCL_IB_DISABLE=1", "NCCL_DEBUG=WARN")
	}
	watchDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(filepath.Join(dir, "cancelled")); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	rank := diagnosticRankRecord{}
	output, stderr, runErr := diagnosticProcessInputStreams(ctx, lease.Member.NCCL.Path, rankArgs, env, nil, func(cmd *exec.Cmd) error {
		rank.PID, rank.StartTicks = cmd.Process.Pid, diagnosticProcessTicks(cmd.Process.Pid)
		if rank.StartTicks == "" {
			return errors.New("owned rank process identity unavailable")
		}
		if err := writeJSONAtomic(filepath.Join(dir, "rank.json"), rank); err != nil {
			return err
		}
		unlock()
		locked = false
		return nil
	}, recipe == diagnosticMPIQuickRecipe || recipe == diagnosticMPITripleRecipe)
	close(watchDone)
	if locked {
		unlock()
		locked = false
	}
	// Process.Wait joined the exact child; the configured nccl-tests recipe
	// does not launch an external process tree. The record survives MPI exit.
	rank.Done, rank.Clean = true, true
	if runErr != nil {
		rank.Error = runErr.Error()
	}
	ctxSave, cancelSave := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSave()
	saveUnlock, err := diagnosticLock(ctxSave, dir)
	if err != nil {
		return 2
	}
	err = writeJSONAtomic(filepath.Join(dir, "rank.json"), rank)
	saveUnlock()
	if err != nil {
		return 2
	}
	_, _ = os.Stdout.Write(output)
	_, _ = os.Stderr.Write(stderr)
	if runErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, "owned NCCL rank failed:", runErr)
		return 1
	}
	return 0
}
